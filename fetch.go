package keel

import (
	"context"
	"unsafe"

	"github.com/jackc/pgx/v5"
)

// fetchOpts adds keyset pagination and a capped count to a fetch.
type fetchOpts struct {
	keyset    func(r *render)
	extraCols []int32
	limit     int64
	countCap  int64
	count     *int64
	// sizeHint overrides the expected row count (One expects one row but limits to two).
	sizeHint int
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
	plan.track()
	plan.sizeHint = int(min(max(o.limit, q.limit), maxSlab))
	if fo != nil && fo.sizeHint > 0 {
		plan.sizeHint = fo.sizeHint
	}
	main := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: root.table.Name,
		customPlan: q.customPlan}
	main.scan = func(rows pgx.Rows) error {
		return plan.scan(run.sc, rows, func(row unsafe.Pointer, _ relKey) { emit(row) })
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
