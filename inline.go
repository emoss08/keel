package keel

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emoss08/keel/schema"
)

// InlineSQL renders e as SQL over table, written as alias, with every value it binds written as a
// literal instead of a placeholder. It is for SQL kept outside a statement, where nothing can be
// bound: a trigger, a view, a generated column, a rollup's refresh. It returns the columns of table
// that e reads, in table order, so a caller can watch exactly those for changes.
//
// Every column e reads must belong to table; one of another table, or taken through a relation, is
// an error. Values are written as Postgres reads an untyped literal, so each takes the type of
// what it is compared with, as a bound parameter would: a string or an enum as a quoted literal, a
// number as itself, a time as a timestamptz, a slice as an array literal, a driver.Valuer through
// its Value.
func InlineSQL(e Expr, table *schema.Table, alias string) (string, []int32, error) {
	if e == nil {
		return "", nil, errors.New("keel: InlineSQL of a nil expression")
	}
	if table == nil {
		return "", nil, errors.New("keel: InlineSQL needs the table the expression reads")
	}
	r := &render{buf: make([]byte, 0, 256)}
	read := map[int32]bool{}
	r.seen = func(c Col) {
		if c.Ref != nil && c.Ref.Table == table && !c.Ref.isPath() {
			read[c.Index] = true
			return
		}
		r.fail(fmt.Errorf("keel: InlineSQL over %s reads %s, which is not one of its own columns",
			table.Name, c.Ref.name()))
	}
	r.push(&Ref{Table: table, alias: alias}, joinNone, true)
	e.writeSQL(r)
	r.pop()
	if r.err != nil {
		return "", nil, r.err
	}
	sql, err := inlineArgs(string(r.buf), r.args)
	if err != nil {
		return "", nil, err
	}
	cols := make([]int32, 0, len(read))
	for i := range read {
		cols = append(cols, i)
	}
	slices.Sort(cols)
	return sql, cols, nil
}

// inlineArgs replaces each $n placeholder in sql with args[n-1] as a literal, leaving quoted
// strings and identifiers alone.
func inlineArgs(sql string, args []any) (string, error) {
	if len(args) == 0 {
		return sql, nil
	}
	var b strings.Builder
	b.Grow(len(sql) + 16*len(args))
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		switch {
		case ch == '\'' || ch == '"':
			end := closeQuote(sql, i)
			b.WriteString(sql[i:end])
			i = end - 1
		case ch == '$' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9':
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(sql[i+1 : j])
			if n < 1 || n > len(args) {
				return "", fmt.Errorf("keel: InlineSQL: placeholder $%d has no value", n)
			}
			lit, err := sqlLiteral(args[n-1])
			if err != nil {
				return "", err
			}
			b.WriteString(lit)
			i = j - 1
		default:
			b.WriteByte(ch)
		}
	}
	return b.String(), nil
}

// closeQuote returns the index just past the quote that closes the one at start, a doubled quote
// being part of the text.
func closeQuote(sql string, start int) int {
	q := sql[start]
	for i := start + 1; i < len(sql); i++ {
		if sql[i] != q {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == q {
			i++
			continue
		}
		return i + 1
	}
	return len(sql)
}

// sqlLiteral writes v as a Postgres literal.
func sqlLiteral(v any) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	if t, ok := v.(time.Time); ok {
		return quoteLiteral(t.UTC().Format(time.RFC3339Nano)) + "::timestamptz", nil
	}
	if valuer, ok := v.(driver.Valuer); ok {
		inner, err := valuer.Value()
		if err != nil {
			return "", fmt.Errorf("keel: InlineSQL: %T: %w", v, err)
		}
		return sqlLiteral(inner)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return "NULL", nil
		}
		return sqlLiteral(rv.Elem().Interface())
	case reflect.String:
		return quoteLiteral(rv.String()), nil
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return `'\x` + fmt.Sprintf("%x", rv.Bytes()) + `'::bytea`, nil
		}
		return arrayLiteral(rv)
	}
	text, err := constText(v)
	if err != nil {
		return "", fmt.Errorf("keel: InlineSQL: %T cannot be written as a literal", v)
	}
	return text, nil
}

// quoteLiteral quotes s as an untyped SQL string literal.
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// arrayLiteral writes a slice as an untyped array literal ('{…}'), which takes the array type of
// what it is compared with, as `= ANY($1)` does with a bound slice.
func arrayLiteral(rv reflect.Value) (string, error) {
	parts := make([]string, rv.Len())
	for i := range rv.Len() {
		el := rv.Index(i).Interface()
		if valuer, ok := el.(driver.Valuer); ok {
			inner, err := valuer.Value()
			if err != nil {
				return "", fmt.Errorf("keel: InlineSQL: %T: %w", el, err)
			}
			el = inner
		}
		if el == nil {
			parts[i] = "NULL"
			continue
		}
		ev := reflect.ValueOf(el)
		if ev.Kind() == reflect.String {
			s := strings.ReplaceAll(strings.ReplaceAll(ev.String(), `\`, `\\`), `"`, `\"`)
			parts[i] = `"` + s + `"`
			continue
		}
		text, err := constText(el)
		if err != nil {
			return "", fmt.Errorf("keel: InlineSQL: %T cannot be an array element", el)
		}
		parts[i] = text
	}
	return quoteLiteral("{" + strings.Join(parts, ",") + "}"), nil
}
