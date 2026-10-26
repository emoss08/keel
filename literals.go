package keel

import (
	"strconv"
	"strings"
	"time"
)

// Typed literals beyond keel.Text, Int64, …: intervals, typed NULLs, values that may be NULL, and
// constants written into the statement text.

// Interval binds s, an interval in Postgres's input syntax ("1 day", "36 hours", "P1DT12H"), as
// $n::interval.
func Interval(s string) IntervalExpr { return IntervalExpr{e: &castLit{v: s, typ: "interval"}} }

// Duration binds d as $n::interval, in microseconds, Postgres's resolution.
func Duration(d time.Duration) IntervalExpr {
	return Interval(strconv.FormatInt(d.Microseconds(), 10) + " microseconds")
}

// Typed NULLs: NULL under the cast of the kind, for a CASE branch, a select list output, a union arm
// or a function argument whose type nothing else fixes.

// NullText is NULL::text.
func NullText() StringExpr { return StringExpr{e: rawText("NULL::text")} }

// NullInt64 is NULL::bigint.
func NullInt64() Int64Expr { return Int64Expr{e: rawText("NULL::bigint")} }

// NullInt32 is NULL::integer.
func NullInt32() Int32Expr { return Int32Expr{e: rawText("NULL::integer")} }

// NullFloat64 is NULL::double precision.
func NullFloat64() Float64Expr { return Float64Expr{e: rawText("NULL::double precision")} }

// NullNumeric is NULL::numeric.
func NullNumeric() NumericExpr { return NumericExpr{e: rawText("NULL::numeric")} }

// NullBool is NULL::boolean.
func NullBool() BoolExpr { return BoolExpr{e: rawText("NULL::boolean")} }

// NullTime is NULL::timestamptz.
func NullTime() TimeExpr { return TimeExpr{e: rawText("NULL::timestamptz")} }

// NullJSONB is NULL::jsonb, SQL NULL rather than the JSON null keel.JSONB(nil) binds.
func NullJSONB() JSONBExpr { return JSONBExpr{e: rawText("NULL::jsonb")} }

// NullInterval is NULL::interval.
func NullInterval() IntervalExpr { return IntervalExpr{e: rawText("NULL::interval")} }

// Values that may be NULL: the pointed-to value bound under the kind's cast, $n::bigint, or NULL
// bound in the same place when the pointer is nil. Equal non-nil values under one cast share a
// placeholder, as keel.Int64's do. keel.Numeric(nil) binds a NULL numeric.

// TextPtr binds *p as $n::text, NULL when p is nil.
func TextPtr(p *string) StringExpr { return StringExpr{e: &castLit{v: deref(p), typ: "text"}} }

// Int64Ptr binds *p as $n::bigint, NULL when p is nil.
func Int64Ptr(p *int64) Int64Expr { return Int64Expr{e: &castLit{v: deref(p), typ: "bigint"}} }

// Int32Ptr binds *p as $n::integer, NULL when p is nil.
func Int32Ptr(p *int32) Int32Expr { return Int32Expr{e: &castLit{v: deref(p), typ: "integer"}} }

// Float64Ptr binds *p as $n::double precision, NULL when p is nil.
func Float64Ptr(p *float64) Float64Expr {
	return Float64Expr{e: &castLit{v: deref(p), typ: "double precision"}}
}

// BoolPtr binds *p as $n::boolean, NULL when p is nil.
func BoolPtr(p *bool) BoolExpr { return BoolExpr{e: &castLit{v: deref(p), typ: "boolean"}} }

// TimePtr binds *p as $n::timestamptz, NULL when p is nil.
func TimePtr(p *time.Time) TimeExpr {
	return TimeExpr{e: &castLit{v: deref(p), typ: "timestamptz"}}
}

// deref is *p, or a nil interface when p is nil, so the parameter binds NULL.
func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// Constants written into the statement text instead of bound: for an expression that must match an
// expression index, the predicate of a partial index or an ON CONFLICT target, which Postgres
// proves only against constants it sees (a parameter is unknown to a generic plan). Each distinct
// constant is a distinct statement, so constants are for values fixed in code; values from users
// are bound (keel.Text, keel.Int64, …). The rendering is DynConst's: an integer as written, a string
// quoted with its single quotes doubled. A negative number is parenthesised, so it never meets a
// minus sign as a comment.

// constExpr is v written into the SQL by constText, or an expression that fails with its error.
func constExpr(v any) Expr {
	text, err := constText(v)
	if err != nil {
		return exprFunc(func(r *render) { r.fail(err) })
	}
	if strings.HasPrefix(text, "-") {
		text = "(" + text + ")"
	}
	return rawText(text)
}

// ConstText writes s into the SQL as a quoted literal, its single quotes doubled. The literal is of
// unknown type until it meets a value, as a constant in an index definition is: coalescing a
// varchar column with ConstText("") resolves to varchar and matches an index on that expression. A
// string holding a NUL byte makes an expression that fails.
func ConstText(s string) StringExpr { return StringExpr{e: constExpr(s)} }

// ConstInt64 writes n into the SQL: 768, or (-1). Postgres types an integer constant as integer when
// it fits and bigint otherwise, and compares and computes it with a bigint column as written in an
// index (dimensions = 768).
func ConstInt64(n int64) Int64Expr { return Int64Expr{e: constExpr(n)} }

// ConstInt32 writes n into the SQL.
func ConstInt32(n int32) Int32Expr { return Int32Expr{e: constExpr(n)} }

// ConstBool writes TRUE or FALSE into the SQL.
func ConstBool(b bool) BoolExpr { return BoolExpr{e: constExpr(b)} }

// ConstFloat64 writes f into the SQL as '0.5'::double precision, a double precision constant
// (Postgres would read a bare 0.5 as numeric and a bare 1 as integer, dividing as integers). NaN
// and infinities make an expression that fails.
func ConstFloat64(f float64) Float64Expr {
	text, err := constText(f)
	if err != nil {
		return Float64Expr{e: exprFunc(func(r *render) { r.fail(err) })}
	}
	return Float64Expr{e: rawText("'" + text + "'::double precision")}
}

// ConstNumeric writes s, a decimal such as "0.035" or "-12", into the SQL as '0.035'::numeric. A
// string that is not an optionally signed decimal makes an expression that fails.
func ConstNumeric(s string) NumericExpr {
	if !decimalText(s) {
		return NumericExpr{e: exprFunc(func(r *render) { r.fail(errf("ConstNumeric of %q", s)) })}
	}
	return NumericExpr{e: rawText("'" + s + "'::numeric")}
}

// decimalText reports whether s is digits with an optional sign and an optional fraction: 12, -0.5,
// +3.25.
func decimalText(s string) bool {
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	whole, frac, dotted := strings.Cut(s, ".")
	return digits(whole) && (!dotted || digits(frac))
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Casts with no other typed form. Each is CAST(e AS type) of any expression: text that holds a
// number, a boolean or a document, a number of another kind, a jsonb path read as text. Postgres
// raises an error for a value the type does not read.

// CastInt32 is CAST(e AS integer).
func CastInt32(e Expr) Int32Expr { return Int32Expr{e: castTo(e, "integer")} }

// CastBool is CAST(e AS boolean): text 'true', 't', 'yes', 'on', '1' and their opposites, or an
// integer.
func CastBool(e Expr) BoolExpr { return BoolExpr{e: castTo(e, "boolean")} }

// CastJSONB is CAST(e AS jsonb): JSON text or a json value as jsonb.
func CastJSONB(e Expr) JSONBExpr { return JSONBExpr{e: castTo(e, "jsonb")} }

// CastTime is CAST(e AS timestamptz): a timestamp, a date or time text as a timestamptz.
func CastTime(e Expr) TimeExpr { return TimeExpr{e: castTo(e, "timestamptz")} }

// CastInterval is CAST(e AS interval): interval text as an interval.
func CastInterval(e Expr) IntervalExpr { return IntervalExpr{e: castTo(e, "interval")} }
