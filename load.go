package keel

import (
	"context"
	"fmt"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// keyKind is the Go kind of a relation key column.
type keyKind uint8

const (
	keyString keyKind = iota
	keyInt
)

func keyKindOf(t *schema.Table, col int32) (keyKind, error) {
	switch t.Columns[col].Kind {
	case schema.KindString:
		return keyString, nil
	case schema.KindInt64, schema.KindInt32, schema.KindInt16:
		return keyInt, nil
	}
	return 0, errf("%s.%s: relation keys must be string or integer columns", t.Name, t.Columns[col].Name)
}

// relKey is one relation key value. It is comparable, so keys index parents without boxing.
type relKey struct {
	s string
	n int64
}

// readKey reads a key column of row. ok is false for a NULL key.
func readKey(t *schema.Table, row unsafe.Pointer, col int32) (relKey, bool) {
	switch p := t.Ptr(row, col).(type) {
	case *string:
		return relKey{s: *p}, true
	case **string:
		if *p == nil {
			return relKey{}, false
		}
		return relKey{s: **p}, true
	case *int64:
		return relKey{n: *p}, true
	case **int64:
		if *p == nil {
			return relKey{}, false
		}
		return relKey{n: **p}, true
	case *int32:
		return relKey{n: int64(*p)}, true
	case **int32:
		if *p == nil {
			return relKey{}, false
		}
		return relKey{n: int64(**p)}, true
	case *int16:
		return relKey{n: int64(*p)}, true
	case **int16:
		if *p == nil {
			return relKey{}, false
		}
		return relKey{n: int64(**p)}, true
	}
	return relKey{}, false
}

// keyScanner decodes the leading key column of a keyed statement into a relKey.
type keyScanner struct {
	plan pgtype.ScanPlan
	s    string
	n    int64
}

func (k *keyScanner) scan(m *pgtype.Map, fd pgconn.FieldDescription, raw []byte, kind keyKind) (relKey, error) {
	var dst any = &k.s
	if kind == keyInt {
		dst = &k.n
	}
	if k.plan == nil {
		k.plan = m.PlanScan(fd.DataTypeOID, fd.Format, dst)
	}
	if err := k.plan.Scan(raw, dst); err != nil {
		return relKey{}, fmt.Errorf("keel: scan relation key: %w", err)
	}
	return relKey{s: k.s, n: k.n}, nil
}

// parentIndex groups the parents of a to-many load by key and keeps the distinct keys as the keel
// array parameter bound to = ANY($1).
type parentIndex struct {
	kind  keyKind
	byKey map[relKey][]unsafe.Pointer
	strs  textArray
	ints  int8Array
}

func indexParents(t *schema.Table, parents []unsafe.Pointer, col int32, kind keyKind) *parentIndex {
	ix := &parentIndex{kind: kind, byKey: make(map[relKey][]unsafe.Pointer, len(parents))}
	for _, p := range parents {
		k, ok := readKey(t, p, col)
		if !ok {
			continue
		}
		ps, seen := ix.byKey[k]
		if !seen {
			if kind == keyString {
				ix.strs = append(ix.strs, k.s)
			} else {
				ix.ints = append(ix.ints, k.n)
			}
		}
		ix.byKey[k] = append(ps, p)
	}
	return ix
}

func (ix *parentIndex) len() int { return len(ix.byKey) }

// arg is the key array to bind.
func (ix *parentIndex) arg() any {
	if ix.kind == keyString {
		return ix.strs
	}
	return ix.ints
}

// arrayType is the array type the key array is cast to where Postgres cannot infer it.
func (ix *parentIndex) arrayType() string {
	if ix.kind == keyString {
		return "text[]"
	}
	return "int8[]"
}

// pending is one to-many load: the node and the parent rows it attaches to.
type pending struct {
	node    *lnode
	parents []unsafe.Pointer
}

// loadLevel loads the to-many nodes under a statement's plan.
func loadLevel(ctx context.Context, run *runner, parentPlan *selectPlan, many []*lnode) error {
	return loadPending(ctx, run, pendingOf(parentPlan, many))
}

func pendingOf(plan *selectPlan, many []*lnode) []pending {
	var work []pending
	for _, n := range many {
		if rows := plan.rows[n.parent.idx]; len(rows) > 0 {
			work = append(work, pending{node: n, parents: rows})
		}
	}
	return work
}

// loadPending sends one statement per to-many node of a level, all in one round trip, then loads
// the next level.
func loadPending(ctx context.Context, run *runner, work []pending) error {
	if len(work) == 0 {
		return nil
	}
	stmts := make([]*stmt, 0, len(work))
	plans := make([]*selectPlan, 0, len(work))
	deeper := false
	for _, w := range work {
		s, plan, err := loadStmt(ctx, run, w)
		if err != nil {
			return err
		}
		if s == nil {
			continue
		}
		stmts = append(stmts, s)
		plans = append(plans, plan)
		deeper = deeper || len(w.node.manyChildren()) > 0
	}
	if err := run.batch(ctx, stmts, !deeper); err != nil {
		return err
	}
	var next []pending
	for i, plan := range plans {
		next = append(next, pendingOf(plan, plan.nodes[0].manyChildren())...)
		if err := plans[i].afterLoad(ctx); err != nil {
			return err
		}
	}
	return loadPending(ctx, run, next)
}

// loadStmt builds the statement that loads a to-many node for its parents: one = ANY($1) filter
// over the parents' keys, through the join table for many-to-many, LATERAL per parent when the load
// has a limit. It returns a nil statement when no parent has a key.
func loadStmt(ctx context.Context, run *runner, w pending) (*stmt, *selectPlan, error) {
	n := w.node
	parent := n.parent.table
	rel := &parent.Relations[n.rel]
	if len(rel.Local) != 1 {
		return nil, nil, errf("relation %s.%s: to-many loads need a single-column key", parent.Name, rel.Name)
	}
	local, remote := rel.Local[0], rel.Remote[0]
	kind, err := keyKindOf(parent, local)
	if err != nil {
		return nil, nil, err
	}
	ix := indexParents(parent, w.parents, local, kind)
	if ix.len() == 0 {
		return nil, nil, nil
	}

	q := query{ref: n.ref}
	if n.spec != nil {
		q = *n.spec
		q.ref = n.ref
	}
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	m2m := rel.Kind == schema.ManyToMany

	var plan *selectPlan
	switch {
	case m2m:
		plan = q.renderSelect(r, n, m2mOpts(r, rel, n.table, remote, ix))
		if q.limit > 0 {
			return nil, nil, errf("relation %s.%s: many-to-many loads do not take a Limit", parent.Name, rel.Name)
		}
	case q.limit > 0:
		plan = lateralSelect(r, &q, n, ix, remote)
	default:
		plan = q.renderSelect(r, n, selectOpts{extraWhere: func(r *render, alias string) {
			r.s(alias)
			r.b('.')
			r.s(n.table.Columns[remote].Quoted)
			r.s(" = ANY(")
			r.arg(ix.arg())
			r.b(')')
		}})
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	plan.keyKind = kind
	plan.sizeHint = min(ix.len(), maxSlab)
	plan.track()
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: n.table.Name,
		customPlan: q.customPlan}
	child := n.table
	s.scan = func(rows pgx.Rows) error {
		return plan.scan(run.sc, s.sql, rows, func(row unsafe.Pointer, key relKey) {
			if !m2m {
				var ok bool
				if key, ok = readKey(child, row, remote); !ok {
					return
				}
			}
			for _, p := range ix.byKey[key] {
				parent.Attach(p, n.rel, row)
			}
		})
	}
	return s, plan, nil
}

// m2mOpts reads a many-to-many relation through its join table, returning the parent key first.
func m2mOpts(r *render, rel *schema.Relation, target *schema.Table, remote int32, ix *parentIndex) selectOpts {
	jalias := r.newAlias(rel.JoinAlias)
	return selectOpts{
		keyColumn: func(r *render) {
			r.s(jalias)
			r.b('.')
			r.s(rel.JoinLocalQ[0])
		},
		extraJoin: func(r *render, alias string) {
			r.s(" JOIN ")
			r.s(rel.JoinQuoted)
			r.b(' ')
			r.s(jalias)
			r.s(" ON ")
			r.s(jalias)
			r.b('.')
			r.s(rel.JoinRemoteQ[0])
			r.s(" = ")
			r.s(alias)
			r.b('.')
			r.s(target.Columns[remote].Quoted)
		},
		extraWhere: func(r *render, _ string) {
			r.s(jalias)
			r.b('.')
			r.s(rel.JoinLocalQ[0])
			r.s(" = ANY(")
			r.arg(ix.arg())
			r.b(')')
		},
	}
}

// lateralSelect loads at most q.limit rows per parent key:
// SELECT … FROM unnest($1) k CROSS JOIN LATERAL (SELECT … WHERE fk = k.k … LIMIT n).
func lateralSelect(r *render, q *query, n *lnode, ix *parentIndex, remote int32) *selectPlan {
	keyAlias := r.newAlias("k")
	var plan *selectPlan
	body := r.section(func() {
		plan = q.renderSelect(r, n, selectOpts{extraWhere: func(r *render, alias string) {
			r.s(alias)
			r.b('.')
			r.s(n.table.Columns[remote].Quoted)
			r.s(" = ")
			r.s(keyAlias)
			r.s(".k")
		}})
	})
	outer := r.newAlias("lat")
	r.s("SELECT ")
	r.s(outer)
	r.s(".* FROM unnest(")
	r.arg(ix.arg())
	r.s("::")
	r.s(ix.arrayType())
	r.s(") AS ")
	r.s(keyAlias)
	r.s("(k) CROSS JOIN LATERAL (")
	r.buf = append(r.buf, body...)
	r.s(") ")
	r.s(outer)
	r.release(body)
	return plan
}
