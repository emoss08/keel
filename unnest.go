package keel

import (
	"reflect"
	"time"
)

// Unnest declares a CTE over Go slices, one per column, read row by row:
// WITH name (columns) AS (SELECT * FROM unnest($1::t1[], $2::t2[], …)). Each slice binds as one
// array parameter, so the statement text does not depend on the number of rows. It is the source
// of a bulk update that sets every row its own values, in one statement:
//
//	out := keel.Unnest("outcome", []string{"id", "status"}, ids, statuses)
//	n, err := t.With(out).Join(out, out.Col("id").EqCol(t.ID)).
//	    Update(ctx, db, t.Status.ToExpr(out.Col("status")))
//
// Slices hold strings, integers, floats, bools, times or byte slices, or named types of those;
// they must be of equal length.
func Unnest(name string, columns []string, arrays ...any) *CTETable {
	return CTE(name, columns, newUnnestBody(columns, arrays))
}

// unnestBody is the body of an Unnest CTE: the arrays, their array types, and the error of an
// array keel cannot bind.
type unnestBody struct {
	arrays []any
	types  []string
	err    error
}

// newUnnestBody checks the arrays against the columns and chooses each array's type.
func newUnnestBody(columns []string, arrays []any) unnestBody {
	b := unnestBody{arrays: arrays, types: make([]string, len(arrays))}
	if len(arrays) != len(columns) {
		b.err = errf("Unnest has %d columns and %d arrays", len(columns), len(arrays))
		return b
	}
	n := -1
	for i, a := range arrays {
		rv := reflect.ValueOf(a)
		if rv.Kind() != reflect.Slice {
			b.err = errf("Unnest column %s: %T is not a slice", columns[i], a)
			return b
		}
		if n >= 0 && rv.Len() != n {
			b.err = errf("Unnest column %s has %d values, the first column %d", columns[i], rv.Len(), n)
			return b
		}
		n = rv.Len()
		typ, ok := unnestType(rv.Type().Elem())
		if !ok {
			b.err = errf("Unnest column %s: cannot bind %T as an array", columns[i], a)
			return b
		}
		b.types[i] = typ
	}
	return b
}

// timeType is the reflect type of time.Time.
var timeType = reflect.TypeFor[time.Time]()

// unnestType returns the array type a slice of elem binds as.
func unnestType(elem reflect.Type) (string, bool) {
	if elem == timeType {
		return "timestamptz[]", true
	}
	switch elem.Kind() {
	case reflect.String:
		return "text[]", true
	case reflect.Int64, reflect.Int:
		return "int8[]", true
	case reflect.Int32:
		return "int4[]", true
	case reflect.Int16:
		return "int2[]", true
	case reflect.Float64:
		return "float8[]", true
	case reflect.Float32:
		return "float4[]", true
	case reflect.Bool:
		return "bool[]", true
	case reflect.Slice:
		if elem.Elem().Kind() == reflect.Uint8 {
			return "bytea[]", true
		}
	}
	return "", false
}

func (b unnestBody) writeSubquery(r *render, _ bool) {
	if b.err != nil {
		r.fail(b.err)
		return
	}
	r.s("SELECT * FROM unnest(")
	for i, a := range b.arrays {
		if i > 0 {
			r.s(", ")
		}
		r.arg(a)
		r.s("::")
		r.s(b.types[i])
	}
	r.b(')')
}
