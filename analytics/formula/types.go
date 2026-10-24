// Package formula is the typed expression language of keel/analytics report calculations: a lexer,
// a Pratt parser, an AST that also has a JSON encoding, a type checker over an environment, a
// function registry with deterministic SQL rendering descriptors, and a canonical printer.
//
// The package does no SQL and touches no database. Identifiers never come from a formula: column,
// field and parameter references are resolved by the environment, and functions render from
// templates that keel owns.
package formula

import "slices"

// Type is the value type of an expression, a catalog field or a report parameter.
type Type string

const (
	// Unknown is the type of a reference the environment cannot type yet. It is accepted
	// wherever a value is, so a definition can be checked for structure before a catalog is
	// available.
	Unknown Type = ""
	// Null is the type of the literal null, which takes on the type of what it is combined with.
	Null Type = "null"
	// String is text.
	String Type = "string"
	// Int is a 64-bit integer.
	Int Type = "int"
	// Decimal is an exact decimal number (Postgres numeric).
	Decimal Type = "decimal"
	// Float is a binary floating-point number (Postgres double precision).
	Float Type = "float"
	// Bool is true or false.
	Bool Type = "bool"
	// Date is a calendar date without a time zone.
	Date Type = "date"
	// Timestamp is an instant (Postgres timestamptz).
	Timestamp Type = "timestamp"
	// Epoch is an instant stored as integer seconds since 1970-01-01 UTC.
	Epoch Type = "epoch"
	// Enum is one of a fixed set of text values.
	Enum Type = "enum"
	// Ref is the identifier of a record of another entity.
	Ref Type = "ref"
	// JSON is a JSON document.
	JSON Type = "json"
)

// Types returns every value type a field or parameter can have, in a stable order.
func Types() []Type {
	return []Type{String, Int, Decimal, Float, Bool, Date, Timestamp, Epoch, Enum, Ref, JSON}
}

// Valid reports whether t is one of Types.
func (t Type) Valid() bool { return slices.Contains(Types(), t) }

// Numeric reports whether t is int, decimal or float.
func (t Type) Numeric() bool { return t == Int || t == Decimal || t == Float }

// Textual reports whether t is string or enum.
func (t Type) Textual() bool { return t == String || t == Enum }

// Temporal reports whether t is date, timestamp or epoch.
func (t Type) Temporal() bool { return t == Date || t == Timestamp || t == Epoch }

// Ordered reports whether values of t can be compared with < and >.
func (t Type) Ordered() bool { return t.Numeric() || t == String || t.Temporal() }

func numericRank(t Type) int {
	switch t {
	case Int:
		return 1
	case Decimal:
		return 2
	case Float:
		return 3
	default:
		return 0
	}
}

// Wider returns the wider of two numeric types: int widens to decimal, decimal to float.
func Wider(a, b Type) Type {
	if numericRank(a) >= numericRank(b) {
		return a
	}
	return b
}

// Common returns the type two values combine to, as the branches of if, case and coalesce do.
// Null takes the other type, Unknown absorbs everything, and ok is false when the types cannot
// be combined.
func Common(a, b Type) (Type, bool) {
	switch {
	case a == Unknown || b == Unknown:
		return Unknown, true
	case a == Null:
		return b, true
	case b == Null, a == b:
		return a, true
	case a.Numeric() && b.Numeric():
		return Wider(a, b), true
	case a.Textual() && b.Textual(), a == Ref && b.Textual(), b == Ref && a.Textual():
		return String, true
	case a.Temporal() && b.Temporal():
		return Timestamp, true
	default:
		return Unknown, false
	}
}

// Level is how a value relates to the rows of the report: a constant, a value per source row, a
// value per group, or a value computed across the grouped result.
type Level int

const (
	// Constant is a literal or a parameter.
	Constant Level = iota
	// Row is a value per source row: a field, a dimension or a row-level calculation.
	Row
	// Aggregate is a value per group: a measure or a calculation over measures.
	Aggregate
	// Window is a value computed over the grouped result: a window column or a calculation over one.
	Window
)

// String names the level.
func (l Level) String() string {
	switch l {
	case Constant:
		return "constant"
	case Row:
		return "row"
	case Aggregate:
		return "aggregate"
	default:
		return "window"
	}
}
