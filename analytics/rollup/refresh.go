package rollup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/schema"
)

// DefaultBatchSize is how many keel_rollup_dirty rows one refresh transaction claims.
const DefaultBatchSize = 500

// RefreshOptions configure Refresh and Rebuild.
type RefreshOptions struct {
	// BatchSize is how many dirty rows each transaction claims; default DefaultBatchSize. A
	// transaction recomputes the distinct slices among them.
	BatchSize int
	// MaxBatches bounds the transactions one call runs; zero runs until nothing claimable is left.
	MaxBatches int
	// StatementTimeout bounds each statement of a refresh transaction; zero means none.
	StatementTimeout time.Duration
}

// Report counts what a refresh did.
type Report struct {
	// Batches is the number of transactions that ran.
	Batches int
	// Slices is the number of (tenant, bucket) slices recomputed.
	Slices int
	// Tenants is the number of tenants rebuilt whole.
	Tenants int
	// Skipped is the number of slices left to another worker that was refreshing them.
	Skipped int
	// Rows is the number of rollup rows written.
	Rows int64
	// Duration is the call's wall time.
	Duration time.Duration
	// Rollups breaks the work down by rollup name.
	Rollups map[string]RollupReport
}

// RollupReport is one rollup's share of a Report.
type RollupReport struct {
	Slices  int
	Tenants int
	Rows    int64
}

// add counts one rollup's work.
func (rep *Report) add(name string, slices, tenants int, rows int64) {
	if rep.Rollups == nil {
		rep.Rollups = map[string]RollupReport{}
	}
	rr := rep.Rollups[name]
	rr.Slices += slices
	rr.Tenants += tenants
	rr.Rows += rows
	rep.Rollups[name] = rr
	rep.Slices += slices
	rep.Tenants += tenants
	rep.Rows += rows
}

// Refresh is RefreshWith with the default options.
func Refresh(ctx context.Context, db *keel.DB, rollups ...*Rollup) (Report, error) {
	return RefreshWith(ctx, db, RefreshOptions{}, rollups...)
}

// RefreshWith recomputes the rollups' dirty slices, a batch per transaction, until none is left
// that it can claim. Each transaction is a keel.Write, so Config.OnBegin runs first with ctx:
// refresh reads every tenant's source rows and writes every tenant's rollup rows, so under
// row-level security call it with the context that lets the application's role do that (a system
// scope). Observers see its statements. Each transaction claims dirty rows with FOR UPDATE SKIP LOCKED, takes an
// advisory lock per slice (exclusive per tenant for a whole-tenant rebuild, shared per tenant and
// exclusive per slice otherwise) without waiting, deletes the dirty rows of the slices it locked
// that are committed, then deletes and re-inserts those slices of the rollup tables from the source
// tables. Workers on many processes refresh concurrently without waiting on each other, and a
// slice another worker holds is left to it.
//
// A refresh never misses a committed write: a write's dirty row commits with it, the refresh
// deletes only dirty rows committed before it, and it reads the source in a later statement, so it
// sees every write whose mark it deleted. Rows marked while it runs stay for the next refresh.
func RefreshWith(ctx context.Context, db *keel.DB, o RefreshOptions, rollups ...*Rollup) (Report, error) {
	start := time.Now()
	rf, err := newRefresher(ctx, db, o, rollups)
	if err != nil {
		return Report{}, err
	}
	for o.MaxBatches <= 0 || rf.rep.Batches < o.MaxBatches {
		more, err := rf.batch(ctx)
		if err != nil {
			rf.rep.Duration = time.Since(start)
			return rf.rep, err
		}
		if !more {
			break
		}
	}
	rf.rep.Duration = time.Since(start)
	return rf.rep, nil
}

// Rebuild recomputes every slice of one tenant of a rollup now, waiting for any worker refreshing
// that tenant, for an initial backfill tenant by tenant or a repair. tenant holds the tenant
// columns' values in the source table's order.
func Rebuild(ctx context.Context, db *keel.DB, o RefreshOptions, r *Rollup, tenant ...any) (Report, error) {
	start := time.Now()
	if len(tenant) != len(r.source.Tenant) {
		return Report{}, fmt.Errorf("rollup: %s: rebuild needs %d tenant values, got %d", r.name, len(r.source.Tenant), len(tenant))
	}
	rf, err := newRefresher(ctx, db, o, []*Rollup{r})
	if err != nil {
		return Report{}, err
	}
	u := unit{rollup: r.name, tenant: keel.TenantKeyOf(tenant...), whole: true}
	err = rf.tx(ctx, func(ctx context.Context) error {
		if err := keel.LockKey(ctx, db, u.tenantLock()); err != nil {
			return err
		}
		if err := rf.unmark(ctx, []unit{u}); err != nil {
			return err
		}
		return rf.recompute(ctx, []unit{u})
	})
	rf.rep.Batches = 1
	rf.rep.Duration = time.Since(start)
	if err != nil {
		return rf.rep, fmt.Errorf("rollup: %s: rebuild: %w", r.name, err)
	}
	return rf.rep, nil
}

// MarkAll marks every tenant with rows in each rollup's source for a whole rebuild, which the next
// Refresh carries out, and returns the number of marks written, in one transaction (a keel.Write,
// so Config.OnBegin runs with ctx). A rollup's migration does this once; call it again after
// writing to a source table with its triggers disabled.
func MarkAll(ctx context.Context, db *keel.DB, rollups ...*Rollup) (int64, error) {
	stmts := make([]keel.Statement, len(rollups))
	for i, r := range rollups {
		stmts[i] = keel.Statement{SQL: r.markAllSQL(false)}
	}
	n, err := keel.WriteValue(ctx, db, func(ctx context.Context) ([]int64, error) {
		return keel.ExecBatch(ctx, db, stmts...)
	})
	if err != nil {
		return 0, fmt.Errorf("rollup: mark all: %w", err)
	}
	var total int64
	for _, m := range n {
		total += m
	}
	return total, nil
}

// Pending returns the number of dirty rows waiting for each rollup, for metrics and alerts.
func Pending(ctx context.Context, db *keel.DB, rollups ...*Rollup) (map[string]int64, error) {
	names := make([]string, len(rollups))
	out := make(map[string]int64, len(rollups))
	for i, r := range rollups {
		names[i] = r.name
		out[r.name] = 0
	}
	err := keel.QueryRows(ctx, db, func(row pgx.CollectableRow) error {
		var name string
		var n int64
		if err := row.Scan(&name, &n); err != nil {
			return err
		}
		out[name] = n
		return nil
	}, "SELECT rollup, count(*) FROM keel_rollup_dirty WHERE rollup = ANY($1) GROUP BY rollup", names)
	if err != nil {
		return nil, fmt.Errorf("rollup: pending: %w", err)
	}
	return out, nil
}

// unit is one piece of refresh work: a whole tenant, the bucket starting at bucket, or the rows
// with a null grain.
type unit struct {
	rollup string
	tenant string
	whole  bool
	null   bool
	bucket time.Time
}

// tenantLock is the advisory lock key of the unit's tenant in its rollup.
func (u unit) tenantLock() string { return "keel_rollup_tenant:" + u.rollup + ":" + u.tenant }

// sliceLock is the advisory lock key of the unit's slice.
func (u unit) sliceLock() string {
	b := "null"
	if !u.null {
		b = strconv.FormatInt(u.bucket.UnixMicro(), 10)
	}
	return "keel_rollup_slice:" + u.rollup + ":" + b + ":" + u.tenant
}

// dirtyBucket is the unit's bucket as keel_rollup_dirty stores it, sentinels included.
func (u unit) dirtyBucket() pgtype.Timestamptz {
	switch {
	case u.whole:
		return pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
	case u.null:
		return pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	}
	return pgtype.Timestamptz{Time: u.bucket, Valid: true}
}

// refresher runs refresh transactions for a set of rollups.
type refresher struct {
	db      *keel.DB
	o       RefreshOptions
	rollups map[string]*refreshPlan
	names   []string
	rep     Report
}

// refreshPlan is a rollup with the statements that recompute its units.
type refreshPlan struct {
	r         *Rollup
	deleteSQL string
	bucketSQL string
	nullSQL   string
	wholeSQL  string
}

// newRefresher prepares the statements of each rollup, reading the tenant columns' types from the
// catalog so tenant values bind as text and compare in their own type.
func newRefresher(ctx context.Context, db *keel.DB, o RefreshOptions, rollups []*Rollup) (*refresher, error) {
	if len(rollups) == 0 {
		return nil, errNoRollups
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	rf := &refresher{db: db, o: o, rollups: map[string]*refreshPlan{}}
	for _, r := range rollups {
		if _, dup := rf.rollups[r.name]; dup {
			return nil, fmt.Errorf("rollup: %s is listed twice", r.name)
		}
		types, err := tenantTypes(ctx, db, r)
		if err != nil {
			return nil, err
		}
		rf.rollups[r.name] = r.refreshPlan(types)
		rf.names = append(rf.names, r.name)
	}
	return rf, nil
}

// tenantTypes returns the SQL types of r's source tenant columns, in order.
func tenantTypes(ctx context.Context, db *keel.DB, r *Rollup) ([]string, error) {
	names := make([]string, len(r.source.Tenant))
	for i, ci := range r.source.Tenant {
		names[i] = r.source.Columns[ci].Name
	}
	byName, err := keel.QueryAll(ctx, db, func(row pgx.CollectableRow) ([2]string, error) {
		var p [2]string
		err := row.Scan(&p[0], &p[1])
		return p, err
	}, `SELECT a.attname, format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped AND a.attname = ANY($2)`,
		r.source.Quoted, names)
	if err != nil {
		return nil, fmt.Errorf("rollup: %s: read tenant column types: %w", r.name, err)
	}
	out := make([]string, len(names))
	for i, n := range names {
		for _, p := range byName {
			if p[0] == n {
				out[i] = p[1]
			}
		}
		if out[i] == "" {
			return nil, fmt.Errorf("rollup: %s: %s.%s does not exist", r.name, r.source.Name, n)
		}
	}
	return out, nil
}

// refreshPlan builds the statements that delete and recompute units of r. Tenant values arrive as
// text arrays, one per tenant column, cast to the column's type.
func (r *Rollup) refreshPlan(types []string) *refreshPlan {
	n := len(r.source.Tenant)
	var params, aliases, onSource, onRollup []string
	for i, ci := range r.source.Tenant {
		params = append(params, "$"+strconv.Itoa(i+1)+"::text[]")
		aliases = append(aliases, "t"+strconv.Itoa(i))
		name := r.source.Columns[ci].Name
		val := "u.t" + strconv.Itoa(i) + "::" + types[i]
		onSource = append(onSource, qcol("s", name)+" = "+val)
		onRollup = append(onRollup, qcol("x", name)+" = "+val)
	}
	bparam := "$" + strconv.Itoa(n+1) + "::timestamptz[]"
	withBucket := "unnest(" + strings.Join(append(params, bparam), ", ") + ") AS u(" + strings.Join(append(aliases, "b"), ", ") + ")"
	tenantsOnly := "unnest(" + strings.Join(params, ", ") + ") AS u(" + strings.Join(aliases, ", ") + ")"
	sel, group := r.selectList("s")
	insert := "INSERT INTO " + schema.Quote(r.name) + " (" + r.columnList() + ") SELECT " + sel
	bucket := "x." + schema.Quote(bucketColumn)
	p := &refreshPlan{r: r}
	p.deleteSQL = "DELETE FROM " + schema.Quote(r.name) + " x USING " + withBucket + " WHERE " +
		strings.Join(append(onRollup, "(u.b = '-infinity' OR "+bucket+" = u.b OR (u.b = 'infinity' AND "+bucket+" IS NULL))"), " AND ")
	grainNull := qcol("s", r.source.Columns[r.grain].Name) + " IS NULL"
	p.bucketSQL = insert + " FROM " + withBucket + " JOIN " + r.source.Quoted + " s ON " +
		strings.Join(append(onSource, r.rangeExpr("s", "u.b")), " AND ") + " GROUP BY " + group
	if n == 0 {
		p.nullSQL = insert + " FROM " + r.source.Quoted + " s WHERE " + grainNull + " GROUP BY " + group
		p.wholeSQL = insert + " FROM " + r.source.Quoted + " s GROUP BY " + group
		return p
	}
	p.nullSQL = insert + " FROM " + tenantsOnly + " JOIN " + r.source.Quoted + " s ON " +
		strings.Join(append(onSource, grainNull), " AND ") + " GROUP BY " + group
	p.wholeSQL = insert + " FROM " + tenantsOnly + " JOIN " + r.source.Quoted + " s ON " +
		strings.Join(onSource, " AND ") + " GROUP BY " + group
	return p
}

// tx runs fn in a refresh transaction: a keel.Write on the primary, with the statement timeout.
func (rf *refresher) tx(ctx context.Context, fn func(ctx context.Context) error) error {
	return keel.Write(ctx, rf.db, func(ctx context.Context) error {
		if rf.o.StatementTimeout > 0 {
			ms := strconv.FormatInt(max(rf.o.StatementTimeout.Milliseconds(), 1), 10)
			if _, err := keel.Exec(ctx, rf.db, "SELECT set_config('statement_timeout', $1, true)", ms); err != nil {
				return err
			}
		}
		return fn(ctx)
	})
}

// batch runs one refresh transaction and reports whether another may find work.
func (rf *refresher) batch(ctx context.Context) (bool, error) {
	var more bool
	err := rf.tx(ctx, func(ctx context.Context) error {
		units, claimed, err := rf.claim(ctx)
		if err != nil || len(units) == 0 {
			return err
		}
		locked, err := rf.lock(ctx, units)
		if err != nil {
			return err
		}
		rf.rep.Skipped += len(units) - len(locked)
		if len(locked) == 0 {
			return nil
		}
		if err := rf.unmark(ctx, locked); err != nil {
			return err
		}
		more = claimed == rf.o.BatchSize
		return rf.recompute(ctx, locked)
	})
	rf.rep.Batches++
	if err != nil {
		return false, fmt.Errorf("rollup: refresh: %w", err)
	}
	return more, nil
}

// claim locks up to BatchSize of the oldest dirty rows no other worker holds and returns the
// distinct units they name, a whole-tenant mark absorbing the tenant's slices.
func (rf *refresher) claim(ctx context.Context) ([]unit, int, error) {
	claimed, err := keel.QueryAll(ctx, rf.db, func(row pgx.CollectableRow) (unit, error) {
		var u unit
		var b pgtype.Timestamptz
		if err := row.Scan(&u.rollup, &u.tenant, &b); err != nil {
			return u, err
		}
		u.whole = b.InfinityModifier == pgtype.NegativeInfinity
		u.null = b.InfinityModifier == pgtype.Infinity
		u.bucket = b.Time
		return u, nil
	}, `SELECT rollup, tenant_key, bucket FROM keel_rollup_dirty
		WHERE rollup = ANY($1) ORDER BY marked_at LIMIT $2 FOR UPDATE SKIP LOCKED`, rf.names, rf.o.BatchSize)
	if err != nil {
		return nil, 0, err
	}
	return distinctUnits(claimed), len(claimed), nil
}

// distinctUnits removes duplicates and the slices of tenants rebuilt whole, keeping first-seen
// order.
func distinctUnits(in []unit) []unit {
	whole := map[[2]string]bool{}
	for _, u := range in {
		if u.whole {
			whole[[2]string{u.rollup, u.tenant}] = true
		}
	}
	seen := map[unit]bool{}
	var out []unit
	for _, u := range in {
		if !u.whole && whole[[2]string{u.rollup, u.tenant}] {
			continue
		}
		u.bucket = u.bucket.UTC()
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// lock takes the units' advisory locks without waiting and returns the units it locked.
func (rf *refresher) lock(ctx context.Context, units []unit) ([]unit, error) {
	tenants := make([]string, len(units))
	slices := make([]string, len(units))
	wholes := make([]bool, len(units))
	for i, u := range units {
		tenants[i], slices[i], wholes[i] = u.tenantLock(), u.sliceLock(), u.whole
	}
	idx, err := keel.QueryAll(ctx, rf.db, pgx.RowTo[int64], `SELECT u.i FROM unnest($1::text[], $2::text[], $3::bool[]) WITH ORDINALITY AS u(tk, sk, whole, i)
		WHERE CASE WHEN u.whole THEN pg_try_advisory_xact_lock(hashtextextended(u.tk, 0))
		ELSE pg_try_advisory_xact_lock_shared(hashtextextended(u.tk, 0)) AND pg_try_advisory_xact_lock(hashtextextended(u.sk, 0)) END`,
		tenants, slices, wholes)
	if err != nil {
		return nil, err
	}
	out := make([]unit, 0, len(idx))
	for _, i := range idx {
		out = append(out, units[i-1])
	}
	return out, nil
}

// unmark deletes the committed dirty rows of the units, skipping rows another worker holds.
func (rf *refresher) unmark(ctx context.Context, units []unit) error {
	names := make([]string, len(units))
	tenants := make([]string, len(units))
	buckets := make([]pgtype.Timestamptz, len(units))
	for i, u := range units {
		names[i], tenants[i], buckets[i] = u.rollup, u.tenant, u.dirtyBucket()
	}
	_, err := keel.Exec(ctx, rf.db, `DELETE FROM keel_rollup_dirty WHERE ctid = ANY(ARRAY(
		SELECT d.ctid FROM keel_rollup_dirty d
		  JOIN unnest($1::text[], $2::text[], $3::timestamptz[]) AS u(r, k, b)
		    ON d.rollup = u.r AND d.tenant_key = u.k AND (u.b = '-infinity' OR d.bucket = u.b)
		   FOR UPDATE OF d SKIP LOCKED))`, names, tenants, buckets)
	return err
}

// recompute deletes the units' rows from their rollup tables and inserts them again from the
// source tables, in one round trip for every rollup.
func (rf *refresher) recompute(ctx context.Context, units []unit) error {
	byRollup := map[string][]unit{}
	var order []string
	for _, u := range units {
		if _, ok := byRollup[u.rollup]; !ok {
			order = append(order, u.rollup)
		}
		byRollup[u.rollup] = append(byRollup[u.rollup], u)
	}
	var stmts []keel.Statement
	counts := make([]queueCounts, len(order))
	for i, name := range order {
		c, err := rf.rollups[name].queue(&stmts, byRollup[name])
		if err != nil {
			return err
		}
		counts[i] = c
	}
	affected, err := keel.ExecBatch(ctx, rf.db, stmts...)
	if err != nil {
		return err
	}
	for i, name := range order {
		var n int64
		for _, m := range affected[1 : 1+counts[i].inserts] {
			n += m
		}
		affected = affected[1+counts[i].inserts:]
		rf.rep.add(name, counts[i].slices, counts[i].whole, n)
	}
	return nil
}

// queueCounts is what queue added for one rollup.
type queueCounts struct {
	slices  int
	whole   int
	inserts int
}

// queue appends the statements that recompute units of p's rollup: one DELETE, then an INSERT for
// each kind of unit present.
func (p *refreshPlan) queue(stmts *[]keel.Statement, units []unit) (queueCounts, error) {
	n := len(p.r.source.Tenant)
	var all, buckets, nulls, wholes columns
	for _, u := range units {
		vals := splitKey(u.tenant)
		if len(vals) != n {
			return queueCounts{}, fmt.Errorf("%s: tenant key %q has %d values, want %d", p.r.name, u.tenant, len(vals), n)
		}
		all.add(vals, u.dirtyBucket())
		switch {
		case u.whole:
			wholes.add(vals, pgtype.Timestamptz{})
		case u.null:
			nulls.add(vals, pgtype.Timestamptz{})
		default:
			buckets.add(vals, u.dirtyBucket())
		}
	}
	*stmts = append(*stmts, keel.Statement{SQL: p.deleteSQL, Args: all.args(n, true)})
	c := queueCounts{slices: len(buckets.buckets) + len(nulls.buckets), whole: len(wholes.buckets)}
	if len(buckets.buckets) > 0 {
		*stmts = append(*stmts, keel.Statement{SQL: p.bucketSQL, Args: buckets.args(n, true)})
		c.inserts++
	}
	for _, q := range []struct {
		cols columns
		sql  string
	}{{nulls, p.nullSQL}, {wholes, p.wholeSQL}} {
		if len(q.cols.buckets) == 0 {
			continue
		}
		st := keel.Statement{SQL: q.sql}
		if n > 0 {
			st.Args = q.cols.args(n, false)
		}
		*stmts = append(*stmts, st)
		c.inserts++
	}
	return c, nil
}

// columns are units as parallel arrays: one per tenant column, and their buckets.
type columns struct {
	tenants [][]string
	buckets []pgtype.Timestamptz
}

// add appends one unit.
func (c *columns) add(tenant []string, bucket pgtype.Timestamptz) {
	if c.tenants == nil {
		c.tenants = make([][]string, len(tenant))
	}
	for i, v := range tenant {
		c.tenants[i] = append(c.tenants[i], v)
	}
	c.buckets = append(c.buckets, bucket)
}

// args returns the arrays as statement arguments: the n tenant arrays, then the buckets when
// withBuckets.
func (c *columns) args(n int, withBuckets bool) []any {
	out := make([]any, 0, n+1)
	for i := range n {
		if c.tenants == nil {
			out = append(out, []string{})
			continue
		}
		out = append(out, c.tenants[i])
	}
	if withBuckets {
		out = append(out, c.buckets)
	}
	return out
}

// errNoRollups is returned when a call names no rollup.
var errNoRollups = errors.New("rollup: no rollups given")
