package keel

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stmt is one statement of a batch.
type stmt struct {
	sql   string
	args  []any
	op    string
	table string

	scan func(rows pgx.Rows) error
	tag  pgconn.CommandTag

	customPlan bool
}

// runner executes the statements of one terminal call. It joins the transaction on the context,
// opens an implicit one when Config.OnBegin is set, or runs on a single pooled connection: a
// replica's for a read on a context marked with OnReplica, else the primary's.
type runner struct {
	db   *DB
	sc   *scopeState
	tx   *txState
	own  bool
	conn *pgxpool.Conn
	rep  *replica
}

// runMode is what a runner runs: a read, which may go to a replica; a write, which records its
// commit position on a context marked with OnReplica; a statement keel cannot see into (Exec),
// which does neither; or a read that must see the primary.
type runMode uint8

// The run modes, in the order runMode describes them.
const (
	modeRead runMode = iota
	modeWrite
	modeExec
	modePrimaryRead
)

// newRunner returns the runner of a read or a write terminal on ctx.
func (db *DB) newRunner(ctx context.Context, write bool) (*runner, error) {
	if write {
		return db.runnerFor(ctx, modeWrite)
	}
	return db.runnerFor(ctx, modeRead)
}

// runnerFor returns the runner of a terminal of the given mode on ctx.
func (db *DB) runnerFor(ctx context.Context, mode runMode) (*runner, error) {
	write := mode == modeWrite || mode == modeExec
	r := &runner{db: db, sc: db.resolveScope(ctx)}
	if tx := txFrom(ctx, db); tx != nil {
		if write && tx.readOnly {
			return nil, ErrReadOnly
		}
		r.tx = tx
		return r, nil
	}
	var rt *route
	if db.replicas != nil {
		rt = routeFrom(ctx)
		if rt != nil && mode == modeRead {
			r.rep = db.replicas.pick(rt.mark(db), true)
		}
		if mode != modeWrite {
			rt = nil
		}
	}
	if db.cfg.OnBegin != nil || rt != nil {
		r.tx = &txState{db: db, readOnly: !write, implicit: true, scopeCtx: ctx, rep: r.rep, route: rt}
		r.own = true
	}
	return r, nil
}

// replica returns the replica the runner's statements run on, or nil for the primary.
func (r *runner) replica() *replica {
	if r.tx != nil {
		return r.tx.rep
	}
	return r.rep
}

// touch records a (tenant, table) pair the runner's write changed: in the transaction, to reach the
// versioner when it commits, or in the call's scope, to reach it when the runner closes.
func (r *runner) touch(k versionKey) {
	if r.tx != nil {
		r.tx.touch(k)
		return
	}
	if r.sc.changes == nil {
		r.sc.changes = versionSet{}
	}
	r.sc.changes[k] = struct{}{}
}

func (r *runner) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if r.tx != nil {
		return r.tx.acquire(ctx)
	}
	if r.conn != nil {
		return r.conn, nil
	}
	pool := r.db.pool
	if r.rep != nil {
		pool = r.rep.pool
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	r.conn = conn
	return conn, nil
}

// started returns the runner's connection with its transaction, if it has one, begun, for a
// statement that cannot join a batch, such as COPY.
func (r *runner) started(ctx context.Context) (*pgxpool.Conn, error) {
	if r.tx != nil {
		if err := r.tx.begin(ctx); err != nil {
			return nil, err
		}
	}
	return r.acquire(ctx)
}

// batch sends stmts in one round trip. last marks the runner's final batch: an implicit
// transaction commits in the same round trip.
func (r *runner) batch(ctx context.Context, stmts []*stmt, last bool) error {
	if len(stmts) == 0 {
		return nil
	}
	custom := false
	if r.db.cfg.PlanCache != PlanCacheOff {
		for _, s := range stmts {
			custom = custom || s.customPlan
		}
	}
	if custom && r.tx == nil {
		r.tx = &txState{db: r.db, readOnly: true, implicit: true, conn: r.conn, rep: r.rep}
		r.conn = nil
		r.own = true
	}
	guard := r.replicaGuard(ctx)
	conn, err := r.acquire(ctx)
	if err != nil {
		return err
	}
	if len(stmts) == 1 && guard == 0 && (r.tx == nil || r.tx.begun) && !(r.own && last) && !custom {
		return r.direct(ctx, conn, stmts[0])
	}
	events, qctx := r.db.startEvents(ctx, r.replica(), stmts)
	start := time.Now()
	err = r.send(qctx, conn, stmts, last, custom, guard)
	if err == errReplicaBehind {
		r.toPrimary(ctx)
		retarget(events)
		if conn, err = r.acquire(ctx); err == nil {
			err = r.send(qctx, conn, stmts, last, custom, 0)
		}
	}
	r.db.endEvents(events, stmts, time.Since(start), err)
	return err
}

// errReplicaBehind is the error of a guarded round trip on a replica that had not replayed the
// context's commit position.
var errReplicaBehind = errors.New("keel: the replica is behind the context's commit position")

// replicaGuardSQL fails, aborting the rest of its pipeline, when the replica has not replayed the
// position $1.
const replicaGuardSQL = "SELECT 1 / COALESCE((pg_last_wal_replay_lsn() - '0/0')::int8 >= $1, false)::int"

// replicaGuard returns the commit position the runner's first round trip on a replica must check,
// or zero: a replica is picked before the lag tracker has seen it replay the context's position
// when the write is older than the replica's lag, and the check rides in the same round trip.
func (r *runner) replicaGuard(ctx context.Context) Position {
	if r.rep == nil || r.conn != nil || r.tx != nil && r.tx.conn != nil {
		return 0
	}
	floor := routeFrom(ctx).mark(r.db).pos
	if Position(r.rep.replay.Load()) >= floor {
		return 0
	}
	return floor
}

// toPrimary moves a runner whose replica failed its guard to the primary: it rolls back the
// implicit transaction begun on the replica, releases the replica and counts a fallback.
func (r *runner) toPrimary(ctx context.Context) {
	r.db.replicas.fallbacks.Add(1)
	if r.conn != nil {
		r.conn.Release()
		r.conn = nil
	}
	if r.own {
		old := r.tx
		_ = old.end(ctx, errReplicaBehind)
		r.tx = &txState{db: r.db, readOnly: old.readOnly, implicit: true, scopeCtx: old.scopeCtx}
	}
	r.rep.release()
	r.rep = nil
}

// send sends stmts on conn in one round trip: BEGIN, the transaction options and the OnBegin
// statements when the transaction begins, the guard when guard is set, the statements with their
// plan-mode switches, and COMMIT when the runner's implicit transaction ends with this batch.
func (r *runner) send(ctx context.Context, conn *pgxpool.Conn, stmts []*stmt, last, custom bool,
	guard Position,
) error {
	beginning := r.tx != nil && !r.tx.begun
	commit := r.own && last
	b := &pgx.Batch{}
	pre := 0
	if beginning {
		var err error
		if pre, err = r.tx.queueBegin(b); err != nil {
			return err
		}
	}
	if guard > 0 {
		b.Queue(replicaGuardSQL, int64(guard))
	}
	for _, s := range stmts {
		if s.customPlan && custom {
			b.Queue("SELECT set_config('plan_cache_mode', 'force_custom_plan', true)")
		}
		b.Queue(s.sql, s.args...)
		if s.customPlan && custom {
			b.Queue("SELECT set_config('plan_cache_mode', $1, true)", r.db.planMode())
		}
	}
	if commit {
		b.Queue("COMMIT")
		if r.tx.route != nil {
			b.Queue(commitPositionSQL)
		}
		r.tx.commitTried = true
	}
	br := conn.SendBatch(ctx, b)
	runErr := r.readBatch(br, pre, guard > 0, stmts, commit, custom)
	closeErr := br.Close()
	if runErr == nil && !(commit && r.tx.committed) {
		runErr = closeErr
	}
	if beginning {
		r.tx.begun = true
	}
	return runErr
}

// readBatch reads the results of a round trip send queued, in order.
func (r *runner) readBatch(br pgx.BatchResults, pre int, guarded bool, stmts []*stmt, commit,
	custom bool,
) error {
	for range pre {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	if guarded {
		if _, err := br.Exec(); err != nil {
			return errReplicaBehind
		}
	}
	for _, s := range stmts {
		wrap := s.customPlan && custom
		if wrap {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
		if err := readOne(br, s); err != nil {
			return err
		}
		if wrap {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
	}
	if commit {
		if _, err := br.Exec(); err != nil {
			return err
		}
		r.tx.committed = true
		if rt := r.tx.route; rt != nil {
			var lsn int64
			err := br.QueryRow().Scan(&lsn)
			r.db.recordCommit(rt, lsn, err == nil)
		}
	}
	return nil
}

func readOne(br pgx.BatchResults, s *stmt) error {
	if s.scan == nil {
		tag, err := br.Exec()
		if err != nil {
			return err
		}
		s.tag = tag
		return nil
	}
	rows, err := br.Query()
	if err != nil {
		return err
	}
	return scanRows(rows, s)
}

// scanRows runs a statement's scanner over rows. A server error surfaces on the rows, so it wins
// over the scanner's error, which it may have caused.
func scanRows(rows pgx.Rows, s *stmt) error {
	scanErr := s.scan(rows)
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	s.tag = rows.CommandTag()
	return nil
}

// direct sends one statement outside a pipeline. pgx's pipeline keeps per-message bookkeeping a
// lone statement does not need.
func (r *runner) direct(ctx context.Context, conn *pgxpool.Conn, s *stmt) error {
	stmts := []*stmt{s}
	events, qctx := r.db.startEvents(ctx, r.replica(), stmts)
	start := time.Now()
	var err error
	if s.scan == nil {
		s.tag, err = conn.Exec(qctx, s.sql, s.args...)
	} else {
		var rows pgx.Rows
		rows, err = conn.Query(qctx, s.sql, s.args...)
		if err == nil {
			err = scanRows(rows, s)
		}
	}
	r.db.endEvents(events, stmts, time.Since(start), err)
	return err
}

// one sends a single statement.
func (r *runner) one(ctx context.Context, s *stmt, last bool) error {
	return r.batch(ctx, []*stmt{s}, last)
}

// close ends an implicit transaction (committing when the last batch did not) and releases the
// connection and the replica. It returns err, joined with any error from ending the transaction.
// Outside a transaction the pairs the runner's write changed go to the versioner even when err is
// set: a statement may commit before its error reaches keel, and a version bumped for nothing
// costs only a cache miss.
func (r *runner) close(ctx context.Context, err error) error {
	if r.conn != nil {
		r.conn.Release()
		r.conn = nil
	}
	if r.rep != nil {
		r.rep.release()
		r.rep = nil
	}
	if r.sc.changes != nil {
		r.db.versions.add(r.sc.changes)
		r.sc.changes = nil
	}
	if !r.own {
		return err
	}
	err = r.tx.end(ctx, err)
	if err == nil {
		r.tx.runAfterCommit(ctx)
	}
	return err
}
