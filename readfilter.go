package keel

import (
	"context"

	"github.com/emoss08/keel/schema"
)

// ReadFilter returns the predicates every reference to table t in a read statement must also
// satisfy, for row-level rules that sit beside tenancy: "own records only", "this region's
// customers". The predicates are built over the table's own columns, from a generated handle
// (shipmentdb.Shipments.OwnerID.Eq(id)) or a dynamic query on the table
// (keel.DynFrom(t).Path("owner_id").Eq(id)); keel resolves those columns to each reference it
// filters. A predicate may also test related rows with an EXISTS (DynFrom(t).Rel(…).Any), but it
// cannot join a relation.
type ReadFilter func(ctx context.Context, t *schema.Table) ([]Pred, error)

// readFilterKey is the context key of a ReadFilter.
type readFilterKey struct{}

// WithReadFilter returns a context whose read statements apply f to every table reference they
// make: the base table, joined relations, picks and each table a pick walks, EXISTS, subqueries,
// CTE bodies and pre-aggregations. Interceptors cover only a statement's base table; a read filter
// covers every table, so a joined row the filter excludes reads as absent (its columns NULL) and
// a related row it excludes does not count. f is called each time a statement references a table,
// so it should return quickly, from a cache when the predicates cost anything to build. A
// ReadFilter already on ctx is replaced.
func WithReadFilter(ctx context.Context, f ReadFilter) context.Context {
	return context.WithValue(ctx, readFilterKey{}, f)
}

// readFiltered returns the read filter's predicates for t, or nil without one.
func (r *render) readFiltered(t *schema.Table) []Pred {
	if r.ctx == nil || t == nil {
		return nil
	}
	f, _ := r.ctx.Value(readFilterKey{}).(ReadFilter)
	if f == nil {
		return nil
	}
	ps, err := f(r.ctx, t)
	if err != nil {
		r.fail(err)
	}
	return ps
}

// pinnedPreds writes ps after an AND, with columns of t resolving to alias: a scope that holds
// only the reference goes on top while they render, so a table referenced more than once in the
// statement is filtered under the right alias.
func (r *render) pinnedPreds(t *schema.Table, alias string, ps []Pred) {
	if len(ps) == 0 {
		return
	}
	r.sc = &scope{parent: r.sc, joinMode: joinNone, refs: []scoped{{ref: &Ref{Table: t}, alias: alias}}}
	for _, p := range ps {
		if p == nil {
			continue
		}
		r.s(" AND ")
		p.writeSQL(r)
	}
	r.pop()
}
