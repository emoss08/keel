// Package value names the types a report column holds and the columnar vectors that hold its
// values.
//
// It is the lowest analytics package: the display spec (analytics/format), the result schema
// (analytics/table), the writers (analytics/export) and run diffs (analytics/diff) all speak
// in these types. A Vector keeps one typed slice per column, so a result decoded from
// Postgres's binary format lands in it without boxing a value in an interface.
package value

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Type is the type of a report column's values. The zero Type is invalid.
type Type uint8

// The value types a report column can have.
const (
	// String is text.
	String Type = iota + 1
	// Int is a whole number, held as int64.
	Int
	// Decimal is an exact decimal (Postgres numeric), held as pgtype.Numeric.
	Decimal
	// Float is a floating-point number, held as float64.
	Float
	// Bool is a boolean.
	Bool
	// Time is an instant (timestamptz), held as time.Time.
	Time
	// Epoch is an instant held as whole seconds since the Unix epoch in an int64.
	Epoch
	// Enum is one of a named set of text values, held as its stored text.
	Enum
	// Ref is a reference to another record, held as its key text.
	Ref
	// JSON is a JSON document, held as its raw text.
	JSON
)

// typeNames are the types' names.
var typeNames = [...]string{
	String:  "string",
	Int:     "int",
	Decimal: "decimal",
	Float:   "float",
	Bool:    "bool",
	Time:    "time",
	Epoch:   "epoch",
	Enum:    "enum",
	Ref:     "ref",
	JSON:    "json",
}

// Types returns every valid value type, in declaration order.
func Types() []Type {
	out := make([]Type, 0, len(typeNames)-1)
	for t := String; t <= JSON; t++ {
		out = append(out, t)
	}
	return out
}

// Valid reports whether t is one of the declared types.
func (t Type) Valid() bool { return t >= String && t <= JSON }

// String returns the type's name ("decimal"), or "" for an invalid type.
func (t Type) String() string {
	if !t.Valid() {
		return ""
	}
	return typeNames[t]
}

// Numeric reports whether values of the type are quantities: Int, Decimal and Float.
func (t Type) Numeric() bool { return t == Int || t == Decimal || t == Float }

// Instant reports whether values of the type denote a point in time: Time and Epoch.
func (t Type) Instant() bool { return t == Time || t == Epoch }

// Textual reports whether values of the type are held as text: String, Enum and Ref.
func (t Type) Textual() bool { return t == String || t == Enum || t == Ref }

// MarshalText writes the type's name.
func (t Type) MarshalText() ([]byte, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("value: invalid type %d", t)
	}
	return []byte(typeNames[t]), nil
}

// UnmarshalText reads a type's name.
func (t *Type) UnmarshalText(b []byte) error {
	parsed, err := ParseType(string(b))
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// ParseType reads a type's name ("decimal").
func ParseType(name string) (Type, error) {
	for t := String; t <= JSON; t++ {
		if typeNames[t] == name {
			return t, nil
		}
	}
	return 0, fmt.Errorf("value: unknown type %q", name)
}

// Vector holds one column of a batch of rows. Exactly one slice is set, chosen by the column's
// type, and it is at least as long as the batch:
//
//	String, Enum, Ref  String
//	Int, Epoch         Int64
//	Decimal            Numeric
//	Float              Float64
//	Bool               Bool
//	Time               Time
//	JSON               JSON
//
// Nulls marks the rows that are SQL NULL; the slot of a null row holds any value and is never
// read. A nil Nulls means no row is null. A Numeric whose Valid field is false is also read as
// null, which is how pgx decodes a NULL numeric. Strings are ordinary immutable Go strings: a
// reader may share one string among many rows, but must not alias a buffer it reuses.
type Vector struct {
	// Nulls marks null rows; nil when there are none.
	Nulls Bitmap
	// String holds String, Enum and Ref values.
	String []string
	// Int64 holds Int and Epoch values.
	Int64 []int64
	// Numeric holds Decimal values.
	Numeric []pgtype.Numeric
	// Float64 holds Float values.
	Float64 []float64
	// Bool holds Bool values.
	Bool []bool
	// Time holds Time values.
	Time []time.Time
	// JSON holds JSON values as raw JSON text.
	JSON [][]byte
}

// Null reports whether row i is null.
func (v *Vector) Null(i int) bool {
	if v.Nulls.Has(i) {
		return true
	}
	return v.Numeric != nil && !v.Numeric[i].Valid
}

// Bitmap is a set of row indexes, one bit per row, least significant bit first. A nil Bitmap is
// empty.
type Bitmap []uint64

// NewBitmap returns an empty bitmap that can hold rows 0 to n-1.
func NewBitmap(n int) Bitmap { return make(Bitmap, (n+63)/64) }

// Has reports whether row i is in the set. Rows past the bitmap's length are not.
func (b Bitmap) Has(i int) bool {
	w := i >> 6
	return w < len(b) && b[w]&(1<<(uint(i)&63)) != 0
}

// Set adds row i, which must be within the bitmap's length.
func (b Bitmap) Set(i int) { b[i>>6] |= 1 << (uint(i) & 63) }

// Clear removes row i, which must be within the bitmap's length.
func (b Bitmap) Clear(i int) { b[i>>6] &^= 1 << (uint(i) & 63) }
