package keel

// Typed CASE. Each result kind has its own builder, started by keel.CaseString(), CaseInt64(), …:
// When adds WHEN cond THEN v, with v a value of the kind, and Else or End finishes the expression
// as the kind's expression type, so the result compares, aggregates and nests as any other value
// of that kind:
//
//	bucket := keel.CaseString().
//		When(age.Lte(30), keel.Text("current")).
//		When(age.Lte(60), keel.Text("31-60")).
//		Else(keel.Text("over 60"))
//
// The builders share one non-generic core. A nil condition (keel.When(false, p)) skips its WHEN, as
// Where skips a nil predicate; a CASE left with no WHEN is its ELSE value, or NULL. A nil value is
// NULL. The SQL is CASE WHEN … THEN … [ELSE …] END, as DynCase writes it.

// caseWhen is one WHEN cond THEN then.
type caseWhen struct {
	cond Pred
	then Expr
}

// caseWhens is the WHEN list of a typed CASE. Adding to it copies, so a builder shared by two
// branches of code stays as it was.
type caseWhens struct{ whens []caseWhen }

func (c caseWhens) add(cond Pred, then Expr) caseWhens {
	if cond == nil {
		return c
	}
	if then == nil {
		then = rawText("NULL")
	}
	whens := make([]caseWhen, len(c.whens), len(c.whens)+1)
	copy(whens, c.whens)
	return caseWhens{whens: append(whens, caseWhen{cond: cond, then: then})}
}

// end is the CASE with ELSE els; a nil els leaves ELSE out.
func (c caseWhens) end(els Expr) Expr {
	whens := c.whens
	if len(whens) == 0 {
		if els == nil {
			return rawText("NULL")
		}
		return els
	}
	return exprFunc(func(r *render) {
		r.s("CASE")
		for _, w := range whens {
			r.s(" WHEN ")
			w.cond.writeSQL(r)
			r.s(" THEN ")
			w.then.writeSQL(r)
		}
		if els != nil {
			r.s(" ELSE ")
			els.writeSQL(r)
		}
		r.s(" END")
	})
}

// elseOf is v as an ELSE value: NULL when v is nil, which End leaves out.
func elseOf(v Expr) Expr {
	if v == nil {
		return rawText("NULL")
	}
	return v
}

// StringCase builds a CASE whose result is text. Start it with CaseString.
type StringCase struct{ c caseWhens }

// CaseString starts a CASE whose result is text.
func CaseString() StringCase { return StringCase{} }

// When adds WHEN cond THEN v.
func (b StringCase) When(cond Pred, v StringValue) StringCase {
	return StringCase{c: b.c.add(cond, v)}
}

// Else ends the CASE with ELSE v.
func (b StringCase) Else(v StringValue) StringExpr { return StringExpr{e: b.c.end(elseOf(v))} }

// End ends the CASE without ELSE: rows no WHEN matches are NULL.
func (b StringCase) End() StringExpr { return StringExpr{e: b.c.end(nil)} }

// Int64Case builds a CASE whose result is bigint. Start it with CaseInt64.
type Int64Case struct{ c caseWhens }

// CaseInt64 starts a CASE whose result is bigint.
func CaseInt64() Int64Case { return Int64Case{} }

// When adds WHEN cond THEN v.
func (b Int64Case) When(cond Pred, v Int64Value) Int64Case { return Int64Case{c: b.c.add(cond, v)} }

// Else ends the CASE with ELSE v.
func (b Int64Case) Else(v Int64Value) Int64Expr { return Int64Expr{e: b.c.end(elseOf(v))} }

// End ends the CASE without ELSE.
func (b Int64Case) End() Int64Expr { return Int64Expr{e: b.c.end(nil)} }

// Int32Case builds a CASE whose result is integer. Start it with CaseInt32.
type Int32Case struct{ c caseWhens }

// CaseInt32 starts a CASE whose result is integer.
func CaseInt32() Int32Case { return Int32Case{} }

// When adds WHEN cond THEN v.
func (b Int32Case) When(cond Pred, v Int32Value) Int32Case { return Int32Case{c: b.c.add(cond, v)} }

// Else ends the CASE with ELSE v.
func (b Int32Case) Else(v Int32Value) Int32Expr { return Int32Expr{e: b.c.end(elseOf(v))} }

// End ends the CASE without ELSE.
func (b Int32Case) End() Int32Expr { return Int32Expr{e: b.c.end(nil)} }

// Float64Case builds a CASE whose result is double precision. Start it with CaseFloat64.
type Float64Case struct{ c caseWhens }

// CaseFloat64 starts a CASE whose result is double precision.
func CaseFloat64() Float64Case { return Float64Case{} }

// When adds WHEN cond THEN v.
func (b Float64Case) When(cond Pred, v Float64Value) Float64Case {
	return Float64Case{c: b.c.add(cond, v)}
}

// Else ends the CASE with ELSE v.
func (b Float64Case) Else(v Float64Value) Float64Expr { return Float64Expr{e: b.c.end(elseOf(v))} }

// End ends the CASE without ELSE.
func (b Float64Case) End() Float64Expr { return Float64Expr{e: b.c.end(nil)} }

// NumericCase builds a CASE whose result is numeric. Start it with CaseNumeric.
type NumericCase struct{ c caseWhens }

// CaseNumeric starts a CASE whose result is numeric.
func CaseNumeric() NumericCase { return NumericCase{} }

// When adds WHEN cond THEN v.
func (b NumericCase) When(cond Pred, v NumericValue) NumericCase {
	return NumericCase{c: b.c.add(cond, v)}
}

// Else ends the CASE with ELSE v.
func (b NumericCase) Else(v NumericValue) NumericExpr { return NumericExpr{e: b.c.end(elseOf(v))} }

// End ends the CASE without ELSE.
func (b NumericCase) End() NumericExpr { return NumericExpr{e: b.c.end(nil)} }

// BoolCase builds a CASE whose result is boolean, itself a predicate. Start it with CaseBool.
type BoolCase struct{ c caseWhens }

// CaseBool starts a CASE whose result is boolean.
func CaseBool() BoolCase { return BoolCase{} }

// When adds WHEN cond THEN v.
func (b BoolCase) When(cond Pred, v BoolValue) BoolCase { return BoolCase{c: b.c.add(cond, v)} }

// Else ends the CASE with ELSE v.
func (b BoolCase) Else(v BoolValue) BoolExpr { return BoolExpr{e: b.c.end(elseOf(v))} }

// End ends the CASE without ELSE.
func (b BoolCase) End() BoolExpr { return BoolExpr{e: b.c.end(nil)} }

// TimeCase builds a CASE whose result is a time. Start it with CaseTime.
type TimeCase struct{ c caseWhens }

// CaseTime starts a CASE whose result is a time.
func CaseTime() TimeCase { return TimeCase{} }

// When adds WHEN cond THEN v.
func (b TimeCase) When(cond Pred, v TimeValue) TimeCase { return TimeCase{c: b.c.add(cond, v)} }

// Else ends the CASE with ELSE v.
func (b TimeCase) Else(v TimeValue) TimeExpr { return TimeExpr{e: b.c.end(elseOf(v))} }

// End ends the CASE without ELSE.
func (b TimeCase) End() TimeExpr { return TimeExpr{e: b.c.end(nil)} }

// JSONBCase builds a CASE whose result is jsonb. Start it with CaseJSONB.
type JSONBCase struct{ c caseWhens }

// CaseJSONB starts a CASE whose result is jsonb.
func CaseJSONB() JSONBCase { return JSONBCase{} }

// When adds WHEN cond THEN v.
func (b JSONBCase) When(cond Pred, v JSONBValue) JSONBCase { return JSONBCase{c: b.c.add(cond, v)} }

// Else ends the CASE with ELSE v.
func (b JSONBCase) Else(v JSONBValue) JSONBExpr { return JSONBExpr{e: b.c.end(elseOf(v))} }

// End ends the CASE without ELSE.
func (b JSONBCase) End() JSONBExpr { return JSONBExpr{e: b.c.end(nil)} }

// IntervalCase builds a CASE whose result is an interval. Start it with CaseInterval.
type IntervalCase struct{ c caseWhens }

// CaseInterval starts a CASE whose result is an interval.
func CaseInterval() IntervalCase { return IntervalCase{} }

// When adds WHEN cond THEN v.
func (b IntervalCase) When(cond Pred, v IntervalValue) IntervalCase {
	return IntervalCase{c: b.c.add(cond, v)}
}

// Else ends the CASE with ELSE v.
func (b IntervalCase) Else(v IntervalValue) IntervalExpr {
	return IntervalExpr{e: b.c.end(elseOf(v))}
}

// End ends the CASE without ELSE.
func (b IntervalCase) End() IntervalExpr { return IntervalExpr{e: b.c.end(nil)} }
