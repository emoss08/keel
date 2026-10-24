package keel

import (
	"context"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5"
)

// fetchOpts adds keyset pagination, hidden values and a capped count to a fetch.
type fetchOpts struct {
	keyset    func(r *render)
	extraCols []int32
	// tail are expressions selected after the rows' columns; tailScan decodes them for each row.
	tail     []Expr
	tailScan tailScanner
	limit    int64
	countCap int64
	count    *int64
	// sizeHint overrides the expected row count (One expects one row but limits to two).
	sizeHint int
}

// rowHint is the number of base rows the fetch expects: the size hint, else the limit the
// statement renders, at most one slab.
func (fo *fetchOpts) rowHint(q *query) int {
	if fo.sizeHint > 0 {
		return fo.sizeHint
	}
	limit := fo.limit
	if limit == 0 {
		limit = q.limit
	}
	return int(min(max(limit, 0), maxSlab))
}

// cardinality is the number of rows a read must return.
type cardinality uint8

const (
	anyRows cardinality = iota
	// firstRow needs at least one row (First).
	firstRow
	// oneRow needs exactly one row (One, IntoOne).
	oneRow
)

// read is one terminal's share of a round trip: the statements it sends, the plan whose to-many
// relations load after them, the timeout that bounds it, the rows it must return, and finish,
// which fills its destination once every read's statements, relation loads and AfterLoad hooks
// have run.
type read struct {
	stmts   []*stmt
	plan    *selectPlan
	many    []*lnode
	timeout time.Duration
	want    cardinality
	rows    int
	finish  func()
}

// check returns the error of a read that returned the wrong number of rows.
func (rd *read) check() error {
	switch {
	case rd.want != anyRows && rd.rows == 0:
		return ErrNotFound
	case rd.want == oneRow && rd.rows > 1:
		return ErrNotSingular
	}
	return nil
}

// runReads prepares items on one runner and sends all their statements in one round trip. It then
// checks the number of rows of single-row items, loads the items' to-many relations a level at a
// time, each level's statements of every item sharing a round trip, runs AfterLoad hooks and fills
// the destinations, only when every step succeeded. The shortest timeout of the items bounds the
// whole call.
func runReads(ctx context.Context, db *DB, items []BatchItem) error {
	if len(items) == 0 {
		return nil
	}
	run, err := db.newRunner(ctx, false)
	if err != nil {
		return err
	}
	reads := make([]*read, len(items))
	var stmts []*stmt
	var timeout time.Duration
	many := false
	for i, it := range items {
		if it.prepare == nil {
			return run.close(ctx, errf("Batch item %d is empty", i))
		}
		rd, err := it.prepare(ctx, run)
		if err != nil {
			return run.close(ctx, err)
		}
		reads[i] = rd
		if len(items) == 1 {
			stmts = rd.stmts
		} else {
			stmts = append(stmts, rd.stmts...)
		}
		if rd.timeout > 0 && (timeout == 0 || rd.timeout < timeout) {
			timeout = rd.timeout
		}
		many = many || len(rd.many) > 0
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	err = run.batch(ctx, stmts, !many)
	for _, rd := range reads {
		if err == nil {
			err = rd.check()
		}
	}
	if err == nil && many {
		var work []pending
		for _, rd := range reads {
			if len(rd.many) > 0 {
				work = append(work, pendingOf(rd.plan, rd.many)...)
			}
		}
		err = loadPending(ctx, run, work)
	}
	for _, rd := range reads {
		if err == nil && rd.plan != nil {
			err = rd.plan.afterLoad(ctx)
		}
	}
	if err = run.close(ctx, err); err != nil {
		return err
	}
	for _, rd := range reads {
		rd.finish()
	}
	return nil
}

// prepareRows renders the query's SELECT and, when fo asks for one, its capped count. After the
// round trip emit has received every row of the base table in order.
func (q *query) prepareRows(ctx context.Context, run *runner, fo fetchOpts, emit func(unsafe.Pointer)) (*read, error) {
	if q.projection() || q.ref.Table.New == nil {
		return nil, errf("a query that picks expressions other than its own columns needs keel.Into")
	}
	root := newRoot(q)
	if err := buildTree(root, q.loads); err != nil {
		return nil, err
	}

	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	plan := q.renderSelect(r, root, selectOpts{keyset: fo.keyset, extraCols: fo.extraCols, limit: fo.limit,
		tail: fo.tail})
	if r.err != nil {
		return nil, r.err
	}
	plan.tailScan = fo.tailScan
	plan.track()
	plan.sizeHint = fo.rowHint(q)
	main := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: root.table.Name,
		customPlan: q.customPlan}
	rd := &read{stmts: []*stmt{main}, plan: plan, many: root.manyChildren(), timeout: q.timeout}
	main.scan = func(rows pgx.Rows) error {
		return plan.scan(run.sc, main.sql, rows, func(row unsafe.Pointer, _ relKey) {
			rd.rows++
			emit(row)
		})
	}

	if fo.count != nil {
		cs, err := q.countStmt(ctx, run.db, run.sc, fo.countCap)
		if err != nil {
			return nil, err
		}
		cs.scan = scanCount(fo.count)
		rd.stmts = append(rd.stmts, cs)
	}
	return rd, nil
}

// fetchInto prepares the query's rows read, needing want rows; its finish hands the rows, typed,
// to set.
func fetchInto[T any](ctx context.Context, run *runner, q *query, fo fetchOpts, want cardinality,
	set func(rows []*T),
) (*read, error) {
	out := make([]*T, 0, fo.rowHint(q))
	rd, err := q.prepareRows(ctx, run, fo, func(p unsafe.Pointer) { out = append(out, (*T)(p)) })
	if err != nil {
		return nil, err
	}
	rd.want = want
	rd.finish = func() { set(out) }
	return rd, nil
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

// countItem counts the query's rows, stopping at limit when limit > 0, and hands the count to set.
func countItem(q *query, limit int64, set func(n int64)) BatchItem {
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		s, err := q.countStmt(ctx, run.db, run.sc, limit)
		if err != nil {
			return nil, err
		}
		var n int64
		s.scan = scanCount(&n)
		return &read{stmts: []*stmt{s}, timeout: q.timeout, finish: func() { set(n) }}, nil
	}}
}

// scanCount reads a count statement's single value into n.
func scanCount(n *int64) func(rows pgx.Rows) error {
	return func(rows pgx.Rows) error {
		if rows.Next() {
			return rows.Scan(n)
		}
		return nil
	}
}
