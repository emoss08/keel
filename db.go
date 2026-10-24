package keel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	// LoadTypes lists user-defined types (domains, composites) and their array types to register
	// on every connection. Enums are registered without being listed.
	LoadTypes []string
	// SkipEnums turns off registering every enum type of the database on each new connection.
	// Registered enums bind as arrays and decode into one shared string per label, so enum columns
	// cost no allocation per row.
	SkipEnums bool
	// JSON encodes and decodes json and jsonb values. Defaults to encoding/json; set it to a faster
	// library (sonic, go-json) with the same signatures.
	JSON *JSONCodec
	// PlanCache chooses how statements are prepared. The default, PlanCacheAuto, prepares every
	// statement shape once per connection.
	PlanCache PlanCache
	// StatementCacheCapacity is how many prepared statements (or, with PlanCacheOff, statement
	// descriptions) each connection keeps. Defaults to 512; the least recently used is
	// deallocated beyond it.
	StatementCacheCapacity int

	// Replicas are streaming replicas of the primary. Reads run on one only when their context is
	// marked with OnReplica, or inside a Read started with ReplicaTx. Every replica pool gets the
	// same connection setup as the primary's.
	Replicas []ReplicaConfig
	// MaxReplicaLag is how far behind the primary a replica may be and still serve reads.
	// Defaults to 2s.
	MaxReplicaLag time.Duration

	// DataVersions turns on the per-tenant, per-table change counters keel maintains from its own
	// write path.
	DataVersions DataVersionsConfig
}

// PlanCache chooses how keel prepares statements.
type PlanCache uint8

const (
	// PlanCacheAuto prepares each statement shape once per connection and lets Postgres choose
	// between a generic and a custom plan (plan_cache_mode = auto): it compares the generic plan's
	// cost with the custom plans of the first executions and keeps custom plans when the generic
	// one is worse. This skips parsing and, usually, planning.
	PlanCacheAuto PlanCache = iota
	// PlanCacheCustom prepares statements but plans every execution for its values
	// (plan_cache_mode = force_custom_plan). It skips parsing only; use it when parameter values
	// change plans a lot, as tenants of very different sizes can.
	PlanCacheCustom
	// PlanCacheOff prepares nothing: every execution is parsed and planned. Use it behind a
	// connection pooler that cannot track prepared statements (PgBouncer before 1.21, or without
	// max_prepared_statements).
	PlanCacheOff
)

// JSONCodec is a JSON implementation.
type JSONCodec struct {
	Marshal   func(v any) ([]byte, error)
	Unmarshal func(data []byte, v any) error
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
	pool     *pgxpool.Pool
	cfg      Config
	hooks    hooks
	icpt     interceptors
	types    columnTypes
	replicas *replicaSet
	versions *versioner

	closeOnce sync.Once
}

// Open connects to Postgres. With Config.Replicas it also opens a pool per replica and starts a
// goroutine that tracks their lag; with Config.DataVersions.Enabled it starts the goroutine that
// writes data versions. Close stops both.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	pc, err := poolConfig("", cfg.DSN, cfg.Pool)
	if err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.StatementCacheCapacity == 0 {
		cfg.StatementCacheCapacity = 512
	}
	if cfg.MaxReplicaLag <= 0 {
		cfg.MaxReplicaLag = defaultMaxReplicaLag
	}
	if cfg.DataVersions.Interval <= 0 {
		cfg.DataVersions.Interval = defaultVersionInterval
	}
	cfg.configure(pc)
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("keel: open pool: %w", err)
	}
	db := &DB{pool: pool, cfg: cfg}
	if len(cfg.Replicas) > 0 {
		if db.replicas, err = openReplicas(ctx, &db.cfg); err != nil {
			pool.Close()
			return nil, err
		}
		db.replicas.start(db)
	}
	if cfg.DataVersions.Enabled {
		db.versions = startVersioner(db)
	}
	return db, nil
}

// poolConfig returns pc, or the configuration parsed from dsn when pc is nil. what names the
// connection in errors: empty for the primary, "replica N" for a replica.
func poolConfig(what, dsn string, pc *pgxpool.Config) (*pgxpool.Config, error) {
	if pc != nil {
		return pc, nil
	}
	prefix := "keel: "
	if what != "" {
		prefix += what + ": "
	}
	if dsn == "" {
		return nil, errors.New(prefix + "Config needs a DSN or a Pool")
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%sparse dsn: %w", prefix, err)
	}
	return pc, nil
}

// configure applies the setup every pool of the DB shares, the primary's and each replica's: the
// query mode and statement cache, plan_cache_mode, and an AfterConnect that registers the JSON
// codec, the enums, LoadTypes and the array codecs before the pool's own AfterConnect and
// Config.AfterConnect run.
func (cfg *Config) configure(pc *pgxpool.Config) {
	switch cfg.PlanCache {
	case PlanCacheOff:
		pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
		pc.ConnConfig.DescriptionCacheCapacity = cfg.StatementCacheCapacity
		pc.ConnConfig.StatementCacheCapacity = 0
	default:
		pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
		pc.ConnConfig.StatementCacheCapacity = cfg.StatementCacheCapacity
		pc.ConnConfig.DescriptionCacheCapacity = 0
		mode := "auto"
		if cfg.PlanCache == PlanCacheCustom {
			mode = "force_custom_plan"
		}
		pc.ConnConfig.RuntimeParams["plan_cache_mode"] = mode
	}
	poolAfterConnect := pc.AfterConnect
	codec, loadTypes, skipEnums, afterConnect := cfg.JSON, cfg.LoadTypes, cfg.SkipEnums, cfg.AfterConnect
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if codec != nil {
			registerJSON(conn.TypeMap(), codec)
		}
		names := append([]string(nil), loadTypes...)
		if !skipEnums {
			enums, err := enumTypes(ctx, conn)
			if err != nil {
				return err
			}
			names = append(names, enums...)
		}
		var loaded []*pgtype.Type
		if len(names) > 0 {
			types, err := conn.LoadTypes(ctx, names)
			if err != nil {
				return fmt.Errorf("keel: load types: %w", err)
			}
			conn.TypeMap().RegisterTypes(types)
			loaded = types
		}
		registerArrays(conn.TypeMap(), loaded)
		if poolAfterConnect != nil {
			if err := poolAfterConnect(ctx, conn); err != nil {
				return err
			}
		}
		if afterConnect != nil {
			return afterConnect(ctx, conn)
		}
		return nil
	}
}

// Close stops the replica lag tracker, writes the pending data versions and closes every
// connection, the replicas' included. Calls after the first do nothing.
func (db *DB) Close() {
	db.closeOnce.Do(func() {
		if db.replicas != nil {
			db.replicas.stop()
		}
		if db.versions != nil {
			db.versions.close()
		}
		if db.replicas != nil {
			db.replicas.close()
		}
		db.pool.Close()
	})
}

// TenantColumns returns Config.TenantColumns: the tenant columns, in the order the values of a
// tenant (Config.Tenant) and of a tenant key (TenantKey) come in. The slice is a copy.
func (db *DB) TenantColumns() []string { return slices.Clone(db.cfg.TenantColumns) }

// Pool returns the underlying pgx pool, for work keel does not cover (LISTEN, COPY of foreign
// data). Statements sent through it bypass scope checks and hooks.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Ping checks that the database is reachable.
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

func (db *DB) now() time.Time { return db.cfg.Now() }

func (db *DB) planMode() string {
	if db.cfg.PlanCache == PlanCacheCustom {
		return "force_custom_plan"
	}
	return "auto"
}

// enumTypes returns the qualified names of the database's enum types and their array types.
func enumTypes(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	rows, err := conn.Query(ctx, `SELECT quote_ident(n.nspname) || '.' || quote_ident(t.typname),
	       quote_ident(n.nspname) || '.' || quote_ident(a.typname)
	  FROM pg_type t
	  JOIN pg_namespace n ON n.oid = t.typnamespace
	  JOIN pg_type a ON a.oid = t.typarray
	 WHERE t.typtype = 'e'
	   AND n.nspname NOT IN ('pg_catalog', 'information_schema')`, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, fmt.Errorf("keel: list enum types: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var enum, array string
		if err := rows.Scan(&enum, &array); err != nil {
			return nil, err
		}
		names = append(names, enum, array)
	}
	return names, rows.Err()
}

func registerJSON(m *pgtype.Map, c *JSONCodec) {
	m.RegisterType(&pgtype.Type{Name: "json", OID: pgtype.JSONOID,
		Codec: &pgtype.JSONCodec{Marshal: c.Marshal, Unmarshal: c.Unmarshal}})
	m.RegisterType(&pgtype.Type{Name: "jsonb", OID: pgtype.JSONBOID,
		Codec: &pgtype.JSONBCodec{Marshal: c.Marshal, Unmarshal: c.Unmarshal}})
}
