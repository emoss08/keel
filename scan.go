package keel

import (
	"context"
	"fmt"
	"reflect"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// selectPlan is the decoding layout of one statement: the tables it reads (the base first, then
// the relations joined into it) and, for each result column, the table and column it fills.
type selectPlan struct {
	nodes   []*lnode
	cols    []lcol
	parents []int
	keep    []bool
	rows    [][]unsafe.Pointer

	// keyed statements return a relation key, one column per key column of keyKinds, before the
	// columns (many-to-many and pick loads).
	keyed    bool
	keyKinds []keyKind
	// sizeHint is the expected number of base rows. Row slabs start at this size, so a statement
	// that returns as many rows as expected allocates each table's rows once.
	sizeHint int
	// tail is the number of result columns after the rows' columns, which tailScan decodes for
	// each base row (a keyset page's order values that are not columns of the row).
	tail     int
	tailScan tailScanner
}

// tailScanner decodes the result columns a statement returns after its rows' columns: fds and raw
// are those columns' descriptions and values, row the base row they came with.
type tailScanner func(conn *pgx.Conn, fds []pgconn.FieldDescription, raw [][]byte, row unsafe.Pointer) error

type lcol struct {
	node int
	col  int32
}

// track marks the nodes whose rows must be kept: parents of to-many loads and tables with an
// AfterLoad hook.
func (p *selectPlan) track() {
	p.rows = make([][]unsafe.Pointer, len(p.nodes))
	for i, n := range p.nodes {
		for _, c := range n.children {
			if !c.joined {
				p.keep[i] = true
			}
		}
		if n.table.AfterLoad != nil {
			p.keep[i] = true
		}
	}
}

// scan decodes rows, calling emit with each base row and, for keyed statements, its relation key.
func (p *selectPlan) scan(sc *scopeState, sql string, rows pgx.Rows, emit func(row unsafe.Pointer, key relKey)) error {
	m := rows.Conn().TypeMap()
	fds := rows.FieldDescriptions()
	off := 0
	if p.keyed {
		off = len(p.keyKinds)
	}
	if len(fds) != len(p.cols)+off+p.tail {
		return errf("statement returned %d columns, expected %d", len(fds), len(p.cols)+off+p.tail)
	}
	plans := cachedPlans(rows.Conn(), sql, p.nodes[0].table, len(p.cols))
	shared := make([]int8, len(p.cols))
	for i, c := range p.cols {
		shared[i] = sharedTenant(sc, p.nodes[c.node].table, c.col, fds[i+off].DataTypeOID)
	}
	hint := p.sizeHint
	if hint <= 0 {
		hint = 16
	}
	slabs := make([]slab, len(p.nodes))
	for i, n := range p.nodes {
		slabs[i] = slab{t: n.table, next: hint}
	}
	keys := make(keyScanners, off)
	cur := make([]unsafe.Pointer, len(p.nodes))
	for rows.Next() {
		raw := rows.RawValues()
		var key relKey
		if p.keyed {
			k, err := keys.scan(m, fds, raw, p.keyKinds)
			if err != nil {
				return err
			}
			key = k
			raw = raw[off:]
		}
		for ni, n := range p.nodes {
			if ni == 0 {
				cur[0] = slabs[0].new()
				continue
			}
			parent := cur[p.parents[ni]]
			if parent == nil || raw[n.presence] == nil {
				cur[ni] = nil
				continue
			}
			cur[ni] = slabs[ni].new()
		}
		for i := range p.cols {
			c := p.cols[i]
			row := cur[c.node]
			if row == nil {
				continue
			}
			t := p.nodes[c.node].table
			dst := t.Ptr(row, c.col)
			if ti := shared[i]; ti >= 0 {
				if v := sc.tenant[ti].(string); string(raw[i]) == v {
					*dst.(*string) = v
					continue
				}
			}
			if plans[i] == nil {
				fd := fds[i+off]
				plans[i] = planColumn(rows.Conn(), fd.DataTypeOID, fd.Format, &t.Columns[c.col], dst)
			}
			if err := plans[i].Scan(raw[i], dst); err != nil {
				return fmt.Errorf("keel: scan %s.%s: %w", t.Name, t.Columns[c.col].Name, err)
			}
		}
		for ni := 1; ni < len(p.nodes); ni++ {
			if cur[ni] == nil {
				continue
			}
			pi := p.parents[ni]
			p.nodes[pi].table.Attach(cur[pi], p.nodes[ni].rel, cur[ni])
		}
		for ni, keep := range p.keep {
			if keep && cur[ni] != nil {
				p.rows[ni] = append(p.rows[ni], cur[ni])
			}
		}
		if p.tail > 0 {
			n := len(p.cols)
			if err := p.tailScan(rows.Conn(), fds[off+n:], raw[n:], cur[0]); err != nil {
				return err
			}
		}
		emit(cur[0], key)
	}
	return nil
}

// planColumn plans decoding column c, of type oid, read on conn, into dst: a jsonnumber column
// keeps its JSON numbers as json.Number, a nullzero column reads NULL as its zero value, any other
// column decodes as planScan plans it.
func planColumn(conn *pgx.Conn, oid uint32, format int16, c *schema.Column, dst any) pgtype.ScanPlan {
	if c.Flags.Has(schema.FlagJSONNumber) {
		if p := planJSONNumber(conn, oid, format); p != nil {
			return p
		}
	}
	m := conn.TypeMap()
	if c.Flags.Has(schema.FlagNullZero) {
		return newZeroScanPlan(m, oid, format, dst)
	}
	return planScan(m, oid, format, dst)
}

// newZeroScanPlan returns the plan of a nullzero column read into dst: a typed zeroScanPlan for
// the field types keel generates, a reflectZeroPlan for any other.
func newZeroScanPlan(m *pgtype.Map, oid uint32, format int16, dst any) pgtype.ScanPlan {
	elem := m.PlanScan(oid, format, dst)
	switch dst.(type) {
	case *string:
		return &zeroScanPlan[string]{elem: elem}
	case *int64:
		return &zeroScanPlan[int64]{elem: elem}
	case *int32:
		return &zeroScanPlan[int32]{elem: elem}
	case *int16:
		return &zeroScanPlan[int16]{elem: elem}
	case *float64:
		return &zeroScanPlan[float64]{elem: elem}
	case *float32:
		return &zeroScanPlan[float32]{elem: elem}
	case *bool:
		return &zeroScanPlan[bool]{elem: elem}
	case *time.Time:
		return &zeroScanPlan[time.Time]{elem: elem}
	case *[]string:
		return &zeroScanPlan[[]string]{elem: elem}
	case *[]int64:
		return &zeroScanPlan[[]int64]{elem: elem}
	case *[]byte:
		return &zeroScanPlan[[]byte]{elem: elem}
	}
	return reflectZeroPlan{elem: elem}
}

// zeroScanPlan decodes a nullzero column into a *V field: NULL stores V's zero value, any other
// value is decoded by elem. It neither reflects nor allocates.
type zeroScanPlan[V any] struct {
	elem pgtype.ScanPlan
}

// Scan decodes src into the *V target.
func (p *zeroScanPlan[V]) Scan(src []byte, target any) error {
	if src == nil {
		var zero V
		*target.(*V) = zero
		return nil
	}
	return p.elem.Scan(src, target)
}

// reflectZeroPlan decodes a nullzero column of a type keel has no typed plan for (a scanner, a
// jsonb struct): NULL stores the field's zero value through reflection, which does not allocate.
type reflectZeroPlan struct {
	elem pgtype.ScanPlan
}

// Scan decodes src into target, a pointer to the field.
func (p reflectZeroPlan) Scan(src []byte, target any) error {
	if src == nil {
		reflect.ValueOf(target).Elem().SetZero()
		return nil
	}
	return p.elem.Scan(src, target)
}

// planScan plans decoding a column of type oid into dst. A nullable field of a basic type (a
// pointer to a pointer) gets a nullScanPlan instead of pgx's, which allocates through reflection.
func planScan(m *pgtype.Map, oid uint32, format int16, dst any) pgtype.ScanPlan {
	switch dst.(type) {
	case **string:
		return newNullScanPlan[string](m, oid, format)
	case **int64:
		return newNullScanPlan[int64](m, oid, format)
	case **int32:
		return newNullScanPlan[int32](m, oid, format)
	case **int16:
		return newNullScanPlan[int16](m, oid, format)
	case **float64:
		return newNullScanPlan[float64](m, oid, format)
	case **float32:
		return newNullScanPlan[float32](m, oid, format)
	case **bool:
		return newNullScanPlan[bool](m, oid, format)
	case **time.Time:
		return newNullScanPlan[time.Time](m, oid, format)
	}
	return m.PlanScan(oid, format, dst)
}

// nullScanPlan decodes a nullable column into a **V field: NULL sets the field to nil, any other
// value is decoded by the plan of V into a newly allocated V.
type nullScanPlan[V any] struct {
	elem pgtype.ScanPlan
}

// newNullScanPlan returns the nullScanPlan of a column of type oid, planning V's decoding once.
func newNullScanPlan[V any](m *pgtype.Map, oid uint32, format int16) pgtype.ScanPlan {
	return &nullScanPlan[V]{elem: m.PlanScan(oid, format, new(V))}
}

// Scan decodes src into the **V target.
func (p *nullScanPlan[V]) Scan(src []byte, target any) error {
	dst := target.(**V)
	if src == nil {
		*dst = nil
		return nil
	}
	v := new(V)
	if err := p.elem.Scan(src, v); err != nil {
		return err
	}
	*dst = v
	return nil
}

// sharedTenant returns the scope position of a tenant column whose rows can reuse the scope's
// string instead of allocating one each: a string tenant column under a tenant scope, read as
// text. It returns -1 for every other column.
func sharedTenant(sc *scopeState, t *schema.Table, col int32, oid uint32) int8 {
	c := &t.Columns[col]
	if sc == nil || sc.tenant == nil || !c.Flags.Has(schema.FlagTenant) || c.Flags.Has(schema.FlagNullable) ||
		c.Kind != schema.KindString || !textOID(oid) {
		return -1
	}
	pos := sc.tenantPos(c.Name)
	if pos < 0 || pos > 127 {
		return -1
	}
	if _, ok := sc.tenant[pos].(string); !ok {
		return -1
	}
	return int8(pos)
}

// textOID reports types whose text and binary forms are both the raw UTF-8 string.
func textOID(oid uint32) bool {
	switch oid {
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.BPCharOID, pgtype.NameOID:
		return true
	}
	return false
}

func (p *selectPlan) afterLoad(ctx context.Context) error {
	for ni, n := range p.nodes {
		if n.table.AfterLoad == nil {
			continue
		}
		for _, row := range p.rows[ni] {
			if err := n.table.AfterLoad(ctx, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// slab hands out rows of one table from arrays allocated in growing chunks, so decoding n rows
// costs a few allocations instead of n. A row keeps its chunk alive while it is referenced.
type slab struct {
	t     *schema.Table
	start unsafe.Pointer
	idx   int
	n     int
	next  int
}

const maxSlab = 1024

func (s *slab) new() unsafe.Pointer {
	if s.t.NewN == nil {
		return s.t.New()
	}
	if s.idx == s.n {
		s.n = min(max(s.next, 1), maxSlab)
		s.start = s.t.NewN(s.n)
		s.idx = 0
		s.next = s.n * 2
	}
	p := unsafe.Add(s.start, uintptr(s.idx)*s.t.Size)
	s.idx++
	return p
}

// planCacheKey identifies a statement's decoding: its text fixes the result columns, its base
// table fixes the Go fields they fill.
type planCacheKey struct {
	sql   string
	table *schema.Table
}

// planCache keeps the scan plans of a connection's statements, so a repeated statement decodes
// without asking pgx to plan each column again. It lives in the connection's CustomData and is
// used only by the goroutine holding the connection.
type planCache struct {
	m map[planCacheKey][]pgtype.ScanPlan
}

const (
	planCacheKeyName = "keel.plans"
	planCacheMax     = 2048
)

// cachedPlans returns the plan slice of a statement on conn, empty on first use. Callers fill
// missing entries; the slice stays in the cache.
func cachedPlans(conn *pgx.Conn, sql string, t *schema.Table, n int) []pgtype.ScanPlan {
	if conn == nil {
		return make([]pgtype.ScanPlan, n)
	}
	data := conn.PgConn().CustomData()
	pc, _ := data[planCacheKeyName].(*planCache)
	if pc == nil {
		pc = &planCache{m: make(map[planCacheKey][]pgtype.ScanPlan)}
		data[planCacheKeyName] = pc
	}
	key := planCacheKey{sql: sql, table: t}
	plans, ok := pc.m[key]
	if ok && len(plans) == n {
		return plans
	}
	if len(pc.m) >= planCacheMax {
		clear(pc.m)
	}
	plans = make([]pgtype.ScanPlan, n)
	pc.m[key] = plans
	return plans
}
