package keel

import (
	"reflect"
	"unsafe"
)

var bytesType = reflect.TypeFor[[]byte]()

// inferredType returns the SQL type a value of Go type t binds as where nothing around it tells
// Postgres its type: an integer is bigint, a uint or uint64 numeric, a float double precision, a
// bool boolean, a string text, a time.Time timestamptz, a []byte bytea and a slice of one of these
// its array. A named type counts as its underlying type only when it has no methods, so a type
// that encodes itself (a Valuer, a time.Duration, a decimal) is left for Postgres to resolve, and
// "" is returned.
func inferredType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType:
		return "timestamptz"
	case bytesType:
		return "bytea"
	}
	if t.NumMethod() > 0 {
		return ""
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return "bigint"
	case reflect.Uint, reflect.Uint64:
		return "numeric"
	case reflect.Float32, reflect.Float64:
		return "double precision"
	case reflect.Bool:
		return "boolean"
	case reflect.String:
		return "text"
	case reflect.Slice:
		if t.Name() != "" {
			return ""
		}
		if elem := inferredType(t.Elem()); elem != "" && t.Elem().Kind() != reflect.Slice {
			return elem + "[]"
		}
	}
	return ""
}

// literalOf returns the bound value e is, when e is a literal.
func literalOf(e Expr) (*litExpr, bool) {
	switch x := e.(type) {
	case *litExpr:
		return x, true
	case DynExpr:
		if x.n != nil && x.n.err == nil {
			l, ok := x.n.e.(*litExpr)
			return l, ok
		}
	}
	return nil, false
}

// typedLiteral is a literal written with the cast its Go type implies.
type typedLiteral struct {
	l   *litExpr
	typ string
}

func (t typedLiteral) writeSQL(r *render) {
	r.argSlot(t.l.v, unsafe.Pointer(t.l), "::"+t.typ)
	r.s("::")
	r.s(t.typ)
}

// typed returns e cast to the type its Go value implies when e is a literal of a type keel can
// name, else e.
func typed(e Expr) Expr {
	if l, ok := literalOf(e); ok {
		if typ := inferredType(reflect.TypeOf(l.v)); typ != "" {
			return typedLiteral{l: l, typ: typ}
		}
	}
	return e
}

// typedIfAllLiterals casts every literal of es when no element of es is anything else: the CASE
// results or array elements that would otherwise resolve to text. When one of them is an
// expression, Postgres types the literals after it, so they are left alone.
func typedIfAllLiterals(es []Expr) {
	for _, e := range es {
		if e == nil {
			continue
		}
		if _, ok := literalOf(e); !ok {
			if t, isText := e.(rawText); !isText || t != "NULL" {
				return
			}
		}
	}
	for i, e := range es {
		if e != nil {
			es[i] = typed(e)
		}
	}
}

// writeOutput writes an output of a select list: a bare literal is cast to the type its Go value
// implies, since nothing else types it.
func writeOutput(r *render, e Expr) {
	switch x := e.(type) {
	case aliased:
		writeOutput(r, x.e)
		r.s(" AS ")
		r.s(x.name)
		return
	}
	typed(e).writeSQL(r)
}

// outputPosition returns the 1-based position in the select list exprs of the output e is, when
// projected (exprs is the whole select list) and e is an expression other than a bare column or a
// literal, so GROUP BY and ORDER BY name it by position instead of repeating it. It returns 0
// otherwise.
func outputPosition(projected bool, exprs []Expr, e Expr) int {
	if !projected {
		return 0
	}
	d, ok := e.(DynExpr)
	if !ok || d.n == nil || d.n.err != nil || d.n.col != nil {
		return 0
	}
	if _, lit := d.n.e.(*litExpr); lit {
		return 0
	}
	for i, x := range exprs {
		if a, isAliased := x.(aliased); isAliased {
			x = a.e
		}
		if o, isDyn := x.(DynExpr); isDyn && o.same(d) {
			return i + 1
		}
	}
	return 0
}
