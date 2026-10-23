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

// fetchOpts adds keyset pagination and a capped count to a fetch.
type fetchOpts struct {
	keyset    func(r *render)
	extraCols []int32
	limit     int64
	countCap  int64
	count     *int64
}

// fetch runs the query and its relation loads, calling emit for every row of the base table in
// order.
func (q *query) fetch(ctx context.Context, db *DB, fo *fetchOpts, emit func(unsafe.Pointer)) error {
	if q.projection() || q.ref.Table.New == nil {
		return errf("a query that picks expressions other than its own columns needs keel.Into")
	}
	ctx, cancel := q.checkTimeout(ctx)
	defer cancel()

	root := newRoot(q)
	if err := buildTree(root, q.loads); err != nil {
		return err
	}
	run, err := db.newRunner(ctx, false)
	if err != nil {
		return err
	}

	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	o := selectOpts{}
	if fo != nil {
		o.keyset, o.extraCols, o.limit = fo.keyset, fo.extraCols, fo.limit
	}
	plan := q.renderSelect(r, root, o)
	if r.err != nil {
		return run.close(ctx, r.err)
	}
	main := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: root.table.Name,
		customPlan: q.customPlan}
	plan.track(root)
	main.scan = func(rows pgx.Rows) error {
		return plan.scan(ctx, rows, func(row unsafe.Pointer, _ any) { emit(row) })
	}
	stmts := []*stmt{main}

	if fo != nil && fo.count != nil {
		cs, err := q.countStmt(ctx, db, run.sc, fo.countCap)
		if err != nil {
			return run.close(ctx, err)
		}
		cs.scan = func(rows pgx.Rows) error {
			if rows.Next() {
				return rows.Scan(fo.count)
			}
			return nil
		}
		stmts = append(stmts, cs)
	}

	many := root.manyChildren()
	err = run.batch(ctx, stmts, len(many) == 0)
	if err == nil && len(many) > 0 {
		err = loadLevel(ctx, run, plan, many)
	}
	if err == nil {
		err = plan.afterLoad(ctx)
	}
	return run.close(ctx, err)
}

// track marks the nodes whose rows must be kept: parents of to-many loads and tables with an
// AfterLoad hook.
func (p *selectPlan) track(sn *lnode) {
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

func (p *selectPlan) scan(ctx context.Context, rows pgx.Rows, emit func(unsafe.Pointer, any)) error {
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
	var keyPlan pgtype.ScanPlan
	var keyFD pgconn.FieldDescription
	if p.keyed {
		keyFD = fds[0]
		fds = fds[1:]
	}
	cur := make([]unsafe.Pointer, len(p.nodes))
	for rows.Next() {
		raw := rows.RawValues()
		var key any
		if p.keyed {
			if keyPlan == nil {
				keyPlan = m.PlanScan(keyFD.DataTypeOID, keyFD.Format, &key)
			}
			if err := keyPlan.Scan(raw[0], &key); err != nil {
				return fmt.Errorf("keel: scan relation key: %w", err)
			}
			raw = raw[1:]
		}
		for ni, n := range p.nodes {
			if ni == 0 {
				cur[0] = n.table.New()
				continue
			}
			parent := cur[p.parents[ni]]
			if parent == nil || raw[n.presence] == nil {
				cur[ni] = nil
				continue
			}
			cur[ni] = n.table.New()
		}
		for i, c := range p.cols {
			row := cur[c.node]
			if row == nil {
				continue
			}
			t := p.nodes[c.node].table
			dst := t.Ptr(row, c.col)
			if plans[i] == nil {
				plans[i] = m.PlanScan(fds[i].DataTypeOID, fds[i].Format, dst)
			}
			if err := plans[i].Scan(raw[i], dst); err != nil {
				return fmt.Errorf("keel: scan %s.%s: %w", t.Name, t.Columns[c.col].Name, err)
			}
		}
		for ni := 1; ni < len(p.nodes); ni++ {
			if cur[ni] == nil {
				continue
			}
			n := p.nodes[ni]
			pi := p.parents[ni]
			p.nodes[pi].table.Attach(cur[pi], n.rel, cur[ni])
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

// pending is one to-many load: the node and the parent rows it attaches to.
type pending struct {
	node    *lnode
	parents []unsafe.Pointer
}

// loadLevel loads the to-many nodes under plan's statement, one statement per node, all in one
// round trip, then recurses into the next level.
func loadLevel(ctx context.Context, run *runner, parentPlan *selectPlan, many []*lnode) error {
	var work []pending
	for _, n := range many {
		rows := parentPlan.rows[n.parent.idx]
		if len(rows) > 0 {
			work = append(work, pending{node: n, parents: rows})
		}
	}
	if len(work) == 0 {
		return nil
	}
	return loadPending(ctx, run, work)
}

func loadPending(ctx context.Context, run *runner, work []pending) error {
	stmts := make([]*stmt, 0, len(work))
	plans := make([]*selectPlan, 0, len(work))
	deeper := false
	for _, w := range work {
		s, plan, err := manyStmt(ctx, run, w)
		if err != nil {
			return err
		}
		stmts = append(stmts, s)
		plans = append(plans, plan)
		if len(w.node.manyChildren()) > 0 {
			deeper = true
		}
	}
	if err := run.batch(ctx, stmts, !deeper); err != nil {
		return err
	}
	var nextWork []pending
	for i, w := range work {
		plan := plans[i]
		for _, n := range w.node.manyChildren() {
			rows := plan.rows[n.parent.idx]
			if len(rows) > 0 {
				nextWork = append(nextWork, pending{node: n, parents: rows})
			}
		}
		if err := plan.afterLoad(ctx); err != nil {
			return err
		}
	}
	if len(nextWork) == 0 {
		return nil
	}
	return loadPending(ctx, run, nextWork)
}

// manyStmt builds the statement that loads a to-many node for its parents.
func manyStmt(ctx context.Context, run *runner, w pending) (*stmt, *selectPlan, error) {
	n := w.node
	parentTable := n.parent.table
	rel := &parentTable.Relations[n.rel]
	if rel.Kind == schema.ManyToMany {
		return m2mStmt(ctx, run, w)
	}
	if len(rel.Local) != 1 {
		return nil, nil, errf("relation %s.%s: to-many loads need a single-column key",
			parentTable.Name, rel.Name)
	}
	local, remote := rel.Local[0], rel.Remote[0]

	byKey := make(map[any][]unsafe.Pointer, len(w.parents))
	keys := make([]any, 0, len(w.parents))
	for _, p := range w.parents {
		k := parentTable.Value(p, local)
		if k == nil {
			continue
		}
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], p)
	}
	keyArg := typedSlice(n.table.Columns[remote].Kind, keys)

	q := n.spec
	if q == nil {
		q = &query{ref: n.ref}
	}
	q2 := *q
	q2.ref = n.ref
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	perParent := q2.limit
	var plan *selectPlan
	if perParent > 0 {
		plan = lateralSelect(r, &q2, n, keyArg, remote)
	} else {
		plan = q2.renderSelect(r, n, selectOpts{
			extraWhere: func(r *render, alias string) {
				r.s(alias)
				r.b('.')
				r.s(n.table.Columns[remote].Quoted)
				r.s(" = ANY(")
				r.arg(keyArg)
				r.b(')')
			},
		})
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	plan.track(n)
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: n.table.Name,
		customPlan: q.customPlan}
	child := n.table
	relIdx := n.rel
	s.scan = func(rows pgx.Rows) error {
		return plan.scan(ctx, rows, func(row unsafe.Pointer, _ any) {
			k := child.Value(row, remote)
			for _, p := range byKey[k] {
				parentTable.Attach(p, relIdx, row)
			}
		})
	}
	return s, plan, nil
}

// lateralSelect loads at most q.limit rows per parent key.
func lateralSelect(r *render, q *query, n *lnode, keyArg any, remote int32) *selectPlan {
	keyAlias := r.newAlias()
	var plan *selectPlan
	body := r.section(func() {
		plan = q.renderSelect(r, n, selectOpts{
			extraWhere: func(r *render, alias string) {
				r.s(alias)
				r.b('.')
				r.s(n.table.Columns[remote].Quoted)
				r.s(" = ")
				r.s(keyAlias)
				r.s(".k")
			},
		})
	})
	outer := r.newAlias()
	r.s("SELECT ")
	r.s(outer)
	r.s(".* FROM unnest(")
	r.arg(keyArg)
	r.s("::")
	r.s(arrayType(n.table.Columns[remote].Kind))
	r.s(") AS ")
	r.s(keyAlias)
	r.s("(k) CROSS JOIN LATERAL (")
	r.buf = append(r.buf, body...)
	r.s(") ")
	r.s(outer)
	return plan
}

func arrayType(k schema.Kind) string {
	switch k {
	case schema.KindInt64:
		return "int8[]"
	case schema.KindInt32:
		return "int4[]"
	case schema.KindInt16:
		return "int2[]"
	}
	return "text[]"
}

func typedSlice(k schema.Kind, keys []any) any {
	switch k {
	case schema.KindString:
		out := make([]string, 0, len(keys))
		for _, v := range keys {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case schema.KindInt64:
		out := make([]int64, 0, len(keys))
		for _, v := range keys {
			if n, ok := v.(int64); ok {
				out = append(out, n)
			}
		}
		return out
	case schema.KindInt32:
		out := make([]int32, 0, len(keys))
		for _, v := range keys {
			if n, ok := v.(int32); ok {
				out = append(out, n)
			}
		}
		return out
	}
	return keys
}

// countStmt counts the query's rows, stopping at limit when limit > 0.
func (q *query) countStmt(ctx context.Context, db *DB, sc *scopeState, limit int64) (*stmt, error) {
	r := newRender(sc, ctx, db)
	defer r.recycle()
	inner := r.section(func() {
		q.renderSelect(r, newRoot(q), selectOpts{countOnly: true, countCap: limit})
	})
	if r.err != nil {
		return nil, r.err
	}
	if limit > 0 || len(q.group) > 0 || len(q.distinctOn) > 0 {
		r.s("SELECT count(*) FROM (")
		r.buf = append(r.buf, inner...)
		r.s(") c")
	} else {
		r.buf = append(r.buf, inner...)
	}
	return &stmt{sql: string(r.buf), args: r.argsCopy(), op: "count", table: q.ref.Table.Name,
		customPlan: q.customPlan}, nil
}

func (q *query) count(ctx context.Context, db *DB, limit int64) (int64, error) {
	ctx, cancel := q.checkTimeout(ctx)
	defer cancel()
	run, err := db.newRunner(ctx, false)
	if err != nil {
		return 0, err
	}
	s, err := q.countStmt(ctx, db, run.sc, limit)
	if err != nil {
		return 0, run.close(ctx, err)
	}
	var n int64
	s.scan = func(rows pgx.Rows) error {
		if rows.Next() {
			return rows.Scan(&n)
		}
		return nil
	}
	err = run.one(ctx, s, true)
	return n, run.close(ctx, err)
}

// update writes UPDATE … SET … WHERE for the query's filter.
func (q *query) update(ctx context.Context, db *DB, sets []Assignment) (int64, error) {
	if len(sets) == 0 {
		return 0, errf("Update needs at least one assignment")
	}
	if !hasPreds(q.where) {
		return 0, errf("Update needs a Where; use Where(keel.True()) to update every row")
	}
	ctx, cancel := q.checkTimeout(ctx)
	defer cancel()
	t := q.ref.Table
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th, OpUpdate)
	if err != nil {
		return 0, err
	}
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	sc, alias := r.push(q.ref, joinFrom, q.allTenants)
	var changed []int32
	setSQL := r.section(func() {
		touched := make(map[int32]bool, len(sets))
		for i, a := range sets {
			if a.col.Ref != q.ref {
				r.fail(errf("Update assigns a column of %s on a query of %s",
					a.col.Ref.name(), q.ref.name()))
				return
			}
			if i > 0 {
				r.s(", ")
			}
			r.s(t.Columns[a.col.Index].Quoted)
			r.s(" = ")
			a.e(r)
			touched[a.col.Index] = true
			changed = append(changed, a.col.Index)
		}
		if t.AutoUpdate >= 0 && !touched[t.AutoUpdate] {
			r.s(", ")
			r.s(t.Columns[t.AutoUpdate].Quoted)
			r.s(" = ")
			r.arg(stampValue(t.Columns[t.AutoUpdate].Kind, db.now()))
		}
		if t.Version >= 0 && !touched[t.Version] {
			r.s(", ")
			r.s(t.Columns[t.Version].Quoted)
			r.s(" = ")
			r.s(alias)
			r.b('.')
			r.s(t.Columns[t.Version].Quoted)
			r.s(" + 1")
		}
	})
	where := r.section(func() {
		wrote := r.tenantPred(t, alias, q.allTenants)
		wrote = r.preds(r.intercepted(t, OpUpdate, false), wrote)
		r.preds(q.where, wrote)
	})
	var conds []byte
	from := r.section(func() { conds = r.writeFromJoins(sc, " FROM ") })
	r.pop()
	if r.err != nil {
		return 0, run.close(ctx, r.err)
	}
	r.s("UPDATE ")
	r.s(t.Quoted)
	r.b(' ')
	r.s(alias)
	r.s(" SET ")
	r.buf = append(r.buf, setSQL...)
	r.buf = append(r.buf, from...)
	r.s(" WHERE ")
	r.buf = append(r.buf, where...)
	if len(conds) > 0 {
		r.s(" AND ")
		r.buf = append(r.buf, conds...)
	}
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "update", table: t.Name}
	bh := bulkHooks(r, s, th, OpUpdate, t, alias, true)
	err = run.one(ctx, s, !th.has(OpUpdate, AfterWrite))
	if err == nil && bh != nil {
		err = db.afterWrite(ctx, run, th, OpUpdate, bh.rows, bh.olds, changed)
	}
	return s.tag.RowsAffected(), run.close(ctx, err)
}

// delete writes DELETE … WHERE for the query's filter.
func (q *query) delete(ctx context.Context, db *DB) (int64, error) {
	if !hasPreds(q.where) {
		return 0, errf("Delete needs a Where; use Where(keel.True()) to delete every row")
	}
	ctx, cancel := q.checkTimeout(ctx)
	defer cancel()
	t := q.ref.Table
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th, OpDelete)
	if err != nil {
		return 0, err
	}
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	sc, alias := r.push(q.ref, joinFrom, q.allTenants)
	where := r.section(func() {
		wrote := r.tenantPred(t, alias, q.allTenants)
		wrote = r.preds(r.intercepted(t, OpDelete, false), wrote)
		r.preds(q.where, wrote)
	})
	var conds []byte
	using := r.section(func() { conds = r.writeFromJoins(sc, " USING ") })
	r.pop()
	if r.err != nil {
		return 0, run.close(ctx, r.err)
	}
	r.s("DELETE FROM ")
	r.s(t.Quoted)
	r.b(' ')
	r.s(alias)
	r.buf = append(r.buf, using...)
	r.s(" WHERE ")
	r.buf = append(r.buf, where...)
	if len(conds) > 0 {
		r.s(" AND ")
		r.buf = append(r.buf, conds...)
	}
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "delete", table: t.Name}
	bh := bulkHooks(r, s, th, OpDelete, t, alias, false)
	err = run.one(ctx, s, !th.has(OpDelete, AfterWrite))
	if err == nil && bh != nil {
		err = db.afterWrite(ctx, run, th, OpDelete, bh.rows, bh.olds, nil)
	}
	return s.tag.RowsAffected(), run.close(ctx, err)
}

// bulkRows holds the rows a bulk write returned for its hooks.
type bulkRows struct {
	rows []unsafe.Pointer
	olds []unsafe.Pointer
}

// bulkHooks extends a bulk UPDATE or DELETE with RETURNING when the table has AfterWrite or
// Committed hooks for op: the written rows, and for updates the previous values of the columns the
// hooks need (RETURNING old.…, which needs PostgreSQL 18). It returns nil when no hook needs the
// rows.
func bulkHooks(r *render, s *stmt, th *tableHooks, op Op, t *schema.Table, alias string,
	update bool,
) *bulkRows {
	if !th.has(op, AfterWrite) && !th.has(op, Committed) {
		return nil
	}
	cols := t.Select
	var needs []int32
	if update {
		needs = th.needs
	}
	r.s(" RETURNING ")
	for i, c := range cols {
		if i > 0 {
			r.s(", ")
		}
		r.s(alias)
		r.b('.')
		r.s(t.Columns[c].Quoted)
	}
	for _, c := range needs {
		r.s(", old.")
		r.s(t.Columns[c].Quoted)
	}
	s.sql = string(r.buf)
	s.args = r.args
	out := &bulkRows{}
	s.scan = func(rows pgx.Rows) error {
		for rows.Next() {
			row := t.New()
			var old unsafe.Pointer
			if len(needs) > 0 {
				old = t.New()
			}
			if err := scanInto(rows, t, row, cols, old, needs); err != nil {
				return err
			}
			out.rows = append(out.rows, row)
			if old != nil {
				out.olds = append(out.olds, old)
			}
		}
		return nil
	}
	return out
}

func hasPreds(ps []Pred) bool {
	for _, p := range ps {
		if p != nil {
			return true
		}
	}
	return false
}

// m2mStmt loads a many-to-many node through its join table.
func m2mStmt(ctx context.Context, run *runner, w pending) (*stmt, *selectPlan, error) {
	n := w.node
	parentTable := n.parent.table
	rel := &parentTable.Relations[n.rel]
	local, remote := rel.Local[0], rel.Remote[0]
	byKey := make(map[any][]unsafe.Pointer, len(w.parents))
	keys := make([]any, 0, len(w.parents))
	for _, p := range w.parents {
		k := parentTable.Value(p, local)
		if k == nil {
			continue
		}
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], p)
	}
	keyArg := typedSlice(parentTable.Columns[local].Kind, keys)

	q := n.spec
	if q == nil {
		q = &query{ref: n.ref}
	}
	q2 := *q
	q2.ref = n.ref
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	jalias := r.newAlias()
	plan := q2.renderSelect(r, n, selectOpts{
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
			r.s(n.table.Columns[remote].Quoted)
		},
		extraWhere: func(r *render, alias string) {
			r.s(jalias)
			r.b('.')
			r.s(rel.JoinLocalQ[0])
			r.s(" = ANY(")
			r.arg(keyArg)
			r.b(')')
		},
	})
	if r.err != nil {
		return nil, nil, r.err
	}
	plan.track(n)
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: n.table.Name}
	relIdx := n.rel
	s.scan = func(rows pgx.Rows) error {
		return plan.scan(ctx, rows, func(row unsafe.Pointer, key any) {
			for _, p := range byKey[key] {
				parentTable.Attach(p, relIdx, row)
			}
		})
	}
	return s, plan, nil
}
