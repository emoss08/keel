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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// unnestChunk is the number of rows per INSERT … SELECT FROM unnest statement. Every chunk shares
// one statement text, so it is prepared once whatever the row count.
const unnestChunk = 10_000

// columnTypes caches the SQL type of every column of a table, read from the catalog once per DB.
type columnTypes struct {
	mu sync.Mutex
	m  map[*schema.Table][]columnType
}

// columnType is the SQL type of a column (format_type) and, for an extension's base type whose
// codec and array codec the connections register (Config.AfterConnect: PostGIS geometry, pgvector),
// the name of its array type: bulk inserts send such a column as an array of driver values that
// the registered codec encodes, instead of as text.
type columnType struct {
	sql   string
	array string
}

// firstUserOID is the first OID Postgres gives objects created after initdb (FirstNormalObjectId):
// types from extensions and migrations, never the built-in ones pgx knows.
const firstUserOID = 16384

// columnTypes returns the SQL type of each of t's columns (format_type), reading the catalog on
// first use, on a connection whose type map tells which extension types have codecs.
func (db *DB) columnTypes(ctx context.Context, t *schema.Table) ([]columnType, error) {
	db.types.mu.Lock()
	types, ok := db.types.m[t]
	db.types.mu.Unlock()
	if ok {
		return types, nil
	}
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("keel: read column types of %s: %w", t.Name, err)
	}
	defer conn.Release()
	byName, err := readColumnTypes(ctx, conn.Conn(), t)
	if err != nil {
		return nil, err
	}
	types = make([]columnType, len(t.Columns))
	for i, c := range t.Columns {
		if !c.Stored() {
			continue
		}
		typ, ok := byName[c.Name]
		if !ok {
			return nil, errf("table %s has no column %s; run migrations or regenerate", t.Name, c.Name)
		}
		if c.Kind != schema.KindOther {
			typ.array = ""
		}
		types[i] = typ
	}
	db.types.mu.Lock()
	if db.types.m == nil {
		db.types.m = make(map[*schema.Table][]columnType)
	}
	db.types.m[t] = types
	db.types.mu.Unlock()
	return types, nil
}

// readColumnTypes reads the types of t's columns from the catalog, by name, with the array type of
// each extension base type that conn has a codec for, and for its array.
func readColumnTypes(ctx context.Context, conn *pgx.Conn, t *schema.Table) (map[string]columnType, error) {
	rows, err := conn.Query(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.atttypid, ty.typtype,
	       ty.typarray, coalesce(format_type(ty.typarray, NULL), '')
	  FROM pg_attribute a
	  JOIN pg_type ty ON ty.oid = a.atttypid
	 WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped`, t.Quoted)
	if err != nil {
		return nil, fmt.Errorf("keel: read column types of %s: %w", t.Name, err)
	}
	defer rows.Close()
	m := conn.TypeMap()
	byName := map[string]columnType{}
	for rows.Next() {
		var name, typ, kind, array string
		var oid, arrayOID uint32
		if err := rows.Scan(&name, &typ, &oid, &kind, &arrayOID, &array); err != nil {
			return nil, err
		}
		ct := columnType{sql: typ}
		if kind == "b" && oid >= firstUserOID && arrayOID != 0 && registered(m, oid) && registered(m, arrayOID) {
			ct.array = array
		}
		byName[name] = ct
	}
	return byName, rows.Err()
}

// registered reports whether m has a codec for the type oid.
func registered(m *pgtype.Map, oid uint32) bool {
	_, ok := m.TypeForOID(oid)
	return ok
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

// buildColumn extracts one column of rows into a keel array parameter, reading through the
// generated field pointers so no value is boxed. Values Postgres has no native array for (enums,
// JSON, numeric, arrays, custom types) travel as text and are cast to the column's type, except
// custom types whose codec the connections register, which travel as an array of their values for
// that codec to encode.
func (db *DB) buildColumn(t *schema.Table, col int32, ct columnType, rows []unsafe.Pointer) (columnArray, error) {
	c := &t.Columns[col]
	sqlType := ct.sql
	if ct.array != "" {
		vals := make([]any, len(rows))
		for i, row := range rows {
			vals[i] = t.Value(row, col)
		}
		return columnArray{arg: vals, param: ct.array}, nil
	}
	n := nullability(c)
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
		return column[string, textArray, nullTextArray](t, col, rows, n, "text[]", cast), nil
	case schema.KindInt64:
		return column[int64, int8Array, nullInt8Array](t, col, rows, n, "int8[]", native("bigint")), nil
	case schema.KindInt32:
		return column[int32, int4Array, nullInt4Array](t, col, rows, n, "int4[]", native("integer")), nil
	case schema.KindInt16:
		return column[int16, int2Array, nullInt2Array](t, col, rows, n, "int2[]", native("smallint")), nil
	case schema.KindFloat64:
		return column[float64, float8Array, nullFloat8Array](t, col, rows, n, "float8[]",
			native("double precision")), nil
	case schema.KindFloat32:
		return column[float32, float4Array, nullFloat4Array](t, col, rows, n, "float4[]",
			native("real")), nil
	case schema.KindBool:
		return column[bool, boolArray, nullBoolArray](t, col, rows, n, "bool[]", native("boolean")), nil
	case schema.KindTime:
		return timeColumn(t, col, rows, n, native("timestamp with time zone")), nil
	case schema.KindBytes:
		return columnArray{arg: valColumn[[]byte, byteaArray](t, col, rows), param: "bytea[]"}, nil
	}
	vals := make([]string, len(rows))
	out := make(nullTextArray, len(rows))
	for i, row := range rows {
		s, null, err := db.textValue(c, t.Value(row, col))
		if err != nil {
			return columnArray{}, fmt.Errorf("keel: %s.%s: %w", t.Name, c.Name, err)
		}
		if !null {
			vals[i] = s
			out[i] = &vals[i]
		}
	}
	return columnArray{arg: out, param: "text[]", cast: sqlType}, nil
}

// nullKind is how a column of a basic type holds NULL in Go: never, as a nil pointer, or, for a
// nullzero column, as its zero value.
type nullKind uint8

const (
	notNull nullKind = iota
	nullPtr
	nullZero
)

// nullability returns how column c holds NULL in Go.
func nullability(c *schema.Column) nullKind {
	switch {
	case c.Flags.Has(schema.FlagNullable):
		return nullPtr
	case c.Flags.Has(schema.FlagNullZero):
		return nullZero
	}
	return notNull
}

// column reads a column of a basic type into its keel array parameter: A for a non-nullable
// column, P, whose nil elements bind as NULL, for a nullable or nullzero one.
func column[V comparable, A ~[]V, P ~[]*V](t *schema.Table, col int32, rows []unsafe.Pointer, n nullKind,
	param, cast string) columnArray {
	switch n {
	case nullPtr:
		return columnArray{arg: ptrColumn[V, P](t, col, rows), param: param, cast: cast}
	case nullZero:
		return columnArray{arg: zeroColumn[V, P](t, col, rows), param: param, cast: cast}
	}
	return columnArray{arg: valColumn[V, A](t, col, rows), param: param, cast: cast}
}

// timeColumn reads a time column into its keel array parameter. A nullzero column's zero times
// bind as NULL; time.Time's zero is tested with IsZero, so a zero time in any location counts.
func timeColumn(t *schema.Table, col int32, rows []unsafe.Pointer, n nullKind, cast string) columnArray {
	const param = "timestamptz[]"
	switch n {
	case nullPtr:
		return columnArray{arg: ptrColumn[time.Time, nullTimestamptzArray](t, col, rows), param: param, cast: cast}
	case nullZero:
		out := make(nullTimestamptzArray, len(rows))
		for i, row := range rows {
			if p := t.Ptr(row, col).(*time.Time); !p.IsZero() {
				out[i] = p
			}
		}
		return columnArray{arg: out, param: param, cast: cast}
	}
	return columnArray{arg: valColumn[time.Time, timestamptzArray](t, col, rows), param: param, cast: cast}
}

// zeroColumn reads a nullzero column into a slice of pointers to the rows' fields, nil where a
// field holds its zero value, so zero values bind as NULL without copying a value.
func zeroColumn[V comparable, P ~[]*V](t *schema.Table, col int32, rows []unsafe.Pointer) P {
	var zero V
	out := make(P, len(rows))
	for i, row := range rows {
		if p := t.Ptr(row, col).(*V); *p != zero {
			out[i] = p
		}
	}
	return out
}

// valColumn reads a non-nullable column into a slice of its basic type.
func valColumn[V any, A ~[]V](t *schema.Table, col int32, rows []unsafe.Pointer) A {
	out := make(A, len(rows))
	for i, row := range rows {
		out[i] = *t.Ptr(row, col).(*V)
	}
	return out
}

// ptrColumn reads a nullable column into a slice of pointers.
func ptrColumn[V any, P ~[]*V](t *schema.Table, col int32, rows []unsafe.Pointer) P {
	out := make(P, len(rows))
	for i, row := range rows {
		out[i] = *t.Ptr(row, col).(**V)
	}
	return out
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
func (db *DB) writeUnnestSource(r *render, t *schema.Table, types []columnType, rows []unsafe.Pointer) error {
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
