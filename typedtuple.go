package keel

// TupleExpr is a row value, ROW(a, b, …), built by keel.Tuple. Rows compare element by element
// in order, so keel.Tuple(e.OccurredAt, e.ID).Gt(keel.Tuple(after.At, after.ID)) is the keyset
// condition of a page ordered by occurred_at, id ascending, which an index on (occurred_at, id)
// serves as one range scan; a Desc page uses Lt.
type TupleExpr struct{ es []Expr }

// Tuple is the row value ROW(vs…): each v is an expression written in place, nil as NULL, or a Go
// value bound as a parameter whose type Postgres takes from the element it is compared with. A
// row needs at least two values; one with fewer makes a comparison that fails.
func Tuple(vs ...any) TupleExpr {
	es := make([]Expr, len(vs))
	for i, v := range vs {
		switch x := v.(type) {
		case Expr:
			es[i] = x
		case nil:
			es[i] = rawText("NULL")
		default:
			es[i] = &litExpr{v: v}
		}
	}
	return TupleExpr{es: es}
}

func (w TupleExpr) writeSQL(r *render) {
	if len(w.es) < 2 {
		r.fail(errf("Tuple needs at least two values, got %d", len(w.es)))
		return
	}
	r.s("ROW(")
	writeExprs(r, w.es)
	r.b(')')
}

// Eq is (w = o).
func (w TupleExpr) Eq(o TupleExpr) Pred { return parenPred(w, " = ", o) }

// Ne is (w <> o): some element differs.
func (w TupleExpr) Ne(o TupleExpr) Pred { return parenPred(w, " <> ", o) }

// Gt is (w > o).
func (w TupleExpr) Gt(o TupleExpr) Pred { return parenPred(w, " > ", o) }

// Gte is (w >= o).
func (w TupleExpr) Gte(o TupleExpr) Pred { return parenPred(w, " >= ", o) }

// Lt is (w < o).
func (w TupleExpr) Lt(o TupleExpr) Pred { return parenPred(w, " < ", o) }

// Lte is (w <= o).
func (w TupleExpr) Lte(o TupleExpr) Pred { return parenPred(w, " <= ", o) }

// IsDistinctFrom is (w IS DISTINCT FROM o): some element differs, NULL counting as a value, as an
// upsert's UpdateWhere skips a row whose stored values equal the proposed ones.
func (w TupleExpr) IsDistinctFrom(o TupleExpr) Pred { return parenPred(w, " IS DISTINCT FROM ", o) }

// IsNotDistinctFrom is (w IS NOT DISTINCT FROM o).
func (w TupleExpr) IsNotDistinctFrom(o TupleExpr) Pred {
	return parenPred(w, " IS NOT DISTINCT FROM ", o)
}

// InUnnest is (w IN (SELECT * FROM unnest(arrays…))): w equals one of the rows the arrays zip into,
// element i of each array making row i. Pass typed array literals (keel.TextArray(keys),
// keel.Int64Array(ns)), each bound as one parameter, so the statement keeps one shape whatever the
// number of rows.
func (w TupleExpr) InUnnest(arrays ...Expr) Pred {
	if len(arrays) == 0 {
		return predExpr{e: func(r *render) { r.fail(errf("InUnnest needs at least one array")) }}
	}
	ops := make([]Expr, len(arrays))
	for i, a := range arrays {
		ops[i] = operandOf(a, "InUnnest")
	}
	return predExpr{e: func(r *render) {
		r.b('(')
		w.writeSQL(r)
		r.s(" IN (SELECT * FROM unnest(")
		writeExprs(r, ops)
		r.s(")))")
	}}
}
