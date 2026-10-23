package keel

import (
	"github.com/emoss08/keel/schema"
)

// CTETable is a named subquery (WITH name AS (…)) that queries declare with With and then join,
// filter on or select from like a table.
//
//	recent := keel.CTE("recent", []string{"customer_id", "last_at"},
//	    s.Group(s.CustomerID).Pick(s.CustomerID, s.CreatedAt.Max()))
//	rows, err := c.With(recent).
//	    Join(recent, recent.Col("customer_id").EqCol(c.ID)).
//	    Where(recent.Col("last_at").Lt(cutoff)).
//	    All(ctx, db)
type CTETable struct {
	ref       *Ref
	body      subquery
	step      subquery
	recursive bool
	unionAll  bool
}

// CTE declares a common table expression. columns name the body's output columns, in order.
func CTE(name string, columns []string, body subquery) *CTETable {
	return &CTETable{ref: cteRef(name, columns), body: body}
}

// RecursiveCTE declares WITH RECURSIVE name (columns) AS (base UNION ALL step). Give it its step with
// Recurse, which can join the CTE itself to walk a hierarchy:
//
//	chain := keel.RecursiveCTE("chain", []string{"id", "depth"}, w.Pick(w.ID, keel.Raw("0")).Where(w.ID.Eq(root)))
//	sub := w.As("sub")
//	chain.Recurse(sub.Pick(sub.ID, keel.Raw("? + 1", chain.Col("depth"))).
//	    Join(chain, chain.Col("id").EqCol(sub.ManagerID)))
func RecursiveCTE(name string, columns []string, base subquery) *CTETable {
	return &CTETable{ref: cteRef(name, columns), body: base, recursive: true, unionAll: true}
}

// Recurse sets the recursive step: base UNION ALL step.
func (c *CTETable) Recurse(step subquery) *CTETable {
	c.step = step
	return c
}

// Distinct makes the recursion UNION instead of UNION ALL, so repeated rows end it.
func (c *CTETable) Distinct() *CTETable {
	c.unionAll = false
	return c
}

func cteRef(name string, columns []string) *Ref {
	t := &schema.Table{Name: name}
	for _, c := range columns {
		t.Columns = append(t.Columns, schema.Column{Name: c, Kind: schema.KindOther})
	}
	schema.Derive(t)
	return &Ref{Table: t}
}

// Col references one of the CTE's columns.
func (c *CTETable) Col(name string) AnyCol {
	i := c.ref.Table.Column(name)
	if i < 0 {
		panic("keel: CTE " + c.ref.Table.Name + " has no column " + name)
	}
	return AnyCol{Col{Ref: c.ref, Index: i}}
}

// TableRef returns the CTE's table reference, so Join accepts it.
func (c *CTETable) TableRef() *Ref { return c.ref }

// Query starts a query over the CTE's rows. Pick what to read and run it with Into.
func (c *CTETable) Query() *Query[Row] { return &Query[Row]{q: query{ref: c.ref}} }

// Row is the row type of a query over a CTE or a compound query; read such queries with Into.
type Row struct{}

func (c *CTETable) writeDefinition(r *render) {
	r.s(c.ref.Table.Quoted)
	r.s(" (")
	for i := range c.ref.Table.Columns {
		if i > 0 {
			r.s(", ")
		}
		r.s(c.ref.Table.Columns[i].Quoted)
	}
	r.s(") AS (")
	saved := r.sc
	r.sc = nil
	c.body.writeSubquery(r, false)
	if c.step != nil {
		if c.unionAll {
			r.s(" UNION ALL ")
		} else {
			r.s(" UNION ")
		}
		c.step.writeSubquery(r, false)
	}
	r.sc = saved
	r.b(')')
}

// With declares CTEs the query uses.
func (q *Query[T]) With(ctes ...*CTETable) *Query[T] {
	q.q.ctes = append(q.q.ctes, ctes...)
	return q
}

// With starts a query that declares CTEs.
func (h Handle[T]) With(ctes ...*CTETable) *Query[T] { return h.Query().With(ctes...) }

func writeWith(r *render, ctes []*CTETable) {
	if len(ctes) == 0 {
		return
	}
	r.s("WITH ")
	for _, c := range ctes {
		if c.recursive {
			r.s("RECURSIVE ")
			break
		}
	}
	for i, c := range ctes {
		if i > 0 {
			r.s(", ")
		}
		c.writeDefinition(r)
	}
	r.b(' ')
}

// Compound is a UNION, UNION ALL, INTERSECT or EXCEPT of queries that pick the same number of
// expressions. Use it as a CTE body, a subquery, or read it with Into.
type Compound struct {
	op    string
	parts []subquery
}

// Union is q1 UNION q2 …: the distinct rows of every query.
func Union(qs ...subquery) Compound { return Compound{op: " UNION ", parts: qs} }

// UnionAll is q1 UNION ALL q2 …: every row of every query.
func UnionAll(qs ...subquery) Compound { return Compound{op: " UNION ALL ", parts: qs} }

// Intersect is q1 INTERSECT q2 ….
func Intersect(qs ...subquery) Compound { return Compound{op: " INTERSECT ", parts: qs} }

// Except is q1 EXCEPT q2 ….
func Except(qs ...subquery) Compound { return Compound{op: " EXCEPT ", parts: qs} }

func (c Compound) writeSubquery(r *render, existsOnly bool) {
	for i, p := range c.parts {
		if i > 0 {
			r.s(c.op)
		}
		r.b('(')
		saved := r.sc
		p.writeSubquery(r, false)
		r.sc = saved
		r.b(')')
	}
}
