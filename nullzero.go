package keel

import (
	"reflect"
	"time"

	"github.com/emoss08/keel/schema"
)

// nullZero reports whether c is a nullzero column, which stores its zero value as NULL.
func (c Col) nullZero() bool {
	return c.Ref != nil && c.Ref.Table != nil && c.Ref.Table.Columns[c.Index].Flags.Has(schema.FlagNullZero)
}

// isZeroValue reports whether v is the zero value of its type: an empty string, a zero number, false,
// a zero time, a nil slice or map. Basic types are tested without reflection.
func isZeroValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case int64:
		return x == 0
	case int32:
		return x == 0
	case int16:
		return x == 0
	case float64:
		return x == 0
	case float32:
		return x == 0
	case bool:
		return !x
	case time.Time:
		return x.IsZero()
	case []string:
		return x == nil
	case []int64:
		return x == nil
	case []byte:
		return x == nil
	}
	if z, ok := v.(interface{ IsZero() bool }); ok {
		return z.IsZero()
	}
	return reflect.ValueOf(v).IsZero()
}

// containsZero reports whether list, a slice of values, holds a zero value.
func containsZero(list any) bool {
	switch x := list.(type) {
	case []string:
		for _, v := range x {
			if v == "" {
				return true
			}
		}
		return false
	case []int64:
		for _, v := range x {
			if v == 0 {
				return true
			}
		}
		return false
	case []int32:
		for _, v := range x {
			if v == 0 {
				return true
			}
		}
		return false
	case []int16:
		for _, v := range x {
			if v == 0 {
				return true
			}
		}
		return false
	}
	rv := reflect.ValueOf(list)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return false
	}
	for i := range rv.Len() {
		if isZeroValue(rv.Index(i).Interface()) {
			return true
		}
	}
	return false
}

// nullZeroCmp is col op v on a nullzero column, where a zero value means NULL: = zero is IS NULL,
// <> zero and IS DISTINCT FROM zero are IS NOT NULL, and <> of any other value is IS DISTINCT FROM,
// so the rows that hold the zero value match it. It returns nil for every other comparison.
func nullZeroCmp(c Col, op string, v any) Pred {
	zero := isZeroValue(v)
	switch {
	case zero && op == " = ":
		return c.IsNull()
	case zero && (op == " <> " || op == " IS DISTINCT FROM "):
		return c.IsNotNull()
	case op == " <> ":
		return predExpr{e: func(r *render) {
			r.col(c)
			r.s(" IS DISTINCT FROM ")
			r.argCol(v, c, " IS DISTINCT FROM ")
		}}
	}
	return nil
}

// nullZeroAny is col = ANY(list) or col <> ALL(list) on a nullzero column: In matches the NULL rows
// too when list holds the zero value, and NotIn keeps them when it does not.
func nullZeroAny(c Col, list any, not bool) Pred {
	zero := containsZero(list)
	if zero == not {
		return nil
	}
	return predExpr{e: func(r *render) {
		r.b('(')
		r.col(c)
		if not {
			r.s(" <> ALL(")
		} else {
			r.s(" = ANY(")
		}
		r.arg(list)
		r.s(") OR ")
		r.col(c)
		r.s(" IS NULL)")
	}}
}
