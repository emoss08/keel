package keel

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"slices"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// PageRequest asks for one keyset page.
type PageRequest struct {
	after string
	size  int
	total int64
}

// After asks for the page of size rows that follows cursor. An empty cursor asks for the first
// page.
func After(cursor string, size int) PageRequest { return PageRequest{after: cursor, size: size} }

// Total asks for the number of matching rows, counted up to limit, in the same round trip.
func (p PageRequest) Total(limit int64) PageRequest { p.total = limit; return p }

// Page is one keyset page.
type Page[T any] struct {
	Items []T
	// Next is the cursor of the following page, empty on the last page.
	Next string
	// Total is the number of matching rows when the request asked for it, capped at its limit.
	Total int64
	// TotalCapped reports that Total stopped at the limit.
	TotalCapped bool

	keys *pageKeys
}

// CursorAt returns the cursor that resumes the query after Items[i]: After(p.CursorAt(i), n)
// reads the n rows that follow it. It is the cursor format Next uses (Next is the cursor of the
// last item when a page follows), computed from the row's order values without a statement, for
// APIs that give every row a cursor, such as a GraphQL connection's edges. It panics when i is out
// of range, as indexing Items does, and returns "" on a Page that Page did not read.
func (p Page[T]) CursorAt(i int) string {
	row := p.Items[i]
	if p.keys == nil {
		return ""
	}
	return p.keys.cursor(toPtr(row), i)
}

// Page returns the page of rows after the request's cursor. The query's order may name columns of
// its table, columns of to-one relations (belongs, one and pick paths, joined as any read joins
// them) and expressions; keel adds the primary key when the order does not name it, so every row
// has a unique position.
func (q *Query[T]) Page(ctx context.Context, db *DB, p PageRequest) (Page[T], error) {
	var out Page[T]
	err := Batch(ctx, db, PageInto(q, p, &out))
	return out, err
}

// pageRead prepares the read of one keyset page, with its capped count when p asks for one; its
// finish fills dst.
func pageRead[T any](ctx context.Context, run *runner, q *query, p PageRequest, dst *Page[T]) (*read, error) {
	if p.size <= 0 {
		return nil, errf("Page needs a positive size")
	}
	keys, err := q.keyset()
	if err != nil {
		return nil, err
	}
	var after []any
	if p.after != "" {
		after, err = decodeCursor(p.after)
		if err != nil {
			return nil, err
		}
		if len(after) != len(keys.terms) {
			return nil, errf("cursor does not match the query's order")
		}
	}
	saved := q.order
	q.order = keys.orders()
	defer func() { q.order = saved }()

	var total int64
	fo := fetchOpts{extraCols: keys.baseCols(), limit: int64(p.size) + 1}
	if keys.width > 0 {
		fo.tail = keys.tailExprs()
		fo.tailScan = keys.scanTail
		keys.vals = make([]any, 0, (p.size+1)*keys.width)
	}
	if after != nil {
		fo.keyset = func(r *render) { writeKeyset(r, keys.terms, after) }
	}
	if p.total > 0 {
		fo.countCap = p.total
		fo.count = &total
	}
	return fetchInto(ctx, run, q, fo, anyRows, func(items []T) {
		out := Page[T]{Total: total, TotalCapped: p.total > 0 && total >= p.total, keys: keys}
		if len(items) > p.size {
			items = items[:p.size]
			out.Next = keys.cursor(toPtr(items[len(items)-1]), len(items)-1)
		}
		out.Items = items
		*dst = out
	})
}

// keyTerm is one term of a keyset order: its expression, direction and NULLs place, whether its
// value may be NULL, and where a row's value comes from: the base table's column base, read from
// the row, or, when base is -1, the hidden value tail the statement returns after the row's
// columns.
type keyTerm struct {
	e        Expr
	desc     bool
	nulls    int8
	nullable bool
	base     int32
	tail     int
}

// write writes the term's expression as an operand of the keyset predicate: a column as is, any
// other expression in parentheses, so its operators bind before the comparison's.
func (kt keyTerm) write(r *render) {
	if _, ok := kt.e.(columnar); ok {
		kt.e.writeSQL(r)
		return
	}
	r.b('(')
	kt.e.writeSQL(r)
	r.b(')')
}

// pageKeys is the keyset order of one page and the values its rows' cursors are made of: the
// base table's row columns, and width hidden values per row, in row order, for the terms that
// are not columns of the table (relation columns and expressions).
type pageKeys struct {
	table *schema.Table
	terms []keyTerm
	width int
	vals  []any
	plans []pgtype.ScanPlan
}

// keyset returns the query's keyset order ending with the primary key. A term on a column of the
// table reads its value from the row; any other term (a to-one relation's column, a joined
// table's column, an expression) is selected as a hidden value, and may be NULL.
func (q *query) keyset() (*pageKeys, error) {
	t := q.ref.Table
	k := &pageKeys{table: t}
	for _, o := range q.order {
		kt := keyTerm{e: o.e, desc: o.desc, nulls: o.nulls, base: -1}
		if o.col != nil && o.col.Ref == q.ref {
			kt.base = o.col.Index
			kt.nullable = t.Columns[o.col.Index].MayBeNull()
		} else {
			if o.col != nil {
				if err := toOne(o.col.Ref, q.ref); err != nil {
					return nil, err
				}
			}
			kt.nullable = true
			kt.tail = k.width
			k.width++
		}
		k.terms = append(k.terms, kt)
	}
	desc := false
	if len(k.terms) > 0 {
		desc = k.terms[len(k.terms)-1].desc
	}
	for _, pk := range t.PK {
		if !slices.ContainsFunc(k.terms, func(kt keyTerm) bool { return kt.base == pk }) {
			k.terms = append(k.terms, keyTerm{e: Col{Ref: q.ref, Index: pk}, desc: desc, base: pk})
		}
	}
	if len(k.terms) == 0 {
		return nil, errf("Page needs an order or a primary key on %s", t.Name)
	}
	return k, nil
}

// toOne checks that a relation path the page orders by reaches root through to-one relations
// only. A reference that is not a path from root (an alias, a joined table, a CTE) passes; the
// statement resolves it.
func toOne(ref, root *Ref) error {
	for r := ref; r != nil && r != root; r = r.parent {
		if !r.isPath() {
			return nil
		}
		if r.relation().Kind.ToMany() {
			return errf("Page cannot order by %s, a column of a to-many relation", ref.name())
		}
	}
	return nil
}

// orders returns the terms as the statement's ORDER BY.
func (k *pageKeys) orders() []Order {
	out := make([]Order, len(k.terms))
	for i, kt := range k.terms {
		out[i] = Order{e: kt.e, desc: kt.desc, nulls: kt.nulls}
	}
	return out
}

// baseCols returns the base table's columns the order reads from the rows.
func (k *pageKeys) baseCols() []int32 {
	var out []int32
	for _, kt := range k.terms {
		if kt.base >= 0 {
			out = append(out, kt.base)
		}
	}
	return out
}

// tailExprs returns the expressions selected as hidden values, in tail order.
func (k *pageKeys) tailExprs() []Expr {
	out := make([]Expr, 0, k.width)
	for _, kt := range k.terms {
		if kt.base < 0 {
			out = append(out, kt.e)
		}
	}
	return out
}

// scanTail decodes one row's hidden order values into cursor values.
func (k *pageKeys) scanTail(conn *pgx.Conn, fds []pgconn.FieldDescription, raw [][]byte, _ unsafe.Pointer) error {
	if k.plans == nil {
		k.plans = make([]pgtype.ScanPlan, k.width)
	}
	for i, src := range raw {
		if src == nil {
			k.vals = append(k.vals, nil)
			continue
		}
		var v any
		if k.plans[i] == nil {
			k.plans[i] = conn.TypeMap().PlanScan(fds[i].DataTypeOID, fds[i].Format, &v)
		}
		if err := k.plans[i].Scan(src, &v); err != nil {
			return fmt.Errorf("keel: scan page order value %d: %w", i+1, err)
		}
		cv, ok := cursorValue(v)
		if !ok {
			return errf("Page cannot order by a value of type %T", v)
		}
		k.vals = append(k.vals, cv)
	}
	return nil
}

// cursor encodes the order values of row, the i-th row of the page.
func (k *pageKeys) cursor(row unsafe.Pointer, i int) string {
	b := make([]byte, 0, 64)
	for _, kt := range k.terms {
		if kt.base >= 0 {
			b = appendCursorValue(b, k.table.Value(row, kt.base))
		} else {
			b = appendCursorValue(b, k.vals[i*k.width+kt.tail])
		}
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// writeKeyset writes the predicate that selects rows after the cursor values. When every term
// sorts the same way and none can be NULL it is a row comparison, which Postgres matches to a
// composite index; otherwise it expands to an OR chain that places NULLs as the order does.
func writeKeyset(r *render, terms []keyTerm, after []any) {
	uniform := true
	for _, kt := range terms {
		if kt.desc != terms[0].desc || kt.nulls != 0 || kt.nullable {
			uniform = false
		}
	}
	for _, v := range after {
		if v == nil {
			uniform = false
		}
	}
	if uniform {
		r.b('(')
		for i, kt := range terms {
			if i > 0 {
				r.s(", ")
			}
			kt.write(r)
		}
		if terms[0].desc {
			r.s(") < (")
		} else {
			r.s(") > (")
		}
		for i, v := range after {
			if i > 0 {
				r.s(", ")
			}
			r.arg(v)
		}
		r.b(')')
		return
	}
	r.b('(')
	for i := range terms {
		if i > 0 {
			r.s(" OR ")
		}
		r.b('(')
		for j := 0; j < i; j++ {
			keysetEq(r, terms[j], after[j])
			r.s(" AND ")
		}
		keysetAfter(r, terms[i], after[i])
		r.b(')')
	}
	r.b(')')
}

// nullsLast reports whether the term sorts NULLs last, as Postgres does by default for ASC.
func nullsLast(kt keyTerm) bool {
	switch kt.nulls {
	case 1:
		return true
	case -1:
		return false
	}
	return !kt.desc
}

func keysetEq(r *render, kt keyTerm, v any) {
	kt.write(r)
	if v == nil {
		r.s(" IS NULL")
		return
	}
	r.s(" = ")
	r.arg(v)
}

func keysetAfter(r *render, kt keyTerm, v any) {
	if v == nil {
		if nullsLast(kt) {
			r.s("FALSE")
			return
		}
		kt.write(r)
		r.s(" IS NOT NULL")
		return
	}
	r.b('(')
	kt.write(r)
	if kt.desc {
		r.s(" < ")
	} else {
		r.s(" > ")
	}
	r.arg(v)
	if nullsLast(kt) && kt.nullable {
		r.s(" OR ")
		kt.write(r)
		r.s(" IS NULL")
	}
	r.b(')')
}

// Cursor encoding: one type byte per value followed by its payload, base64url without padding.
const (
	curNil    = 'n'
	curString = 's'
	curInt    = 'i'
	curFloat  = 'f'
	curBool   = 'b'
	curTime   = 't'
)

func appendCursorValue(b []byte, v any) []byte {
	cv, _ := cursorValue(v)
	switch x := cv.(type) {
	case string:
		b = append(b, curString)
		b = binary.AppendUvarint(b, uint64(len(x)))
		return append(b, x...)
	case int64:
		b = append(b, curInt)
		return binary.AppendVarint(b, x)
	case float64:
		b = append(b, curFloat)
		return binary.BigEndian.AppendUint64(b, math.Float64bits(x))
	case bool:
		if x {
			return append(b, curBool, 1)
		}
		return append(b, curBool, 0)
	case time.Time:
		b = append(b, curTime)
		return binary.AppendVarint(b, x.UnixNano())
	case []byte:
		b = append(b, curString)
		b = binary.AppendUvarint(b, uint64(len(x)))
		return append(b, x...)
	}
	return append(b, curNil)
}

// cursorValue returns v as one of the values a cursor encodes: nil, a string, an int64, a float64,
// a bool, a time or bytes. A driver.Valuer gives its value, named types their underlying value, a
// float32 widens and a 16-byte array (a uuid read without a Go type) becomes its text form. ok is
// false for any other value.
func cursorValue(v any) (out any, ok bool) {
	if dv, isValuer := v.(driver.Valuer); isValuer {
		if val, err := dv.Value(); err == nil {
			v = val
		}
	}
	switch x := normalize(v).(type) {
	case nil, string, int64, float64, bool, time.Time, []byte:
		return x, true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	case float32:
		return float64(x), true
	case [16]byte:
		return uuidText(x), true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	case reflect.Bool:
		return rv.Bool(), true
	}
	return nil, false
}

// uuidText formats a uuid's 16 bytes as its canonical text.
func uuidText(u [16]byte) string {
	var buf [36]byte
	encodeUUID(buf[:], u[:])
	return string(buf[:])
}

func decodeCursor(s string) ([]any, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errf("invalid cursor")
	}
	var out []any
	for len(b) > 0 {
		typ := b[0]
		b = b[1:]
		switch typ {
		case curNil:
			out = append(out, nil)
		case curString:
			n, k := binary.Uvarint(b)
			if k <= 0 || uint64(len(b)-k) < n {
				return nil, errf("invalid cursor")
			}
			out = append(out, string(b[k:k+int(n)]))
			b = b[k+int(n):]
		case curInt:
			v, k := binary.Varint(b)
			if k <= 0 {
				return nil, errf("invalid cursor")
			}
			out = append(out, v)
			b = b[k:]
		case curFloat:
			if len(b) < 8 {
				return nil, errf("invalid cursor")
			}
			out = append(out, math.Float64frombits(binary.BigEndian.Uint64(b)))
			b = b[8:]
		case curBool:
			if len(b) < 1 {
				return nil, errf("invalid cursor")
			}
			out = append(out, b[0] == 1)
			b = b[1:]
		case curTime:
			v, k := binary.Varint(b)
			if k <= 0 {
				return nil, errf("invalid cursor")
			}
			out = append(out, time.Unix(0, v).UTC())
			b = b[k:]
		default:
			return nil, errf("invalid cursor")
		}
	}
	return out, nil
}
