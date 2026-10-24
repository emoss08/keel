package rollup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
)

// ErrNotCovered is wrapped by Match's error when no rollup can answer a report.
var ErrNotCovered = errors.New("rollup: no rollup covers the report")

// Router answers reports from rollups. It implements analytics.RollupRouter, so an application
// configures it in one line:
//
//	runner, err := analytics.NewRunner(db, cat, analytics.RunnerOptions{
//	    Rollups: rollup.NewRouter(ShipmentsDaily, ShipmentsMonthly),
//	})
//
// Everything else is derived: field paths resolve through the report's catalog, the time zone and
// clock come from the report, and the tenant and its columns from the database and the run's
// context. A Router is immutable and safe for concurrent use.
type Router struct {
	list []*indexed
	err  error
}

// indexed is a rollup with the rollup table relation each source relation became.
type indexed struct {
	r     *Rollup
	joins map[int32]int32
}

// NewRouter returns a Router over rollups, trying coarser grains, then fewer dimensions, first.
// Two rollups with one name make every route fail; Check, which NewRunner calls, reports it.
func NewRouter(rollups ...*Rollup) *Router {
	rt := &Router{}
	seen := map[string]bool{}
	for _, r := range rollups {
		if r == nil {
			rt.err = errors.New("rollup: a rollup is nil")
			continue
		}
		if seen[r.name] {
			rt.err = fmt.Errorf("rollup: %s is listed twice", r.name)
			continue
		}
		seen[r.name] = true
		rt.list = append(rt.list, index(r))
	}
	slices.SortStableFunc(rt.list, func(a, b *indexed) int {
		if c := cmp.Compare(rank(b.r.unit), rank(a.r.unit)); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.r.dims), len(b.r.dims)); c != 0 {
			return c
		}
		return cmp.Compare(len(a.r.cols), len(b.r.cols))
	})
	return rt
}

// index maps r's source relations to the rollup table's relations.
func index(r *Rollup) *indexed {
	ix := &indexed{r: r, joins: map[int32]int32{}}
	for i := range r.table.Relations {
		ix.joins[r.source.Relation(r.table.Relations[i].Name)] = int32(i)
	}
	return ix
}

// Check reports why the router cannot answer reports of cat: two rollups with one name, or a
// rollup whose source table the catalog does not report on, which would never be used.
func (rt *Router) Check(cat *analytics.Catalog) error {
	if rt.err != nil {
		return rt.err
	}
	for _, ix := range rt.list {
		if _, ok := cat.EntityOf(ix.r.source); !ok {
			return fmt.Errorf("rollup: %s aggregates %s, which the catalog does not report on", ix.r.name, ix.r.source.Name)
		}
	}
	return nil
}

// Route returns the plan that answers q's report from a rollup, or false when no rollup covers
// it. The plan's Fresh checks, in the run's transaction, that the rollup has been built and that
// the report's tenant has no slice pending refresh in the bucket range it reads.
func (rt *Router) Route(_ context.Context, q analytics.RollupQuery) (*analytics.RollupPlan, bool) {
	plan, err := rt.Match(q)
	return plan, err == nil
}

// Match returns the plan of the first rollup that covers q's report, without checking freshness.
// Its error says why each rollup of the report's entity does not cover it, and wraps
// ErrNotCovered.
func (rt *Router) Match(q analytics.RollupQuery) (*analytics.RollupPlan, error) {
	if rt.err != nil {
		return nil, rt.err
	}
	if q.Catalog == nil || q.Resolved == nil || q.Resolved.Definition == nil {
		return nil, fmt.Errorf("%w: no catalog or definition", ErrNotCovered)
	}
	entity, ok := q.Catalog.Entity(q.Resolved.Definition.Entity)
	if !ok {
		return nil, fmt.Errorf("%w: no entity %s", ErrNotCovered, q.Resolved.Definition.Entity)
	}
	if q.Location == nil {
		q.Location = time.UTC
	}
	var reasons []string
	for _, ix := range rt.list {
		if ix.r.source != entity.Table {
			continue
		}
		c := newCoverer(ix, q)
		if err := c.run(); err != nil {
			reasons = append(reasons, ix.r.name+": "+err.Error())
			continue
		}
		return c.plan, nil
	}
	if len(reasons) == 0 {
		return nil, fmt.Errorf("%w: no rollup of %s", ErrNotCovered, entity.Key)
	}
	return nil, fmt.Errorf("%w: %s", ErrNotCovered, strings.Join(reasons, "; "))
}

// freshness returns the check of a plan reading [from, to) of r: the rollup is built and the
// context's tenant (every tenant under a system scope) has no slice pending refresh in the
// range. A plan whose distinct counts are exact per tenant needs a tenant.
func (r *Rollup) freshness(from, to time.Time, exact bool) func(ctx context.Context, db *keel.DB) (bool, error) {
	return func(ctx context.Context, db *keel.DB) (bool, error) {
		tenant, ok := r.tenantKey(ctx, db)
		if !ok || tenant == "" && exact && len(r.source.Tenant) > 0 {
			return false, nil
		}
		sql, args := r.freshnessSQL(from, to, tenant)
		fresh, err := keel.QueryValue[bool](ctx, db, sql, args...)
		if err != nil {
			return false, fmt.Errorf("rollup: %s: freshness: %w", r.name, err)
		}
		return fresh, nil
	}
}

// tenantKey returns the key of ctx's tenant over the rollup's source tenant columns, as the
// rollup's triggers write it, and false when the tenant cannot be keyed over them.
func (r *Rollup) tenantKey(ctx context.Context, db *keel.DB) (string, bool) {
	key := db.TenantKey(ctx)
	if key == "" || len(r.source.Tenant) == 0 {
		return "", true
	}
	vals := splitKey(key)
	cols := db.TenantColumns()
	out := make([]string, len(r.source.Tenant))
	for i, ci := range r.source.Tenant {
		pos := i
		if len(cols) > 0 {
			pos = slices.Index(cols, r.source.Columns[ci].Name)
		}
		if pos < 0 || pos >= len(vals) {
			return "", false
		}
		out[i] = vals[pos]
	}
	return joinKey(out), true
}

// freshnessSQL returns a query of one boolean: whether the rollup is built and the tenant (every
// tenant when it is empty) has no slice pending refresh within [from, to).
func (r *Rollup) freshnessSQL(from, to time.Time, tenant string) (string, []any) {
	lo := pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
	hi := pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	if !from.IsZero() {
		lo = pgtype.Timestamptz{Time: from, Valid: true}
	}
	if !to.IsZero() {
		hi = pgtype.Timestamptz{Time: to.Add(-time.Microsecond), Valid: true}
	}
	filter := ""
	args := []any{r.name, lo, hi}
	if tenant != "" {
		filter = " AND tenant_key = $4"
		args = append(args, tenant)
	}
	return `SELECT EXISTS (SELECT 1 FROM keel_rollup_state WHERE rollup = $1)
	AND NOT EXISTS (SELECT 1 FROM keel_rollup_dirty WHERE rollup = $1` + filter + `
	AND (bucket = '-infinity' OR bucket BETWEEN $2 AND $3))`, args
}

// rank orders units from finest to coarsest.
func rank(u ir.Bucket) int { return slices.Index(ir.Buckets(), u) }

// coarsens reports whether every bucket of unit q is a whole number of buckets of grain g.
func coarsens(g, q ir.Bucket) bool {
	switch g {
	case ir.BucketHour:
		return slices.Contains(ir.Buckets(), q)
	case ir.BucketDay:
		return q != ir.BucketHour && slices.Contains(ir.Buckets(), q)
	case ir.BucketWeek:
		return q == ir.BucketWeek
	case ir.BucketMonth:
		return q == ir.BucketMonth || q == ir.BucketQuarter || q == ir.BucketYear
	case ir.BucketQuarter:
		return q == ir.BucketQuarter || q == ir.BucketYear
	}
	return q == ir.BucketYear
}
