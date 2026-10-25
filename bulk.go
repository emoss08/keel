package keel

import (
	"context"
	"fmt"
	"slices"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// update writes UPDATE … SET … WHERE for the query's filter.
func (q *query) update(ctx context.Context, db *DB, sets []Assignment) (int64, error) {
	if err := q.checkUpdate(sets); err != nil {
		return 0, err
	}
	ctx, cancel := q.checkTimeout(ctx)
	defer cancel()
	t := q.ref.Table
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th.has(OpUpdate, AfterWrite))
	if err != nil {
		return 0, err
	}
	defer run.abandon()
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	alias, changed := q.writeUpdate(r, db, sets)
	if r.err != nil {
		return 0, run.close(ctx, r.err)
	}
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "update", table: t.Name}
	bh := bulkHooks(r, s, th, OpUpdate, t, alias)
	n, err := run.bulk(ctx, s, t, alias, bh, !th.has(OpUpdate, AfterWrite))
	if err == nil && bh != nil {
		err = db.afterWrite(ctx, run, th, OpUpdate, bh.rows, bh.olds, changed)
	}
	return n, run.close(ctx, err)
}

// checkUpdate refuses a bulk update without assignments or without a filter.
func (q *query) checkUpdate(sets []Assignment) error {
	if len(sets) == 0 {
		return errf("Update needs at least one assignment")
	}
	if !hasPreds(q.where) {
		return errf("Update needs a Where; use Where(keel.True()) to update every row")
	}
	return nil
}

// writeUpdate writes [WITH …] UPDATE … SET … [FROM …] WHERE … for the query's filter into r and
// returns the updated table's alias and the columns the assignments name. The autoupdate column
// is stamped and the version column incremented unless an assignment names them.
func (q *query) writeUpdate(r *render, db *DB, sets []Assignment) (string, []int32) {
	t := q.ref.Table
	sc, alias := r.push(q.ref, joinFrom, q.allTenants)
	joined := r.bulkJoins(sc, q)
	var changed []int32
	setSQL := r.section(func() {
		touched := make(map[int32]bool, len(sets))
		for i, a := range sets {
			if a.col.Ref != q.ref {
				r.fail(errf("Update assigns a column of %s on a query of %s",
					a.col.Ref.name(), q.ref.name()))
				return
			}
			if !t.Columns[a.col.Index].Stored() {
				r.fail(errf("Update assigns the computed field %s.%s", t.Name, t.Columns[a.col.Index].Name))
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
		wrote = r.preds(q.where, wrote)
		r.bulkJoinConds(q, joined, wrote)
	})
	var conds []byte
	from := r.section(func() {
		conds = r.writeFromJoins(sc, " FROM ")
		r.writeBulkJoins(q, joined, len(sc.joins) > 0, " FROM ")
	})
	with := r.section(func() { writeWith(r, q.ctes) })
	r.pop()
	r.buf = append(r.buf, with...)
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
	r.release(with, setSQL, from, where, conds)
	return alias, changed
}

// updateReturning runs the query's bulk update and reads the updated rows in the same statement:
// WITH keel_upd AS (UPDATE … RETURNING alias.*[, old.c AS keel_old_N…]) SELECT … FROM keel_upd
// alias, with the query's to-one loads joined to it and its order, then the to-many loads. emit
// receives each row in order. The update applies the tenant predicate, the update interceptors
// and the stamps, so the read applies neither tenant predicate nor read interceptors to the
// updated rows; the relations it joins and loads get theirs, read as of the statement's snapshot,
// before the update.
func (q *query) updateReturning(ctx context.Context, db *DB, sets []Assignment, emit func(unsafe.Pointer)) error {
	if err := q.checkUpdate(sets); err != nil {
		return err
	}
	switch {
	case q.projection() || q.ref.Table.New == nil:
		return errf("UpdateReturning returns rows; Pick only columns of %s", q.ref.Table.Name)
	case q.limit > 0 || q.offset > 0:
		return errf("UpdateReturning updates every matching row; remove Limit and Offset")
	case q.lock != "" || len(q.group) > 0 || len(q.having) > 0 || len(q.distinctOn) > 0 || q.distinct:
		return errf("UpdateReturning takes no Lock, Group, Having or DistinctOn")
	}
	ctx, cancel := q.checkTimeout(ctx)
	defer cancel()
	t := q.ref.Table
	th := db.hooks.forTable(t)
	after := th.has(OpUpdate, AfterWrite)
	hooked := after || th.has(OpUpdate, Committed)
	run, err := db.writeRunner(ctx, after)
	if err != nil {
		return err
	}
	defer run.abandon()

	outer := *q
	outer.where, outer.joins, outer.ctes = nil, nil, nil
	root := newRoot(&outer)
	if err := buildTree(root, outer.loads); err != nil {
		return run.close(ctx, err)
	}
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	var changed []int32
	var needs []int32
	body := r.section(func() {
		var alias string
		alias, changed = q.writeUpdate(r, db, sets)
		r.s(" RETURNING ")
		r.s(alias)
		r.s(".*")
		if hooked {
			needs = th.needs
		}
		for i, c := range needs {
			r.s(", old.")
			r.s(t.Columns[c].Quoted)
			r.s(" AS keel_old_")
			r.int(int64(i))
		}
	})
	clear(r.aliases)
	r.aliases = r.aliases[:0]
	extra := t.Tenant
	if hooked {
		extra = slices.Concat(t.Tenant, t.Select)
	}
	tail := make([]Expr, len(needs))
	for i := range needs {
		tail[i] = exprFunc(func(r *render) {
			r.s(r.alias(q.ref))
			r.s(".keel_old_")
			r.int(int64(i))
		})
	}
	r.s("WITH keel_upd AS (")
	r.buf = append(r.buf, body...)
	r.s(") ")
	r.release(body)
	plan := outer.renderSelect(r, root, selectOpts{extraCols: extra, tail: tail, prefiltered: true,
		from: func(r *render, alias string) {
			r.s("keel_upd ")
			r.s(alias)
		}})
	if r.err != nil {
		return run.close(ctx, r.err)
	}
	plan.track()
	plan.sizeHint = 16
	var rows, olds []unsafe.Pointer
	if len(needs) > 0 {
		plan.tailScan = oldScanner(t, needs, &olds)
	}
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "update", table: t.Name, customPlan: q.customPlan}
	s.scan = func(pr pgx.Rows) error {
		return plan.scan(run.sc, s.sql, pr, func(row unsafe.Pointer, _ relKey) { rows = append(rows, row) })
	}
	many := root.manyChildren()
	err = run.one(ctx, s, !after && len(many) == 0)
	if err != nil {
		run.touchTable(t)
	} else {
		run.touchRows(t, rows)
	}
	if err == nil && hooked {
		err = db.afterWrite(ctx, run, th, OpUpdate, rows, olds, changed)
	}
	if err == nil && len(many) > 0 {
		err = loadPending(ctx, run, pendingOf(plan, many))
	}
	if err == nil {
		err = plan.afterLoad(ctx)
	}
	if err = run.close(ctx, err); err != nil {
		return err
	}
	for _, row := range rows {
		emit(row)
	}
	return nil
}

// oldScanner decodes the previous values of the columns needs, returned after a row's columns,
// into a row of t of their own, appended to olds.
func oldScanner(t *schema.Table, needs []int32, olds *[]unsafe.Pointer) tailScanner {
	var plans []pgtype.ScanPlan
	return func(conn *pgx.Conn, fds []pgconn.FieldDescription, raw [][]byte, _ unsafe.Pointer) error {
		if plans == nil {
			plans = make([]pgtype.ScanPlan, len(needs))
		}
		old := t.New()
		for i, c := range needs {
			dst := t.Ptr(old, c)
			if plans[i] == nil {
				plans[i] = planColumn(conn, fds[i].DataTypeOID, fds[i].Format, &t.Columns[c], dst)
			}
			if err := plans[i].Scan(raw[i], dst); err != nil {
				return fmt.Errorf("keel: scan old %s.%s: %w", t.Name, t.Columns[c].Name, err)
			}
		}
		*olds = append(*olds, old)
		return nil
	}
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
	run, err := db.writeRunner(ctx, th.has(OpDelete, AfterWrite))
	if err != nil {
		return 0, err
	}
	defer run.abandon()
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	sc, alias := r.push(q.ref, joinFrom, q.allTenants)
	joined := r.bulkJoins(sc, q)
	where := r.section(func() {
		wrote := r.tenantPred(t, alias, q.allTenants)
		wrote = r.preds(r.intercepted(t, OpDelete, false), wrote)
		wrote = r.preds(q.where, wrote)
		r.bulkJoinConds(q, joined, wrote)
	})
	var conds []byte
	using := r.section(func() {
		conds = r.writeFromJoins(sc, " USING ")
		r.writeBulkJoins(q, joined, len(sc.joins) > 0, " USING ")
	})
	with := r.section(func() { writeWith(r, q.ctes) })
	r.pop()
	if r.err != nil {
		return 0, run.close(ctx, r.err)
	}
	r.buf = append(r.buf, with...)
	r.release(with)
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
	bh := bulkHooks(r, s, th, OpDelete, t, alias)
	n, err := run.bulk(ctx, s, t, alias, bh, !th.has(OpDelete, AfterWrite))
	if err == nil && bh != nil {
		err = db.afterWrite(ctx, run, th, OpDelete, bh.rows, bh.olds, nil)
	}
	return n, run.close(ctx, err)
}

// bulkJoins gives the explicit joins of a bulk update or delete their aliases in scope sc, so the
// statement's assignments and predicates can use their columns, and returns the aliases. Only
// inner joins of tables and CTEs fit the FROM or USING list of an UPDATE or DELETE.
func (r *render) bulkJoins(sc *scope, q *query) []string {
	aliases := make([]string, len(q.joins))
	for i, j := range q.joins {
		if j.lateral != nil || j.kind != " JOIN " {
			r.fail(errf("bulk updates and deletes take inner joins of tables and CTEs only; %s is%s",
				j.ref.name(), j.kind))
		}
		aliases[i] = r.newAlias(aliasBase(j.ref))
		sc.refs = append(sc.refs, scoped{ref: j.ref, alias: aliases[i]})
	}
	return aliases
}

// writeBulkJoins writes the explicit joins of a bulk update or delete as FROM or USING items
// (keyword), after the relation joins when there are any.
func (r *render) writeBulkJoins(q *query, aliases []string, after bool, keyword string) {
	for i, j := range q.joins {
		if i > 0 || after {
			r.s(", ")
		} else {
			r.s(keyword)
		}
		r.s(r.tableName(j.ref))
		r.b(' ')
		r.s(aliases[i])
	}
}

// bulkJoinConds writes the join conditions of a bulk update's or delete's explicit joins into its
// WHERE clause, with each joined table's tenant predicate.
func (r *render) bulkJoinConds(q *query, aliases []string, wrote bool) {
	for i, j := range q.joins {
		tenant := r.section(func() { r.tenantPred(j.ref.Table, aliases[i], q.allTenants) })
		if len(tenant) > 0 {
			if wrote {
				r.s(" AND ")
			}
			r.buf = append(r.buf, tenant...)
			wrote = true
		}
		r.release(tenant)
		wrote = r.preds(j.on, wrote)
	}
}

// bulk sends a bulk UPDATE, DELETE or INSERT … SELECT on t, written under alias (empty for an
// INSERT), and returns the rows it wrote. It records the data versions the statement changes: the
// scope's tenant, or, for a system-scope write to a tenant table, the tenants of the rows bh
// returned for hooks, else the tenants the statement returns when returnTenants wraps it. A
// failed system-scope write records the table for every tenant, since it may have committed.
func (r *runner) bulk(ctx context.Context, s *stmt, t *schema.Table, alias string, bh *bulkRows,
	last bool,
) (int64, error) {
	if !r.tenantsReturned(t) {
		r.touchTable(t)
		err := r.one(ctx, s, last)
		return s.tag.RowsAffected(), err
	}
	if bh != nil {
		err := r.one(ctx, s, last)
		if err != nil {
			r.touchTable(t)
		} else {
			r.touchRows(t, bh.rows)
		}
		return s.tag.RowsAffected(), err
	}
	var n int64
	r.returnTenants(s, t, func(col string) string {
		if alias == "" {
			return col
		}
		return alias + "." + col
	}, &n)
	err := r.one(ctx, s, last)
	if err != nil {
		r.touchTable(t)
	}
	return n, err
}

// bulkRows holds the rows a bulk write returned for its hooks.
type bulkRows struct {
	rows []unsafe.Pointer
	olds []unsafe.Pointer
}

// bulkHooks extends a bulk UPDATE or DELETE with RETURNING when the table has AfterWrite or
// Committed hooks for op: the written rows (for a delete, the deleted rows), and the previous
// values of the columns the hooks need (RETURNING old.…, which needs PostgreSQL 18). It returns
// nil when no hook needs the rows.
func bulkHooks(r *render, s *stmt, th *tableHooks, op Op, t *schema.Table, alias string) *bulkRows {
	if !th.has(op, AfterWrite) && !th.has(op, Committed) {
		return nil
	}
	cols := t.Select
	needs := th.needs
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
