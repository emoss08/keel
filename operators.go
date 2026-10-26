package keel

import (
	"strings"
	"time"

	"github.com/emoss08/keel/schema"
)

// Operators of the typed expression kinds: arithmetic on numbers, times and intervals, text
// concatenation with ||, collation, IS [NOT] DISTINCT FROM between two values of a kind, and
// membership in an array value. Each operator takes the value interface of its kind and returns the
// expression type of its result, and renders as keel's dynamic expressions do: an arithmetic or
// concatenation is parenthesised, (a + b), so it nests at any precedence. Kinds do not mix: an
// integer meets a bigint through CastInt64, a number meets a numeric through CastNumeric, which is
// also where Postgres's own promotion would take them.

// operandOf is e as the operand of op, or an expression that fails the statement when e is nil.
func operandOf(e Expr, op string) Expr {
	if e == nil {
		op = strings.TrimSpace(op)
		return exprFunc(func(r *render) { r.fail(errf("operand of %s is a nil expression", op)) })
	}
	return e
}

// arith is (a op b).
func arith(a Expr, op string, b Expr) Expr {
	return binOp{a: operandOf(a, op), b: operandOf(b, op), op: op}
}

// neg is (-e).
func neg(e Expr) Expr {
	e = operandOf(e, "-")
	return exprFunc(func(r *render) {
		r.s("(-")
		e.writeSQL(r)
		r.b(')')
	})
}

// parenPred is (a op b) as a predicate.
func parenPred(a Expr, op string, b Expr) Pred {
	a, b = operandOf(a, op), operandOf(b, op)
	return predExpr{e: func(r *render) {
		r.b('(')
		a.writeSQL(r)
		r.s(op)
		b.writeSQL(r)
		r.b(')')
	}}
}

// distinctExpr is a IS [NOT] DISTINCT FROM b.
func distinctExpr(a Expr, not bool, b Expr) Pred {
	op := " IS DISTINCT FROM "
	if not {
		op = " IS NOT DISTINCT FROM "
	}
	return binExpr(a, op, operandOf(b, op))
}

// Arithmetic on bigint values. Integer division truncates toward zero; Postgres raises an error on
// overflow and on division by zero (NullIf(0) on the divisor makes it NULL instead).

// Add is (e + v).
func (e Int64Expr) Add(v Int64Value) Int64Expr { return Int64Expr{e: arith(e.e, " + ", v)} }

// Sub is (e - v).
func (e Int64Expr) Sub(v Int64Value) Int64Expr { return Int64Expr{e: arith(e.e, " - ", v)} }

// Mul is (e * v).
func (e Int64Expr) Mul(v Int64Value) Int64Expr { return Int64Expr{e: arith(e.e, " * ", v)} }

// Div is (e / v), truncated toward zero.
func (e Int64Expr) Div(v Int64Value) Int64Expr { return Int64Expr{e: arith(e.e, " / ", v)} }

// Mod is (e % v), the remainder, with the sign of e.
func (e Int64Expr) Mod(v Int64Value) Int64Expr { return Int64Expr{e: arith(e.e, " % ", v)} }

// Neg is (-e).
func (e Int64Expr) Neg() Int64Expr { return Int64Expr{e: neg(e.e)} }

// Add is (col + v), an expression; Increment is the assignment col = col + v.
func (c Int64Col) Add(v Int64Value) Int64Expr { return Int64Expr{e: c.Col}.Add(v) }

// Sub is (col - v).
func (c Int64Col) Sub(v Int64Value) Int64Expr { return Int64Expr{e: c.Col}.Sub(v) }

// Mul is (col * v).
func (c Int64Col) Mul(v Int64Value) Int64Expr { return Int64Expr{e: c.Col}.Mul(v) }

// Div is (col / v), truncated toward zero.
func (c Int64Col) Div(v Int64Value) Int64Expr { return Int64Expr{e: c.Col}.Div(v) }

// Mod is (col % v).
func (c Int64Col) Mod(v Int64Value) Int64Expr { return Int64Expr{e: c.Col}.Mod(v) }

// Neg is (-col).
func (c Int64Col) Neg() Int64Expr { return Int64Expr{e: c.Col}.Neg() }

// Arithmetic on integer values: integer results, so a product past 2^31 overflows; CastInt64 widens
// first.

// Add is (e + v).
func (e Int32Expr) Add(v Int32Value) Int32Expr { return Int32Expr{e: arith(e.e, " + ", v)} }

// Sub is (e - v).
func (e Int32Expr) Sub(v Int32Value) Int32Expr { return Int32Expr{e: arith(e.e, " - ", v)} }

// Mul is (e * v).
func (e Int32Expr) Mul(v Int32Value) Int32Expr { return Int32Expr{e: arith(e.e, " * ", v)} }

// Div is (e / v), truncated toward zero.
func (e Int32Expr) Div(v Int32Value) Int32Expr { return Int32Expr{e: arith(e.e, " / ", v)} }

// Mod is (e % v).
func (e Int32Expr) Mod(v Int32Value) Int32Expr { return Int32Expr{e: arith(e.e, " % ", v)} }

// Neg is (-e).
func (e Int32Expr) Neg() Int32Expr { return Int32Expr{e: neg(e.e)} }

// Add is (col + v), an expression; Increment is the assignment.
func (c Int32Col) Add(v Int32Value) Int32Expr { return Int32Expr{e: c.Col}.Add(v) }

// Sub is (col - v).
func (c Int32Col) Sub(v Int32Value) Int32Expr { return Int32Expr{e: c.Col}.Sub(v) }

// Mul is (col * v).
func (c Int32Col) Mul(v Int32Value) Int32Expr { return Int32Expr{e: c.Col}.Mul(v) }

// Div is (col / v), truncated toward zero.
func (c Int32Col) Div(v Int32Value) Int32Expr { return Int32Expr{e: c.Col}.Div(v) }

// Mod is (col % v).
func (c Int32Col) Mod(v Int32Value) Int32Expr { return Int32Expr{e: c.Col}.Mod(v) }

// Neg is (-col).
func (c Int32Col) Neg() Int32Expr { return Int32Expr{e: c.Col}.Neg() }

// Arithmetic on double precision values. Postgres has no % for double precision; CastNumeric first.

// Add is (e + v).
func (e Float64Expr) Add(v Float64Value) Float64Expr { return Float64Expr{e: arith(e.e, " + ", v)} }

// Sub is (e - v).
func (e Float64Expr) Sub(v Float64Value) Float64Expr { return Float64Expr{e: arith(e.e, " - ", v)} }

// Mul is (e * v).
func (e Float64Expr) Mul(v Float64Value) Float64Expr { return Float64Expr{e: arith(e.e, " * ", v)} }

// Div is (e / v).
func (e Float64Expr) Div(v Float64Value) Float64Expr { return Float64Expr{e: arith(e.e, " / ", v)} }

// Neg is (-e).
func (e Float64Expr) Neg() Float64Expr { return Float64Expr{e: neg(e.e)} }

// Add is (col + v), an expression; Increment is the assignment.
func (c Float64Col) Add(v Float64Value) Float64Expr { return Float64Expr{e: c.Col}.Add(v) }

// Sub is (col - v).
func (c Float64Col) Sub(v Float64Value) Float64Expr { return Float64Expr{e: c.Col}.Sub(v) }

// Mul is (col * v).
func (c Float64Col) Mul(v Float64Value) Float64Expr { return Float64Expr{e: c.Col}.Mul(v) }

// Div is (col / v).
func (c Float64Col) Div(v Float64Value) Float64Expr { return Float64Expr{e: c.Col}.Div(v) }

// Neg is (-col).
func (c Float64Col) Neg() Float64Expr { return Float64Expr{e: c.Col}.Neg() }

// Arithmetic on numeric values, exact; a division keeps the scale Postgres chooses (at least the
// larger of the operands' scales, 16 digits or more).

// Add is (e + v).
func (e NumericExpr) Add(v NumericValue) NumericExpr { return NumericExpr{e: arith(e.e, " + ", v)} }

// Sub is (e - v).
func (e NumericExpr) Sub(v NumericValue) NumericExpr { return NumericExpr{e: arith(e.e, " - ", v)} }

// Mul is (e * v).
func (e NumericExpr) Mul(v NumericValue) NumericExpr { return NumericExpr{e: arith(e.e, " * ", v)} }

// Div is (e / v).
func (e NumericExpr) Div(v NumericValue) NumericExpr { return NumericExpr{e: arith(e.e, " / ", v)} }

// Mod is (e % v).
func (e NumericExpr) Mod(v NumericValue) NumericExpr { return NumericExpr{e: arith(e.e, " % ", v)} }

// Neg is (-e).
func (e NumericExpr) Neg() NumericExpr { return NumericExpr{e: neg(e.e)} }

// Add is (col + v), an expression over a numeric (scanner) column; Increment is the assignment.
func (c AnyCol) Add(v NumericValue) NumericExpr { return NumericExpr{e: c.Col}.Add(v) }

// Sub is (col - v).
func (c AnyCol) Sub(v NumericValue) NumericExpr { return NumericExpr{e: c.Col}.Sub(v) }

// Mul is (col * v).
func (c AnyCol) Mul(v NumericValue) NumericExpr { return NumericExpr{e: c.Col}.Mul(v) }

// Div is (col / v).
func (c AnyCol) Div(v NumericValue) NumericExpr { return NumericExpr{e: c.Col}.Div(v) }

// Mod is (col % v).
func (c AnyCol) Mod(v NumericValue) NumericExpr { return NumericExpr{e: c.Col}.Mod(v) }

// Neg is (-col).
func (c AnyCol) Neg() NumericExpr { return NumericExpr{e: c.Col}.Neg() }

// Time arithmetic.

// Add is (e + i): the time an interval later.
func (e TimeExpr) Add(i IntervalValue) TimeExpr { return TimeExpr{e: arith(e.e, " + ", i)} }

// Sub is (e - i): the time an interval earlier.
func (e TimeExpr) Sub(i IntervalValue) TimeExpr { return TimeExpr{e: arith(e.e, " - ", i)} }

// Since is (e - u): the interval from u to e, negative when u is later.
func (e TimeExpr) Since(u TimeValue) IntervalExpr { return IntervalExpr{e: arith(e.e, " - ", u)} }

// Add is (col + i).
func (c TimeCol) Add(i IntervalValue) TimeExpr { return TimeExpr{e: c.Col}.Add(i) }

// Sub is (col - i).
func (c TimeCol) Sub(i IntervalValue) TimeExpr { return TimeExpr{e: c.Col}.Sub(i) }

// Since is (col - u).
func (c TimeCol) Since(u TimeValue) IntervalExpr { return TimeExpr{e: c.Col}.Since(u) }

// IntervalValue is an interval: an IntervalExpr (keel.Interval, keel.Duration, TimeExpr.Since, a
// typed view).
type IntervalValue interface {
	Expr
	intervalValue()
}

// IntervalExpr is a computed interval: the difference of two times, a bound interval.
type IntervalExpr struct{ e Expr }

func (e IntervalExpr) writeSQL(r *render) { e.e.writeSQL(r) }
func (IntervalExpr) intervalValue()       {}

// IntervalOf types e as an interval without changing the SQL.
func IntervalOf(e Expr) IntervalExpr { return IntervalExpr{e: viewOf(e)} }

// Add is (e + o).
func (e IntervalExpr) Add(o IntervalValue) IntervalExpr {
	return IntervalExpr{e: arith(e.e, " + ", o)}
}

// Sub is (e - o).
func (e IntervalExpr) Sub(o IntervalValue) IntervalExpr {
	return IntervalExpr{e: arith(e.e, " - ", o)}
}

// Mul is (e * n): keel.Interval("1 day").Mul(c.GraceDays) is that many days.
func (e IntervalExpr) Mul(n NumberValue) IntervalExpr { return IntervalExpr{e: arith(e.e, " * ", n)} }

// Div is (e / n).
func (e IntervalExpr) Div(n NumberValue) IntervalExpr { return IntervalExpr{e: arith(e.e, " / ", n)} }

// Neg is (-e).
func (e IntervalExpr) Neg() IntervalExpr { return IntervalExpr{e: neg(e.e)} }

// Seconds is date_part('epoch', e): the interval's length in seconds, months counted as 30 days.
func (e IntervalExpr) Seconds() Float64Expr {
	return Float64Expr{e: callOf("date_part", rawText("'epoch'"), e.e)}
}

// EqExpr is e = o.
func (e IntervalExpr) EqExpr(o IntervalValue) Pred { return binExpr(e.e, " = ", operandOf(o, " = ")) }

// NeExpr is e <> o.
func (e IntervalExpr) NeExpr(o IntervalValue) Pred {
	return binExpr(e.e, " <> ", operandOf(o, " <> "))
}

// GtExpr is e > o.
func (e IntervalExpr) GtExpr(o IntervalValue) Pred { return binExpr(e.e, " > ", operandOf(o, " > ")) }

// GteExpr is e >= o.
func (e IntervalExpr) GteExpr(o IntervalValue) Pred {
	return binExpr(e.e, " >= ", operandOf(o, " >= "))
}

// LtExpr is e < o.
func (e IntervalExpr) LtExpr(o IntervalValue) Pred { return binExpr(e.e, " < ", operandOf(o, " < ")) }

// LteExpr is e <= o.
func (e IntervalExpr) LteExpr(o IntervalValue) Pred {
	return binExpr(e.e, " <= ", operandOf(o, " <= "))
}

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e IntervalExpr) IsDistinctFromExpr(o IntervalValue) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e IntervalExpr) IsNotDistinctFromExpr(o IntervalValue) Pred {
	return distinctExpr(e.e, true, o)
}

// IsNull is e IS NULL.
func (e IntervalExpr) IsNull() Pred { return suffix(e.e, " IS NULL") }

// IsNotNull is e IS NOT NULL.
func (e IntervalExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }

// Asc orders by e ascending.
func (e IntervalExpr) Asc() Order { return Order{e: e.e} }

// Desc orders by e descending.
func (e IntervalExpr) Desc() Order { return Order{e: e.e, desc: true} }

// Sum is the aggregate sum(e).
func (e IntervalExpr) Sum() Agg { return Agg{fn: "sum", arg: e} }

// Avg is the aggregate avg(e).
func (e IntervalExpr) Avg() Agg { return Agg{fn: "avg", arg: e} }

// Min is the aggregate min(e).
func (e IntervalExpr) Min() Agg { return Agg{fn: "min", arg: e} }

// Max is the aggregate max(e).
func (e IntervalExpr) Max() Agg { return Agg{fn: "max", arg: e} }

// Text concatenation.

// Cat is (e || v), each further value appended the same way, ((e || v) || w): text joined with
// SQL's NULL semantics, so a NULL operand makes the whole result NULL. keel.Concat and ConcatWS
// skip NULL arguments instead. Pass keel.Text(s) for a Go string.
func (e StringExpr) Cat(vs ...StringValue) StringExpr {
	out := e.e
	for _, v := range vs {
		out = arith(out, " || ", v)
	}
	return StringExpr{e: out}
}

// Cat is (col || v), each further value appended the same way.
func (c StringCol) Cat(vs ...StringValue) StringExpr { return StringExpr{e: c.Col}.Cat(vs...) }

// Collation.

// collate is (e COLLATE "name"). The name is quoted as one identifier: it must be non-empty, at most
// 63 bytes (Postgres's identifier limit) and hold no double quote, NUL or control character.
func collate(e Expr, name string) Expr {
	if !validCollation(name) {
		return exprFunc(func(r *render) { r.fail(errf("collation name %q", name)) })
	}
	quoted := schema.Quote(name)
	return exprFunc(func(r *render) {
		r.b('(')
		e.writeSQL(r)
		r.s(" COLLATE ")
		r.s(quoted)
		r.b(')')
	})
}

func validCollation(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c == '"' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// Collate is (e COLLATE "name"): e compared and ordered by that collation, as Collate("C") orders by
// bytes, which a text_pattern_ops or "C" index serves. A name that is empty, longer than 63 bytes or
// holds a double quote or control character makes an expression that fails.
func (e StringExpr) Collate(name string) StringExpr { return StringExpr{e: collate(e.e, name)} }

// Collate is (col COLLATE "name").
func (c StringCol) Collate(name string) StringExpr { return StringExpr{e: collate(c.Col, name)} }

// IS [NOT] DISTINCT FROM: equality and inequality that treat NULL as a value.

// IsNotDistinctFrom is e IS NOT DISTINCT FROM v.
func (e StringExpr) IsNotDistinctFrom(v string) Pred {
	return cmp(e.e, " IS NOT DISTINCT FROM ", v)
}

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e StringExpr) IsDistinctFromExpr(o StringValue) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e StringExpr) IsNotDistinctFromExpr(o StringValue) Pred { return distinctExpr(e.e, true, o) }

// IsNotDistinctFrom is e IS NOT DISTINCT FROM v.
func (e Int64Expr) IsNotDistinctFrom(v int64) Pred { return cmp(e.e, " IS NOT DISTINCT FROM ", v) }

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e Int64Expr) IsDistinctFromExpr(o Int64Value) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e Int64Expr) IsNotDistinctFromExpr(o Int64Value) Pred { return distinctExpr(e.e, true, o) }

// IsNotDistinctFrom is e IS NOT DISTINCT FROM v.
func (e Int32Expr) IsNotDistinctFrom(v int32) Pred { return cmp(e.e, " IS NOT DISTINCT FROM ", v) }

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e Int32Expr) IsDistinctFromExpr(o Int32Value) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e Int32Expr) IsNotDistinctFromExpr(o Int32Value) Pred { return distinctExpr(e.e, true, o) }

// IsNotDistinctFrom is e IS NOT DISTINCT FROM v.
func (e Float64Expr) IsNotDistinctFrom(v float64) Pred {
	return cmp(e.e, " IS NOT DISTINCT FROM ", v)
}

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e Float64Expr) IsDistinctFromExpr(o Float64Value) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e Float64Expr) IsNotDistinctFromExpr(o Float64Value) Pred {
	return distinctExpr(e.e, true, o)
}

// IsNotDistinctFrom is e IS NOT DISTINCT FROM v.
func (e NumericExpr) IsNotDistinctFrom(v any) Pred { return cmp(e.e, " IS NOT DISTINCT FROM ", v) }

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e NumericExpr) IsDistinctFromExpr(o NumericValue) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e NumericExpr) IsNotDistinctFromExpr(o NumericValue) Pred {
	return distinctExpr(e.e, true, o)
}

// IsDistinctFrom is e IS DISTINCT FROM v.
func (e BoolExpr) IsDistinctFrom(v bool) Pred { return cmp(e.e, " IS DISTINCT FROM ", v) }

// IsNotDistinctFrom is e IS NOT DISTINCT FROM v.
func (e BoolExpr) IsNotDistinctFrom(v bool) Pred { return cmp(e.e, " IS NOT DISTINCT FROM ", v) }

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e BoolExpr) IsDistinctFromExpr(o BoolValue) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e BoolExpr) IsNotDistinctFromExpr(o BoolValue) Pred { return distinctExpr(e.e, true, o) }

// IsDistinctFrom is e IS DISTINCT FROM v.
func (e TimeExpr) IsDistinctFrom(v time.Time) Pred { return cmp(e.e, " IS DISTINCT FROM ", v) }

// IsNotDistinctFrom is e IS NOT DISTINCT FROM v.
func (e TimeExpr) IsNotDistinctFrom(v time.Time) Pred {
	return cmp(e.e, " IS NOT DISTINCT FROM ", v)
}

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e TimeExpr) IsDistinctFromExpr(o TimeValue) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e TimeExpr) IsNotDistinctFromExpr(o TimeValue) Pred { return distinctExpr(e.e, true, o) }

// IsDistinctFromExpr is e IS DISTINCT FROM o.
func (e JSONBExpr) IsDistinctFromExpr(o JSONBValue) Pred { return distinctExpr(e.e, false, o) }

// IsNotDistinctFromExpr is e IS NOT DISTINCT FROM o.
func (e JSONBExpr) IsNotDistinctFromExpr(o JSONBValue) Pred { return distinctExpr(e.e, true, o) }

// The same tests on columns. A value v compared with a nullzero column follows nullzero's meaning of
// the zero value, as the column's other comparisons do.

// IsNotDistinctFrom is col IS NOT DISTINCT FROM v.
func (c StringCol) IsNotDistinctFrom(v string) Pred {
	return cmp(c.Col, " IS NOT DISTINCT FROM ", v)
}

// IsDistinctFromExpr is col IS DISTINCT FROM o.
func (c StringCol) IsDistinctFromExpr(o StringValue) Pred { return distinctExpr(c.Col, false, o) }

// IsNotDistinctFromExpr is col IS NOT DISTINCT FROM o.
func (c StringCol) IsNotDistinctFromExpr(o StringValue) Pred { return distinctExpr(c.Col, true, o) }

// IsNotDistinctFrom is col IS NOT DISTINCT FROM v.
func (c Int64Col) IsNotDistinctFrom(v int64) Pred { return cmp(c.Col, " IS NOT DISTINCT FROM ", v) }

// IsDistinctFromExpr is col IS DISTINCT FROM o.
func (c Int64Col) IsDistinctFromExpr(o Int64Value) Pred { return distinctExpr(c.Col, false, o) }

// IsNotDistinctFromExpr is col IS NOT DISTINCT FROM o.
func (c Int64Col) IsNotDistinctFromExpr(o Int64Value) Pred { return distinctExpr(c.Col, true, o) }

// IsNotDistinctFrom is col IS NOT DISTINCT FROM v.
func (c Int32Col) IsNotDistinctFrom(v int32) Pred { return cmp(c.Col, " IS NOT DISTINCT FROM ", v) }

// IsDistinctFromExpr is col IS DISTINCT FROM o.
func (c Int32Col) IsDistinctFromExpr(o Int32Value) Pred { return distinctExpr(c.Col, false, o) }

// IsNotDistinctFromExpr is col IS NOT DISTINCT FROM o.
func (c Int32Col) IsNotDistinctFromExpr(o Int32Value) Pred { return distinctExpr(c.Col, true, o) }

// IsNotDistinctFrom is col IS NOT DISTINCT FROM v.
func (c Int16Col) IsNotDistinctFrom(v int16) Pred { return cmp(c.Col, " IS NOT DISTINCT FROM ", v) }

// IsNotDistinctFrom is col IS NOT DISTINCT FROM v.
func (c Float64Col) IsNotDistinctFrom(v float64) Pred {
	return cmp(c.Col, " IS NOT DISTINCT FROM ", v)
}

// IsDistinctFromExpr is col IS DISTINCT FROM o.
func (c Float64Col) IsDistinctFromExpr(o Float64Value) Pred { return distinctExpr(c.Col, false, o) }

// IsNotDistinctFromExpr is col IS NOT DISTINCT FROM o.
func (c Float64Col) IsNotDistinctFromExpr(o Float64Value) Pred {
	return distinctExpr(c.Col, true, o)
}

// IsNotDistinctFrom is col IS NOT DISTINCT FROM v.
func (c AnyCol) IsNotDistinctFrom(v any) Pred { return cmp(c.Col, " IS NOT DISTINCT FROM ", v) }

// IsDistinctFromExpr is col IS DISTINCT FROM o.
func (c AnyCol) IsDistinctFromExpr(o NumericValue) Pred { return distinctExpr(c.Col, false, o) }

// IsNotDistinctFromExpr is col IS NOT DISTINCT FROM o.
func (c AnyCol) IsNotDistinctFromExpr(o NumericValue) Pred { return distinctExpr(c.Col, true, o) }

// IsDistinctFrom is col IS DISTINCT FROM v.
func (c BoolCol) IsDistinctFrom(v bool) Pred { return cmp(c.Col, " IS DISTINCT FROM ", v) }

// IsNotDistinctFrom is col IS NOT DISTINCT FROM v.
func (c BoolCol) IsNotDistinctFrom(v bool) Pred { return cmp(c.Col, " IS NOT DISTINCT FROM ", v) }

// IsDistinctFromExpr is col IS DISTINCT FROM o.
func (c BoolCol) IsDistinctFromExpr(o BoolValue) Pred { return distinctExpr(c.Col, false, o) }

// IsNotDistinctFromExpr is col IS NOT DISTINCT FROM o.
func (c BoolCol) IsNotDistinctFromExpr(o BoolValue) Pred { return distinctExpr(c.Col, true, o) }

// IsDistinctFrom is col IS DISTINCT FROM v.
func (c TimeCol) IsDistinctFrom(v time.Time) Pred { return cmp(c.Col, " IS DISTINCT FROM ", v) }

// IsNotDistinctFrom is col IS NOT DISTINCT FROM v.
func (c TimeCol) IsNotDistinctFrom(v time.Time) Pred {
	return cmp(c.Col, " IS NOT DISTINCT FROM ", v)
}

// IsDistinctFromExpr is col IS DISTINCT FROM o.
func (c TimeCol) IsDistinctFromExpr(o TimeValue) Pred { return distinctExpr(c.Col, false, o) }

// IsNotDistinctFromExpr is col IS NOT DISTINCT FROM o.
func (c TimeCol) IsNotDistinctFromExpr(o TimeValue) Pred { return distinctExpr(c.Col, true, o) }

// IsDistinctFromExpr is col IS DISTINCT FROM o.
func (c JSONBCol) IsDistinctFromExpr(o JSONBValue) Pred { return distinctExpr(c.Col, false, o) }

// IsNotDistinctFromExpr is col IS NOT DISTINCT FROM o.
func (c JSONBCol) IsNotDistinctFromExpr(o JSONBValue) Pred { return distinctExpr(c.Col, true, o) }

// Membership in an array value: (e = ANY(a)) and (e <> ALL(a)), for an array column, a typed array
// expression or a typed view of a CTE's array column. A NULL element or array makes the test NULL
// where no element matches, as in SQL.

// inArray is (e = ANY(a)), or (e <> ALL(a)) when not.
func inArray(e Expr, a Expr, not bool) Pred {
	op := " = ANY("
	if not {
		op = " <> ALL("
	}
	e, a = operandOf(e, op), operandOf(a, op)
	return predExpr{e: func(r *render) {
		r.b('(')
		e.writeSQL(r)
		r.s(op)
		a.writeSQL(r)
		r.s("))")
	}}
}

// InArray is (e = ANY(a)): e equals an element of a.
func (e StringExpr) InArray(a StringArrayValue) Pred { return inArray(e.e, a, false) }

// NotInArray is (e <> ALL(a)).
func (e StringExpr) NotInArray(a StringArrayValue) Pred { return inArray(e.e, a, true) }

// InArray is (col = ANY(a)): keel.StringArrayOf(cte.Col("ids")) types a CTE's array column.
func (c StringCol) InArray(a StringArrayValue) Pred { return inArray(c.Col, a, false) }

// NotInArray is (col <> ALL(a)).
func (c StringCol) NotInArray(a StringArrayValue) Pred { return inArray(c.Col, a, true) }

// InArray is (e = ANY(a)).
func (e Int64Expr) InArray(a Int64ArrayValue) Pred { return inArray(e.e, a, false) }

// NotInArray is (e <> ALL(a)).
func (e Int64Expr) NotInArray(a Int64ArrayValue) Pred { return inArray(e.e, a, true) }

// InArray is (col = ANY(a)).
func (c Int64Col) InArray(a Int64ArrayValue) Pred { return inArray(c.Col, a, false) }

// NotInArray is (col <> ALL(a)).
func (c Int64Col) NotInArray(a Int64ArrayValue) Pred { return inArray(c.Col, a, true) }

// InArray is (e = ANY(a)).
func (e Int32Expr) InArray(a Int32ArrayValue) Pred { return inArray(e.e, a, false) }

// NotInArray is (e <> ALL(a)).
func (e Int32Expr) NotInArray(a Int32ArrayValue) Pred { return inArray(e.e, a, true) }

// InArray is (col = ANY(a)).
func (c Int32Col) InArray(a Int32ArrayValue) Pred { return inArray(c.Col, a, false) }

// NotInArray is (col <> ALL(a)).
func (c Int32Col) NotInArray(a Int32ArrayValue) Pred { return inArray(c.Col, a, true) }

// InArray is (e = ANY(a)).
func (e Float64Expr) InArray(a Float64ArrayValue) Pred { return inArray(e.e, a, false) }

// NotInArray is (e <> ALL(a)).
func (e Float64Expr) NotInArray(a Float64ArrayValue) Pred { return inArray(e.e, a, true) }

// InArray is (col = ANY(a)).
func (c Float64Col) InArray(a Float64ArrayValue) Pred { return inArray(c.Col, a, false) }

// NotInArray is (col <> ALL(a)).
func (c Float64Col) NotInArray(a Float64ArrayValue) Pred { return inArray(c.Col, a, true) }

// InArray is (e = ANY(a)).
func (e NumericExpr) InArray(a NumericArrayValue) Pred { return inArray(e.e, a, false) }

// NotInArray is (e <> ALL(a)).
func (e NumericExpr) NotInArray(a NumericArrayValue) Pred { return inArray(e.e, a, true) }

// InArray is (col = ANY(a)).
func (c AnyCol) InArray(a NumericArrayValue) Pred { return inArray(c.Col, a, false) }

// NotInArray is (col <> ALL(a)).
func (c AnyCol) NotInArray(a NumericArrayValue) Pred { return inArray(c.Col, a, true) }

// InArray is (e = ANY(a)).
func (e TimeExpr) InArray(a TimeArrayValue) Pred { return inArray(e.e, a, false) }

// NotInArray is (e <> ALL(a)).
func (e TimeExpr) NotInArray(a TimeArrayValue) Pred { return inArray(e.e, a, true) }

// InArray is (col = ANY(a)).
func (c TimeCol) InArray(a TimeArrayValue) Pred { return inArray(c.Col, a, false) }

// NotInArray is (col <> ALL(a)).
func (c TimeCol) NotInArray(a TimeArrayValue) Pred { return inArray(c.Col, a, true) }
