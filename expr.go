package keel

import (
	"strconv"
	"strings"
	"unsafe"
)

// Expr is a SQL expression. keel's column, aggregate and combinator values implement it; code
// outside keel builds expressions from those and from Raw.
type Expr interface {
	writeSQL(r *render)
}

// Pred is a boolean expression, accepted by Where, Having and the combinators.
type Pred interface {
	Expr
	pred()
}

type predExpr struct{ e func(r *render) }

func (p predExpr) writeSQL(r *render) { p.e(r) }
func (predExpr) pred()                {}

// And joins ps with AND. Nil predicates are skipped; And of nothing is TRUE.
func And(ps ...Pred) Pred { return boolList{ps: ps, op: " AND ", empty: "TRUE"} }

// Or joins ps with OR. Nil predicates are skipped; Or of nothing is FALSE.
func Or(ps ...Pred) Pred { return boolList{ps: ps, op: " OR ", empty: "FALSE"} }

type boolList struct {
	ps    []Pred
	op    string
	empty string
}

func (l boolList) writeSQL(r *render) {
	n := 0
	for _, p := range l.ps {
		if p != nil {
			n++
		}
	}
	if n == 0 {
		r.s(l.empty)
		return
	}
	r.b('(')
	first := true
	for _, p := range l.ps {
		if p == nil {
			continue
		}
		if !first {
			r.s(l.op)
		}
		first = false
		p.writeSQL(r)
	}
	r.b(')')
}

func (boolList) pred() {}

// Not negates p.
func Not(p Pred) Pred {
	return predExpr{e: func(r *render) {
		r.s("NOT (")
		p.writeSQL(r)
		r.b(')')
	}}
}

// When returns p when cond holds and nil otherwise. Where and the combinators skip nil, which makes
// optional filters one expression each.
func When(cond bool, p Pred) Pred {
	if !cond {
		return nil
	}
	return p
}

// True is the predicate TRUE.
func True() Pred { return predExpr{e: func(r *render) { r.s("TRUE") }} }

// Raw is SQL text that keel does not understand. Each ? in sql is replaced by the next argument:
// an Expr (a column, a subquery) is written in place with its alias resolved, any other value is
// bound as a parameter. Use ?? for a literal question mark.
func Raw(sql string, args ...any) RawExpr { return RawExpr{sql: sql, args: args} }

// RawExpr is the result of Raw. It is both an expression and a predicate.
type RawExpr struct {
	sql  string
	args []any
}

func (e RawExpr) writeSQL(r *render) {
	sql := e.sql
	n := 0
	for {
		i := strings.IndexByte(sql, '?')
		if i < 0 {
			r.s(sql)
			return
		}
		r.s(sql[:i])
		if i+1 < len(sql) && sql[i+1] == '?' {
			r.b('?')
			sql = sql[i+2:]
			continue
		}
		sql = sql[i+1:]
		if n >= len(e.args) {
			r.fail(errRawArgs)
			return
		}
		writeOperand(r, e.args[n], unsafe.Pointer(&e.args[n]), e.sql)
		n++
	}
}

func (RawExpr) pred() {}

// writeOperand writes v in place when it is an expression and binds it otherwise. slot is where
// the expression stores v and ctx the expression's text, so the same expression written twice
// binds v once.
func writeOperand(r *render, v any, slot unsafe.Pointer, ctx string) {
	if e, ok := v.(Expr); ok {
		e.writeSQL(r)
		return
	}
	r.argSlot(v, slot, ctx)
}

// Exists is EXISTS (q).
func Exists(q subquery) Pred {
	return predExpr{e: func(r *render) {
		r.s("EXISTS (")
		q.writeSubquery(r, true)
		r.b(')')
	}}
}

// NotExists is NOT EXISTS (q).
func NotExists(q subquery) Pred {
	return predExpr{e: func(r *render) {
		r.s("NOT EXISTS (")
		q.writeSubquery(r, true)
		r.b(')')
	}}
}

// subquery is a query used inside another statement. *Query[T] implements it.
type subquery interface {
	writeSubquery(r *render, existsOnly bool)
}

// Val binds v as a parameter where an expression is expected. The same Val written twice in a
// statement binds v once.
func Val(v any) Expr { return &valExpr{v: v} }

type valExpr struct{ v any }

func (e *valExpr) writeSQL(r *render) { r.argSlot(e.v, unsafe.Pointer(e), "") }

// Order is one ORDER BY term.
type Order struct {
	e     Expr
	col   *Col
	desc  bool
	nulls int8
}

// NullsFirst sorts NULL values first.
func (o Order) NullsFirst() Order { o.nulls = -1; return o }

// NullsLast sorts NULL values last.
func (o Order) NullsLast() Order { o.nulls = 1; return o }

func (o Order) writeSQL(r *render) {
	o.e.writeSQL(r)
	o.writeDirection(r)
}

// writeAt orders by the output at position pos of the select list.
func (o Order) writeAt(r *render, pos int) {
	r.s(strconv.Itoa(pos))
	o.writeDirection(r)
}

func (o Order) writeDirection(r *render) {
	if o.desc {
		r.s(" DESC")
	} else {
		r.s(" ASC")
	}
	switch o.nulls {
	case -1:
		r.s(" NULLS FIRST")
	case 1:
		r.s(" NULLS LAST")
	}
}

// Asc orders by e ascending.
func Asc(e Expr) Order { return Order{e: e} }

// Desc orders by e descending.
func Desc(e Expr) Order { return Order{e: e, desc: true} }

// Assignment is one SET term of an update, built by a column's To, ToNull or Add.
type Assignment struct {
	col Col
	e   func(r *render)
}

func escapeLike(s string) string {
	if !strings.ContainsAny(s, `%_\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
