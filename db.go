package keel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config configures a DB.
type Config struct {
	// DSN is the connection string. Pool, when set, is used instead.
	DSN  string
	Pool *pgxpool.Config

	// TenantColumns names the tenant columns, in the order Tenant returns their values.
	TenantColumns []string
	// Tenant returns the tenant of the context.
	Tenant func(ctx context.Context) ([]any, bool)
	// System reports whether the context carries a system scope, and its reason.
	System func(ctx context.Context) (reason string, ok bool)

	// OnBegin returns statements to run at the start of every transaction, explicit or implicit,
	// for example a row-level security set_config. When it is set, terminals outside a transaction
	// run in an implicit one.
	OnBegin func(ctx context.Context, tx TxInfo) ([]Statement, error)

	// NewID returns a new primary key for autoid columns.
	NewID func(prefix string) string
	// Now returns the time stamped into autocreate and autoupdate columns. Defaults to time.Now.
	Now func() time.Time

	// Observers see every statement.
	Observers []Observer

	// AfterConnect runs on every new connection, after keel registers its types; use it to
	// register codecs (a decimal type, a faster JSON codec).
	AfterConnect func(ctx context.Context, conn *pgx.Conn) error
	// LoadTypes lists user-defined types (enums, domains) and their array types to register on
	// every connection, so arrays of them bind as parameters. Enums named here bind from Go
	// strings.
	LoadTypes []string
	// DescriptionCacheCapacity sizes pgx's per-connection statement description cache. Defaults
	// to 4096.
	DescriptionCacheCapacity int
}

// TxInfo describes the transaction being started.
type TxInfo struct {
	ReadOnly bool
	Implicit bool
}

// Statement is one statement with its arguments.
type Statement struct {
	SQL  string
	Args []any
}

// DB is a pool of connections and the configuration keel applies to them.
type DB struct {
	pool  *pgxpool.Pool
	cfg   Config
	hooks hooks
	icpt  interceptors
}

// Open connects to Postgres.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	pc := cfg.Pool
	if pc == nil {
		if cfg.DSN == "" {
			return nil, errors.New("keel: Config needs a DSN or a Pool")
		}
		var err error
		pc, err = pgxpool.ParseConfig(cfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("keel: parse dsn: %w", err)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.DescriptionCacheCapacity == 0 {
		cfg.DescriptionCacheCapacity = 4096
	}
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	pc.ConnConfig.DescriptionCacheCapacity = cfg.DescriptionCacheCapacity
	pc.ConnConfig.StatementCacheCapacity = 0
	userAfterConnect := pc.AfterConnect
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if len(cfg.LoadTypes) > 0 {
			types, err := conn.LoadTypes(ctx, cfg.LoadTypes)
			if err != nil {
				return fmt.Errorf("keel: load types: %w", err)
			}
			conn.TypeMap().RegisterTypes(types)
		}
		if userAfterConnect != nil {
			if err := userAfterConnect(ctx, conn); err != nil {
				return err
			}
		}
		if cfg.AfterConnect != nil {
			return cfg.AfterConnect(ctx, conn)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("keel: open pool: %w", err)
	}
	return &DB{pool: pool, cfg: cfg}, nil
}

// Close closes every connection.
func (db *DB) Close() { db.pool.Close() }

// Pool returns the underlying pgx pool, for work keel does not cover (LISTEN, COPY of foreign
// data). Statements sent through it bypass scope checks and hooks.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Ping checks that the database is reachable.
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

func (db *DB) now() time.Time { return db.cfg.Now() }
