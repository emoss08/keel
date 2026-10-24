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
	// on every connection, through pgx's LoadTypes, after the enums. Enums are registered without
	// being listed.
	LoadTypes []string
	// SkipEnums turns off registering every enum type of the database on each new connection.
	// keel reads them all from the catalog in one query, however many there are. Registered enums
	// bind as arrays and decode into one shared string per label, so enum columns cost no
	// allocation per row.
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
		var loaded []*pgtype.Type
		if !skipEnums {
			enums, err := registerEnums(ctx, conn)
			if err != nil {
				return err
			}
			loaded = enums
		}
		if len(loadTypes) > 0 {
			types, err := conn.LoadTypes(ctx, loadTypes)
			if err != nil {
				return fmt.Errorf("keel: load types: %w", err)
			}
			conn.TypeMap().RegisterTypes(types)
			loaded = append(loaded, types...)
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

// enumTypesSQL reads every enum type of the database with its array type.
const enumTypesSQL = `SELECT t.oid, t.typname, n.nspname, a.oid, a.typname, a.typdelim
  FROM pg_type t
  JOIN pg_namespace n ON n.oid = t.typnamespace
  JOIN pg_type a ON a.oid = t.typarray
 WHERE t.typtype = 'e'
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')`

// registerEnums registers every enum type of the database and its array type on conn, from one
// catalog query: an enum as pgx's EnumCodec, which decodes each label into one shared string, and
// its array as an ArrayCodec of it, under the type's name and its schema-qualified name as
// LoadTypes registers them. It returns the array types under both names, for registerArrays.
func registerEnums(ctx context.Context, conn *pgx.Conn) ([]*pgtype.Type, error) {
	rows, err := conn.Query(ctx, enumTypesSQL, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, fmt.Errorf("keel: list enum types: %w", err)
	}
	defer rows.Close()
	m := conn.TypeMap()
	var arrays []*pgtype.Type
	for rows.Next() {
		var oid, arrayOID uint32
		var name, nsp, arrayName, delim string
		if err := rows.Scan(&oid, &name, &nsp, &arrayOID, &arrayName, &delim); err != nil {
			return nil, fmt.Errorf("keel: list enum types: %w", err)
		}
		enum := &pgtype.Type{Name: name, OID: oid, Codec: &pgtype.EnumCodec{}}
		array := &pgtype.Type{Name: arrayName, OID: arrayOID,
			Codec: &pgtype.ArrayCodec{ElementType: enum, Delimiter: arrayDelimiter(delim)}}
		qualified := &pgtype.Type{Name: nsp + "." + arrayName, OID: arrayOID, Codec: array.Codec}
		m.RegisterType(enum)
		m.RegisterType(&pgtype.Type{Name: nsp + "." + name, OID: oid, Codec: enum.Codec})
		m.RegisterType(array)
		m.RegisterType(qualified)
		arrays = append(arrays, array, qualified)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keel: list enum types: %w", err)
	}
	return arrays, nil
}

// arrayDelimiter is the element delimiter of an array type's text format, a comma unless the
// catalog names another.
func arrayDelimiter(typdelim string) byte {
	if len(typdelim) == 1 {
		return typdelim[0]
	}
	return ','
}

func registerJSON(m *pgtype.Map, c *JSONCodec) {
	m.RegisterType(&pgtype.Type{Name: "json", OID: pgtype.JSONOID,
		Codec: &pgtype.JSONCodec{Marshal: c.Marshal, Unmarshal: c.Unmarshal}})
	m.RegisterType(&pgtype.Type{Name: "jsonb", OID: pgtype.JSONBOID,
		Codec: &pgtype.JSONBCodec{Marshal: c.Marshal, Unmarshal: c.Unmarshal}})
}
