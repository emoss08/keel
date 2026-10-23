package keel

// Agg is a computed value: an aggregate (count, sum, min, max, avg), a window function or a scalar
// function keel knows. It compares to values, orders, and filters or windows when it aggregates.
type Agg struct {
	fn       string
	arg      Expr
	distinct bool
	star     bool
	scalar   bool
	custom   func(r *render)
	filter   Pred
	over     *window
}

type window struct {
	partition []Expr
	order     []Order
}

// Count is count(*).
func Count() Agg { return Agg{fn: "count", star: true} }

// Sum is sum(e).
func Sum(e Expr) Agg { return Agg{fn: "sum", arg: e} }

// RowNumber is row_number(); give it a window with Over.
func RowNumber() Agg { return Agg{fn: "row_number", star: true, scalar: true} }

// Filter restricts the rows an aggregate sees: count(*) FILTER (WHERE p).
func (a Agg) Filter(p Pred) Agg { a.filter = p; return a }

// Over turns the aggregate into a window function.
func (a Agg) Over(partition []Expr, order ...Order) Agg {
	a.over = &window{partition: partition, order: order}
	return a
}

func (a Agg) writeSQL(r *render) {
	if a.custom != nil {
		a.custom(r)
		return
	}
	r.s(a.fn)
	r.b('(')
	switch {
	case a.star && a.fn == "count":
		r.b('*')
	case a.star:
	default:
		if a.distinct {
			r.s("DISTINCT ")
		}
		a.arg.writeSQL(r)
	}
	r.b(')')
	if a.filter != nil {
		r.s(" FILTER (WHERE ")
		a.filter.writeSQL(r)
		r.b(')')
	}
	if a.over != nil {
		r.s(" OVER (")
		if len(a.over.partition) > 0 {
			r.s("PARTITION BY ")
			for i, e := range a.over.partition {
				if i > 0 {
					r.s(", ")
				}
				e.writeSQL(r)
			}
		}
		if len(a.over.order) > 0 {
			if len(a.over.partition) > 0 {
				r.b(' ')
			}
			r.s("ORDER BY ")
			for i, o := range a.over.order {
				if i > 0 {
					r.s(", ")
				}
				o.writeSQL(r)
			}
		}
		r.b(')')
	}
}

func (a Agg) Eq(v any) Pred  { return cmp(a, " = ", v) }
func (a Agg) Ne(v any) Pred  { return cmp(a, " <> ", v) }
func (a Agg) Gt(v any) Pred  { return cmp(a, " > ", v) }
func (a Agg) Gte(v any) Pred { return cmp(a, " >= ", v) }
func (a Agg) Lt(v any) Pred  { return cmp(a, " < ", v) }
func (a Agg) Lte(v any) Pred { return cmp(a, " <= ", v) }
func (a Agg) Asc() Order     { return Order{e: a} }
func (a Agg) Desc() Order    { return Order{e: a, desc: true} }
