package keel

import "github.com/emoss08/keel/schema"

// RowsTable is a set of rows computed by expressions, usually set-returning functions such as
// unnest or generate_series, joined laterally so the expressions can read the row they join:
// LEFT JOIN LATERAL (SELECT unnest(r.operations) AS "op") op ON TRUE. Build it with Rows and join
// it with JoinLateral or LeftJoinLateral.
type RowsTable struct {
	ref   *Ref
	exprs []Expr
}

// Rows declares a lateral source named name whose columns are the values of exprs, one column per
// expression in order. Several set-returning functions zip, row i of each making row i.
//
//	op := keel.Rows("op", []string{"op"}, keel.DynFunc("unnest", rp.Operations))
//	roles.LeftJoinLateral(op).Pick(r.ID, op.Col("op"))
func Rows(name string, columns []string, exprs ...Expr) *RowsTable {
	return &RowsTable{ref: cteRef(name, columns), exprs: exprs}
}

// Col references one of the source's columns.
func (s *RowsTable) Col(name string) AnyCol {
	i := s.ref.Table.Column(name)
	if i < 0 {
		panic("keel: Rows " + s.ref.Table.Name + " has no column " + name)
	}
	return AnyCol{Col{Ref: s.ref, Index: i}}
}

// TableRef returns the source's table reference, so the lateral joins accept it.
func (s *RowsTable) TableRef() *Ref { return s.ref }

func (s *RowsTable) writeSubquery(r *render, _ bool) {
	cols := s.ref.Table.Columns
	if len(s.exprs) != len(cols) {
		r.fail(errf("Rows %s has %d columns and %d expressions", s.ref.Table.Name, len(cols), len(s.exprs)))
		return
	}
	r.s("SELECT ")
	for i, e := range s.exprs {
		if i > 0 {
			r.s(", ")
		}
		e.writeSQL(r)
		r.s(" AS ")
		r.s(schema.Quote(cols[i].Name))
	}
}
