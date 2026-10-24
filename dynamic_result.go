package keel

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"iter"
	"math"
	"strconv"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// ValueType is the type of a column of a dynamic result, and a type Cast accepts.
type ValueType uint8

const (
	// TypeString is text, varchar, char, name, enums, uuid and any type read in its text form.
	TypeString ValueType = iota + 1
	// TypeInt64 is smallint, integer, bigint and oid.
	TypeInt64
	// TypeFloat64 is real and double precision.
	TypeFloat64
	// TypeNumeric is numeric, kept exact as a Decimal.
	TypeNumeric
	// TypeBool is boolean.
	TypeBool
	// TypeTime is timestamptz and timestamp, in UTC.
	TypeTime
	// TypeDate is date, as midnight UTC.
	TypeDate
	// TypeBytes is bytea.
	TypeBytes
	// TypeJSON is json, jsonb and every other type (arrays, ranges, intervals), as raw JSON.
	TypeJSON
)

// String returns the type's name.
func (t ValueType) String() string {
	switch t {
	case TypeString:
		return "string"
	case TypeInt64:
		return "int64"
	case TypeFloat64:
		return "float64"
	case TypeNumeric:
		return "numeric"
	case TypeBool:
		return "bool"
	case TypeTime:
		return "time"
	case TypeDate:
		return "date"
	case TypeBytes:
		return "bytes"
	case TypeJSON:
		return "json"
	}
	return "invalid"
}

// sqlType is the SQL type Cast writes for t.
func (t ValueType) sqlType() string {
	switch t {
	case TypeString:
		return "text"
	case TypeInt64:
		return "bigint"
	case TypeFloat64:
		return "double precision"
	case TypeNumeric:
		return "numeric"
	case TypeBool:
		return "boolean"
	case TypeTime:
		return "timestamptz"
	case TypeDate:
		return "date"
	case TypeBytes:
		return "bytea"
	case TypeJSON:
		return "jsonb"
	}
	return ""
}

// Decimal is an exact decimal number in the text form Postgres writes for numeric: "-12.3400",
// "0.5", "NaN", "Infinity", "-Infinity". It keeps every digit and the scale, compares exactly as
// text after normalisation by the application, binds as a numeric parameter, and goes to CSV,
// JSON and spreadsheets without conversion.
type Decimal string

// String returns the decimal's text.
func (d Decimal) String() string { return string(d) }

// Float64 returns the nearest float64, for charts and approximate arithmetic.
func (d Decimal) Float64() (float64, error) { return strconv.ParseFloat(string(d), 64) }

// DynColumn is one column of a dynamic result: its label, its value type and one typed vector,
// the one Type selects, with a zero value at each NULL row. Strings, decimals, bytes and JSON
// share the result's memory instead of allocating per value.
type DynColumn struct {
	Name string
	Type ValueType
	// OID is the column's Postgres type.
	OID uint32
	// Nulls is a bitmap with bit i%64 of word i/64 set when row i is NULL; it is shorter than the
	// rows when the last rows are not NULL, and empty when none is.
	Nulls []uint64

	String  []string
	Int64   []int64
	Float64 []float64
	Numeric []Decimal
	Bool    []bool
	Time    []time.Time
	Bytes   [][]byte
	JSON    [][]byte

	decode dynDecoder
}

// IsNull reports whether row i is NULL.
func (c *DynColumn) IsNull(i int) bool {
	w := i / 64
	return w < len(c.Nulls) && c.Nulls[w]&(1<<(uint(i)%64)) != 0
}

// Value returns row i boxed: nil for NULL, else int64, float64, Decimal, string, bool, time.Time
// or []byte (bytes and JSON). The vectors are the fast path; Value is for code that handles a
// few values generically.
func (c *DynColumn) Value(i int) any {
	if c.IsNull(i) {
		return nil
	}
	switch c.Type {
	case TypeString:
		return c.String[i]
	case TypeInt64:
		return c.Int64[i]
	case TypeFloat64:
		return c.Float64[i]
	case TypeNumeric:
		return c.Numeric[i]
	case TypeBool:
		return c.Bool[i]
	case TypeTime, TypeDate:
		return c.Time[i]
	case TypeBytes:
		return c.Bytes[i]
	case TypeJSON:
		return c.JSON[i]
	}
	return nil
}

// null appends a NULL at row i.
func (c *DynColumn) null(i int) {
	w := i / 64
	for len(c.Nulls) <= w {
		c.Nulls = append(c.Nulls, 0)
	}
	c.Nulls[w] |= 1 << (uint(i) % 64)
	switch c.Type {
	case TypeString:
		c.String = append(c.String, "")
	case TypeInt64:
		c.Int64 = append(c.Int64, 0)
	case TypeFloat64:
		c.Float64 = append(c.Float64, 0)
	case TypeNumeric:
		c.Numeric = append(c.Numeric, "")
	case TypeBool:
		c.Bool = append(c.Bool, false)
	case TypeTime, TypeDate:
		c.Time = append(c.Time, time.Time{})
	case TypeBytes:
		c.Bytes = append(c.Bytes, nil)
	case TypeJSON:
		c.JSON = append(c.JSON, nil)
	}
}

// reserve sizes the column's vector for n rows.
func (c *DynColumn) reserve(n int) {
	switch c.Type {
	case TypeString:
		c.String = make([]string, 0, n)
	case TypeInt64:
		c.Int64 = make([]int64, 0, n)
	case TypeFloat64:
		c.Float64 = make([]float64, 0, n)
	case TypeNumeric:
		c.Numeric = make([]Decimal, 0, n)
	case TypeBool:
		c.Bool = make([]bool, 0, n)
	case TypeTime, TypeDate:
		c.Time = make([]time.Time, 0, n)
	case TypeBytes:
		c.Bytes = make([][]byte, 0, n)
	case TypeJSON:
		c.JSON = make([][]byte, 0, n)
	}
}

// truncate keeps the first n rows.
func (c *DynColumn) truncate(n int) {
	c.String = clipTo(c.String, n)
	c.Int64 = clipTo(c.Int64, n)
	c.Float64 = clipTo(c.Float64, n)
	c.Numeric = clipTo(c.Numeric, n)
	c.Bool = clipTo(c.Bool, n)
	c.Time = clipTo(c.Time, n)
	c.Bytes = clipTo(c.Bytes, n)
	c.JSON = clipTo(c.JSON, n)
	if w := (n + 63) / 64; len(c.Nulls) > w {
		c.Nulls = c.Nulls[:w]
	}
	if n%64 != 0 && len(c.Nulls) == (n+63)/64 {
		c.Nulls[len(c.Nulls)-1] &= 1<<(uint(n)%64) - 1
	}
}

func clipTo[V any](s []V, n int) []V {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// DynResult is the typed columnar result of a dynamic query: one DynColumn per selected
// expression, in order, and the number of rows. Next is the cursor of the following page after
// Page, empty on the last page.
type DynResult struct {
	Columns []DynColumn
	Rows    int
	Next    string

	arena dynArena
}

// Column returns the column labelled name, or nil.
func (res *DynResult) Column(name string) *DynColumn {
	for i := range res.Columns {
		if res.Columns[i].Name == name {
			return &res.Columns[i]
		}
	}
	return nil
}

// dynArena hands out byte slices from chunks that are never reused, so strings made over them
// stay valid for the life of the result.
type dynArena struct {
	buf     []byte
	next    int
	scratch []byte
}

func (a *dynArena) alloc(n int) []byte {
	if cap(a.buf)-len(a.buf) < n {
		a.next = min(max(a.next*2, 4<<10), 1<<20)
		a.buf = make([]byte, 0, max(a.next, n))
	}
	start := len(a.buf)
	a.buf = a.buf[:start+n]
	return a.buf[start : start+n : start+n]
}

func (a *dynArena) bytes(b []byte) []byte {
	d := a.alloc(len(b))
	copy(d, b)
	return d
}

func (a *dynArena) str(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	d := a.bytes(b)
	return unsafe.String(unsafe.SliceData(d), len(d))
}

// dynDecoder decodes one non-NULL value of a column and appends it to the column's vector.
type dynDecoder func(c *DynColumn, src []byte, a *dynArena) error

// dynDecoderFor chooses the value type and decoder of a result column from its type's codec, so
// domains decode as their base type and registered enums as strings.
func dynDecoderFor(db *DB, m *pgtype.Map, fd pgconn.FieldDescription) (ValueType, dynDecoder) {
	text := fd.Format == pgtype.TextFormatCode
	t, ok := m.TypeForOID(fd.DataTypeOID)
	if !ok {
		if text {
			return TypeString, decodeText
		}
		return TypeBytes, decodeBytes
	}
	switch t.Codec.(type) {
	case pgtype.Int2Codec, pgtype.Int4Codec, pgtype.Int8Codec, pgtype.Uint32Codec:
		if text {
			return TypeInt64, decodeIntText
		}
		return TypeInt64, decodeIntBinary
	case pgtype.Float4Codec, pgtype.Float8Codec:
		if text {
			return TypeFloat64, decodeFloatText
		}
		return TypeFloat64, decodeFloatBinary
	case pgtype.NumericCodec:
		if text {
			return TypeNumeric, decodeNumericText
		}
		return TypeNumeric, decodeNumericBinary
	case pgtype.BoolCodec:
		if text {
			return TypeBool, decodeBoolText
		}
		return TypeBool, decodeBoolBinary
	case *pgtype.TimestamptzCodec, *pgtype.TimestampCodec:
		if text {
			return TypeTime, decodeTimeText(m, fd.DataTypeOID)
		}
		return TypeTime, decodeTimestampBinary
	case pgtype.DateCodec:
		if text {
			return TypeDate, decodeTimeText(m, fd.DataTypeOID)
		}
		return TypeDate, decodeDateBinary
	case pgtype.ByteaCodec:
		if text {
			return TypeBytes, decodeByteaText
		}
		return TypeBytes, decodeBytes
	case *pgtype.JSONCodec:
		return TypeJSON, decodeJSON
	case *pgtype.JSONBCodec:
		if text {
			return TypeJSON, decodeJSON
		}
		return TypeJSON, decodeJSONBBinary
	case pgtype.UUIDCodec:
		if text {
			return TypeString, decodeText
		}
		return TypeString, decodeUUIDBinary
	case pgtype.TextCodec, *pgtype.EnumCodec:
		return TypeString, decodeText
	}
	if text {
		return TypeString, decodeText
	}
	return TypeJSON, decodeOther(db, m, t.Codec, fd.DataTypeOID)
}

func decodeText(c *DynColumn, src []byte, a *dynArena) error {
	c.String = append(c.String, a.str(src))
	return nil
}

func decodeBytes(c *DynColumn, src []byte, a *dynArena) error {
	c.Bytes = append(c.Bytes, a.bytes(src))
	return nil
}

func decodeIntBinary(c *DynColumn, src []byte, _ *dynArena) error {
	var v int64
	switch len(src) {
	case 2:
		v = int64(int16(binary.BigEndian.Uint16(src)))
	case 4:
		v = int64(int32(binary.BigEndian.Uint32(src)))
	case 8:
		v = int64(binary.BigEndian.Uint64(src))
	default:
		return errf("invalid integer of %d bytes", len(src))
	}
	c.Int64 = append(c.Int64, v)
	return nil
}

func decodeIntText(c *DynColumn, src []byte, _ *dynArena) error {
	v, err := strconv.ParseInt(string(src), 10, 64)
	if err != nil {
		return err
	}
	c.Int64 = append(c.Int64, v)
	return nil
}

func decodeFloatBinary(c *DynColumn, src []byte, _ *dynArena) error {
	switch len(src) {
	case 4:
		c.Float64 = append(c.Float64, float64(math.Float32frombits(binary.BigEndian.Uint32(src))))
	case 8:
		c.Float64 = append(c.Float64, math.Float64frombits(binary.BigEndian.Uint64(src)))
	default:
		return errf("invalid float of %d bytes", len(src))
	}
	return nil
}

func decodeFloatText(c *DynColumn, src []byte, _ *dynArena) error {
	v, err := strconv.ParseFloat(string(src), 64)
	if err != nil {
		return err
	}
	c.Float64 = append(c.Float64, v)
	return nil
}

func decodeNumericText(c *DynColumn, src []byte, a *dynArena) error {
	c.Numeric = append(c.Numeric, Decimal(a.str(src)))
	return nil
}

func decodeNumericBinary(c *DynColumn, src []byte, a *dynArena) error {
	var err error
	a.scratch, err = appendNumeric(a.scratch[:0], src)
	if err != nil {
		return err
	}
	c.Numeric = append(c.Numeric, Decimal(a.str(a.scratch)))
	return nil
}

// appendNumeric appends the text form of a numeric in Postgres's binary format: ndigits, weight,
// sign and display scale as 16-bit integers, then ndigits base-10000 digits.
func appendNumeric(dst, src []byte) ([]byte, error) {
	if len(src) < 8 {
		return dst, errf("invalid numeric of %d bytes", len(src))
	}
	ndigits := int(binary.BigEndian.Uint16(src))
	weight := int(int16(binary.BigEndian.Uint16(src[2:])))
	sign := binary.BigEndian.Uint16(src[4:])
	dscale := int(binary.BigEndian.Uint16(src[6:]))
	if len(src) != 8+2*ndigits {
		return dst, errf("invalid numeric of %d bytes for %d digits", len(src), ndigits)
	}
	switch sign {
	case 0xC000:
		return append(dst, "NaN"...), nil
	case 0xD000:
		return append(dst, "Infinity"...), nil
	case 0xF000:
		return append(dst, "-Infinity"...), nil
	case 0x4000:
		dst = append(dst, '-')
	}
	digit := func(i int) int {
		if i < 0 || i >= ndigits {
			return 0
		}
		return int(binary.BigEndian.Uint16(src[8+2*i:]))
	}
	if weight < 0 {
		dst = append(dst, '0')
	} else {
		dst = strconv.AppendInt(dst, int64(digit(0)), 10)
		for i := 1; i <= weight; i++ {
			dst = appendDigits(dst, digit(i), 4)
		}
	}
	if dscale > 0 {
		dst = append(dst, '.')
		for i, left := weight+1, dscale; left > 0; i++ {
			n := min(left, 4)
			dst = appendDigits(dst, digit(i)/pow10[4-n], n)
			left -= n
		}
	}
	return dst, nil
}

var pow10 = [5]int{1, 10, 100, 1000, 10000}

// appendDigits appends v as exactly n decimal digits, zero-padded.
func appendDigits(dst []byte, v, n int) []byte {
	for i := n - 1; i >= 0; i-- {
		dst = append(dst, byte('0'+v/pow10[i]%10))
	}
	return dst
}

func decodeBoolBinary(c *DynColumn, src []byte, _ *dynArena) error {
	if len(src) != 1 {
		return errf("invalid boolean of %d bytes", len(src))
	}
	c.Bool = append(c.Bool, src[0] != 0)
	return nil
}

func decodeBoolText(c *DynColumn, src []byte, _ *dynArena) error {
	c.Bool = append(c.Bool, len(src) > 0 && src[0] == 't')
	return nil
}

// pgEpoch is 2000-01-01 00:00:00 UTC, the origin of Postgres's binary dates and timestamps, in
// Unix seconds.
const pgEpoch = 946684800

func decodeTimestampBinary(c *DynColumn, src []byte, _ *dynArena) error {
	if len(src) != 8 {
		return errf("invalid timestamp of %d bytes", len(src))
	}
	us := int64(binary.BigEndian.Uint64(src))
	if us == math.MaxInt64 || us == math.MinInt64 {
		return errf("infinite timestamps are not supported in dynamic results")
	}
	sec, rem := us/1_000_000, us%1_000_000
	if rem < 0 {
		sec, rem = sec-1, rem+1_000_000
	}
	c.Time = append(c.Time, time.Unix(pgEpoch+sec, rem*1000).UTC())
	return nil
}

func decodeDateBinary(c *DynColumn, src []byte, _ *dynArena) error {
	if len(src) != 4 {
		return errf("invalid date of %d bytes", len(src))
	}
	days := int32(binary.BigEndian.Uint32(src))
	if days == math.MaxInt32 || days == math.MinInt32 {
		return errf("infinite dates are not supported in dynamic results")
	}
	c.Time = append(c.Time, time.Date(2000, 1, 1+int(days), 0, 0, 0, 0, time.UTC))
	return nil
}

// decodeTimeText decodes a date or timestamp sent as text, through pgx's scan plan.
func decodeTimeText(m *pgtype.Map, oid uint32) dynDecoder {
	var t time.Time
	plan := m.PlanScan(oid, pgtype.TextFormatCode, &t)
	return func(c *DynColumn, src []byte, _ *dynArena) error {
		if err := plan.Scan(src, &t); err != nil {
			return err
		}
		c.Time = append(c.Time, t.UTC())
		return nil
	}
}

func decodeByteaText(c *DynColumn, src []byte, a *dynArena) error {
	if len(src) < 2 || src[0] != '\\' || src[1] != 'x' {
		return errf("bytea text is not in hex format")
	}
	d := a.alloc(hex.DecodedLen(len(src) - 2))
	if _, err := hex.Decode(d, src[2:]); err != nil {
		return err
	}
	c.Bytes = append(c.Bytes, d)
	return nil
}

func decodeJSON(c *DynColumn, src []byte, a *dynArena) error {
	c.JSON = append(c.JSON, a.bytes(src))
	return nil
}

func decodeJSONBBinary(c *DynColumn, src []byte, a *dynArena) error {
	if len(src) == 0 || src[0] != 1 {
		return errf("unknown jsonb binary version")
	}
	c.JSON = append(c.JSON, a.bytes(src[1:]))
	return nil
}

func decodeUUIDBinary(c *DynColumn, src []byte, a *dynArena) error {
	if len(src) != 16 {
		return errf("invalid uuid of %d bytes", len(src))
	}
	d := a.alloc(36)
	hex.Encode(d[0:8], src[0:4])
	d[8] = '-'
	hex.Encode(d[9:13], src[4:6])
	d[13] = '-'
	hex.Encode(d[14:18], src[6:8])
	d[18] = '-'
	hex.Encode(d[19:23], src[8:10])
	d[23] = '-'
	hex.Encode(d[24:], src[10:16])
	c.String = append(c.String, unsafe.String(unsafe.SliceData(d), len(d)))
	return nil
}

// decodeOther decodes a type the columnar path has no vector for (arrays, ranges, intervals)
// through its pgx codec and stores it as JSON.
func decodeOther(db *DB, m *pgtype.Map, codec pgtype.Codec, oid uint32) dynDecoder {
	return func(c *DynColumn, src []byte, a *dynArena) error {
		v, err := codec.DecodeValue(m, oid, pgtype.BinaryFormatCode, src)
		if err != nil {
			return err
		}
		b, err := db.marshalJSON(v)
		if err != nil {
			return err
		}
		c.JSON = append(c.JSON, a.bytes(b))
		return nil
	}
}

// dynReader decodes the rows of a dynamic statement into DynResults: labels name the visible
// columns, and columns past them are hidden ones keel added for a keyset page.
type dynReader struct {
	db     *DB
	labels []string
	hint   int
	types  []ValueType
	decs   []dynDecoder
}

// start plans the decoding of a statement's columns.
func (rd *dynReader) start(rows pgx.Rows) error {
	fds := rows.FieldDescriptions()
	if len(fds) < len(rd.labels) {
		return errf("dynamic statement returned %d columns, expected %d", len(fds), len(rd.labels))
	}
	m := rows.Conn().TypeMap()
	rd.types = make([]ValueType, len(fds))
	rd.decs = make([]dynDecoder, len(fds))
	for i, fd := range fds {
		rd.types[i], rd.decs[i] = dynDecoderFor(rd.db, m, fd)
	}
	return nil
}

// result returns an empty result with the statement's columns.
func (rd *dynReader) result(fds []pgconn.FieldDescription) *DynResult {
	res := &DynResult{Columns: make([]DynColumn, len(fds))}
	for i, fd := range fds {
		c := &res.Columns[i]
		if i < len(rd.labels) {
			c.Name = rd.labels[i]
		}
		c.Type, c.OID, c.decode = rd.types[i], fd.DataTypeOID, rd.decs[i]
		c.reserve(rd.hint)
	}
	return res
}

// add decodes one row into res.
func (res *DynResult) add(raw [][]byte) error {
	row := res.Rows
	for i := range res.Columns {
		c := &res.Columns[i]
		if raw[i] == nil {
			c.null(row)
			continue
		}
		if err := c.decode(c, raw[i], &res.arena); err != nil {
			return errf("decode column %d (%s): %w", i+1, c.Name, err)
		}
	}
	res.Rows++
	return nil
}

// hide drops the columns past the first n.
func (res *DynResult) hide(n int) {
	if len(res.Columns) > n {
		res.Columns = res.Columns[:n]
	}
}

// dynStatement renders q for run, as a page when pg is set, and returns the statement and the
// labels of its visible columns.
func (q *DynQuery) dynStatement(ctx context.Context, run *runner, pg *dynPage) (*stmt, []string, error) {
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	outs := q.outputs()
	labels := make([]string, len(outs))
	for i, o := range outs {
		labels[i] = o.Label()
	}
	if pg != nil {
		q.writePage(r, pg)
	} else {
		q.writeSelect(r, nil)
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	table := "dynamic"
	if t := q.Table(); t != nil {
		table = t.Name
	}
	return &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: table,
		customPlan: q.q.customPlan}, labels, nil
}

// rowHint is the number of rows a dynamic statement is expected to return.
func (q *DynQuery) rowHint() int {
	if q.q.limit > 0 {
		return int(min(q.q.limit, maxSlab))
	}
	return 64
}

// DynInto reads the rows of a dynamic query into dst, as Run does, as one item of a Batch: a
// report and its row count or dynamic pivot values in one round trip.
func DynInto(q *DynQuery, dst **DynResult) BatchItem {
	if dst == nil {
		return noDestination("DynInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		if q == nil {
			return nil, errf("DynInto: nil dynamic query")
		}
		s, labels, err := q.dynStatement(ctx, run, nil)
		if err != nil {
			return nil, err
		}
		rdr := &dynReader{db: run.db, labels: labels, hint: q.rowHint()}
		var res *DynResult
		rd := &read{stmts: []*stmt{s}, timeout: q.q.timeout}
		rd.finish = func() { *dst = res }
		s.scan = func(rows pgx.Rows) error {
			if err := rdr.start(rows); err != nil {
				return err
			}
			res = rdr.result(rows.FieldDescriptions())
			for rows.Next() {
				if err := res.add(rows.RawValues()); err != nil {
					return err
				}
			}
			rd.rows = res.Rows
			return nil
		}
		return rd, nil
	}}
}

// DynCountInto stores the number of rows the dynamic query returns in dst: SELECT count(*) FROM
// (query).
func DynCountInto(q *DynQuery, dst *int64) BatchItem {
	if dst == nil {
		return noDestination("DynCountInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		if q == nil {
			return nil, errf("DynCountInto: nil dynamic query")
		}
		r := newRender(run.sc, ctx, run.db)
		defer r.recycle()
		r.s("SELECT count(*) FROM (")
		q.writeSelect(r, nil)
		r.s(") c")
		if r.err != nil {
			return nil, r.err
		}
		s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "count", table: "dynamic",
			customPlan: q.q.customPlan}
		var n int64
		s.scan = scanCount(&n)
		return &read{stmts: []*stmt{s}, timeout: q.q.timeout, finish: func() { *dst = n }}, nil
	}}
}

// DynPageInto reads one keyset page of the dynamic query into dst, as Page does.
func DynPageInto(q *DynQuery, p PageRequest, dst **DynResult) BatchItem {
	if dst == nil {
		return noDestination("DynPageInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		if q == nil {
			return nil, errf("DynPageInto: nil dynamic query")
		}
		pg, err := q.pagePlan(p)
		if err != nil {
			return nil, err
		}
		s, labels, err := q.dynStatement(ctx, run, pg)
		if err != nil {
			return nil, err
		}
		rdr := &dynReader{db: run.db, labels: labels, hint: p.size + 1}
		var res *DynResult
		rd := &read{stmts: []*stmt{s}, timeout: q.q.timeout}
		rd.finish = func() { *dst = res }
		s.scan = func(rows pgx.Rows) error {
			if err := rdr.start(rows); err != nil {
				return err
			}
			res = rdr.result(rows.FieldDescriptions())
			for rows.Next() {
				if err := res.add(rows.RawValues()); err != nil {
					return err
				}
			}
			rd.rows = res.Rows
			return pg.finish(res, len(labels))
		}
		return rd, nil
	}}
}

// Run reads every row the query returns into a columnar result.
func (q *DynQuery) Run(ctx context.Context, db *DB) (*DynResult, error) {
	var res *DynResult
	err := Batch(ctx, db, DynInto(q, &res))
	return res, err
}

// Count returns the number of rows the query returns.
func (q *DynQuery) Count(ctx context.Context, db *DB) (int64, error) {
	var n int64
	err := Batch(ctx, db, DynCountInto(q, &n))
	return n, err
}

// Page reads the page of rows after the request's cursor, in the query's order. Every order term
// must be a selected expression or one keel can add as a hidden column; a query on a table that
// neither groups nor is DISTINCT gets its primary key appended to the order, so each row has a
// unique position. Grouped and derived queries must order by expressions that are unique
// together. The result's Next is the cursor of the following page.
func (q *DynQuery) Page(ctx context.Context, db *DB, p PageRequest) (*DynResult, error) {
	var res *DynResult
	err := Batch(ctx, db, DynPageInto(q, p, &res))
	return res, err
}

// errStreamStopped ends the decoding of a stream whose consumer stopped iterating.
var errStreamStopped = errors.New("keel: stream stopped")

// Stream runs the query once and yields its rows in batches of size rows, decoding each batch as
// the rows arrive, so an export of any size holds one batch in memory. Each batch is a separate
// DynResult the consumer may keep. A query without rows yields one empty batch, so the columns
// are known. Breaking out of the loop stops the query: outside a transaction keel cancels it;
// inside one the driver reads and discards the remaining rows. The loop body must not run
// statements on the transaction the stream reads from.
func (q *DynQuery) Stream(ctx context.Context, db *DB, size int) iter.Seq2[*DynResult, error] {
	return func(yield func(*DynResult, error) bool) {
		if size <= 0 {
			yield(nil, errf("Stream needs a positive batch size"))
			return
		}
		if q == nil {
			yield(nil, errf("Stream: nil dynamic query"))
			return
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		inTx := InTx(ctx, db)
		stopped := false
		item := BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
			s, labels, err := q.dynStatement(ctx, run, nil)
			if err != nil {
				return nil, err
			}
			rdr := &dynReader{db: run.db, labels: labels, hint: size}
			s.scan = func(rows pgx.Rows) error {
				if err := rdr.start(rows); err != nil {
					return err
				}
				fds := rows.FieldDescriptions()
				res := rdr.result(fds)
				yielded := false
				for rows.Next() {
					if err := res.add(rows.RawValues()); err != nil {
						return err
					}
					if res.Rows == size {
						yielded = true
						if !yield(res, nil) {
							return stop(&stopped, inTx, cancel)
						}
						res = rdr.result(fds)
					}
				}
				if (res.Rows > 0 || !yielded) && rows.Err() == nil {
					if !yield(res, nil) {
						return stop(&stopped, inTx, cancel)
					}
				}
				return nil
			}
			return &read{stmts: []*stmt{s}, timeout: q.q.timeout, finish: func() {}}, nil
		}}
		if err := Batch(ctx, db, item); err != nil && !stopped {
			yield(nil, err)
		}
	}
}

// stop records that a stream's consumer stopped and, outside a transaction, cancels the query so
// the driver does not read the remaining rows.
func stop(stopped *bool, inTx bool, cancel context.CancelFunc) error {
	*stopped = true
	if !inTx {
		cancel()
	}
	return errStreamStopped
}

// dynPage is the shape of one keyset page of a dynamic query: the hidden outputs it adds, its
// order by output position, the cursor values and the page size.
type dynPage struct {
	hidden []Expr
	order  []pageTerm
	after  []any
	size   int
	width  int
}

// pageTerm is one order term of a page: the output it sorts by, its direction and NULLs place,
// and whether the output is known never to be NULL.
type pageTerm struct {
	out     int
	desc    bool
	nulls   int8
	notNull bool
}

// pagePlan maps the query's order to output positions, adding hidden outputs for order terms
// that are not selected and, on an ungrouped table query, the primary key.
func (q *DynQuery) pagePlan(p PageRequest) (*dynPage, error) {
	if q.err != nil {
		return nil, q.err
	}
	if p.size <= 0 {
		return nil, errf("Page needs a positive size")
	}
	if q.q.offset > 0 || q.q.limit > 0 {
		return nil, errf("Page reads by keyset; remove Limit and Offset")
	}
	outs := q.outputs()
	pg := &dynPage{size: p.size}
	find := func(e Expr) int {
		for i, o := range outs {
			if d, ok := e.(DynExpr); ok && o.same(d) {
				return i
			}
			if c, ok := e.(Col); ok && o.n != nil && o.n.col != nil && *o.n.col == c {
				return i
			}
		}
		for i, h := range pg.hidden {
			if c, ok := e.(Col); ok && h == Expr(c) {
				return len(outs) + i
			}
		}
		pg.hidden = append(pg.hidden, e)
		return len(outs) + len(pg.hidden) - 1
	}
	notNull := func(e Expr) bool {
		var c *Col
		switch x := e.(type) {
		case DynExpr:
			if x.n != nil {
				c = x.n.col
			}
		case Col:
			c = &x
		}
		return c != nil && c.Ref == q.q.ref && q.derived == nil &&
			!c.Ref.Table.Columns[c.Index].MayBeNull()
	}
	desc := false
	for _, o := range q.q.order {
		pg.order = append(pg.order, pageTerm{out: find(o.e), desc: o.desc, nulls: o.nulls,
			notNull: notNull(o.e)})
		desc = o.desc
	}
	unique := len(q.q.group) == 0 && !q.q.distinct && len(q.q.distinctOn) == 0 && q.derived == nil
	if unique {
		for _, pk := range q.q.ref.Table.PK {
			c := Col{Ref: q.q.ref, Index: pk}
			ordered := false
			for _, o := range q.q.order {
				if x, ok := o.e.(DynExpr); ok && x.n != nil && x.n.col != nil && *x.n.col == c {
					ordered = true
				}
				if x, ok := o.e.(Col); ok && x == c {
					ordered = true
				}
			}
			if !ordered {
				pg.order = append(pg.order, pageTerm{out: find(c), desc: desc, notNull: true})
			}
		}
	}
	if len(pg.order) == 0 {
		return nil, errf("Page needs an order")
	}
	pg.width = len(outs) + len(pg.hidden)
	if p.after != "" {
		after, err := decodeCursor(p.after)
		if err != nil {
			return nil, err
		}
		if len(after) != len(pg.order) {
			return nil, errf("cursor does not match the query's order")
		}
		pg.after = after
	}
	return pg, nil
}

// writePage writes SELECT k.c1, … FROM (query) k WHERE keyset ORDER BY … LIMIT size+1.
func (q *DynQuery) writePage(r *render, pg *dynPage) {
	t := &schema.Table{Name: "k", Quoted: `"k"`, Alias: "k"}
	for i := range pg.width {
		t.Columns = append(t.Columns, schema.Column{Name: outputName(i), Quoted: outputName(i),
			Flags: schema.FlagNullable})
	}
	for _, o := range pg.order {
		if o.notNull {
			t.Columns[o.out].Flags = 0
		}
	}
	k := &Ref{Table: t}
	_, ka := r.push(k, joinNone, false)
	defer r.pop()
	orders := make([]Order, len(pg.order))
	for i, o := range pg.order {
		c := Col{Ref: k, Index: int32(o.out)}
		orders[i] = Order{e: c, col: &c, desc: o.desc, nulls: o.nulls}
	}
	r.s("SELECT ")
	for i := range pg.width {
		if i > 0 {
			r.s(", ")
		}
		r.s(ka)
		r.b('.')
		r.s(outputName(i))
	}
	r.s(" FROM (")
	inner := *q
	inner.q.order = nil
	inner.writeSelect(r, pg.hidden)
	r.s(") ")
	r.s(ka)
	if pg.after != nil {
		r.s(" WHERE ")
		writeKeyset(r, orders, pg.after)
	}
	r.s(" ORDER BY ")
	for i, o := range orders {
		if i > 0 {
			r.s(", ")
		}
		o.writeSQL(r)
	}
	r.s(" LIMIT ")
	r.int(int64(pg.size) + 1)
}

// finish trims a page's extra row, sets its cursor from the last row it keeps and drops the hidden
// columns.
func (pg *dynPage) finish(res *DynResult, visible int) error {
	if res.Rows > pg.size {
		last := pg.size - 1
		vals := make([]byte, 0, 64)
		for _, o := range pg.order {
			c := &res.Columns[o.out]
			if c.Type == TypeBytes || c.Type == TypeJSON {
				return errf("Page cannot order by the %s column %d", c.Type, o.out+1)
			}
			vals = appendCursorValue(vals, c.Value(last))
		}
		res.Next = base64.RawURLEncoding.EncodeToString(vals)
		for i := range res.Columns {
			res.Columns[i].truncate(pg.size)
		}
		res.Rows = pg.size
	}
	res.hide(visible)
	return nil
}
