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
	conn      *pgxpool.Conn
	readOnly  bool
	implicit  bool
	begun     bool
	committed bool
	opts      txOptions
	scopeCtx  context.Context

	afterCommit []func(context.Context)
}

type txOptions struct {
	lockTimeout time.Duration
	isolation   string
	retries     int
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

// Read runs fn in a read-only transaction. A Read or Write already on the context is joined.
func Read(ctx context.Context, db *DB, fn func(ctx context.Context) error, opts ...TxOption) error {
	return db.runTx(ctx, true, fn, opts)
}

// Write runs fn in a read-write transaction. A Write already on the context is joined; a Read is
// not, and returns ErrReadOnly.
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

func (db *DB) runTx(ctx context.Context, readOnly bool, fn func(context.Context) error,
	opts []TxOption,
) error {
	if cur := txFrom(ctx, db); cur != nil {
		if !readOnly && cur.readOnly {
			return ErrReadOnly
		}
		return fn(ctx)
	}
	var o txOptions
	for _, opt := range opts {
		opt(&o)
	}
	for attempt := 0; ; attempt++ {
		tx := &txState{db: db, readOnly: readOnly, opts: o, scopeCtx: ctx}
		err := fn(context.WithValue(ctx, txKey{}, tx))
		err = tx.end(ctx, err)
		if err != nil && attempt < o.retries && IsRetryable(err) {
			continue
		}
		if err == nil {
			tx.runAfterCommit(ctx)
		}
		return err
	}
}

func (tx *txState) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if tx.conn != nil {
		return tx.conn, nil
	}
	conn, err := tx.db.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("keel: acquire connection: %w", err)
	}
	tx.conn = conn
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

// end commits or rolls back and releases the connection. A transaction that never sent a
// statement costs nothing.
func (tx *txState) end(ctx context.Context, err error) error {
	if tx.conn == nil {
		return err
	}
	defer func() {
		tx.conn.Release()
		tx.conn = nil
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
	if _, cerr := tx.conn.Exec(ctx, "COMMIT"); cerr != nil {
		return fmt.Errorf("keel: commit: %w", cerr)
	}
	tx.committed = true
	return nil
}

func (tx *txState) runAfterCommit(ctx context.Context) {
	for _, fn := range tx.afterCommit {
		fn(ctx)
	}
}

