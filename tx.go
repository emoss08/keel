package keel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type txKey struct{}

// txState is one transaction. It owns its connection from the first statement to the end and
// is used by one goroutine at a time.
type txState struct {
	db        *DB
	conn      pgConn
	pooled    *pgxpool.Conn
	joined    bool
	readOnly  bool
	implicit  bool
	begun     bool
	committed bool
	opts      txOptions
	scopeCtx  context.Context
	scope     *scopeState

	savepoints int

	afterCommit []func(context.Context)

	// slot is the tenant slot the transaction holds (ownsSlot) or runs under, an enclosing
	// transaction's or write's, when Config.TenantConcurrency is set.
	slot     *tenantSlot
	ownsSlot bool

	rep         *replica
	route       *route
	changes     versionSet
	commitTried bool
}

type txOptions struct {
	lockTimeout time.Duration
	isolation   Isolation
	retries     int
	replica     bool
	detached    bool
	settings    []setting
	err         error
}

type setting struct{ name, value string }

// TxOption configures Read and Write. The isolation levels (ReadCommitted, RepeatableRead,
// Serializable) are options themselves: keel.Read(ctx, db, fn, keel.RepeatableRead).
type TxOption interface {
	applyTx(o *txOptions)
}

// txOptionFunc is a TxOption that is a function.
type txOptionFunc func(*txOptions)

func (f txOptionFunc) applyTx(o *txOptions) { f(o) }

// Isolation is a transaction isolation level. The zero value leaves the level to the server's
// default_transaction_isolation, READ COMMITTED unless the server is configured otherwise. A level
// is a TxOption, and Config.ReadIsolation sets the level of explicit Read transactions that do not
// pass one.
type Isolation uint8

// The isolation levels keel begins transactions with.
const (
	// ReadCommitted runs the transaction at READ COMMITTED: each statement sees what was committed
	// before it began.
	ReadCommitted Isolation = iota + 1
	// RepeatableRead runs the transaction at REPEATABLE READ: every statement sees the snapshot
	// taken by the transaction's first statement. A read-only transaction at this level reads one
	// consistent snapshot across all its statements.
	RepeatableRead
	// Serializable runs the transaction at SERIALIZABLE. Combine it with Retry: a serialization
	// failure is expected under contention.
	Serializable
)

// String returns the level as SQL names it, or "default" for the zero value.
func (l Isolation) String() string {
	switch l {
	case 0:
		return "default"
	case ReadCommitted:
		return "READ COMMITTED"
	case RepeatableRead:
		return "REPEATABLE READ"
	case Serializable:
		return "SERIALIZABLE"
	}
	return "Isolation(" + strconv.Itoa(int(l)) + ")"
}

func (l Isolation) applyTx(o *txOptions) {
	if l > Serializable {
		o.err = errf("unknown isolation level %d", uint8(l))
		return
	}
	o.isolation = l
}

// beginSQL is the BEGIN statement of each isolation level, read-write and read-only, so beginning
// a transaction builds no text.
var beginSQL = [Serializable + 1][2]string{
	{"BEGIN", "BEGIN READ ONLY"},
	{"BEGIN ISOLATION LEVEL READ COMMITTED", "BEGIN ISOLATION LEVEL READ COMMITTED READ ONLY"},
	{"BEGIN ISOLATION LEVEL REPEATABLE READ", "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"},
	{"BEGIN ISOLATION LEVEL SERIALIZABLE", "BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY"},
}

// LockTimeout bounds how long any statement of the transaction waits for a lock.
func LockTimeout(d time.Duration) TxOption {
	return txOptionFunc(func(o *txOptions) { o.lockTimeout = d })
}

// Setting sets the run-time parameter name to value for the transaction, as SET LOCAL does:
// set_config(name, value, true), for a planner or extension setting one query needs, such as
// hnsw.ef_search. A transaction Read or Write begins queues it with BEGIN, in the same round trip.
// A Read or Write that joins a transaction already on the context sets it for fn and restores the
// joined transaction's value when fn returns.
func Setting(name, value string) TxOption {
	return txOptionFunc(func(o *txOptions) {
		if !validSetting(name) {
			o.err = errf("setting name %q must be letters, digits, _ and dots", name)
			return
		}
		o.settings = append(o.settings, setting{name: name, value: value})
	})
}

func validSetting(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c != '_' && c != '.' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// Retry runs a top-level transaction up to n more times after a serialization failure or a
// deadlock. fn must be safe to repeat.
func Retry(n int) TxOption { return txOptionFunc(func(o *txOptions) { o.retries = n }) }

// Detached runs Read or Write in a new transaction on a connection of its own even when the
// context carries a transaction, which it then neither joins nor waits for: it commits or rolls
// back on its own, and its work is not undone when the outer transaction rolls back. It begins
// with the context's scope, so Config.OnBegin sees that scope, and for fn's duration it hides the
// outer transaction: statements, Savepoint, LockKey and AfterCommit inside fn use the detached
// transaction, and its AfterCommit callbacks run when it commits, with the caller's context. Use
// it for work under another scope than the outer transaction's (a system-scoped read inside a
// tenant's transaction) and for work that must commit whatever the caller does (allocating a
// number from a sequence table).
//
// It sees what other transactions committed, not the outer transaction's uncommitted writes, and
// holds a second pool connection while the outer one holds its first: size the pool for it, and do
// not wait inside it on locks the outer transaction holds, which never come free. Outside a
// transaction it is a plain Read or Write.
func Detached() TxOption { return txOptionFunc(func(o *txOptions) { o.detached = true }) }

// Read runs fn in a read-only transaction, on the primary unless ReplicaTx is given, at the
// isolation level an option gives, else Config.ReadIsolation. A Read or Write already on the
// context is joined, unless Detached is given; joining it under another scope than the one it
// began with returns ErrScopeMismatch.
//
// When fn returns, keel sends COMMIT (ROLLBACK when fn failed) and returns without waiting for the
// reply, which a goroutine reads before the connection goes back to the pool; a connection whose
// reply fails is closed. A read-only transaction has nothing left to commit, so the reply carries
// no information the caller needs. Serializable reads, whose COMMIT can report a serialization
// failure, and Config.AwaitReadCommit wait as writes do.
func Read(ctx context.Context, db *DB, fn func(ctx context.Context) error, opts ...TxOption) error {
	return db.runTx(ctx, true, fn, opts)
}

// Write runs fn in a read-write transaction on the primary. A Write already on the context is
// joined, unless Detached is given; a Read is not, and returns ErrReadOnly. Joining a transaction
// under another scope than the one it began with returns ErrScopeMismatch. On a context marked
// with OnReplica, COMMIT reads the commit position in its round trip, for the context's replica
// reads.
func Write(ctx context.Context, db *DB, fn func(ctx context.Context) error, opts ...TxOption) error {
	return db.runTx(ctx, false, fn, opts)
}

// ReadValue is Read for a function that returns a value.
func ReadValue[T any](ctx context.Context, db *DB, fn func(ctx context.Context) (T, error),
	opts ...TxOption,
) (T, error) {
	var out T
	err := db.runTx(ctx, true, func(ctx context.Context) error {
		var err error
		out, err = fn(ctx)
		return err
	}, opts)
	return out, err
}

// WriteValue is Write for a function that returns a value.
func WriteValue[T any](ctx context.Context, db *DB, fn func(ctx context.Context) (T, error),
	opts ...TxOption,
) (T, error) {
	var out T
	err := db.runTx(ctx, false, func(ctx context.Context) error {
		var err error
		out, err = fn(ctx)
		return err
	}, opts)
	return out, err
}

// InTx reports whether ctx carries a transaction of db.
func InTx(ctx context.Context, db *DB) bool { return txFrom(ctx, db) != nil }

// AfterCommit runs fn after the transaction on ctx commits; it is dropped if the transaction rolls
// back. Outside a transaction fn runs at once.
func AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	if tx, ok := ctx.Value(txKey{}).(*txState); ok && tx != nil {
		tx.afterCommit = append(tx.afterCommit, fn)
		return
	}
	fn(ctx)
}

func txFrom(ctx context.Context, db *DB) *txState {
	tx, ok := ctx.Value(txKey{}).(*txState)
	if !ok || tx == nil || tx.db != db {
		return nil
	}
	return tx
}

// checkScope returns ErrScopeMismatch when sc, the scope of a statement or call that would join
// the transaction, differs from the scope the transaction began with. The transaction's
// Config.OnBegin statements (a row-level security setting) were made for its own scope, so joining
// it under another would run under settings made for a different tenant.
func (tx *txState) checkScope(sc *scopeState) error {
	if tx.scope == nil {
		if tx.scopeCtx == nil {
			return nil
		}
		tx.scope = tx.db.resolveScope(tx.scopeCtx)
	}
	if !tx.scope.same(sc) {
		return ErrScopeMismatch
	}
	return nil
}

// txRunner returns a runner of the statements a call sends in tx, the transaction on ctx.
func (db *DB) txRunner(ctx context.Context, tx *txState) (*runner, error) {
	sc := db.resolveScope(ctx)
	if err := tx.checkScope(sc); err != nil {
		return nil, err
	}
	return &runner{db: db, sc: sc, tx: tx}, nil
}

func (db *DB) runTx(ctx context.Context, readOnly bool, fn func(context.Context) error,
	opts []TxOption,
) error {
	var o txOptions
	for _, opt := range opts {
		if opt != nil {
			opt.applyTx(&o)
		}
	}
	if o.err != nil {
		return o.err
	}
	if cur := txFrom(ctx, db); cur != nil && !o.detached {
		if !readOnly && cur.readOnly {
			return ErrReadOnly
		}
		if err := cur.checkScope(db.resolveScope(ctx)); err != nil {
			return err
		}
		if len(o.settings) > 0 {
			return db.withSettings(ctx, o.settings, fn)
		}
		return fn(ctx)
	}
	if o.replica && !readOnly {
		return errf("ReplicaTx needs a Read")
	}
	if o.replica && o.isolation == Serializable {
		return errf("ReplicaTx cannot run Serializable: a hot standby refuses it")
	}
	for attempt := 0; ; attempt++ {
		tx := &txState{db: db, readOnly: readOnly, opts: o, scopeCtx: ctx}
		if db.tenants != nil {
			tx.scope = db.resolveScope(ctx)
			slot, owned, err := db.tenants.enter(ctx, tx.scope)
			if err != nil {
				return err
			}
			tx.slot, tx.ownsSlot = slot, owned
		}
		if db.replicas != nil {
			if o.replica {
				tx.rep = db.replicas.pick(routeFrom(ctx).mark(db), false)
			} else if !readOnly {
				tx.route = routeFrom(ctx)
			}
		}
		err := tx.run(ctx, fn)
		if err != nil && attempt < o.retries && IsRetryable(err) {
			continue
		}
		if err == nil {
			tx.runAfterCommit(ctx)
		}
		return err
	}
}

// run calls fn with the transaction on its context and ends the transaction: it commits when fn
// returns nil and rolls back when it returns an error. When fn panics, it rolls back and lets the
// connection, replica and tenant slot go before the panic continues, so a recovered panic leaves
// no connection checked out in an open transaction. The slot is let go before the caller runs the
// AfterCommit callbacks, which may start work of their own for the tenant.
func (tx *txState) run(ctx context.Context, fn func(context.Context) error) (err error) {
	returned := false
	defer func() {
		if !returned {
			_ = tx.end(ctx, errPanicked)
		}
		if tx.rep != nil {
			tx.rep.release()
		}
		if tx.ownsSlot {
			tx.slot.release()
		}
	}()
	fctx := context.WithValue(ctx, txKey{}, tx)
	if tx.slot != nil {
		fctx = context.WithValue(fctx, slotKey{}, tx.slot)
	}
	err = fn(fctx)
	returned = true
	return tx.end(ctx, err)
}

func (tx *txState) acquire(ctx context.Context) (pgConn, error) {
	if tx.conn != nil {
		return tx.conn, nil
	}
	if tx.joined {
		return nil, errf("the joined transaction has ended")
	}
	pool := tx.db.pool
	if tx.rep != nil {
		pool = tx.rep.pool
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("keel: acquire connection: %w", err)
	}
	tx.conn, tx.pooled = conn, conn
	return conn, nil
}

// withSettings runs fn with settings applied to the transaction on ctx and restores their values
// after it. A failed fn leaves them: its error rolls the transaction back or the caller handles it
// after a savepoint rollback, which restores them itself. A setting the session did not know yet,
// an extension's before its library loads, is restored to its reset value: fn may load the library
// (pgvector's hnsw.ef_search on the first vector query of a connection), which turns the
// placeholder into a typed setting that refuses the empty string.
func (db *DB) withSettings(ctx context.Context, settings []setting, fn func(context.Context) error) error {
	prev := make([]*string, len(settings))
	for i, st := range settings {
		v, err := QueryValue[*string](ctx, db, "SELECT current_setting($1, true)", st.name)
		if err != nil {
			return err
		}
		prev[i] = v
		if _, err := Exec(ctx, db, "SELECT set_config($1, $2, true)", st.name, st.value); err != nil {
			return err
		}
	}
	if err := fn(ctx); err != nil {
		return err
	}
	for i := len(settings) - 1; i >= 0; i-- {
		if _, err := Exec(ctx, db, restoreSettingSQL, settings[i].name, prev[i]); err != nil {
			return err
		}
	}
	return nil
}

const restoreSettingSQL = "SELECT set_config($1, coalesce($2::text, " +
	"(SELECT reset_val FROM pg_catalog.pg_settings WHERE name = $1), ''), true)"

// level returns the isolation level the transaction begins with: its own option, else, for an
// explicit Read, Config.ReadIsolation (REPEATABLE READ in place of SERIALIZABLE on a replica, which
// refuses it), else zero, the server's default. A transaction another data layer began (JoinTx)
// reports zero: keel does not know its level.
func (tx *txState) level() Isolation {
	if tx.joined {
		return 0
	}
	if tx.opts.isolation != 0 {
		return tx.opts.isolation
	}
	if !tx.readOnly || tx.implicit {
		return 0
	}
	if l := tx.db.cfg.ReadIsolation; l != Serializable || tx.rep == nil {
		return l
	}
	return RepeatableRead
}

// snapshot reports whether every statement of the transaction reads one snapshot: it runs at
// REPEATABLE READ or SERIALIZABLE.
func (tx *txState) snapshot() bool { return tx.level() >= RepeatableRead }

// durationSetting formats d as a millisecond setting value, at least 1ms.
func durationSetting(d time.Duration) string {
	return strconv.FormatInt(max(d.Milliseconds(), 1), 10) + "ms"
}

// queueBegin queues BEGIN, the transaction options and the OnBegin statements. With
// Config.DeadlineTimeouts it also sets statement_timeout to the time left before the deadline of
// the transaction's context: Read's or Write's for an explicit transaction, ctx, the context of the
// round trip that begins it, for an implicit one (a Query.Timeout is a deadline on it).
func (tx *txState) queueBegin(ctx context.Context, b *pgx.Batch) (int, error) {
	ro := 0
	if tx.readOnly {
		ro = 1
	}
	b.Queue(beginSQL[tx.level()][ro])
	n := 1
	if tx.opts.lockTimeout > 0 {
		b.Queue("SELECT set_config('lock_timeout', $1, true)", durationSetting(tx.opts.lockTimeout))
		n++
	}
	if tx.db.cfg.DeadlineTimeouts {
		dctx := ctx
		if !tx.implicit && tx.scopeCtx != nil {
			dctx = tx.scopeCtx
		}
		if deadline, ok := dctx.Deadline(); ok {
			b.Queue("SELECT set_config('statement_timeout', $1, true)", durationSetting(time.Until(deadline)))
			n++
		}
	}
	for _, st := range tx.opts.settings {
		b.Queue("SELECT set_config($1, $2, true)", st.name, st.value)
		n++
	}
	if tx.db.cfg.OnBegin != nil {
		stmts, err := tx.db.cfg.OnBegin(tx.scopeCtx, TxInfo{ReadOnly: tx.readOnly, Implicit: tx.implicit})
		if err != nil {
			return 0, err
		}
		for _, s := range stmts {
			b.Queue(s.SQL, s.Args...)
			n++
		}
	}
	return n, nil
}

// begin sends BEGIN, the transaction options and the OnBegin statements in one round trip, unless
// the transaction has begun, for a statement that cannot join a batch, such as COPY.
func (tx *txState) begin(ctx context.Context) error {
	if tx.begun {
		return nil
	}
	conn, err := tx.acquire(ctx)
	if err != nil {
		return err
	}
	b := &pgx.Batch{}
	n, err := tx.queueBegin(ctx, b)
	if err != nil {
		return err
	}
	br := conn.SendBatch(ctx, b)
	tx.begun = true
	chargeErr := chargeBudgets(ctx, nil, 1)
	for range n {
		if _, err = br.Exec(); err != nil {
			break
		}
	}
	if closeErr := br.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = chargeErr
	}
	return err
}

// end commits or rolls back and releases the connection. A transaction that never sent a
// statement costs nothing. The data versions the transaction changed go to the DB's versioner
// once COMMIT was sent, even if it failed, since it may have committed.
func (tx *txState) end(ctx context.Context, err error) error {
	err = tx.finish(ctx, err)
	if tx.changes != nil && (err == nil || tx.commitTried) {
		tx.db.versions.add(tx.changes)
		tx.changes = nil
	}
	return err
}

// finish commits, or rolls back when err is set, and releases the connection. A read-only
// transaction on a connection of keel's own pools ends without waiting (see unawaited).
func (tx *txState) finish(ctx context.Context, err error) error {
	if tx.conn == nil {
		return err
	}
	if tx.begun && !tx.committed && tx.unawaited() {
		sql := "ROLLBACK"
		if err == nil {
			sql = "COMMIT"
			tx.commitTried, tx.committed = true, true
		}
		tx.db.endUnawaited(tx.pooled, sql)
		tx.conn, tx.pooled = nil, nil
		return err
	}
	defer func() {
		if tx.pooled != nil {
			tx.pooled.Release()
		}
		tx.conn, tx.pooled = nil, nil
	}()
	if !tx.begun || tx.committed {
		return err
	}
	if err != nil {
		if _, rbErr := tx.conn.Exec(context.WithoutCancel(ctx), "ROLLBACK"); rbErr != nil {
			return errors.Join(err, fmt.Errorf("keel: rollback: %w", rbErr))
		}
		return err
	}
	tx.commitTried = true
	if err := tx.commit(ctx); err != nil {
		return fmt.Errorf("keel: commit: %w", err)
	}
	tx.committed = true
	return nil
}

// unawaited reports whether the transaction's COMMIT or ROLLBACK is sent without waiting for its
// reply: a read-only transaction keel began on a connection of its own pools, below SERIALIZABLE
// (whose COMMIT can fail a serialization check), unless Config.AwaitReadCommit is set.
func (tx *txState) unawaited() bool {
	return tx.readOnly && tx.pooled != nil && !tx.joined && !tx.db.cfg.AwaitReadCommit &&
		tx.level() != Serializable
}

// drainTimeout bounds how long a connection may take to answer a COMMIT or ROLLBACK keel did not
// wait for; one that has not answered by then is closed.
const drainTimeout = 30 * time.Second

// endUnawaited sends sql, COMMIT or ROLLBACK, on conn and returns once it is written. A goroutine
// reads the reply and then releases conn to its pool, which would destroy a connection still
// awaiting a reply; a failed reply closes the connection instead. Close waits for these
// goroutines. A connection already closed (a statement whose context ended) is released at once.
// The reply is read under a context of its own, so the end of the caller's context
// (a request that returned) does not cancel it.
func (db *DB) endUnawaited(conn *pgxpool.Conn, sql string) {
	if conn.Conn().IsClosed() {
		conn.Release()
		return
	}
	dctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	reply := conn.Conn().PgConn().Exec(dctx, sql)
	db.drains.Add(1)
	go db.drain(conn, reply, cancel)
}

// drain reads the reply of an unawaited COMMIT or ROLLBACK and lets conn go.
func (db *DB) drain(conn *pgxpool.Conn, reply *pgconn.MultiResultReader, cancel context.CancelFunc) {
	defer db.drains.Done()
	err := reply.Close()
	cancel()
	if err != nil {
		db.drainFailures.Add(1)
		cctx, ccancel := context.WithTimeout(context.Background(), time.Second)
		_ = conn.Conn().Close(cctx)
		ccancel()
	}
	conn.Release()
}

// commit sends COMMIT. A write transaction on a context marked with OnReplica reads the primary's
// WAL position after it, in the same round trip, so the context's replica reads see the commit.
func (tx *txState) commit(ctx context.Context) error {
	if tx.route == nil {
		_, err := tx.conn.Exec(ctx, "COMMIT")
		return err
	}
	b := &pgx.Batch{}
	b.Queue("COMMIT")
	b.Queue(commitPositionSQL)
	br := tx.conn.SendBatch(ctx, b)
	_, err := br.Exec()
	var lsn int64
	posErr := err
	if err == nil {
		posErr = br.QueryRow().Scan(&lsn)
	}
	if closeErr := br.Close(); posErr == nil {
		posErr = closeErr
	}
	if err != nil {
		return err
	}
	tx.db.recordCommit(tx.route, lsn, posErr == nil)
	return nil
}

// touch records a (tenant, table) pair the transaction changed.
func (tx *txState) touch(k versionKey) {
	if tx.changes == nil {
		tx.changes = versionSet{}
	}
	tx.changes[k] = struct{}{}
}

func (tx *txState) runAfterCommit(ctx context.Context) {
	for _, fn := range tx.afterCommit {
		fn(ctx)
	}
}

// Savepoint runs fn inside a savepoint of the transaction on ctx: an error from fn rolls back to
// the savepoint, drops the Committed hooks and AfterCommit callbacks queued inside it, and is
// returned, leaving the transaction usable. Outside a transaction it runs fn in a new Write. Under
// another scope than the transaction's it returns ErrScopeMismatch.
func Savepoint(ctx context.Context, db *DB, fn func(ctx context.Context) error) error {
	tx := txFrom(ctx, db)
	if tx == nil {
		return Write(ctx, db, fn)
	}
	if tx.readOnly {
		return ErrReadOnly
	}
	run, err := db.txRunner(ctx, tx)
	if err != nil {
		return err
	}
	tx.savepoints++
	name := "keel_sp_" + strconv.Itoa(tx.savepoints)
	if err := run.one(ctx, &stmt{sql: "SAVEPOINT " + name, op: "savepoint"}, false); err != nil {
		return err
	}
	queued := len(tx.afterCommit)
	if err := fn(ctx); err != nil {
		tx.afterCommit = tx.afterCommit[:queued]
		if rbErr := run.one(ctx, &stmt{sql: "ROLLBACK TO SAVEPOINT " + name, op: "savepoint"}, false); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	return run.one(ctx, &stmt{sql: "RELEASE SAVEPOINT " + name, op: "savepoint"}, false)
}

// LockKey takes a transaction-scoped advisory lock on key, waiting for it, so work without a row to
// lock is serialised across processes. It needs a transaction; the lock is released when the
// transaction ends.
func LockKey(ctx context.Context, db *DB, key string) error {
	tx := txFrom(ctx, db)
	if tx == nil {
		return errf("LockKey needs a transaction on the context")
	}
	run, err := db.txRunner(ctx, tx)
	if err != nil {
		return err
	}
	return run.one(ctx, &stmt{sql: "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
		args: []any{key}, op: "lock"}, false)
}

// TryLockKey is LockKey without waiting: it reports whether the lock was taken.
func TryLockKey(ctx context.Context, db *DB, key string) (bool, error) {
	tx := txFrom(ctx, db)
	if tx == nil {
		return false, errf("TryLockKey needs a transaction on the context")
	}
	run, err := db.txRunner(ctx, tx)
	if err != nil {
		return false, err
	}
	var got bool
	s := &stmt{sql: "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))", args: []any{key}, op: "lock"}
	s.scan = func(rows pgx.Rows) error {
		if rows.Next() {
			return rows.Scan(&got)
		}
		return nil
	}
	err = run.one(ctx, s, false)
	return got, err
}
