package keel

// RowExpr is a row value, ROW(a, b, …), built by keel.Row. Rows compare element by element in
// order, so keel.Row(e.OccurredAt, e.ID).Gt(keel.Row(after.At, after.ID)) is the keyset condition of
// a page ordered by occurred_at, id ascending, which an index on (occurred_at, id) serves as one
// range scan; a Desc page uses Lt.
type RowExpr struct{ es []Expr }

// Row is the row value ROW(vs…): each v is an expression written in place, nil as NULL, or a Go
// value bound as a parameter whose type Postgres takes from the element it is compared with. A
// row needs at least two values; one with fewer makes a comparison that fails.
func Row(vs ...any) RowExpr {
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
	return RowExpr{es: es}
}

func (w RowExpr) writeSQL(r *render) {
	if len(w.es) < 2 {
		r.fail(errf("Row needs at least two values, got %d", len(w.es)))
		return
	}
	r.s("ROW(")
	writeExprs(r, w.es)
	r.b(')')
}

// Eq is (w = o).
func (w RowExpr) Eq(o RowExpr) Pred { return parenPred(w, " = ", o) }

// Ne is (w <> o): some element differs.
func (w RowExpr) Ne(o RowExpr) Pred { return parenPred(w, " <> ", o) }

// Gt is (w > o).
func (w RowExpr) Gt(o RowExpr) Pred { return parenPred(w, " > ", o) }

// Gte is (w >= o).
func (w RowExpr) Gte(o RowExpr) Pred { return parenPred(w, " >= ", o) }

// Lt is (w < o).
func (w RowExpr) Lt(o RowExpr) Pred { return parenPred(w, " < ", o) }

// Lte is (w <= o).
func (w RowExpr) Lte(o RowExpr) Pred { return parenPred(w, " <= ", o) }

// IsDistinctFrom is (w IS DISTINCT FROM o): some element differs, NULL counting as a value, as an
// upsert's UpdateWhere skips a row whose stored values equal the proposed ones.
func (w RowExpr) IsDistinctFrom(o RowExpr) Pred { return parenPred(w, " IS DISTINCT FROM ", o) }

// IsNotDistinctFrom is (w IS NOT DISTINCT FROM o).
func (w RowExpr) IsNotDistinctFrom(o RowExpr) Pred {
	return parenPred(w, " IS NOT DISTINCT FROM ", o)
}

// InUnnest is (w IN (SELECT * FROM unnest(arrays…))): w equals one of the rows the arrays zip into,
// element i of each array making row i. Pass typed array literals (keel.TextArray(keys),
// keel.Int64Array(ns)), each bound as one parameter, so the statement keeps one shape whatever the
// number of rows.
func (w RowExpr) InUnnest(arrays ...Expr) Pred {
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
