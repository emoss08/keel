package keel

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
)

// unnestChunk is the number of rows per INSERT … SELECT FROM unnest statement. Every chunk shares
// one statement text, so it is prepared once whatever the row count.
const unnestChunk = 10_000

// columnTypes caches the SQL type of every column of a table, read from the catalog once per DB.
type columnTypes struct {
	mu sync.Mutex
	m  map[*schema.Table][]string
}

// columnTypes returns the SQL type of each of t's columns (format_type), reading the catalog on
// first use.
func (db *DB) columnTypes(ctx context.Context, t *schema.Table) ([]string, error) {
	db.types.mu.Lock()
	types, ok := db.types.m[t]
	db.types.mu.Unlock()
	if ok {
		return types, nil
	}
	rows, err := db.pool.Query(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod)
	  FROM pg_attribute a
	 WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped`, t.Quoted)
	if err != nil {
		return nil, fmt.Errorf("keel: read column types of %s: %w", t.Name, err)
	}
	byName := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			rows.Close()
			return nil, err
		}
		byName[name] = typ
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	types = make([]string, len(t.Columns))
	for i, c := range t.Columns {
		typ, ok := byName[c.Name]
		if !ok {
			return nil, errf("table %s has no column %s; run migrations or regenerate", t.Name, c.Name)
		}
		types[i] = typ
	}
	db.types.mu.Lock()
	if db.types.m == nil {
		db.types.m = make(map[*schema.Table][]string)
	}
	db.types.m[t] = types
	db.types.mu.Unlock()
	return types, nil
}

// ForgetColumnTypes drops the cached column types, so the next bulk write reads them again. Call it
// after a migration changes a table while the process runs.
func (db *DB) ForgetColumnTypes() {
	db.types.mu.Lock()
	db.types.m = nil
	db.types.mu.Unlock()
}

// columnArray is one column of a bulk write: the typed array bound as a parameter, the array type
// the parameter is declared as, and the cast to the column's type when the array's element type
// does not convert to it implicitly.
type columnArray struct {
	arg   any
	param string
	cast  string
}

// buildColumn extracts one column of rows into a typed array, reading through the generated field
// pointers so no value is boxed. Values Postgres has no native array for (enums, JSON, numeric,
// arrays, custom types) travel as text and are cast to the column's type.
func (db *DB) buildColumn(t *schema.Table, col int32, sqlType string, rows []unsafe.Pointer) (columnArray, error) {
	c := &t.Columns[col]
	nullable := c.Flags.Has(schema.FlagNullable)
	native := func(own string) string {
		if sqlType == own {
			return ""
		}
		return sqlType
	}
	switch c.Kind {
	case schema.KindString:
		cast := sqlType
		if textFamily(sqlType) {
			cast = ""
		}
		if nullable {
			return columnArray{arg: ptrColumn[string](t, col, rows), param: "text[]", cast: cast}, nil
		}
		return columnArray{arg: valColumn[string](t, col, rows), param: "text[]", cast: cast}, nil
	case schema.KindInt64:
		return numericColumn[int64](t, col, rows, nullable, "int8[]", native("bigint")), nil
	case schema.KindInt32:
		return numericColumn[int32](t, col, rows, nullable, "int4[]", native("integer")), nil
	case schema.KindInt16:
		return numericColumn[int16](t, col, rows, nullable, "int2[]", native("smallint")), nil
	case schema.KindFloat64:
		return numericColumn[float64](t, col, rows, nullable, "float8[]", native("double precision")), nil
	case schema.KindFloat32:
		return numericColumn[float32](t, col, rows, nullable, "float4[]", native("real")), nil
	case schema.KindBool:
		return numericColumn[bool](t, col, rows, nullable, "bool[]", native("boolean")), nil
	case schema.KindTime:
		return numericColumn[time.Time](t, col, rows, nullable, "timestamptz[]",
			native("timestamp with time zone")), nil
	case schema.KindBytes:
		out := make([][]byte, len(rows))
		for i, row := range rows {
			out[i] = *t.Ptr(row, col).(*[]byte)
		}
		return columnArray{arg: out, param: "bytea[]"}, nil
	}
	out := make([]*string, len(rows))
	for i, row := range rows {
		s, null, err := db.textValue(c, t.Value(row, col))
		if err != nil {
			return columnArray{}, fmt.Errorf("keel: %s.%s: %w", t.Name, c.Name, err)
		}
		if !null {
			out[i] = &s
		}
	}
	return columnArray{arg: out, param: "text[]", cast: sqlType}, nil
}

// valColumn reads a non-nullable column into a slice of its basic type.
func valColumn[V any](t *schema.Table, col int32, rows []unsafe.Pointer) []V {
	out := make([]V, len(rows))
	for i, row := range rows {
		out[i] = *t.Ptr(row, col).(*V)
	}
	return out
}

// ptrColumn reads a nullable column into a slice of pointers; nil elements bind as NULL.
func ptrColumn[V any](t *schema.Table, col int32, rows []unsafe.Pointer) []*V {
	out := make([]*V, len(rows))
	for i, row := range rows {
		out[i] = *t.Ptr(row, col).(**V)
	}
	return out
}

func numericColumn[V any](t *schema.Table, col int32, rows []unsafe.Pointer, nullable bool, param, cast string) columnArray {
	if nullable {
		return columnArray{arg: ptrColumn[V](t, col, rows), param: param, cast: cast}
	}
	return columnArray{arg: valColumn[V](t, col, rows), param: param, cast: cast}
}

// textFamily reports SQL types a text value converts to by assignment, so the insert needs no
// explicit cast; an explicit cast to varchar(n) would truncate instead of failing.
func textFamily(sqlType string) bool {
	switch {
	case sqlType == "text", sqlType == "name", sqlType == "citext",
		strings.HasPrefix(sqlType, "character varying"), strings.HasPrefix(sqlType, "character("),
		sqlType == "character":
		return true
	}
	return false
}

// textValue renders a value as the text Postgres parses for the column's type. null reports SQL
// NULL.
func (db *DB) textValue(c *schema.Column, v any) (s string, null bool, err error) {
	if v == nil {
		return "", true, nil
	}
	if c.Kind == schema.KindJSON {
		if b, ok := v.([]byte); ok {
			return string(b), false, nil
		}
		b, err := db.marshalJSON(v)
		return string(b), false, err
	}
	switch x := v.(type) {
	case []string:
		return stringArrayLiteral(x), false, nil
	case []int64:
		return intArrayLiteral(x), false, nil
	}
	if dv, ok := v.(driver.Valuer); ok {
		val, err := dv.Value()
		if err != nil {
			return "", false, err
		}
		if val == nil {
			return "", true, nil
		}
		v = val
	}
	switch x := v.(type) {
	case string:
		return x, false, nil
	case []byte:
		return string(x), false, nil
	case int64:
		return strconv.FormatInt(x, 10), false, nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), false, nil
	case bool:
		return strconv.FormatBool(x), false, nil
	case time.Time:
		return x.Format(time.RFC3339Nano), false, nil
	}
	return fmt.Sprint(v), false, nil
}

func (db *DB) marshalJSON(v any) ([]byte, error) {
	if db.cfg.JSON != nil {
		return db.cfg.JSON.Marshal(v)
	}
	return json.Marshal(v)
}

// stringArrayLiteral renders a text array literal with every element quoted.
func stringArrayLiteral(xs []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, x := range xs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for j := 0; j < len(x); j++ {
			if x[j] == '"' || x[j] == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(x[j])
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func intArrayLiteral(xs []int64) string {
	b := make([]byte, 0, 2+len(xs)*8)
	b = append(b, '{')
	for i, x := range xs {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, x, 10)
	}
	return string(append(b, '}'))
}

// writeUnnestSource writes SELECT u.c1[::type], … FROM unnest($1::t1[], …) AS u(c1, …) for the
// insert columns of rows, binding one array per column.
func (db *DB) writeUnnestSource(r *render, t *schema.Table, types []string, rows []unsafe.Pointer) error {
	arrays := make([]columnArray, len(t.Insert))
	for i, c := range t.Insert {
		a, err := db.buildColumn(t, c, types[c], rows)
		if err != nil {
			return err
		}
		arrays[i] = a
	}
	r.s("SELECT ")
	for i, c := range t.Insert {
		if i > 0 {
			r.s(", ")
		}
		r.s("u.")
		r.s(t.Columns[c].Quoted)
		if arrays[i].cast != "" {
			r.s("::")
			r.s(arrays[i].cast)
		}
	}
	r.s(" FROM unnest(")
	for i, a := range arrays {
		if i > 0 {
			r.s(", ")
		}
		r.arg(a.arg)
		r.s("::")
		r.s(a.param)
	}
	r.s(") AS u(")
	for i, c := range t.Insert {
		if i > 0 {
			r.s(", ")
		}
		r.s(t.Columns[c].Quoted)
	}
	r.b(')')
	return nil
}
