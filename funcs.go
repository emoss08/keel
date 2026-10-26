package keel

import (
	"strings"

	"github.com/emoss08/keel/schema"
)

// The typed function library. String functions are methods of string columns and StringExpr,
// numeric functions methods of the numeric kinds, and functions whose result kind does not follow
// one argument (Concat, Now, DatePart, JSONBuildObject, …) are package functions. Each returns the
// expression type of its result, so what it is compared with, assigned to or passed to is checked
// by the compiler; Go values are bound as parameters, cast where Postgres cannot infer their type.

// sqlCall is name(args…).
type sqlCall struct {
	name string
	args []Expr
}

func (c sqlCall) writeSQL(r *render) {
	r.s(c.name)
	r.b('(')
	writeExprs(r, c.args)
	r.b(')')
}

// callOf is name(args…); a nil argument is NULL.
func callOf(name string, args ...Expr) sqlCall {
	for i, a := range args {
		if a == nil {
			args[i] = rawText("NULL")
		}
	}
	return sqlCall{name: name, args: args}
}

// bindOf binds v as a parameter whose type Postgres infers from the function it is passed to. The
// literal is written once per statement wherever the expression repeats.
func bindOf(v any) Expr { return &litExpr{v: v} }

// coalesceOf is coalesce(e, vs…): each v a Go value bound as a parameter of e's type, or an
// expression, taken in order where everything before it is NULL.
func coalesceOf(e Expr, vs []any) Expr {
	args := make([]Expr, 1, len(vs)+1)
	args[0] = e
	for _, v := range vs {
		args = append(args, operandFor(e, "coalesce", v))
	}
	return callOf("coalesce", args...)
}

// operandFor is v as an operand of a function over e: an expression in place, nil as NULL, a value
// compared with a column sharing its placeholder as the column's comparisons do, any other value
// bound.
func operandFor(e Expr, op string, v any) Expr {
	switch x := v.(type) {
	case Expr:
		return x
	case nil:
		return rawText("NULL")
	}
	if c, ok := e.(Col); ok {
		return colValue{c: c, op: op, v: v}
	}
	return &litExpr{v: v}
}

// anyArg is an argument of a function whose parameters are of type "any" (concat, concat_ws,
// jsonb_build_object, jsonb_build_array): an expression in place, nil as NULL, a value bound under
// the cast its Go type implies, since nothing else types it.
func anyArg(v any) Expr {
	switch x := v.(type) {
	case Expr:
		return x
	case nil:
		return rawText("NULL")
	}
	return typed(&litExpr{v: v})
}

// exprsOf is first followed by vs as expressions.
func exprsOf[V Expr](first Expr, vs []V) []Expr {
	out := make([]Expr, 0, len(vs)+1)
	out = append(out, first)
	for _, v := range vs {
		out = append(out, v)
	}
	return out
}

// nonNil is e, or an expression that fails the statement when e is nil.
func nonNil(e Expr, what string) Expr {
	if e == nil {
		return exprFunc(func(r *render) { r.fail(errf("%s of a nil expression", what)) })
	}
	return e
}

// castTo is CAST(e AS typ).
func castTo(e Expr, typ string) Expr {
	e = nonNil(e, "CAST AS "+typ)
	return exprFunc(func(r *render) {
		r.s("CAST(")
		e.writeSQL(r)
		r.s(" AS ")
		r.s(typ)
		r.b(')')
	})
}

// textOperand is a string column as the argument of a text function: the column itself, or the
// column cast to text when it holds an enum or its tag declares a SQL type that is not text, since
// Postgres's text functions do not take a Postgres enum. A cast of a text column to text is no
// cast at all to Postgres, so an expression index on lower(col) still matches.
func textOperand(c Col) Expr {
	if c.Ref == nil || c.Ref.Table == nil || int(c.Index) >= len(c.Ref.Table.Columns) {
		return c
	}
	col := &c.Ref.Table.Columns[c.Index]
	typ := strings.ToLower(col.SQLType)
	textual := typ == "" || typ == "text" || typ == "citext" || strings.HasPrefix(typ, "varchar") ||
		strings.HasPrefix(typ, "character")
	if textual && len(col.Enum) == 0 {
		return c
	}
	return castTo(c, "text")
}

// String functions.

// Lower is lower(e).
func (e StringExpr) Lower() StringExpr { return StringExpr{e: callOf("lower", e.e)} }

// Upper is upper(e).
func (e StringExpr) Upper() StringExpr { return StringExpr{e: callOf("upper", e.e)} }

// Trim is btrim(e): e without leading and trailing spaces (SQL trim(e)).
func (e StringExpr) Trim() StringExpr { return StringExpr{e: callOf("btrim", e.e)} }

// TrimChars is btrim(e, chars): e without leading and trailing characters in chars.
func (e StringExpr) TrimChars(chars string) StringExpr {
	return StringExpr{e: callOf("btrim", e.e, bindOf(chars))}
}

// LTrim is ltrim(e): e without leading spaces.
func (e StringExpr) LTrim() StringExpr { return StringExpr{e: callOf("ltrim", e.e)} }

// LTrimChars is ltrim(e, chars): e without leading characters in chars (ltrim(n, '0') drops
// leading zeros).
func (e StringExpr) LTrimChars(chars string) StringExpr {
	return StringExpr{e: callOf("ltrim", e.e, bindOf(chars))}
}

// RTrim is rtrim(e): e without trailing spaces.
func (e StringExpr) RTrim() StringExpr { return StringExpr{e: callOf("rtrim", e.e)} }

// RTrimChars is rtrim(e, chars): e without trailing characters in chars.
func (e StringExpr) RTrimChars(chars string) StringExpr {
	return StringExpr{e: callOf("rtrim", e.e, bindOf(chars))}
}

// Length is length(e), the number of characters.
func (e StringExpr) Length() Int32Expr { return Int32Expr{e: callOf("length", e.e)} }

// Left is left(e, n): the first n characters, or all but the last -n when n is negative.
func (e StringExpr) Left(n int) StringExpr {
	return StringExpr{e: callOf("left", e.e, bindOf(int64(n)))}
}

// Right is right(e, n): the last n characters, or all but the first -n when n is negative.
func (e StringExpr) Right(n int) StringExpr {
	return StringExpr{e: callOf("right", e.e, bindOf(int64(n)))}
}

// Substr is substr(e, start, count): count characters from position start, from 1.
func (e StringExpr) Substr(start, count int) StringExpr {
	return StringExpr{e: callOf("substr", e.e, bindOf(int64(start)), bindOf(int64(count)))}
}

// SubstrFrom is substr(e, start): the characters from position start, from 1, to the end.
func (e StringExpr) SubstrFrom(start int) StringExpr {
	return StringExpr{e: callOf("substr", e.e, bindOf(int64(start)))}
}

// Replace is replace(e, from, to): every occurrence of from replaced by to.
func (e StringExpr) Replace(from, to string) StringExpr {
	return StringExpr{e: callOf("replace", e.e, bindOf(from), bindOf(to))}
}

// SplitPart is split_part(e, delim, n): the n-th field of e split at delim, from 1; a negative n
// counts from the end.
func (e StringExpr) SplitPart(delim string, n int) StringExpr {
	return StringExpr{e: callOf("split_part", e.e, bindOf(delim), bindOf(int64(n)))}
}

// MD5 is md5(e), the hex digest.
func (e StringExpr) MD5() StringExpr { return StringExpr{e: callOf("md5", e.e)} }

// RegexpReplace is regexp_replace(e, pattern, replacement[, flags]): the first match of the POSIX
// pattern replaced, every match with flags "g". Empty flags leave the argument out.
func (e StringExpr) RegexpReplace(pattern, replacement, flags string) StringExpr {
	if flags == "" {
		return StringExpr{e: callOf("regexp_replace", e.e, bindOf(pattern), bindOf(replacement))}
	}
	return StringExpr{e: callOf("regexp_replace", e.e, bindOf(pattern), bindOf(replacement),
		bindOf(flags))}
}

// Coalesce is coalesce(e, vs…): each v a string bound as a parameter or an expression, taken in
// order where everything before it is NULL.
func (e StringExpr) Coalesce(vs ...any) StringExpr { return StringExpr{e: coalesceOf(e.e, vs)} }

// NullIf is nullif(e, v): NULL where e equals v, as NullIf("") turns empty strings into NULL.
func (e StringExpr) NullIf(v string) StringExpr {
	return StringExpr{e: callOf("nullif", e.e, operandFor(e.e, "nullif", v))}
}

// Greatest is greatest(e, others…), the largest non-NULL value.
func (e StringExpr) Greatest(others ...StringValue) StringExpr {
	return StringExpr{e: callOf("greatest", exprsOf(e.e, others)...)}
}

// Least is least(e, others…), the smallest non-NULL value.
func (e StringExpr) Least(others ...StringValue) StringExpr {
	return StringExpr{e: callOf("least", exprsOf(e.e, others)...)}
}

// asText is the column as the argument of a text function.
func (c StringCol) asText() StringExpr { return StringExpr{e: textOperand(c.Col)} }

// Lower is lower(col).
func (c StringCol) Lower() StringExpr { return c.asText().Lower() }

// Upper is upper(col).
func (c StringCol) Upper() StringExpr { return c.asText().Upper() }

// Trim is btrim(col).
func (c StringCol) Trim() StringExpr { return c.asText().Trim() }

// TrimChars is btrim(col, chars).
func (c StringCol) TrimChars(chars string) StringExpr { return c.asText().TrimChars(chars) }

// LTrim is ltrim(col).
func (c StringCol) LTrim() StringExpr { return c.asText().LTrim() }

// LTrimChars is ltrim(col, chars).
func (c StringCol) LTrimChars(chars string) StringExpr { return c.asText().LTrimChars(chars) }

// RTrim is rtrim(col).
func (c StringCol) RTrim() StringExpr { return c.asText().RTrim() }

// RTrimChars is rtrim(col, chars).
func (c StringCol) RTrimChars(chars string) StringExpr { return c.asText().RTrimChars(chars) }

// Length is length(col).
func (c StringCol) Length() Int32Expr { return c.asText().Length() }

// Left is left(col, n).
func (c StringCol) Left(n int) StringExpr { return c.asText().Left(n) }

// Right is right(col, n).
func (c StringCol) Right(n int) StringExpr { return c.asText().Right(n) }

// Substr is substr(col, start, count).
func (c StringCol) Substr(start, count int) StringExpr { return c.asText().Substr(start, count) }

// SubstrFrom is substr(col, start).
func (c StringCol) SubstrFrom(start int) StringExpr { return c.asText().SubstrFrom(start) }

// Replace is replace(col, from, to).
func (c StringCol) Replace(from, to string) StringExpr { return c.asText().Replace(from, to) }

// SplitPart is split_part(col, delim, n).
func (c StringCol) SplitPart(delim string, n int) StringExpr { return c.asText().SplitPart(delim, n) }

// MD5 is md5(col).
func (c StringCol) MD5() StringExpr { return c.asText().MD5() }

// RegexpReplace is regexp_replace(col, pattern, replacement[, flags]).
func (c StringCol) RegexpReplace(pattern, replacement, flags string) StringExpr {
	return c.asText().RegexpReplace(pattern, replacement, flags)
}

// Coalesce is coalesce(col, vs…), typed as text: each v is a value bound as a parameter (sharing
// the placeholder of an equal value compared with the column) or an expression.
func (c StringCol) Coalesce(vs ...any) StringExpr { return StringExpr{e: c.Col.Coalesce(vs...)} }

// NullIf is nullif(col, v).
func (c StringCol) NullIf(v string) StringExpr {
	return StringExpr{e: callOf("nullif", c.Col, operandFor(c.Col, "nullif", v))}
}

// Greatest is greatest(col, others…).
func (c StringCol) Greatest(others ...StringValue) StringExpr {
	return StringExpr{e: callOf("greatest", exprsOf(c.Col, others)...)}
}

// Least is least(col, others…).
func (c StringCol) Least(others ...StringValue) StringExpr {
	return StringExpr{e: callOf("least", exprsOf(c.Col, others)...)}
}

// Concat is concat(args…): the text of every argument joined, NULL arguments skipped. An argument
// is an expression of any type or a Go value bound under the cast its type implies.
func Concat(args ...any) StringExpr {
	ops := make([]Expr, len(args))
	for i, a := range args {
		ops[i] = anyArg(a)
	}
	return StringExpr{e: callOf("concat", ops...)}
}

// ConcatWS is concat_ws(sep, args…): the non-NULL arguments joined with sep, as
// keel.ConcatWS(" ", w.FirstName, w.LastName) builds a full name.
func ConcatWS(sep string, args ...any) StringExpr {
	ops := make([]Expr, 0, len(args)+1)
	ops = append(ops, anyArg(sep))
	for _, a := range args {
		ops = append(ops, anyArg(a))
	}
	return StringExpr{e: callOf("concat_ws", ops...)}
}

// GenRandomUUID is the text of a new random UUID: CAST(gen_random_uuid() AS text).
func GenRandomUUID() StringExpr { return StringExpr{e: rawText("CAST(gen_random_uuid() AS text)")} }

// CastText is CAST(e AS text): an enum, a number, a uuid or a time as text.
func CastText(e Expr) StringExpr { return StringExpr{e: castTo(e, "text")} }

// Numeric functions.

// Abs is abs(e).
func (e Int64Expr) Abs() Int64Expr { return Int64Expr{e: callOf("abs", e.e)} }

// Coalesce is coalesce(e, vs…): each v an integer bound as a parameter or an expression, taken in
// order where everything before it is NULL.
func (e Int64Expr) Coalesce(vs ...any) Int64Expr { return Int64Expr{e: coalesceOf(e.e, vs)} }

// NullIf is nullif(e, v): NULL where e equals v, for a divisor that may be zero.
func (e Int64Expr) NullIf(v int64) Int64Expr {
	return Int64Expr{e: callOf("nullif", e.e, operandFor(e.e, "nullif", v))}
}

// Greatest is greatest(e, others…), the largest non-NULL value.
func (e Int64Expr) Greatest(others ...Int64Value) Int64Expr {
	return Int64Expr{e: callOf("greatest", exprsOf(e.e, others)...)}
}

// Least is least(e, others…), the smallest non-NULL value.
func (e Int64Expr) Least(others ...Int64Value) Int64Expr {
	return Int64Expr{e: callOf("least", exprsOf(e.e, others)...)}
}

// Abs is abs(col).
func (c Int64Col) Abs() Int64Expr { return Int64Expr{e: c.Col}.Abs() }

// Coalesce is coalesce(col, vs…), typed as bigint.
func (c Int64Col) Coalesce(vs ...any) Int64Expr { return Int64Expr{e: c.Col.Coalesce(vs...)} }

// NullIf is nullif(col, v).
func (c Int64Col) NullIf(v int64) Int64Expr { return Int64Expr{e: c.Col}.NullIf(v) }

// Greatest is greatest(col, others…).
func (c Int64Col) Greatest(others ...Int64Value) Int64Expr {
	return Int64Expr{e: c.Col}.Greatest(others...)
}

// Least is least(col, others…).
func (c Int64Col) Least(others ...Int64Value) Int64Expr { return Int64Expr{e: c.Col}.Least(others...) }

// Abs is abs(e).
func (e Int32Expr) Abs() Int32Expr { return Int32Expr{e: callOf("abs", e.e)} }

// Coalesce is coalesce(e, vs…).
func (e Int32Expr) Coalesce(vs ...any) Int32Expr { return Int32Expr{e: coalesceOf(e.e, vs)} }

// NullIf is nullif(e, v).
func (e Int32Expr) NullIf(v int32) Int32Expr {
	return Int32Expr{e: callOf("nullif", e.e, operandFor(e.e, "nullif", v))}
}

// Greatest is greatest(e, others…).
func (e Int32Expr) Greatest(others ...Int32Value) Int32Expr {
	return Int32Expr{e: callOf("greatest", exprsOf(e.e, others)...)}
}

// Least is least(e, others…).
func (e Int32Expr) Least(others ...Int32Value) Int32Expr {
	return Int32Expr{e: callOf("least", exprsOf(e.e, others)...)}
}

// Abs is abs(col).
func (c Int32Col) Abs() Int32Expr { return Int32Expr{e: c.Col}.Abs() }

// Coalesce is coalesce(col, vs…), typed as integer.
func (c Int32Col) Coalesce(vs ...any) Int32Expr { return Int32Expr{e: c.Col.Coalesce(vs...)} }

// NullIf is nullif(col, v).
func (c Int32Col) NullIf(v int32) Int32Expr { return Int32Expr{e: c.Col}.NullIf(v) }

// Greatest is greatest(col, others…).
func (c Int32Col) Greatest(others ...Int32Value) Int32Expr {
	return Int32Expr{e: c.Col}.Greatest(others...)
}

// Least is least(col, others…).
func (c Int32Col) Least(others ...Int32Value) Int32Expr { return Int32Expr{e: c.Col}.Least(others...) }

// Abs is abs(e).
func (e Float64Expr) Abs() Float64Expr { return Float64Expr{e: callOf("abs", e.e)} }

// Round is round(e), to the nearest integer, halves away from zero. Postgres rounds double precision
// only to whole numbers; CastNumeric(e).RoundTo(places) rounds to decimal places.
func (e Float64Expr) Round() Float64Expr { return Float64Expr{e: callOf("round", e.e)} }

// Floor is floor(e).
func (e Float64Expr) Floor() Float64Expr { return Float64Expr{e: callOf("floor", e.e)} }

// Ceil is ceil(e).
func (e Float64Expr) Ceil() Float64Expr { return Float64Expr{e: callOf("ceil", e.e)} }

// Ln is ln(e), the natural logarithm.
func (e Float64Expr) Ln() Float64Expr { return Float64Expr{e: callOf("ln", e.e)} }

// Exp is exp(e).
func (e Float64Expr) Exp() Float64Expr { return Float64Expr{e: callOf("exp", e.e)} }

// Coalesce is coalesce(e, vs…).
func (e Float64Expr) Coalesce(vs ...any) Float64Expr { return Float64Expr{e: coalesceOf(e.e, vs)} }

// NullIf is nullif(e, v).
func (e Float64Expr) NullIf(v float64) Float64Expr {
	return Float64Expr{e: callOf("nullif", e.e, operandFor(e.e, "nullif", v))}
}

// Greatest is greatest(e, others…).
func (e Float64Expr) Greatest(others ...Float64Value) Float64Expr {
	return Float64Expr{e: callOf("greatest", exprsOf(e.e, others)...)}
}

// Least is least(e, others…).
func (e Float64Expr) Least(others ...Float64Value) Float64Expr {
	return Float64Expr{e: callOf("least", exprsOf(e.e, others)...)}
}

// Abs is abs(col).
func (c Float64Col) Abs() Float64Expr { return Float64Expr{e: c.Col}.Abs() }

// Round is round(col).
func (c Float64Col) Round() Float64Expr { return Float64Expr{e: c.Col}.Round() }

// Floor is floor(col).
func (c Float64Col) Floor() Float64Expr { return Float64Expr{e: c.Col}.Floor() }

// Ceil is ceil(col).
func (c Float64Col) Ceil() Float64Expr { return Float64Expr{e: c.Col}.Ceil() }

// Ln is ln(col).
func (c Float64Col) Ln() Float64Expr { return Float64Expr{e: c.Col}.Ln() }

// Exp is exp(col).
func (c Float64Col) Exp() Float64Expr { return Float64Expr{e: c.Col}.Exp() }

// Coalesce is coalesce(col, vs…), typed as double precision.
func (c Float64Col) Coalesce(vs ...any) Float64Expr { return Float64Expr{e: c.Col.Coalesce(vs...)} }

// NullIf is nullif(col, v).
func (c Float64Col) NullIf(v float64) Float64Expr { return Float64Expr{e: c.Col}.NullIf(v) }

// Greatest is greatest(col, others…).
func (c Float64Col) Greatest(others ...Float64Value) Float64Expr {
	return Float64Expr{e: c.Col}.Greatest(others...)
}

// Least is least(col, others…).
func (c Float64Col) Least(others ...Float64Value) Float64Expr {
	return Float64Expr{e: c.Col}.Least(others...)
}

// Abs is abs(e).
func (e NumericExpr) Abs() NumericExpr { return NumericExpr{e: callOf("abs", e.e)} }

// Round is round(e), to the nearest integer, halves away from zero.
func (e NumericExpr) Round() NumericExpr { return NumericExpr{e: callOf("round", e.e)} }

// RoundTo is round(e, places): e rounded to places decimal places (to tens, hundreds, … when
// places is negative).
func (e NumericExpr) RoundTo(places int) NumericExpr {
	return NumericExpr{e: callOf("round", e.e, bindOf(int64(places)))}
}

// Floor is floor(e).
func (e NumericExpr) Floor() NumericExpr { return NumericExpr{e: callOf("floor", e.e)} }

// Ceil is ceil(e).
func (e NumericExpr) Ceil() NumericExpr { return NumericExpr{e: callOf("ceil", e.e)} }

// Ln is ln(e), the natural logarithm.
func (e NumericExpr) Ln() NumericExpr { return NumericExpr{e: callOf("ln", e.e)} }

// Exp is exp(e).
func (e NumericExpr) Exp() NumericExpr { return NumericExpr{e: callOf("exp", e.e)} }

// Coalesce is coalesce(e, vs…).
func (e NumericExpr) Coalesce(vs ...any) NumericExpr { return NumericExpr{e: coalesceOf(e.e, vs)} }

// NullIf is nullif(e, v).
func (e NumericExpr) NullIf(v any) NumericExpr {
	return NumericExpr{e: callOf("nullif", e.e, operandFor(e.e, "nullif", v))}
}

// Greatest is greatest(e, others…).
func (e NumericExpr) Greatest(others ...NumericValue) NumericExpr {
	return NumericExpr{e: callOf("greatest", exprsOf(e.e, others)...)}
}

// Least is least(e, others…).
func (e NumericExpr) Least(others ...NumericValue) NumericExpr {
	return NumericExpr{e: callOf("least", exprsOf(e.e, others)...)}
}

// CastInt64 is CAST(e AS bigint) of a number of another kind (a fraction rounds to the nearest
// integer), of text holding an integer, or of any expression Postgres casts to bigint.
func CastInt64(e Expr) Int64Expr { return Int64Expr{e: castTo(e, "bigint")} }

// CastFloat64 is CAST(e AS double precision) of a number or of text holding one.
func CastFloat64(e Expr) Float64Expr { return Float64Expr{e: castTo(e, "double precision")} }

// CastNumeric is CAST(e AS numeric) of a number or of text holding one: exact arithmetic and
// rounding to decimal places.
func CastNumeric(e Expr) NumericExpr { return NumericExpr{e: castTo(e, "numeric")} }

// WidthBucket is width_bucket(v, bounds): the number of bounds, sorted ascending, that are at most
// v (0 below the first, len(bounds) at or above the last). bounds binds as one bigint[] parameter.
func WidthBucket(v Int64Value, bounds []int64) Int32Expr {
	return Int32Expr{e: callOf("width_bucket", v, &castLit{v: bounds, typ: "bigint[]"})}
}

// WidthBucketRange is width_bucket(v, lo, hi, buckets): the bucket, from 1 to buckets, of v in
// buckets equal-width buckets over [lo, hi); 0 below lo and buckets+1 at or above hi.
func WidthBucketRange(v Float64Value, lo, hi float64, buckets int) Int32Expr {
	return Int32Expr{e: callOf("width_bucket", v, &castLit{v: lo, typ: "double precision"},
		&castLit{v: hi, typ: "double precision"}, &castLit{v: int64(buckets), typ: "integer"})}
}

// Time functions.

// Now is now(), the start time of the current transaction.
func Now() TimeExpr { return TimeExpr{e: rawText("now()")} }

// Epoch is CAST(extract(epoch FROM t) AS bigint): t in Unix seconds, rounded.
func Epoch(t TimeValue) Int64Expr {
	e := nonNil(t, "Epoch")
	return Int64Expr{e: exprFunc(func(r *render) {
		r.s("CAST(extract(epoch FROM ")
		e.writeSQL(r)
		r.s(") AS bigint)")
	})}
}

// NowEpoch is the current time in Unix seconds, CAST(extract(epoch FROM now()) AS bigint), for
// models that store times as epoch seconds.
func NowEpoch() Int64Expr { return Epoch(Now()) }

// ToTimestamp is to_timestamp(seconds): Unix seconds as a timestamptz.
func ToTimestamp(seconds NumberValue) TimeExpr {
	return TimeExpr{e: callOf("to_timestamp", seconds)}
}

// datePartFields are the fields date_part reads; dateTruncFields are those date_trunc truncates to.
var (
	datePartFields = map[string]bool{
		"century": true, "day": true, "decade": true, "dow": true, "doy": true, "epoch": true,
		"hour": true, "isodow": true, "isoyear": true, "julian": true, "microseconds": true,
		"millennium": true, "milliseconds": true, "minute": true, "month": true, "quarter": true,
		"second": true, "timezone": true, "timezone_hour": true, "timezone_minute": true,
		"week": true, "year": true,
	}
	dateTruncFields = map[string]bool{
		"microseconds": true, "milliseconds": true, "second": true, "minute": true, "hour": true,
		"day": true, "week": true, "month": true, "quarter": true, "year": true, "decade": true,
		"century": true, "millennium": true,
	}
)

// dateFieldCall is name('field', t). The field is one of a closed set, so it is written as a
// constant: Postgres then sees it when it matches an expression index, which a parameter never
// lets it.
func dateFieldCall(name, field string, fields map[string]bool, t TimeValue) Expr {
	field = strings.ToLower(field)
	if !fields[field] {
		return exprFunc(func(r *render) { r.fail(errf("%s: unknown field %q", name, field)) })
	}
	return callOf(name, rawText("'"+field+"'"), t)
}

// DatePart is date_part('field', t): a field of t as double precision ("hour", "dow", "epoch", …).
// An unknown field makes an expression that fails.
func DatePart(field string, t TimeValue) Float64Expr {
	return Float64Expr{e: dateFieldCall("date_part", field, datePartFields, t)}
}

// DateTrunc is date_trunc('field', t): t truncated to the start of its field ("day", "month",
// "year", …). An unknown field makes an expression that fails.
func DateTrunc(field string, t TimeValue) TimeExpr {
	return TimeExpr{e: dateFieldCall("date_trunc", field, dateTruncFields, t)}
}

// ToChar is to_char(e, format): e formatted by a Postgres template ("YYYY-MM-DD").
func (e TimeExpr) ToChar(format string) StringExpr {
	return StringExpr{e: callOf("to_char", e.e, bindOf(format))}
}

// AtTimeZone is timezone(zone, e): a timestamptz as the local time in zone, a timestamp without
// time zone read as a time in zone. Pass keel.Text(name) for a zone name from Go.
func (e TimeExpr) AtTimeZone(zone StringValue) TimeExpr {
	return TimeExpr{e: callOf("timezone", zone, e.e)}
}

// Coalesce is coalesce(e, vs…): each v a time bound as a parameter or an expression, taken in
// order where everything before it is NULL.
func (e TimeExpr) Coalesce(vs ...any) TimeExpr { return TimeExpr{e: coalesceOf(e.e, vs)} }

// Greatest is greatest(e, others…), the latest non-NULL time.
func (e TimeExpr) Greatest(others ...TimeValue) TimeExpr {
	return TimeExpr{e: callOf("greatest", exprsOf(e.e, others)...)}
}

// Least is least(e, others…), the earliest non-NULL time.
func (e TimeExpr) Least(others ...TimeValue) TimeExpr {
	return TimeExpr{e: callOf("least", exprsOf(e.e, others)...)}
}

// ToChar is to_char(col, format).
func (c TimeCol) ToChar(format string) StringExpr { return TimeExpr{e: c.Col}.ToChar(format) }

// AtTimeZone is timezone(zone, col).
func (c TimeCol) AtTimeZone(zone StringValue) TimeExpr { return TimeExpr{e: c.Col}.AtTimeZone(zone) }

// Coalesce is coalesce(col, vs…), typed as a time.
func (c TimeCol) Coalesce(vs ...any) TimeExpr { return TimeExpr{e: c.Col.Coalesce(vs...)} }

// Greatest is greatest(col, others…).
func (c TimeCol) Greatest(others ...TimeValue) TimeExpr {
	return TimeExpr{e: c.Col}.Greatest(others...)
}

// Least is least(col, others…).
func (c TimeCol) Least(others ...TimeValue) TimeExpr { return TimeExpr{e: c.Col}.Least(others...) }

// Boolean functions.

// Coalesce is coalesce(e, vs…).
func (e BoolExpr) Coalesce(vs ...any) BoolExpr { return BoolExpr{e: coalesceOf(e.e, vs)} }

// Coalesce is coalesce(col, vs…), typed as boolean.
func (c BoolCol) Coalesce(vs ...any) BoolExpr { return BoolExpr{e: c.Col.Coalesce(vs...)} }

// BoolOr is the aggregate bool_or(p): whether p holds for any row, NULL over no rows.
func BoolOr(p Pred) Agg { return Agg{fn: "bool_or", arg: nonNil(p, "BoolOr")} }

// BoolAnd is the aggregate bool_and(p): whether p holds for every row, NULL over no rows.
func BoolAnd(p Pred) Agg { return Agg{fn: "bool_and", arg: nonNil(p, "BoolAnd")} }

// JSON functions.

// JSONPair is one key and value of JSONBuildObject.
type JSONPair struct {
	key string
	v   any
}

// Pair is the key and value of one member of a JSONBuildObject: v is an expression or a Go value
// bound under the cast its type implies (use keel.JSONB for a document).
func Pair(key string, v any) JSONPair { return JSONPair{key: key, v: v} }

// JSONBuildObject is jsonb_build_object(key, value, …), every key bound as text.
func JSONBuildObject(pairs ...JSONPair) JSONBExpr {
	ops := make([]Expr, 0, 2*len(pairs))
	for _, p := range pairs {
		ops = append(ops, &castLit{v: p.key, typ: "text"}, anyArg(p.v))
	}
	return JSONBExpr{e: callOf("jsonb_build_object", ops...)}
}

// JSONBuildArray is jsonb_build_array(vs…); no values is the empty array.
func JSONBuildArray(vs ...any) JSONBExpr {
	ops := make([]Expr, len(vs))
	for i, v := range vs {
		ops[i] = anyArg(v)
	}
	return JSONBExpr{e: callOf("jsonb_build_array", ops...)}
}

// JSONBAgg is the aggregate jsonb_agg(e), the values of e as a jsonb array (NULL over no rows).
// OrderBy orders the elements.
func JSONBAgg(e Expr) Agg { return Agg{fn: "jsonb_agg", arg: nonNil(e, "JSONBAgg")} }

// TypeOf is jsonb_typeof(e): "object", "array", "string", "number", "boolean" or "null".
func (e JSONBExpr) TypeOf() StringExpr { return StringExpr{e: callOf("jsonb_typeof", e.e)} }

// Set is jsonb_set(e, path, v): e with the value at path replaced by v, created when missing.
func (e JSONBExpr) Set(path []string, v JSONBValue) JSONBExpr {
	if path == nil {
		path = []string{}
	}
	return JSONBExpr{e: callOf("jsonb_set", e.e, bindOf(path), v)}
}

// Concat is e || o: the objects merged (o's keys win) or the arrays concatenated.
func (e JSONBExpr) Concat(o JSONBValue) JSONBExpr {
	return JSONBExpr{e: binOp{a: e.e, b: nonNil(o, "Concat"), op: " || "}}
}

// ArrayElementsText is the set-returning jsonb_array_elements_text(e): one text row per element of
// the array e, for keel.Rows.
func (e JSONBExpr) ArrayElementsText() StringExpr {
	return StringExpr{e: callOf("jsonb_array_elements_text", e.e)}
}

// Coalesce is coalesce(e, v): v, an expression or a value encoded as JSON, where e is NULL.
func (e JSONBExpr) Coalesce(v any) JSONBExpr {
	switch v.(type) {
	case Expr, nil:
		return JSONBExpr{e: callOf("coalesce", e.e, operandFor(e.e, "coalesce", v))}
	}
	return JSONBExpr{e: callOf("coalesce", e.e, &jsonLit{v: v})}
}

// TypeOf is jsonb_typeof(col).
func (c JSONBCol) TypeOf() StringExpr { return JSONBExpr{e: c.Col}.TypeOf() }

// Set is jsonb_set(col, path, v); assign it with ToExpr.
func (c JSONBCol) Set(path []string, v JSONBValue) JSONBExpr { return JSONBExpr{e: c.Col}.Set(path, v) }

// Concat is col || o.
func (c JSONBCol) Concat(o JSONBValue) JSONBExpr { return JSONBExpr{e: c.Col}.Concat(o) }

// ArrayElementsText is jsonb_array_elements_text(col), for keel.Rows.
func (c JSONBCol) ArrayElementsText() StringExpr { return JSONBExpr{e: c.Col}.ArrayElementsText() }

// Coalesce is coalesce(col, vs…), typed as jsonb; a value v is encoded as JSON.
func (c JSONBCol) Coalesce(v any) JSONBExpr {
	switch v.(type) {
	case Expr, nil:
		return JSONBExpr{e: c.Col.Coalesce(v)}
	}
	return JSONBExpr{e: callOf("coalesce", c.Col, &jsonLit{v: v})}
}

// Array functions.

// Unnest is the set-returning unnest(col): one row per element, for keel.Rows.
func (c StringArrayCol) Unnest() StringExpr { return StringExpr{e: callOf("unnest", c.Col)} }

// Append sets the column to itself with v appended: array_append(col, v). A NULL array becomes {v}.
func (c StringArrayCol) Append(v string) Assignment {
	return Assignment{col: c.Col, e: callOf("array_append", c.Col, &castLit{v: v, typ: "text"}).writeSQL}
}

// Len is cardinality(col).
func (c Int64ArrayCol) Len() Int32Expr { return Int32Expr{e: callOf("cardinality", c.Col)} }

// Unnest is the set-returning unnest(col), for keel.Rows.
func (c Int64ArrayCol) Unnest() Int64Expr { return Int64Expr{e: callOf("unnest", c.Col)} }

// Append sets the column to itself with v appended: array_append(col, v).
func (c Int64ArrayCol) Append(v int64) Assignment {
	return Assignment{col: c.Col, e: callOf("array_append", c.Col, &castLit{v: v, typ: "bigint"}).writeSQL}
}

// Remove sets the column to itself without the elements equal to v: array_remove(col, v).
func (c Int64ArrayCol) Remove(v int64) Assignment {
	return Assignment{col: c.Col, e: callOf("array_remove", c.Col, &castLit{v: v, typ: "bigint"}).writeSQL}
}

// ArrayPosition is array_position(list, v): the position of v in list, from 1, or NULL. list binds
// as one text[] parameter, so ordering by it keeps rows in a caller's order:
// Order(keel.ArrayPosition(ids, q.ID).Asc()).
func ArrayPosition(list []string, v StringValue) Int32Expr {
	return Int32Expr{e: callOf("array_position", &castLit{v: list, typ: "text[]"}, v)}
}

// Set-returning series.

// GenerateSeries is the set-returning generate_series(start, stop): the integers from start to
// stop, for keel.Rows.
func GenerateSeries(start, stop Int64Value) Int64Expr {
	return Int64Expr{e: callOf("generate_series", start, stop)}
}

// GenerateSeriesBy is generate_series(start, stop, step).
func GenerateSeriesBy(start, stop, step Int64Value) Int64Expr {
	return Int64Expr{e: callOf("generate_series", start, stop, step)}
}

// GenerateTimeSeries is generate_series(start, stop, step): the times from start to stop every
// step, an interval such as "1 day" bound as $n::interval.
func GenerateTimeSeries(start, stop TimeValue, step string) TimeExpr {
	return TimeExpr{e: callOf("generate_series", start, stop, &castLit{v: step, typ: "interval"})}
}

// Ranges.

// Int64Range is a bigint range, built by Int8Range.
type Int64Range struct{ e Expr }

func (g Int64Range) writeSQL(r *render) { g.e.writeSQL(r) }

// Int8Range is int8range(lo, hi): the bigints from lo, included, to hi, excluded; a NULL bound is
// unbounded.
func Int8Range(lo, hi Int64Value) Int64Range { return Int64Range{e: callOf("int8range", lo, hi)} }

// Covers is g @> v: v lies in the range.
func (g Int64Range) Covers(v Int64Value) Pred { return binExpr(g.e, " @> ", nonNil(v, "Covers")) }

// Overlaps is g && o: the ranges share a value.
func (g Int64Range) Overlaps(o Int64Range) Pred { return binExpr(g.e, " && ", o.e) }

// Typed declarations of database functions.

// FuncDecl is a database function declared with DeclareFunc0 … DeclareFunc3: its name, the number
// of arguments its calls pass and the kind of value it returns. VerifyFuncs checks it against the
// catalog.
type FuncDecl struct {
	name   string
	nsp    string
	proc   string
	quoted string
	arity  int
	result schema.Kind
}

// Name is the function's name as declared, schema-qualified or not.
func (d *FuncDecl) Name() string { return d.name }

// Arity is the number of arguments the declaration's calls pass.
func (d *FuncDecl) Arity() int { return d.arity }

// call is the function called with args.
func (d *FuncDecl) call(args ...Expr) Expr { return callOf(d.quoted, args...) }

// DeclaredFunc is a typed function declaration, accepted by VerifyFuncs: a Func0, Func1, Func2 or
// Func3.
type DeclaredFunc interface {
	Decl() *FuncDecl
}

// FuncResult is the expression type a declared function returns, which fixes how its calls compare
// and what they decode into.
type FuncResult interface {
	StringExpr | Int64Expr | Int32Expr | Float64Expr | NumericExpr | BoolExpr | TimeExpr | JSONBExpr
	Expr
	kind() (schema.Kind, bool)
}

// declare parses name, "fn" or "schema.fn", into a declaration of arity arguments returning R. A
// name that is empty, has more than one dot or holds a double quote or NUL panics: declarations
// are package variables, so the mistake shows at start-up.
func declare[R FuncResult](name string, arity int) *FuncDecl {
	nsp, proc, qualified := strings.Cut(name, ".")
	if !qualified {
		nsp, proc = "", name
	}
	if proc == "" || (qualified && nsp == "") || strings.Contains(proc, ".") ||
		strings.ContainsAny(name, "\"\x00") {
		panic("keel: DeclareFunc: invalid function name \"" + name + "\"")
	}
	var zero R
	k, _ := zero.kind()
	quoted := schema.Quote(proc)
	if qualified {
		quoted = schema.Quote(nsp) + "." + quoted
	}
	return &FuncDecl{name: name, nsp: nsp, proc: proc, quoted: quoted, arity: arity, result: k}
}

// resultOf types a call as R.
func resultOf[R FuncResult](e Expr) R { return R(struct{ e Expr }{e: e}) }

// Func0 is a database function of no arguments returning R, declared with DeclareFunc0.
type Func0[R FuncResult] struct{ d *FuncDecl }

// DeclareFunc0 declares the database function name ("fn" or "schema.fn") taking no arguments and
// returning R:
//
//	var nextBatch = keel.DeclareFunc0[keel.Int64Expr]("billing.next_batch_number")
func DeclareFunc0[R FuncResult](name string) Func0[R] { return Func0[R]{d: declare[R](name, 0)} }

// Call is name().
func (f Func0[R]) Call() R { return resultOf[R](f.d.call()) }

// Decl returns the declaration VerifyFuncs checks.
func (f Func0[R]) Decl() *FuncDecl { return f.d }

// Func1 is a database function of one argument of type A (a value interface such as StringValue,
// or an expression type) returning R, declared with DeclareFunc1.
type Func1[R FuncResult, A Expr] struct{ d *FuncDecl }

// DeclareFunc1 declares the database function name taking one A and returning R:
//
//	var slug = keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("public.slugify")
//	q.Where(slug.Call(c.Name).Eq("acme-freight"))
func DeclareFunc1[R FuncResult, A Expr](name string) Func1[R, A] {
	return Func1[R, A]{d: declare[R](name, 1)}
}

// Call is name(a).
func (f Func1[R, A]) Call(a A) R { return resultOf[R](f.d.call(Expr(a))) }

// Decl returns the declaration VerifyFuncs checks.
func (f Func1[R, A]) Decl() *FuncDecl { return f.d }

// Func2 is a database function of two arguments returning R, declared with DeclareFunc2.
type Func2[R FuncResult, A, B Expr] struct{ d *FuncDecl }

// DeclareFunc2 declares the database function name taking an A and a B and returning R.
func DeclareFunc2[R FuncResult, A, B Expr](name string) Func2[R, A, B] {
	return Func2[R, A, B]{d: declare[R](name, 2)}
}

// Call is name(a, b).
func (f Func2[R, A, B]) Call(a A, b B) R { return resultOf[R](f.d.call(Expr(a), Expr(b))) }

// Decl returns the declaration VerifyFuncs checks.
func (f Func2[R, A, B]) Decl() *FuncDecl { return f.d }

// Func3 is a database function of three arguments returning R, declared with DeclareFunc3.
type Func3[R FuncResult, A, B, C Expr] struct{ d *FuncDecl }

// DeclareFunc3 declares the database function name taking an A, a B and a C and returning R.
func DeclareFunc3[R FuncResult, A, B, C Expr](name string) Func3[R, A, B, C] {
	return Func3[R, A, B, C]{d: declare[R](name, 3)}
}

// Call is name(a, b, c).
func (f Func3[R, A, B, C]) Call(a A, b B, c C) R {
	return resultOf[R](f.d.call(Expr(a), Expr(b), Expr(c)))
}

// Decl returns the declaration VerifyFuncs checks.
func (f Func3[R, A, B, C]) Decl() *FuncDecl { return f.d }
