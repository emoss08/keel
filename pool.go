package keel

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// connPool is a pgx pool as keel uses it: the primary's and each replica's. DB holds its pools
// through this interface rather than as *pgxpool.Pool, and no method of DB names a pgxpool type,
// because *pgxpool.Pool holds puddle's generic pool and *pgxpool.Conn its generic resource: Go
// compiles every method of an instantiated generic type again in each package that can see it,
// about 90 functions in every package that holds a *DB. Neither interface here names a type that
// leads to them, so the pools' code compiles once, in keel. Pool hands the pool out.
type connPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	Begin(ctx context.Context) (pgx.Tx, error)
	Ping(ctx context.Context) error
	Stat() *pgxpool.Stat
	Config() *pgxpool.Config
	Close()
}

// poolConn is a connection acquired from a connPool, held until Release.
type poolConn interface {
	pgConn
	Conn() *pgx.Conn
	Release()
}

// acquireConn takes a connection from pool.
func acquireConn(ctx context.Context, pool connPool) (poolConn, error) {
	conn, err := pool.(*pgxpool.Pool).Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Pool returns db's underlying pgx pool, for work keel does not cover (LISTEN, COPY of foreign
// data). Statements sent through it bypass scope checks and hooks. It is a function rather than
// a method of DB so that DB's method set names no pgxpool type (see connPool).
func Pool(db *DB) *pgxpool.Pool { return db.pool.(*pgxpool.Pool) }

// Stat returns the state of the primary's pool: connections acquired, idle, open, its maximum and
// how often and how long acquires waited.
func (db *DB) Stat() *pgxpool.Stat { return db.pool.Stat() }
