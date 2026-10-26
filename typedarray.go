package keel

import (
	"time"

	"github.com/emoss08/keel/schema"
)

// Typed arrays. Each element kind has an array value interface (StringArrayValue, Int64ArrayValue,
// …), an array expression type (StringArrayExpr, …), a literal that binds a Go slice as one
// parameter under its array cast (keel.TextArray(ids) is $1::text[]), a constructor of ARRAY[…]
// from expressions of the kind (keel.ArrayOfString(a, b)) and a view (keel.StringArrayOf(e)) for an
// array keel cannot type itself, such as a CTE's array column. The element side tests membership
// with InArray (e = ANY(a)). Names follow the rest of the typed library: a function of Go values is
// named after the SQL type (Text, TextArray), one of typed expressions after the kind (StringOf,
// ArrayOfString).

// arrayLit binds v as one parameter under the array cast typ ("text[]").
func arrayLit(v any, typ string) Expr { return &castLit{v: v, typ: typ} }

// arrayCons is ARRAY[es…], or ARRAY[]::typ when es is empty, which Postgres cannot type otherwise. A
// nil element is NULL.
func arrayCons(typ string, es []Expr) Expr {
	for i, e := range es {
		if e == nil {
			es[i] = rawText("NULL")
		}
	}
	if len(es) == 0 {
		return rawText("ARRAY[]::" + typ)
	}
	return exprFunc(func(r *render) {
		r.s("ARRAY[")
		writeExprs(r, es)
		r.b(']')
	})
}

// elems is vs as expressions.
func elems[V Expr](vs []V) []Expr {
	out := make([]Expr, len(vs))
	for i, v := range vs {
		out[i] = v
	}
	return out
}

// arrayAt is (a)[i], the i-th element from 1, NULL past either end. The index is written as a
// constant, as dynamic expressions write it.
func arrayAt(a Expr, i int) Expr {
	a = operandOf(a, "[]")
	return exprFunc(func(r *render) {
		r.b('(')
		a.writeSQL(r)
		r.s(")[")
		r.int(int64(i))
		r.b(']')
	})
}

// arrayRemove is array_remove(a, v): a without the elements equal to v (without its NULLs when v is
// nil).
func arrayRemove(a Expr, v Expr) Expr { return callOf("array_remove", operandOf(a, "array_remove"), v) }

// StringArrayValue is a text[] value: a StringArrayCol or a StringArrayExpr.
type StringArrayValue interface {
	Expr
	stringArrayValue()
}

// Int64ArrayValue is a bigint[] value: an Int64ArrayCol or an Int64ArrayExpr.
type Int64ArrayValue interface {
	Expr
	int64ArrayValue()
}

// Int32ArrayValue is an integer[] value: an Int32ArrayExpr.
type Int32ArrayValue interface {
	Expr
	int32ArrayValue()
}

// Float64ArrayValue is a double precision[] value: a Float64ArrayExpr.
type Float64ArrayValue interface {
	Expr
	float64ArrayValue()
}

// NumericArrayValue is a numeric[] value: a NumericArrayExpr.
type NumericArrayValue interface {
	Expr
	numericArrayValue()
}

// BoolArrayValue is a boolean[] value: a BoolArrayExpr.
type BoolArrayValue interface {
	Expr
	boolArrayValue()
}

// TimeArrayValue is a timestamptz[] value: a TimeArrayExpr.
type TimeArrayValue interface {
	Expr
	timeArrayValue()
}

func (StringArrayCol) stringArrayValue() {}
func (Int64ArrayCol) int64ArrayValue()   {}

// Literals: a Go slice bound as one parameter, so the statement text does not depend on its length.
// A nil slice binds the empty array.

// TextArray binds vs as $n::text[].
func TextArray(vs []string) StringArrayExpr {
	if vs == nil {
		vs = []string{}
	}
	return StringArrayExpr{e: arrayLit(vs, "text[]")}
}

// Int64Array binds vs as $n::bigint[].
func Int64Array(vs []int64) Int64ArrayExpr {
	if vs == nil {
		vs = []int64{}
	}
	return Int64ArrayExpr{e: arrayLit(vs, "bigint[]")}
}

// Int32Array binds vs as $n::integer[].
func Int32Array(vs []int32) Int32ArrayExpr {
	if vs == nil {
		vs = []int32{}
	}
	return Int32ArrayExpr{e: arrayLit(vs, "integer[]")}
}

// Float64Array binds vs as $n::double precision[].
func Float64Array(vs []float64) Float64ArrayExpr {
	if vs == nil {
		vs = []float64{}
	}
	return Float64ArrayExpr{e: arrayLit(vs, "double precision[]")}
}

// NumericArray binds vs, a slice of decimals or of any values Postgres reads as numeric, as
// $n::numeric[].
func NumericArray(vs any) NumericArrayExpr { return NumericArrayExpr{e: arrayLit(vs, "numeric[]")} }

// BoolArray binds vs as $n::boolean[].
func BoolArray(vs []bool) BoolArrayExpr {
	if vs == nil {
		vs = []bool{}
	}
	return BoolArrayExpr{e: arrayLit(vs, "boolean[]")}
}

// TimeArray binds vs as $n::timestamptz[].
func TimeArray(vs []time.Time) TimeArrayExpr {
	if vs == nil {
		vs = []time.Time{}
	}
	return TimeArrayExpr{e: arrayLit(vs, "timestamptz[]")}
}

// Constructors: ARRAY[e1, e2, …] of expressions of the kind; no elements is the typed empty array,
// ARRAY[]::text[].

// ArrayOfString is ARRAY[es…] of text values.
func ArrayOfString(es ...StringValue) StringArrayExpr {
	return StringArrayExpr{e: arrayCons("text[]", elems(es))}
}

// ArrayOfInt64 is ARRAY[es…] of bigint values: keel.ArrayOfInt64(keel.Int64Of(keel.Count().Filter(p)), …)
// returns several filtered counts in one column.
func ArrayOfInt64(es ...Int64Value) Int64ArrayExpr {
	return Int64ArrayExpr{e: arrayCons("bigint[]", elems(es))}
}

// ArrayOfInt32 is ARRAY[es…] of integer values.
func ArrayOfInt32(es ...Int32Value) Int32ArrayExpr {
	return Int32ArrayExpr{e: arrayCons("integer[]", elems(es))}
}

// ArrayOfFloat64 is ARRAY[es…] of double precision values.
func ArrayOfFloat64(es ...Float64Value) Float64ArrayExpr {
	return Float64ArrayExpr{e: arrayCons("double precision[]", elems(es))}
}

// ArrayOfNumeric is ARRAY[es…] of numeric values.
func ArrayOfNumeric(es ...NumericValue) NumericArrayExpr {
	return NumericArrayExpr{e: arrayCons("numeric[]", elems(es))}
}

// ArrayOfBool is ARRAY[es…] of boolean values.
func ArrayOfBool(es ...BoolValue) BoolArrayExpr {
	return BoolArrayExpr{e: arrayCons("boolean[]", elems(es))}
}

// ArrayOfTime is ARRAY[es…] of times.
func ArrayOfTime(es ...TimeValue) TimeArrayExpr {
	return TimeArrayExpr{e: arrayCons("timestamptz[]", elems(es))}
}

// Views: an expression typed as an array of the kind without changing the SQL.

// StringArrayOf types e as text[].
func StringArrayOf(e Expr) StringArrayExpr { return StringArrayExpr{e: viewOf(e)} }

// Int64ArrayOf types e as bigint[].
func Int64ArrayOf(e Expr) Int64ArrayExpr { return Int64ArrayExpr{e: viewOf(e)} }

// Int32ArrayOf types e as integer[].
func Int32ArrayOf(e Expr) Int32ArrayExpr { return Int32ArrayExpr{e: viewOf(e)} }

// Float64ArrayOf types e as double precision[].
func Float64ArrayOf(e Expr) Float64ArrayExpr { return Float64ArrayExpr{e: viewOf(e)} }

// NumericArrayOf types e as numeric[].
func NumericArrayOf(e Expr) NumericArrayExpr { return NumericArrayExpr{e: viewOf(e)} }

// BoolArrayOf types e as boolean[].
func BoolArrayOf(e Expr) BoolArrayExpr { return BoolArrayExpr{e: viewOf(e)} }

// TimeArrayOf types e as timestamptz[].
func TimeArrayOf(e Expr) TimeArrayExpr { return TimeArrayExpr{e: viewOf(e)} }

// StringArrayExpr is a computed text[] value. Its methods are those of every array expression: At,
// Len (cardinality), Unnest, Without (array_remove), ContainsAllExpr (@>), ContainedByExpr (<@),
// OverlapsExpr (&&), Coalesce, IsNull, IsNotNull.
type StringArrayExpr struct{ e Expr }

func (e StringArrayExpr) writeSQL(r *render)      { e.e.writeSQL(r) }
func (StringArrayExpr) stringArrayValue()         {}
func (StringArrayExpr) kind() (schema.Kind, bool) { return schema.KindStringArray, true }

// At is (e)[i], the i-th element from 1.
func (e StringArrayExpr) At(i int) StringExpr { return StringExpr{e: arrayAt(e.e, i)} }

// Len is cardinality(e).
func (e StringArrayExpr) Len() Int32Expr { return Int32Expr{e: callOf("cardinality", e.e)} }

// Unnest is the set-returning unnest(e), for keel.Rows.
func (e StringArrayExpr) Unnest() StringExpr { return StringExpr{e: callOf("unnest", e.e)} }

// Without is array_remove(e, v).
func (e StringArrayExpr) Without(v StringValue) StringArrayExpr {
	return StringArrayExpr{e: arrayRemove(e.e, v)}
}

// ContainsAllExpr is (e @> o): e holds every element of o.
func (e StringArrayExpr) ContainsAllExpr(o StringArrayValue) Pred { return parenPred(e.e, " @> ", o) }

// ContainedByExpr is (e <@ o): o holds every element of e.
func (e StringArrayExpr) ContainedByExpr(o StringArrayValue) Pred { return parenPred(e.e, " <@ ", o) }

// OverlapsExpr is (e && o): the arrays share an element.
func (e StringArrayExpr) OverlapsExpr(o StringArrayValue) Pred { return parenPred(e.e, " && ", o) }

// Coalesce is coalesce(e, o).
func (e StringArrayExpr) Coalesce(o StringArrayValue) StringArrayExpr {
	return StringArrayExpr{e: callOf("coalesce", e.e, o)}
}

// IsNull is e IS NULL.
func (e StringArrayExpr) IsNull() Pred { return suffix(e.e, " IS NULL") }

// IsNotNull is e IS NOT NULL.
func (e StringArrayExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }

// At is (col)[i], the i-th element from 1.
func (c StringArrayCol) At(i int) StringExpr { return StringArrayExpr{e: c.Col}.At(i) }

// Without is array_remove(col, v), an expression; Remove is the assignment.
func (c StringArrayCol) Without(v StringValue) StringArrayExpr {
	return StringArrayExpr{e: c.Col}.Without(v)
}

// ContainsAllExpr is (col @> o).
func (c StringArrayCol) ContainsAllExpr(o StringArrayValue) Pred {
	return StringArrayExpr{e: c.Col}.ContainsAllExpr(o)
}

// ContainedByExpr is (col <@ o).
func (c StringArrayCol) ContainedByExpr(o StringArrayValue) Pred {
	return StringArrayExpr{e: c.Col}.ContainedByExpr(o)
}

// OverlapsExpr is (col && o).
func (c StringArrayCol) OverlapsExpr(o StringArrayValue) Pred {
	return StringArrayExpr{e: c.Col}.OverlapsExpr(o)
}

// Int64ArrayExpr is a computed bigint[] value, with the methods of StringArrayExpr.
type Int64ArrayExpr struct{ e Expr }

func (e Int64ArrayExpr) writeSQL(r *render)      { e.e.writeSQL(r) }
func (Int64ArrayExpr) int64ArrayValue()          {}
func (Int64ArrayExpr) kind() (schema.Kind, bool) { return schema.KindInt64Array, true }

// At is (e)[i], the i-th element from 1.
func (e Int64ArrayExpr) At(i int) Int64Expr { return Int64Expr{e: arrayAt(e.e, i)} }

// Len is cardinality(e).
func (e Int64ArrayExpr) Len() Int32Expr { return Int32Expr{e: callOf("cardinality", e.e)} }

// Unnest is the set-returning unnest(e), for keel.Rows.
func (e Int64ArrayExpr) Unnest() Int64Expr { return Int64Expr{e: callOf("unnest", e.e)} }

// Without is array_remove(e, v).
func (e Int64ArrayExpr) Without(v Int64Value) Int64ArrayExpr {
	return Int64ArrayExpr{e: arrayRemove(e.e, v)}
}

// ContainsAllExpr is (e @> o).
func (e Int64ArrayExpr) ContainsAllExpr(o Int64ArrayValue) Pred { return parenPred(e.e, " @> ", o) }

// ContainedByExpr is (e <@ o).
func (e Int64ArrayExpr) ContainedByExpr(o Int64ArrayValue) Pred { return parenPred(e.e, " <@ ", o) }

// OverlapsExpr is (e && o).
func (e Int64ArrayExpr) OverlapsExpr(o Int64ArrayValue) Pred { return parenPred(e.e, " && ", o) }

// Coalesce is coalesce(e, o).
func (e Int64ArrayExpr) Coalesce(o Int64ArrayValue) Int64ArrayExpr {
	return Int64ArrayExpr{e: callOf("coalesce", e.e, o)}
}

// IsNull is e IS NULL.
func (e Int64ArrayExpr) IsNull() Pred { return suffix(e.e, " IS NULL") }

// IsNotNull is e IS NOT NULL.
func (e Int64ArrayExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }

// At is (col)[i].
func (c Int64ArrayCol) At(i int) Int64Expr { return Int64ArrayExpr{e: c.Col}.At(i) }

// Without is array_remove(col, v), an expression; Remove is the assignment.
func (c Int64ArrayCol) Without(v Int64Value) Int64ArrayExpr {
	return Int64ArrayExpr{e: c.Col}.Without(v)
}

// ContainsAllExpr is (col @> o).
func (c Int64ArrayCol) ContainsAllExpr(o Int64ArrayValue) Pred {
	return Int64ArrayExpr{e: c.Col}.ContainsAllExpr(o)
}

// ContainedByExpr is (col <@ o).
func (c Int64ArrayCol) ContainedByExpr(o Int64ArrayValue) Pred {
	return Int64ArrayExpr{e: c.Col}.ContainedByExpr(o)
}

// OverlapsExpr is (col && o).
func (c Int64ArrayCol) OverlapsExpr(o Int64ArrayValue) Pred {
	return Int64ArrayExpr{e: c.Col}.OverlapsExpr(o)
}

// Int32ArrayExpr is a computed integer[] value, with the methods of StringArrayExpr.
type Int32ArrayExpr struct{ e Expr }

func (e Int32ArrayExpr) writeSQL(r *render) { e.e.writeSQL(r) }
func (Int32ArrayExpr) int32ArrayValue()     {}

// At is (e)[i], the i-th element from 1.
func (e Int32ArrayExpr) At(i int) Int32Expr { return Int32Expr{e: arrayAt(e.e, i)} }

// Len is cardinality(e).
func (e Int32ArrayExpr) Len() Int32Expr { return Int32Expr{e: callOf("cardinality", e.e)} }

// Unnest is the set-returning unnest(e), for keel.Rows.
func (e Int32ArrayExpr) Unnest() Int32Expr { return Int32Expr{e: callOf("unnest", e.e)} }

// Without is array_remove(e, v).
func (e Int32ArrayExpr) Without(v Int32Value) Int32ArrayExpr {
	return Int32ArrayExpr{e: arrayRemove(e.e, v)}
}

// ContainsAllExpr is (e @> o).
func (e Int32ArrayExpr) ContainsAllExpr(o Int32ArrayValue) Pred { return parenPred(e.e, " @> ", o) }

// ContainedByExpr is (e <@ o).
func (e Int32ArrayExpr) ContainedByExpr(o Int32ArrayValue) Pred { return parenPred(e.e, " <@ ", o) }

// OverlapsExpr is (e && o).
func (e Int32ArrayExpr) OverlapsExpr(o Int32ArrayValue) Pred { return parenPred(e.e, " && ", o) }

// Coalesce is coalesce(e, o).
func (e Int32ArrayExpr) Coalesce(o Int32ArrayValue) Int32ArrayExpr {
	return Int32ArrayExpr{e: callOf("coalesce", e.e, o)}
}

// IsNull is e IS NULL.
func (e Int32ArrayExpr) IsNull() Pred { return suffix(e.e, " IS NULL") }

// IsNotNull is e IS NOT NULL.
func (e Int32ArrayExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }

// Float64ArrayExpr is a computed double precision[] value, with the methods of StringArrayExpr.
type Float64ArrayExpr struct{ e Expr }

func (e Float64ArrayExpr) writeSQL(r *render) { e.e.writeSQL(r) }
func (Float64ArrayExpr) float64ArrayValue()   {}

// At is (e)[i], the i-th element from 1.
func (e Float64ArrayExpr) At(i int) Float64Expr { return Float64Expr{e: arrayAt(e.e, i)} }

// Len is cardinality(e).
func (e Float64ArrayExpr) Len() Int32Expr { return Int32Expr{e: callOf("cardinality", e.e)} }

// Unnest is the set-returning unnest(e), for keel.Rows.
func (e Float64ArrayExpr) Unnest() Float64Expr { return Float64Expr{e: callOf("unnest", e.e)} }

// Without is array_remove(e, v).
func (e Float64ArrayExpr) Without(v Float64Value) Float64ArrayExpr {
	return Float64ArrayExpr{e: arrayRemove(e.e, v)}
}

// ContainsAllExpr is (e @> o).
func (e Float64ArrayExpr) ContainsAllExpr(o Float64ArrayValue) Pred {
	return parenPred(e.e, " @> ", o)
}

// ContainedByExpr is (e <@ o).
func (e Float64ArrayExpr) ContainedByExpr(o Float64ArrayValue) Pred {
	return parenPred(e.e, " <@ ", o)
}

// OverlapsExpr is (e && o).
func (e Float64ArrayExpr) OverlapsExpr(o Float64ArrayValue) Pred { return parenPred(e.e, " && ", o) }

// Coalesce is coalesce(e, o).
func (e Float64ArrayExpr) Coalesce(o Float64ArrayValue) Float64ArrayExpr {
	return Float64ArrayExpr{e: callOf("coalesce", e.e, o)}
}

// IsNull is e IS NULL.
func (e Float64ArrayExpr) IsNull() Pred { return suffix(e.e, " IS NULL") }

// IsNotNull is e IS NOT NULL.
func (e Float64ArrayExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }

// NumericArrayExpr is a computed numeric[] value, with the methods of StringArrayExpr.
type NumericArrayExpr struct{ e Expr }

func (e NumericArrayExpr) writeSQL(r *render) { e.e.writeSQL(r) }
func (NumericArrayExpr) numericArrayValue()   {}

// At is (e)[i], the i-th element from 1.
func (e NumericArrayExpr) At(i int) NumericExpr { return NumericExpr{e: arrayAt(e.e, i)} }

// Len is cardinality(e).
func (e NumericArrayExpr) Len() Int32Expr { return Int32Expr{e: callOf("cardinality", e.e)} }

// Unnest is the set-returning unnest(e), for keel.Rows.
func (e NumericArrayExpr) Unnest() NumericExpr { return NumericExpr{e: callOf("unnest", e.e)} }

// Without is array_remove(e, v).
func (e NumericArrayExpr) Without(v NumericValue) NumericArrayExpr {
	return NumericArrayExpr{e: arrayRemove(e.e, v)}
}

// ContainsAllExpr is (e @> o).
func (e NumericArrayExpr) ContainsAllExpr(o NumericArrayValue) Pred {
	return parenPred(e.e, " @> ", o)
}

// ContainedByExpr is (e <@ o).
func (e NumericArrayExpr) ContainedByExpr(o NumericArrayValue) Pred {
	return parenPred(e.e, " <@ ", o)
}

// OverlapsExpr is (e && o).
func (e NumericArrayExpr) OverlapsExpr(o NumericArrayValue) Pred { return parenPred(e.e, " && ", o) }

// Coalesce is coalesce(e, o).
func (e NumericArrayExpr) Coalesce(o NumericArrayValue) NumericArrayExpr {
	return NumericArrayExpr{e: callOf("coalesce", e.e, o)}
}

// IsNull is e IS NULL.
func (e NumericArrayExpr) IsNull() Pred { return suffix(e.e, " IS NULL") }

// IsNotNull is e IS NOT NULL.
func (e NumericArrayExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }

// BoolArrayExpr is a computed boolean[] value, with the methods of StringArrayExpr.
type BoolArrayExpr struct{ e Expr }

func (e BoolArrayExpr) writeSQL(r *render) { e.e.writeSQL(r) }
func (BoolArrayExpr) boolArrayValue()      {}

// At is (e)[i], the i-th element from 1.
func (e BoolArrayExpr) At(i int) BoolExpr { return BoolExpr{e: arrayAt(e.e, i)} }

// Len is cardinality(e).
func (e BoolArrayExpr) Len() Int32Expr { return Int32Expr{e: callOf("cardinality", e.e)} }

// Unnest is the set-returning unnest(e), for keel.Rows.
func (e BoolArrayExpr) Unnest() BoolExpr { return BoolExpr{e: callOf("unnest", e.e)} }

// Without is array_remove(e, v).
func (e BoolArrayExpr) Without(v BoolValue) BoolArrayExpr {
	return BoolArrayExpr{e: arrayRemove(e.e, v)}
}

// ContainsAllExpr is (e @> o).
func (e BoolArrayExpr) ContainsAllExpr(o BoolArrayValue) Pred { return parenPred(e.e, " @> ", o) }

// ContainedByExpr is (e <@ o).
func (e BoolArrayExpr) ContainedByExpr(o BoolArrayValue) Pred { return parenPred(e.e, " <@ ", o) }

// OverlapsExpr is (e && o).
func (e BoolArrayExpr) OverlapsExpr(o BoolArrayValue) Pred { return parenPred(e.e, " && ", o) }

// Coalesce is coalesce(e, o).
func (e BoolArrayExpr) Coalesce(o BoolArrayValue) BoolArrayExpr {
	return BoolArrayExpr{e: callOf("coalesce", e.e, o)}
}

// IsNull is e IS NULL.
func (e BoolArrayExpr) IsNull() Pred { return suffix(e.e, " IS NULL") }

// IsNotNull is e IS NOT NULL.
func (e BoolArrayExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }

// TimeArrayExpr is a computed timestamptz[] value, with the methods of StringArrayExpr.
type TimeArrayExpr struct{ e Expr }

func (e TimeArrayExpr) writeSQL(r *render) { e.e.writeSQL(r) }
func (TimeArrayExpr) timeArrayValue()      {}

// At is (e)[i], the i-th element from 1.
func (e TimeArrayExpr) At(i int) TimeExpr { return TimeExpr{e: arrayAt(e.e, i)} }

// Len is cardinality(e).
func (e TimeArrayExpr) Len() Int32Expr { return Int32Expr{e: callOf("cardinality", e.e)} }

// Unnest is the set-returning unnest(e), for keel.Rows.
func (e TimeArrayExpr) Unnest() TimeExpr { return TimeExpr{e: callOf("unnest", e.e)} }

// Without is array_remove(e, v).
func (e TimeArrayExpr) Without(v TimeValue) TimeArrayExpr {
	return TimeArrayExpr{e: arrayRemove(e.e, v)}
}

// ContainsAllExpr is (e @> o).
func (e TimeArrayExpr) ContainsAllExpr(o TimeArrayValue) Pred { return parenPred(e.e, " @> ", o) }

// ContainedByExpr is (e <@ o).
func (e TimeArrayExpr) ContainedByExpr(o TimeArrayValue) Pred { return parenPred(e.e, " <@ ", o) }

// OverlapsExpr is (e && o).
func (e TimeArrayExpr) OverlapsExpr(o TimeArrayValue) Pred { return parenPred(e.e, " && ", o) }

// Coalesce is coalesce(e, o).
func (e TimeArrayExpr) Coalesce(o TimeArrayValue) TimeArrayExpr {
	return TimeArrayExpr{e: callOf("coalesce", e.e, o)}
}

// IsNull is e IS NULL.
func (e TimeArrayExpr) IsNull() Pred { return suffix(e.e, " IS NULL") }

// IsNotNull is e IS NOT NULL.
func (e TimeArrayExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }

// NullIf is nullif(e, o): NULL where e equals o, as NullIf(keel.TextArray(nil)) turns an empty
// array into NULL.
func (e StringArrayExpr) NullIf(o StringArrayValue) StringArrayExpr {
	return StringArrayExpr{e: callOf("nullif", e.e, o)}
}

// NullIf is nullif(e, o): NULL where e equals o, as NullIf(keel.Int64Array(nil)) turns an empty
// array into NULL.
func (e Int64ArrayExpr) NullIf(o Int64ArrayValue) Int64ArrayExpr {
	return Int64ArrayExpr{e: callOf("nullif", e.e, o)}
}

// NullIf is nullif(e, o): NULL where e equals o, as NullIf(keel.Int32Array(nil)) turns an empty
// array into NULL.
func (e Int32ArrayExpr) NullIf(o Int32ArrayValue) Int32ArrayExpr {
	return Int32ArrayExpr{e: callOf("nullif", e.e, o)}
}

// NullIf is nullif(e, o): NULL where e equals o, as NullIf(keel.Float64Array(nil)) turns an empty
// array into NULL.
func (e Float64ArrayExpr) NullIf(o Float64ArrayValue) Float64ArrayExpr {
	return Float64ArrayExpr{e: callOf("nullif", e.e, o)}
}

// NullIf is nullif(e, o): NULL where e equals o, as NullIf(keel.NumericArray(nil)) turns an empty
// array into NULL.
func (e NumericArrayExpr) NullIf(o NumericArrayValue) NumericArrayExpr {
	return NumericArrayExpr{e: callOf("nullif", e.e, o)}
}

// NullIf is nullif(e, o): NULL where e equals o, as NullIf(keel.BoolArray(nil)) turns an empty
// array into NULL.
func (e BoolArrayExpr) NullIf(o BoolArrayValue) BoolArrayExpr {
	return BoolArrayExpr{e: callOf("nullif", e.e, o)}
}

// NullIf is nullif(e, o): NULL where e equals o, as NullIf(keel.TimeArray(nil)) turns an empty
// array into NULL.
func (e TimeArrayExpr) NullIf(o TimeArrayValue) TimeArrayExpr {
	return TimeArrayExpr{e: callOf("nullif", e.e, o)}
}
