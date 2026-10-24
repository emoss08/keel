package keel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
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

	rep         *replica
	route       *route
	changes     versionSet
	commitTried bool
}

type txOptions struct {
	lockTimeout time.Duration
	isolation   string
	retries     int
	replica     bool
	detached    bool
}

// TxOption configures Read and Write.
type TxOption func(*txOptions)

// LockTimeout bounds how long any statement of the transaction waits for a lock.
func LockTimeout(d time.Duration) TxOption { return func(o *txOptions) { o.lockTimeout = d } }

// Serializable runs the transaction at the SERIALIZABLE isolation level.
func Serializable() TxOption { return func(o *txOptions) { o.isolation = "SERIALIZABLE" } }

// RepeatableRead runs the transaction at the REPEATABLE READ isolation level.
func RepeatableRead() TxOption { return func(o *txOptions) { o.isolation = "REPEATABLE READ" } }

// Retry runs a top-level transaction up to n more times after a serialization failure or a
// deadlock. fn must be safe to repeat.
func Retry(n int) TxOption { return func(o *txOptions) { o.retries = n } }

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
func Detached() TxOption { return func(o *txOptions) { o.detached = true } }

// Read runs fn in a read-only transaction, on the primary unless ReplicaTx is given. A Read or
// Write already on the context is joined, unless Detached is given; joining it under another scope
// than the one it began with returns ErrScopeMismatch.
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
		opt(&o)
	}
	if cur := txFrom(ctx, db); cur != nil && !o.detached {
		if !readOnly && cur.readOnly {
			return ErrReadOnly
		}
		if err := cur.checkScope(db.resolveScope(ctx)); err != nil {
			return err
		}
		return fn(ctx)
	}
	if o.replica && !readOnly {
		return errf("ReplicaTx needs a Read")
	}
	if o.replica && o.isolation == "SERIALIZABLE" {
		return errf("ReplicaTx cannot run Serializable: a hot standby refuses it")
	}
	for attempt := 0; ; attempt++ {
		tx := &txState{db: db, readOnly: readOnly, opts: o, scopeCtx: ctx}
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
// connection and replica go before the panic continues, so a recovered panic leaves no connection
// checked out in an open transaction.
func (tx *txState) run(ctx context.Context, fn func(context.Context) error) (err error) {
	returned := false
	defer func() {
		if !returned {
			_ = tx.end(ctx, errPanicked)
		}
		if tx.rep != nil {
			tx.rep.release()
		}
	}()
	err = fn(context.WithValue(ctx, txKey{}, tx))
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

// queueBegin queues BEGIN, the transaction options and the OnBegin statements.
func (tx *txState) queueBegin(b *pgx.Batch) (int, error) {
	begin := "BEGIN"
	if tx.opts.isolation != "" {
		begin += " ISOLATION LEVEL " + tx.opts.isolation
	}
	if tx.readOnly {
		begin += " READ ONLY"
	}
	b.Queue(begin)
	n := 1
	if tx.opts.lockTimeout > 0 {
		ms := max(tx.opts.lockTimeout.Milliseconds(), 1)
		b.Queue("SELECT set_config('lock_timeout', $1, true)", strconv.FormatInt(ms, 10)+"ms")
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
	n, err := tx.queueBegin(b)
	if err != nil {
		return err
	}
	br := conn.SendBatch(ctx, b)
	tx.begun = true
	for range n {
		if _, err = br.Exec(); err != nil {
			break
		}
	}
	if closeErr := br.Close(); err == nil {
		err = closeErr
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

// finish commits, or rolls back when err is set, and releases the connection.
func (tx *txState) finish(ctx context.Context, err error) error {
	if tx.conn == nil {
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
