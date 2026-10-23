package keel

import (
	"context"
	"fmt"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
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

	// keyed statements return a relation key before the columns (many-to-many loads).
	keyed   bool
	keyKind keyKind
	// sizeHint is the expected number of base rows. Row slabs start at this size, so a statement
	// that returns as many rows as expected allocates each table's rows once.
	sizeHint int
}

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
func (p *selectPlan) scan(sc *scopeState, rows pgx.Rows, emit func(row unsafe.Pointer, key relKey)) error {
	m := rows.Conn().TypeMap()
	fds := rows.FieldDescriptions()
	off := 0
	if p.keyed {
		off = 1
	}
	if len(fds) != len(p.cols)+off {
		return errf("statement returned %d columns, expected %d", len(fds), len(p.cols)+off)
	}
	plans := make([]pgtype.ScanPlan, len(p.cols))
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
	var keys keyScanner
	cur := make([]unsafe.Pointer, len(p.nodes))
	for rows.Next() {
		raw := rows.RawValues()
		var key relKey
		if p.keyed {
			k, err := keys.scan(m, fds[0], raw[0], p.keyKind)
			if err != nil {
				return err
			}
			key = k
			raw = raw[1:]
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
				plans[i] = m.PlanScan(fd.DataTypeOID, fd.Format, dst)
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
		emit(cur[0], key)
	}
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
