package keel

import (
	"context"
	"reflect"
	"sync"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Projection is a query whose picked expressions fill a struct. *Query[T] implements it.
type Projection interface {
	projectionQuery() *query
}

func (q *Query[T]) projectionQuery() *query { return &q.q }

// Into runs a projection and decodes each row into R: the picked expressions fill R's exported
// fields in order. R may also be a single scalar type when one expression is picked.
//
//	type statusCount struct {
//	    Status shipment.Status
//	    Total  int64
//	}
//	rows, err := keel.Into[statusCount](ctx, db, s.Group(s.Status).Pick(s.Status, keel.Count()))
func Into[R any](ctx context.Context, db *DB, p Projection) ([]R, error) {
	q := p.projectionQuery()
	if len(q.exprs) == 0 {
		return nil, errf("Into needs Pick")
	}
	layout, err := layoutOf(reflect.TypeFor[R](), len(q.exprs))
	if err != nil {
		return nil, err
	}
	ctx, cancel := q.checkTimeout(ctx)
	defer cancel()
	run, err := db.newRunner(ctx, false)
	if err != nil {
		return nil, err
	}
	r := &render{scope: run.sc, ctx: ctx, db: db}
	root := newRoot(q)
	q.renderSelect(r, root, selectOpts{exact: true})
	if r.err != nil {
		return nil, run.close(ctx, r.err)
	}
	var out []R
	if q.limit > 0 && q.limit <= 1024 {
		out = make([]R, 0, q.limit)
	}
	s := &stmt{sql: string(r.buf), args: r.args, op: "select", table: q.ref.Table.Name}
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
		return nil
	}
	err = run.one(ctx, s, true)
	return out, run.close(ctx, err)
}

// IntoOne is Into for a projection that returns exactly one row, such as an aggregate.
func IntoOne[R any](ctx context.Context, db *DB, p Projection) (R, error) {
	var zero R
	rows, err := Into[R](ctx, db, p)
	if err != nil {
		return zero, err
	}
	switch len(rows) {
	case 0:
		return zero, ErrNotFound
	case 1:
		return rows[0], nil
	}
	return zero, ErrNotSingular
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
