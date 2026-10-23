package keel

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/binary"
	"math"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
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
	Items []*T
	// Next is the cursor of the following page, empty on the last page.
	Next string
	// Total is the number of matching rows when the request asked for it, capped at its limit.
	Total int64
	// TotalCapped reports that Total stopped at the limit.
	TotalCapped bool
}

// Page returns the page of rows after the request's cursor. The query's order must be columns of
// its table; keel adds the primary key when the order does not end with it, so every row has a
// unique position.
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
	order, err := q.keysetOrder()
	if err != nil {
		return nil, err
	}
	var after []any
	if p.after != "" {
		after, err = decodeCursor(p.after)
		if err != nil {
			return nil, err
		}
		if len(after) != len(order) {
			return nil, errf("cursor does not match the query's order")
		}
	}
	saved := q.order
	q.order = order
	defer func() { q.order = saved }()

	extra := make([]int32, len(order))
	for i, o := range order {
		extra[i] = o.col.Index
	}
	var total int64
	fo := fetchOpts{extraCols: extra, limit: int64(p.size) + 1}
	if after != nil {
		fo.keyset = func(r *render) { writeKeyset(r, order, after) }
	}
	if p.total > 0 {
		fo.countCap = p.total
		fo.count = &total
	}
	table := q.ref.Table
	return fetchInto(ctx, run, q, fo, anyRows, func(items []*T) {
		out := Page[T]{Total: total, TotalCapped: p.total > 0 && total >= p.total}
		if len(items) > p.size {
			items = items[:p.size]
			out.Next = encodeCursor(table, order, unsafe.Pointer(items[len(items)-1]))
		}
		out.Items = items
		*dst = out
	})
}

// keysetOrder returns the query's order ending with the primary key.
func (q *query) keysetOrder() ([]Order, error) {
	t := q.ref.Table
	order := append([]Order(nil), q.order...)
	for _, o := range order {
		if o.col == nil || o.col.Ref != q.ref {
			return nil, errf("Page orders by columns of %s only", t.Name)
		}
	}
	desc := false
	if len(order) > 0 {
		desc = order[len(order)-1].desc
	}
	for _, pk := range t.PK {
		found := false
		for _, o := range order {
			if o.col.Index == pk {
				found = true
			}
		}
		if !found {
			c := Col{Ref: q.ref, Index: pk}
			order = append(order, Order{e: c, col: &c, desc: desc})
		}
	}
	if len(order) == 0 {
		return nil, errf("Page needs an order or a primary key on %s", t.Name)
	}
	return order, nil
}

// writeKeyset writes the predicate that selects rows after the cursor values. When every column
// sorts the same way and none can be NULL it is a row comparison, which Postgres matches to a
// composite index; otherwise it expands to an OR chain that places NULLs as the order does.
func writeKeyset(r *render, order []Order, after []any) {
	t := order[0].col.Ref.Table
	uniform := true
	for _, o := range order {
		c := &t.Columns[o.col.Index]
		if o.desc != order[0].desc || o.nulls != 0 || c.Flags.Has(schema.FlagNullable) {
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
		for i, o := range order {
			if i > 0 {
				r.s(", ")
			}
			r.col(*o.col)
		}
		if order[0].desc {
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
	for i := range order {
		if i > 0 {
			r.s(" OR ")
		}
		r.b('(')
		for j := 0; j < i; j++ {
			keysetEq(r, order[j], after[j])
			r.s(" AND ")
		}
		keysetAfter(r, order[i], after[i])
		r.b(')')
	}
	r.b(')')
}

func nullsLast(o Order) bool {
	switch o.nulls {
	case 1:
		return true
	case -1:
		return false
	}
	return !o.desc
}

func keysetEq(r *render, o Order, v any) {
	if v == nil {
		r.col(*o.col)
		r.s(" IS NULL")
		return
	}
	r.col(*o.col)
	r.s(" = ")
	r.arg(v)
}

func keysetAfter(r *render, o Order, v any) {
	if v == nil {
		if nullsLast(o) {
			r.s("FALSE")
			return
		}
		r.col(*o.col)
		r.s(" IS NOT NULL")
		return
	}
	r.b('(')
	r.col(*o.col)
	if o.desc {
		r.s(" < ")
	} else {
		r.s(" > ")
	}
	r.arg(v)
	if nullsLast(o) {
		r.s(" OR ")
		r.col(*o.col)
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

func encodeCursor(t *schema.Table, order []Order, row unsafe.Pointer) string {
	b := make([]byte, 0, 64)
	for _, o := range order {
		b = appendCursorValue(b, t.Value(row, o.col.Index))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func appendCursorValue(b []byte, v any) []byte {
	if dv, ok := v.(driver.Valuer); ok {
		val, err := dv.Value()
		if err == nil {
			v = val
		}
	}
	switch x := normalize(v).(type) {
	case nil:
		return append(b, curNil)
	case string:
		b = append(b, curString)
		b = binary.AppendUvarint(b, uint64(len(x)))
		return append(b, x...)
	case int64:
		b = append(b, curInt)
		return binary.AppendVarint(b, x)
	case int32:
		b = append(b, curInt)
		return binary.AppendVarint(b, int64(x))
	case int16:
		b = append(b, curInt)
		return binary.AppendVarint(b, int64(x))
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
