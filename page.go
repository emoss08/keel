package keel

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// PageRequest asks for one keyset page.
type PageRequest struct {
	after    string
	size     int
	total    int64
	estimate bool
}

// After asks for the page of size rows that follows cursor. An empty cursor asks for the first
// page.
func After(cursor string, size int) PageRequest { return PageRequest{after: cursor, size: size} }

// Total asks for the number of matching rows, counted exactly up to limit, in the same round trip.
// When more rows match, Page.Total is limit and Page.TotalExact is false; Estimate makes it the
// planner's estimate instead.
func (p PageRequest) Total(limit int64) PageRequest { p.total = limit; return p }

// Estimate makes a total that passes Total's limit the planner's estimate of the matching rows,
// as CountEstimate reads it: EXPLAIN (FORMAT JSON) of the counted rows rides in the page's round
// trip. A list showing "about 1.2M" pays for counting at most limit+1 rows. It needs Total.
func (p PageRequest) Estimate() PageRequest { p.estimate = true; return p }

// Page is one keyset page.
type Page[T any] struct {
	Items []T
	// Next is the cursor of the following page, empty on the last page.
	Next string
	// Total is the number of matching rows when the request asked for it. It is exact up to the
	// request's limit; when more rows match it is the limit, or, when the request asked for an
	// Estimate, the planner's estimate (never less than the limit plus one).
	Total int64
	// TotalExact reports that Total is the exact number of matching rows. It is false when the
	// request asked for no total and when more rows matched than the request's limit.
	TotalExact bool

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

// pageRead prepares the read of one keyset page, with its count when p asks for a total (and the
// count's estimate when p asks for one); its finish fills dst.
func pageRead[T any](ctx context.Context, run *runner, q *query, p PageRequest, dst *Page[T]) (*read, error) {
	if p.size <= 0 {
		return nil, errf("Page needs a positive size")
	}
	if p.estimate && p.total <= 0 {
		return nil, errf("Page: Estimate needs Total")
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
	var est *estimate
	fo := fetchOpts{extraCols: keys.baseCols(), limit: int64(p.size) + 1}
	if keys.width > 0 {
		fo.tail = keys.tailExprs()
		fo.tailScan = keys.scanTail
		keys.vals = make([]any, 0, (p.size+1)*keys.width)
	}
	if after != nil {
		fo.keyset = func(r *render) { writeKeyset(r, keys.terms, after) }
	}
	if p.total > 0 && !p.estimate {
		fo.countCap = countPast(p.total)
		fo.count = &total
	}
	rd, err := fetchInto(ctx, run, q, fo, anyRows, func(items []T) {
		out := Page[T]{keys: keys}
		switch {
		case est != nil:
			c := est.count()
			out.Total, out.TotalExact = c.N, c.Exact
		case p.total > 0:
			out.Total, out.TotalExact = min(total, p.total), total <= p.total
		}
		if len(items) > p.size {
			items = items[:p.size]
			out.Next = keys.cursor(toPtr(items[len(items)-1]), len(items)-1)
		}
		out.Items = items
		*dst = out
	})
	if err != nil {
		return nil, err
	}
	if p.estimate {
		est = &estimate{upTo: p.total}
		stmts, err := q.estimateStmts(ctx, run, est)
		if err != nil {
			return nil, err
		}
		rd.stmts = append(rd.stmts, stmts...)
	}
	return rd, nil
}

// RowCount is a number of matching rows that is exact up to a limit and the planner's estimate
// above it, as CountEstimate returns it.
type RowCount struct {
	// N is the number of rows: counted when Exact is set, else the planner's estimate, never less
	// than the limit plus one.
	N int64
	// Exact reports that N was counted: no more rows matched than the limit.
	Exact bool
}

// CountEstimate returns the number of matching rows, counted exactly up to exactUpTo, and the
// planner's estimate when more match: a list header shows "1,204" for a small set and "about
// 1.2M" for a large one without counting it. The count reads at most exactUpTo+1 rows
// (SELECT count(*) FROM (SELECT 1 … LIMIT exactUpTo+1)), and EXPLAIN (FORMAT JSON) of the same
// rows without the limit, with the same text and bound arguments, rides in the same round trip;
// the estimate is the top plan node's row count, so it reflects the query's filter, joins, groups
// and the bound values. The query's own Limit and Offset do not apply, as for CountUpTo. An
// estimate is as good as the table's statistics: it can be well off on a table whose
// statistics are stale, but it is never less than exactUpTo+1. exactUpTo must not be negative.
func (q *Query[T]) CountEstimate(ctx context.Context, db *DB, exactUpTo int64) (RowCount, error) {
	var c RowCount
	err := Batch(ctx, db, CountEstimateInto(q, exactUpTo, &c))
	return c, err
}

// CountEstimate returns the number of rows the dynamic query returns, counted exactly up to
// exactUpTo, and the planner's estimate when it returns more, as Query.CountEstimate does: the
// count wraps the query (SELECT count(*) FROM (SELECT 1 FROM (query) d LIMIT exactUpTo+1) c), so
// a Limit on the query bounds both the count and the estimate, and EXPLAIN (FORMAT JSON) of the
// query rides in the same round trip.
func (q *DynQuery) CountEstimate(ctx context.Context, db *DB, exactUpTo int64) (RowCount, error) {
	var c RowCount
	err := Batch(ctx, db, DynCountEstimateInto(q, exactUpTo, &c))
	return c, err
}

// explainPrefix asks for the plan whose top node carries the planner's row estimate.
const explainPrefix = "EXPLAIN (FORMAT JSON) "

// estimate is an estimated count in flight: the limit it counts exactly up to, the rows counted
// (at most upTo+1) and the planner's estimate of the rows.
type estimate struct {
	upTo    int64
	n       int64
	planned float64
}

// newEstimate returns the estimate of a count exact up to upTo.
func newEstimate(upTo int64) (*estimate, error) {
	if upTo < 0 {
		return nil, errf("CountEstimate needs a limit of zero or more, not %d", upTo)
	}
	return &estimate{upTo: upTo}, nil
}

// countPast returns how many rows a count reads to tell whether more than upTo match: upTo+1,
// saturating.
func countPast(upTo int64) int64 {
	if upTo == math.MaxInt64 {
		return upTo
	}
	return upTo + 1
}

// count returns the counted rows when no more than upTo matched, else the planner's estimate,
// raised to upTo+1 when the statistics put it lower than the rows already seen.
func (e *estimate) count() RowCount {
	if e.n <= e.upTo {
		return RowCount{N: e.n, Exact: true}
	}
	n := e.upTo + 1
	switch {
	case e.planned >= math.MaxInt64:
		n = math.MaxInt64
	case e.planned > float64(n):
		n = int64(e.planned)
	}
	return RowCount{N: n}
}

// scanPlan reads the planner's row estimate from the top plan node of EXPLAIN's JSON.
func (e *estimate) scanPlan(rows pgx.Rows) error {
	if !rows.Next() {
		return nil
	}
	raw := rows.RawValues()
	if len(raw) == 0 {
		return errf("EXPLAIN returned no plan")
	}
	p, err := parsePlan(raw[0])
	if err != nil {
		return err
	}
	e.planned = p.Rows
	return nil
}

// stmts returns the estimate's two statements from r, whose buffer holds the count of the rows
// that rows selects: the count, then EXPLAIN (FORMAT JSON) of rows. Both carry r's arguments, so
// the plan is the one Postgres builds for these values. The EXPLAIN needs no plan-mode switch: a
// prepared EXPLAIN plans its query afresh on every run, with the bound values.
func (e *estimate) stmts(r *render, rows []byte, table string, customPlan bool) []*stmt {
	count := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "count", table: table,
		customPlan: customPlan}
	count.scan = scanCount(&e.n)
	r.buf = append(append(r.buf[:0], explainPrefix...), rows...)
	plan := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "explain", table: table}
	plan.scan = e.scanPlan
	return []*stmt{count, plan}
}

// estimateStmts renders the estimated count of the query's rows from one render: the count
// statement counts the rows SELECT 1 … LIMIT upTo+1 returns, and the EXPLAIN is the same
// SELECT 1 … without its LIMIT, so both carry the same placeholders and arguments.
func (q *query) estimateStmts(ctx context.Context, run *runner, e *estimate) ([]*stmt, error) {
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	limit := countPast(e.upTo)
	inner := r.section(func() {
		q.renderSelect(r, newRoot(q), selectOpts{countOnly: true, countCap: limit})
	})
	defer r.release(inner)
	if r.err != nil {
		return nil, r.err
	}
	var tail [32]byte
	suffix := strconv.AppendInt(append(tail[:0], " LIMIT "...), limit, 10)
	if !bytes.HasSuffix(inner, suffix) {
		return nil, errf("CountEstimate could not render the rows of %s without their limit",
			q.ref.Table.Name)
	}
	r.s("SELECT count(*) FROM (")
	r.buf = append(r.buf, inner...)
	r.s(") c")
	return e.stmts(r, inner[:len(inner)-len(suffix)], q.ref.Table.Name, q.customPlan), nil
}

// estimateStmts renders the estimated count of the rows the dynamic query returns: the count of
// at most upTo+1 of them and the EXPLAIN of the query, from one render.
func (q *DynQuery) estimateStmts(ctx context.Context, run *runner, e *estimate) ([]*stmt, error) {
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	rows := r.section(func() { q.writeSelect(r, nil) })
	defer r.release(rows)
	if r.err != nil {
		return nil, r.err
	}
	table := "dynamic"
	if t := q.Table(); t != nil {
		table = t.Name
	}
	r.s("SELECT count(*) FROM (SELECT 1 FROM (")
	r.buf = append(r.buf, rows...)
	r.s(") d LIMIT ")
	r.int(countPast(e.upTo))
	r.s(") c")
	return e.stmts(r, rows, table, q.q.customPlan), nil
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
