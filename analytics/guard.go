package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emoss08/keel"
)

// ComplexityLimits bound what a report's statements may do, beyond the definition limits. A zero
// field is no limit.
type ComplexityLimits struct {
	// MaxJoins caps the to-one relation paths joined.
	MaxJoins int
	// MaxToMany caps the to-many relation paths read.
	MaxToMany int
	// MaxPreAggs caps the pre-aggregations.
	MaxPreAggs int
	// MaxWindows caps the window columns.
	MaxWindows int
	// MaxScore caps Complexity.Score.
	MaxScore int
}

// checkComplexity refuses a report past the limits.
func checkComplexity(cx Complexity, l ComplexityLimits) error {
	over := func(n, limit int, what string) error {
		if limit > 0 && n > limit {
			return fail(CodeTooComplex, "", "the report %s %d; the most is %d", what, n, limit)
		}
		return nil
	}
	return errors.Join(over(cx.Joins, l.MaxJoins, "joins"),
		over(cx.ToMany, l.MaxToMany, "reads to-many relations"),
		over(cx.PreAggs, l.MaxPreAggs, "pre-aggregates"),
		over(cx.Windows, l.MaxWindows, "has window columns:"),
		over(cx.Score(), l.MaxScore, "scores"))
}

// planCache remembers EXPLAIN's estimates per tenant and statement shape for a short time, so a
// busy dashboard pays for EXPLAIN once per shape. It holds at most planCacheSize entries and drops
// expired ones first.
type planCache struct {
	mu      sync.Mutex
	entries map[string]planEntry
}

// planEntry is one remembered estimate, or one remembered pre-aggregation choice (all).
type planEntry struct {
	cost, rows float64
	all        bool
	expires    time.Time
}

// planCacheSize bounds the plan cache.
const planCacheSize = 4096

// get returns a remembered estimate.
func (pc *planCache) get(key string, now time.Time) (planEntry, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	e, ok := pc.entries[key]
	if !ok || now.After(e.expires) {
		return planEntry{}, false
	}
	return e, true
}

// put remembers an estimate, evicting expired entries, or any entry, when full.
func (pc *planCache) put(key string, e planEntry, now time.Time) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if len(pc.entries) >= planCacheSize {
		for k, old := range pc.entries {
			if now.After(old.expires) {
				delete(pc.entries, k)
			}
		}
		for k := range pc.entries {
			if len(pc.entries) < planCacheSize {
				break
			}
			delete(pc.entries, k)
		}
	}
	pc.entries[key] = e
}

// checkCost refuses a statement whose planned cost or rows exceed the runner's limits. The plan
// is EXPLAINed in the run's transaction, on the server that runs it.
func (r *Runner) checkCost(ctx context.Context, q *keel.DynQuery) error {
	if r.opts.MaxCost <= 0 && r.opts.MaxPlanRows <= 0 {
		return nil
	}
	e, err := r.plan(ctx, q)
	if err != nil {
		return err
	}
	if r.opts.MaxCost > 0 && e.cost > r.opts.MaxCost {
		return fail(CodeTooCostly, "", "the report would cost %.0f to run; the most is %.0f; add filters or remove columns", e.cost, r.opts.MaxCost)
	}
	if r.opts.MaxPlanRows > 0 && e.rows > r.opts.MaxPlanRows {
		return fail(CodeTooCostly, "", "the report would return about %.0f rows; the most is %.0f; add filters", e.rows, r.opts.MaxPlanRows)
	}
	return nil
}

// plan returns EXPLAIN's estimates for a statement, from the plan cache when the tenant ran its
// shape within ExplainTTL.
func (r *Runner) plan(ctx context.Context, q *keel.DynQuery) (planEntry, error) {
	key, err := r.planKey(ctx, q)
	if err != nil {
		return planEntry{}, err
	}
	now := time.Now()
	if e, ok := r.plans.get(key, now); ok {
		return e, nil
	}
	p, err := q.Explain(ctx, r.db, keel.ExplainOptions{})
	if err != nil {
		return planEntry{}, err
	}
	e := planEntry{cost: p.TotalCost, rows: p.Rows, expires: now.Add(r.opts.ExplainTTL)}
	r.plans.put(key, e, now)
	return e, nil
}

// planKey identifies a statement shape for a tenant: its text, without its values.
func (r *Runner) planKey(ctx context.Context, q *keel.DynQuery) (string, error) {
	sql, _, err := q.ToSQL(ctx, r.db)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(r.db.TenantKey(ctx) + "\x00" + sql))
	return hex.EncodeToString(sum[:]), nil
}

// choosePreAgg picks how a report whose to-many measures are pre-aggregated in CTEs under its
// filters reads them: restricted to the parents the filters select, or every parent in one pass,
// whichever the planner estimates cheaper. The choice is remembered with the plan estimates, per
// tenant and statement shape, so a busy dashboard pays for the two EXPLAINs once.
func (r *Runner) choosePreAgg(ctx context.Context, comp *Compiled) (*Compiled, error) {
	if !comp.choosable {
		return comp, nil
	}
	key, err := r.planKey(ctx, comp.Query)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if e, ok := r.plans.get("choice/"+key, now); ok {
		if e.all {
			return comp.withStrategy(ctx, StrategyCTEAll)
		}
		return comp, nil
	}
	all, err := comp.withStrategy(ctx, StrategyCTEAll)
	if err != nil {
		return nil, err
	}
	restricted, err := r.plan(ctx, comp.Query)
	if err != nil {
		return nil, err
	}
	every, err := r.plan(ctx, all.Query)
	if err != nil {
		return nil, err
	}
	pick := every.cost < restricted.cost
	r.plans.put("choice/"+key, planEntry{all: pick, expires: now.Add(r.opts.ExplainTTL)}, now)
	if pick {
		return all, nil
	}
	return comp, nil
}

// tenantLock is the first key of a tenant's advisory locks: a hash of its data-versions tenant key.
func tenantLock(tenant string) int32 {
	h := fnv.New32a()
	h.Write([]byte("keel-analytics/" + tenant))
	return int32(h.Sum32())
}

// slotSQL returns the statement that takes one of n advisory lock slots: CASE tries them in order
// and stops at the first it takes, and the statement returns a row only when it took one. The lock
// is transaction-scoped, so it is released with the run's transaction whatever happens to the
// process.
func slotSQL(n int) string {
	var b strings.Builder
	b.WriteString("SELECT 1 WHERE CASE")
	for i := range n {
		b.WriteString(" WHEN pg_try_advisory_xact_lock($1, $")
		b.WriteString(strconv.Itoa(i + 2))
		b.WriteString(") THEN true")
	}
	b.WriteString(" ELSE false END")
	return b.String()
}

// acquireSlot takes one of the tenant's slots on the run's connection, trying them from a random
// one so concurrent runs spread out, and waiting up to SlotWait for one to free.
func (r *Runner) acquireSlot(ctx context.Context) error {
	n := r.opts.Slots
	if n <= 0 {
		return nil
	}
	args := make([]any, n+1)
	args[0] = tenantLock(r.db.TenantKey(ctx))
	start := rand.IntN(n)
	for i := range n {
		args[i+1] = int32((start + i) % n)
	}
	deadline := time.Now().Add(r.opts.SlotWait)
	wait := 25 * time.Millisecond
	for {
		got, err := keel.Exec(ctx, r.db, r.slotSQL, args...)
		if err != nil {
			return err
		}
		if got > 0 {
			return nil
		}
		if time.Now().Add(wait).After(deadline) {
			return fail(CodeBusy, "", "the organization is already running %d reports; try again shortly", n)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, 500*time.Millisecond)
	}
}

// supersedeTag is the application_name of a keyed run: the tenant and the key hashed, so a key
// never reaches pg_stat_activity in clear and two tenants' keys never meet.
func supersedeTag(tenant, key string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + key))
	return "keel-analytics:" + hex.EncodeToString(sum[:16])
}

// cancelSQL cancels the statements of other backends tagged with an application_name.
const cancelSQL = "SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE application_name = $1 AND pid <> pg_backend_pid()"

// errSuperseded is the cancellation cause of a run a later run with its key replaced.
var errSuperseded = errors.New("analytics: superseded")

// supersede cancels the previous run with the same key: in this process through its context, in
// others through pg_cancel_backend on every server, found by its tag in pg_stat_activity. It
// returns the context of the new run, which a later run with the key cancels, and a function that
// releases the key.
func (r *Runner) supersede(ctx context.Context, tag string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	r.mu.Lock()
	if prev, ok := r.running[tag]; ok {
		prev.cancel(errSuperseded)
	}
	token := &running{cancel: cancel}
	r.running[tag] = token
	r.mu.Unlock()
	_, _ = keel.ExecAll(context.WithoutCancel(ctx), r.db, cancelSQL, tag)
	return ctx, func() {
		r.mu.Lock()
		if r.running[tag] == token {
			delete(r.running, tag)
		}
		r.mu.Unlock()
		cancel(nil)
	}
}

// running is a keyed run in this process.
type running struct {
	cancel context.CancelCauseFunc
}
