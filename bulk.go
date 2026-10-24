package keel

import (
	"context"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
)

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
	run, err := db.writeRunner(ctx, th.has(OpUpdate, AfterWrite))
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
	bh := bulkHooks(r, s, th, OpUpdate, t, alias)
	n, err := run.bulk(ctx, s, t, alias, bh, !th.has(OpUpdate, AfterWrite))
	if err == nil && bh != nil {
		err = db.afterWrite(ctx, run, th, OpUpdate, bh.rows, bh.olds, changed)
	}
	return n, run.close(ctx, err)
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
	bh := bulkHooks(r, s, th, OpDelete, t, alias)
	n, err := run.bulk(ctx, s, t, alias, bh, !th.has(OpDelete, AfterWrite))
	if err == nil && bh != nil {
		err = db.afterWrite(ctx, run, th, OpDelete, bh.rows, bh.olds, nil)
	}
	return n, run.close(ctx, err)
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
