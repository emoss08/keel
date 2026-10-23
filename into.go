package keel

import (
	"context"
	"reflect"
	"sync"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Projection is a statement whose output columns fill a struct: a query that picks expressions,
// or a Compound. *Query[T] and Compound implement it.
type Projection interface {
	writeProjection(r *render) projectionInfo
}

type projectionInfo struct {
	table      string
	exprs      int
	limit      int64
	timeout    time.Duration
	customPlan bool
}

func (q *Query[T]) writeProjection(r *render) projectionInfo {
	q.q.renderSelect(r, newRoot(&q.q), selectOpts{exact: true})
	return projectionInfo{table: q.q.ref.Table.Name, exprs: len(q.q.exprs), limit: q.q.limit,
		timeout: q.q.timeout, customPlan: q.q.customPlan}
}

func (c Compound) writeProjection(r *render) projectionInfo {
	c.writeSubquery(r, false)
	n := 0
	if len(c.parts) > 0 {
		if q, ok := c.parts[0].(interface{ pickCount() int }); ok {
			n = q.pickCount()
		}
	}
	return projectionInfo{table: "compound", exprs: n}
}

func (q *Query[T]) pickCount() int { return len(q.q.exprs) }

// Into runs a projection and decodes each row into R: the picked expressions fill R's exported
// fields in order. R may also be a single scalar type when one expression is picked.
//
//	type statusCount struct {
//	    Status shipment.Status
//	    Total  int64
//	}
//	rows, err := keel.Into[statusCount](ctx, db, s.Group(s.Status).Pick(s.Status, keel.Count()))
func Into[R any](ctx context.Context, db *DB, p Projection) ([]R, error) {
	var out []R
	err := Batch(ctx, db, IntoSlice(p, &out))
	return out, err
}

// IntoOne is Into for a projection that returns exactly one row, such as an aggregate.
func IntoOne[R any](ctx context.Context, db *DB, p Projection) (R, error) {
	var out R
	err := Batch(ctx, db, IntoValue(p, &out))
	return out, err
}

// intoItem runs a projection that must return want rows, decodes its rows into R and hands them
// to set.
func intoItem[R any](p Projection, want cardinality, set func(rows []R)) BatchItem {
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		r := newRender(run.sc, ctx, run.db)
		defer r.recycle()
		info := p.writeProjection(r)
		if r.err != nil {
			return nil, r.err
		}
		if info.exprs == 0 {
			return nil, errf("Into needs Pick")
		}
		layout, err := layoutOf(reflect.TypeFor[R](), info.exprs)
		if err != nil {
			return nil, err
		}
		var out []R
		if info.limit > 0 && info.limit <= 1024 {
			out = make([]R, 0, info.limit)
		}
		s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: info.table,
			customPlan: info.customPlan}
		rd := &read{stmts: []*stmt{s}, timeout: info.timeout, want: want}
		rd.finish = func() { set(out) }
		s.scan = func(rows pgx.Rows) error {
			m := rows.Conn().TypeMap()
			fds := rows.FieldDescriptions()
			if len(fds) != len(layout.fields) {
				return errf("projection returned %d columns, %s has %d fields", len(fds),
					layout.typ, len(layout.fields))
			}
			plans := make([]pgtype.ScanPlan, len(fds))
			for rows.Next() {
				var v R
				base := unsafe.Pointer(&v)
				raw := rows.RawValues()
				for i, f := range layout.fields {
					dst := reflect.NewAt(f.typ, unsafe.Add(base, f.offset)).Interface()
					if plans[i] == nil {
						plans[i] = m.PlanScan(fds[i].DataTypeOID, fds[i].Format, dst)
					}
					if err := plans[i].Scan(raw[i], dst); err != nil {
						return errf("scan %s field %d: %w", layout.typ, i, err)
					}
				}
				out = append(out, v)
			}
			rd.rows = len(out)
			return nil
		}
		return rd, nil
	}}
}

type intoField struct {
	typ    reflect.Type
	offset uintptr
}

type intoLayout struct {
	typ    reflect.Type
	fields []intoField
}

var layouts = struct {
	sync.RWMutex
	m map[reflect.Type]*intoLayout
}{m: make(map[reflect.Type]*intoLayout)}

func layoutOf(t reflect.Type, n int) (*intoLayout, error) {
	layouts.RLock()
	l, ok := layouts.m[t]
	layouts.RUnlock()
	if !ok {
		l = &intoLayout{typ: t}
		if t.Kind() == reflect.Struct && !scansWhole(t) {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if !f.IsExported() {
					continue
				}
				l.fields = append(l.fields, intoField{typ: f.Type, offset: f.Offset})
			}
		} else {
			l.fields = []intoField{{typ: t}}
		}
		layouts.Lock()
		layouts.m[t] = l
		layouts.Unlock()
	}
	if len(l.fields) != n {
		return nil, errf("%s has %d fields, the projection picks %d expressions", t,
			len(l.fields), n)
	}
	return l, nil
}

// scansWhole reports whether a struct type is a value pgx scans as one column (time.Time, a
// decimal, a type implementing sql.Scanner).
func scansWhole(t reflect.Type) bool {
	if t.PkgPath() == "time" && t.Name() == "Time" {
		return true
	}
	scanner := reflect.TypeFor[interface{ Scan(src any) error }]()
	return reflect.PointerTo(t).Implements(scanner)
}
