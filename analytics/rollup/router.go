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
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// ErrNotCovered is wrapped by Match's error when no rollup can answer a report.
var ErrNotCovered = errors.New("rollup: no rollup covers the report")

// RouterOptions configure a Router.
type RouterOptions struct {
	// Keys name the schema the way the catalog does; zero fields default to DefaultKeys.
	Keys Keys
	// TenantColumns is the application's keel.Config.TenantColumns, the order of the values in a
	// tenant key. Empty assumes each source table declares its tenant columns in that order.
	TenantColumns []string
	// Location returns the time zone a report runs in; nil, or a nil result, is UTC.
	Location func(ctx context.Context) *time.Location
	// Now is the clock relative date filters are evaluated with; default time.Now.
	Now func() time.Time
	// OnError receives the errors of freshness checks; Route answers false after one.
	OnError func(err error)
}

// Router answers reports from rollups: given a resolved definition, it finds a rollup that covers
// it and checks that the report's tenant has no slice pending refresh in the range the report
// reads.
type Router struct {
	db       *keel.DB
	o        RouterOptions
	byEntity map[string][]*indexed
}

// indexed is a rollup with its catalog keys resolved: the source's columns and belongs-to
// relations by key, the columns of each relation's target by key, and the rollup table relation
// each source relation became.
type indexed struct {
	r       *Rollup
	fields  map[string]int32
	edges   map[string]int32
	targets map[int32]map[string]int32
	joins   map[int32]int32
}

// NewRouter returns a Router over rollups. With a nil db it only matches: Route answers false.
func NewRouter(db *keel.DB, o RouterOptions, rollups ...*Rollup) (*Router, error) {
	o.Keys = o.Keys.WithDefaults()
	if o.Now == nil {
		o.Now = time.Now
	}
	rt := &Router{db: db, o: o, byEntity: map[string][]*indexed{}}
	seen := map[string]bool{}
	for _, r := range rollups {
		if seen[r.name] {
			return nil, fmt.Errorf("rollup: %s is listed twice", r.name)
		}
		seen[r.name] = true
		e := o.Keys.Entity(r.source)
		rt.byEntity[e] = append(rt.byEntity[e], rt.index(r))
	}
	for _, list := range rt.byEntity {
		slices.SortStableFunc(list, func(a, b *indexed) int {
			if c := cmp.Compare(rank(b.r.unit), rank(a.r.unit)); c != 0 {
				return c
			}
			if c := cmp.Compare(len(a.r.dims), len(b.r.dims)); c != 0 {
				return c
			}
			return cmp.Compare(len(a.r.cols), len(b.r.cols))
		})
	}
	return rt, nil
}

// index resolves r's field and edge keys.
func (rt *Router) index(r *Rollup) *indexed {
	k := rt.o.Keys
	ix := &indexed{r: r, fields: map[string]int32{}, edges: map[string]int32{},
		targets: map[int32]map[string]int32{}, joins: map[int32]int32{}}
	for i := range r.source.Columns {
		ix.fields[k.Field(r.source, int32(i))] = int32(i)
	}
	for i := range r.source.Relations {
		rel := &r.source.Relations[i]
		if rel.Kind != schema.BelongsTo || rel.Target == nil {
			continue
		}
		ix.edges[k.Edge(r.source, int32(i))] = int32(i)
		targets := map[string]int32{}
		for c := range rel.Target.Columns {
			targets[k.Field(rel.Target, int32(c))] = int32(c)
		}
		ix.targets[int32(i)] = targets
	}
	for i := range r.table.Relations {
		ix.joins[r.source.Relation(r.table.Relations[i].Name)] = int32(i)
	}
	return ix
}

// Route returns the plan that answers res from a rollup, or false when no rollup covers it, its
// tenant has a slice pending refresh in the range it reads, or the rollup has not been built.
// The report then reads its source tables.
func (rt *Router) Route(ctx context.Context, res *ir.Resolved) (*Plan, bool) {
	if rt.db == nil {
		return nil, false
	}
	loc := time.UTC
	if rt.o.Location != nil {
		if l := rt.o.Location(ctx); l != nil {
			loc = l
		}
	}
	plan, err := rt.Match(res, loc)
	if err != nil {
		return nil, false
	}
	tenant, ok := rt.tenantKey(ctx, plan.Rollup)
	if !ok || tenant == "" && plan.exact && len(plan.Rollup.source.Tenant) > 0 {
		return nil, false
	}
	plan.Tenant = tenant
	fresh, err := rt.Fresh(ctx, plan)
	if err != nil {
		if rt.o.OnError != nil {
			rt.o.OnError(err)
		}
		return nil, false
	}
	if !fresh {
		return nil, false
	}
	return plan, true
}

// Match returns the plan of the first rollup that covers res when it runs in loc, trying
// coarser grains and fewer dimensions first, without checking freshness. Its error says why each
// rollup of the entity does not cover the report, and wraps ErrNotCovered.
func (rt *Router) Match(res *ir.Resolved, loc *time.Location) (*Plan, error) {
	if res == nil || res.Definition == nil {
		return nil, fmt.Errorf("%w: no definition", ErrNotCovered)
	}
	list := rt.byEntity[res.Definition.Entity]
	if len(list) == 0 {
		return nil, fmt.Errorf("%w: no rollup of %s", ErrNotCovered, res.Definition.Entity)
	}
	var reasons []string
	for _, ix := range list {
		c := newCoverer(ix, res, loc, rt.o.Now())
		if err := c.run(); err != nil {
			reasons = append(reasons, ix.r.name+": "+err.Error())
			continue
		}
		return c.plan, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotCovered, strings.Join(reasons, "; "))
}

// tenantKey returns the key of ctx's tenant over the rollup's tenant columns, and false when the
// tenant cannot be keyed over them.
func (rt *Router) tenantKey(ctx context.Context, r *Rollup) (string, bool) {
	key := rt.db.TenantKey(ctx)
	if key == "" || len(r.source.Tenant) == 0 {
		return "", true
	}
	vals := splitKey(key)
	out := make([]string, len(r.source.Tenant))
	for i, ci := range r.source.Tenant {
		pos := i
		if len(rt.o.TenantColumns) > 0 {
			pos = slices.Index(rt.o.TenantColumns, r.source.Columns[ci].Name)
		}
		if pos < 0 || pos >= len(vals) {
			return "", false
		}
		out[i] = vals[pos]
	}
	return joinKey(out), true
}

// Fresh reports whether the plan's rollup has been built and its tenant has no slice pending
// refresh in the range the plan reads; a plan without a tenant needs every tenant fresh. It reads
// the primary. A report read from a replica should run Plan.Freshness in its own transaction
// instead, so both see one snapshot.
func (rt *Router) Fresh(ctx context.Context, p *Plan) (bool, error) {
	sql, args := p.Freshness()
	var fresh bool
	if err := rt.db.Pool().QueryRow(ctx, sql, args...).Scan(&fresh); err != nil {
		return false, fmt.Errorf("rollup: %s: freshness: %w", p.Rollup.name, err)
	}
	return fresh, nil
}

// Plan is how a report covered by a rollup reads it instead of its source table.
type Plan struct {
	// Rollup is the rollup that answers the report.
	Rollup *Rollup
	// Table is the rollup table's schema, the report's base table.
	Table *schema.Table
	// Location is the time zone of the rollup's buckets, which the report runs in.
	Location *time.Location
	// Tenant is the tenant key Route checked freshness for; empty for every tenant.
	Tenant string
	// From and To bound the buckets the report reads, [From, To); zero when unbounded.
	From time.Time
	To   time.Time
	// Fields maps every field path the report's dimensions, filters and pivot use to the rollup.
	Fields map[string]Field
	// Dimensions maps each dimension column id to its field and any re-bucketing.
	Dimensions map[string]Dimension
	// Measures maps each measure column id to its re-aggregation over the rollup table.
	Measures map[string]MeasurePlan

	exact bool
}

// Field is where a report's field lives in the rollup.
type Field struct {
	// Column is the field's column in Plan.Table, or -1 when it is reached through Relation.
	Column int32
	// Relation is the relation of Plan.Table that reaches the field, or -1.
	Relation int32
	// Target is the field's column in the relation's target table.
	Target int32
	// Grain marks the source's grain field: Column is the bucket, a timestamptz at the start of
	// each grain bucket in Plan.Location. Filter values on the field, written for the source
	// column (epoch seconds, dates), must be compared as instants.
	Grain bool
	// Source is the value type of the source field.
	Source schema.ValueType
}

// Dimension is a report dimension answered from the rollup.
type Dimension struct {
	// Field is the dimension's field path, a key of Plan.Fields.
	Field string
	// Bucket is the coarser bucket the rollup's bucket column is truncated to, in Plan.Location;
	// empty when the column is used as it is.
	Bucket ir.Bucket
}

// Reaggregate is one aggregate over a rollup column.
type Reaggregate struct {
	// Fn is sum, min or max.
	Fn string
	// Column is the column's index in Plan.Table.
	Column int32
	// Name is the column's name.
	Name string
}

// MeasurePlan re-aggregates a report measure from stored aggregates: sums and counts are summed,
// minimums and maximums are taken again, and an average divides a summed sum by a summed count.
type MeasurePlan struct {
	// Agg is the aggregation the report asked for.
	Agg ir.Agg
	// Value is the aggregate, or an average's numerator.
	Value Reaggregate
	// Divisor is an average's denominator.
	Divisor *Reaggregate
	// Integer marks counts, which are cast to bigint and are 0 over no rows.
	Integer bool
	// Float marks an average of floating-point values, which stays floating point.
	Float bool
}

// SQL renders the measure over the rollup table under alias, with filter, when not empty, as each
// aggregate's FILTER (WHERE …) condition.
func (m MeasurePlan) SQL(alias, filter string) string {
	agg := func(a Reaggregate) string {
		s := a.Fn + "(" + qcol(alias, a.Name) + ")"
		if filter != "" {
			s += " FILTER (WHERE " + filter + ")"
		}
		return s
	}
	switch {
	case m.Divisor != nil && m.Float:
		return agg(m.Value) + " / nullif(" + agg(*m.Divisor) + ", 0)"
	case m.Divisor != nil:
		return agg(m.Value) + "::numeric / nullif(" + agg(*m.Divisor) + ", 0)"
	case m.Integer:
		return "coalesce(" + agg(m.Value) + ", 0)::int8"
	}
	return agg(m.Value)
}

// BucketSQL renders a grain dimension over the rollup table under alias: the bucket column, or
// it truncated to unit in the rollup's zone when unit is coarser than the grain.
func (p *Plan) BucketSQL(alias string, unit ir.Bucket) string {
	c := qcol(alias, bucketColumn)
	if unit == "" || unit == p.Rollup.unit {
		return c
	}
	return "date_trunc(" + lit(string(unit)) + ", " + c + ", " + lit(p.Rollup.zoneName) + ")"
}

// Freshness returns a query of one boolean: whether the rollup is built and the plan's tenant
// (every tenant when Tenant is empty) has no slice pending refresh within [From, To).
func (p *Plan) Freshness() (string, []any) {
	lo := pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
	hi := pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	if !p.From.IsZero() {
		lo = pgtype.Timestamptz{Time: p.From, Valid: true}
	}
	if !p.To.IsZero() {
		hi = pgtype.Timestamptz{Time: p.To.Add(-time.Microsecond), Valid: true}
	}
	tenant := ""
	args := []any{p.Rollup.name, lo, hi}
	if p.Tenant != "" {
		tenant = " AND tenant_key = $4"
		args = append(args, p.Tenant)
	}
	return `SELECT EXISTS (SELECT 1 FROM keel_rollup_state WHERE rollup = $1)
	AND NOT EXISTS (SELECT 1 FROM keel_rollup_dirty WHERE rollup = $1` + tenant + `
	AND (bucket = '-infinity' OR bucket BETWEEN $2 AND $3))`, args
}

// rank orders units from finest to coarsest.
func rank(u ir.Bucket) int { return slices.Index(ir.Buckets(), u) }

// coarsens reports whether every bucket of unit q is a whole number of buckets of grain g.
func coarsens(g, q ir.Bucket) bool {
	switch g {
	case ir.BucketHour:
		return true
	case ir.BucketDay:
		return q != ir.BucketHour
	case ir.BucketWeek:
		return q == ir.BucketWeek
	case ir.BucketMonth:
		return q == ir.BucketMonth || q == ir.BucketQuarter || q == ir.BucketYear
	case ir.BucketQuarter:
		return q == ir.BucketQuarter || q == ir.BucketYear
	}
	return q == ir.BucketYear
}
