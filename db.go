package keel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emoss08/keel/schema"
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
	// run in an implicit one. It is called when the transaction begins, with its first statement,
	// whichever terminal or callback sends it, on the context the transaction was opened with,
	// without that transaction and with its tenant slot: a statement it sends runs in a transaction
	// of its own on a second pool connection, which calls OnBegin again, and shares the slot rather
	// than waiting for another.
	OnBegin func(ctx context.Context, tx TxInfo) ([]Statement, error)

	// NewID returns a new primary key for autoid columns.
	NewID func(prefix string) string
	// Now returns the time stamped into autocreate and autoupdate columns. Defaults to time.Now.
	Now func() time.Time

	// Observers see every statement.
	Observers []Observer
	// ObserveArgs hands observers each statement's bound values (QueryEvent.Values). The values
	// are the data itself, so turn it on only where a query log may hold it, in development.
	ObserveArgs bool

	// Tables are the generated tables (keeltables.All()) that constraint violations resolve
	// against: a *ConstraintError of a listed table carries the model's field names, labels and
	// the message its Constraints method declares. Open reads their constraints' columns from the
	// catalog in one query, so resolving a violation never waits for a connection.
	Tables []*schema.Table

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
	// JSON encodes and decodes json and jsonb values. Each function left nil defaults to sonic in
	// its encoding/json-compatible mode, which encodes a value to the bytes encoding/json would;
	// set sonic.ConfigDefault's functions, which skip HTML escaping and map key sorting, for more
	// speed. Its UnmarshalNumber decodes the columns tagged jsonnumber.
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
	// Cache configures the result cache; nil turns it off.
	Cache *CacheConfig

	// ReadIsolation is the isolation level of explicit Read transactions that pass none of their
	// own. Zero leaves it to the server's default (READ COMMITTED unless the server says
	// otherwise); RepeatableRead gives every statement of a Read one snapshot. A ReplicaTx Read
	// runs REPEATABLE READ in place of Serializable, which a hot standby refuses.
	ReadIsolation Isolation
	// AwaitReadCommit makes read-only transactions wait for the reply to their COMMIT or ROLLBACK,
	// as writes do. By default keel sends it and returns, and a goroutine reads the reply before the
	// connection goes back to the pool.
	AwaitReadCommit bool
	// DeadlineTimeouts makes every transaction keel begins, explicit or implicit, set
	// statement_timeout to the time left before its context's deadline, in its BEGIN round trip,
	// so the server stops a statement the caller no longer waits for even when the cancel request
	// does not reach it. Statements outside a transaction rely on the context's cancellation alone.
	DeadlineTimeouts bool
	// TenantConcurrency bounds how many terminals and transactions of one tenant run at once on
	// the DB; zero turns the bound off. Work past it waits for a slot or for its context. A
	// transaction holds one slot from its start to its end and its statements take none; work
	// started under a context that carries a slot of the same tenant (a Detached transaction, a
	// write's hooks) shares it. System scopes and unscoped work take none. DB.TenantWaits counts
	// the waits.
	TenantConcurrency int
	// SlowWaits reports work that waits too long for a tenant slot, a pool connection or another
	// goroutine's read of a result cache entry (see SlowWaitConfig). Off unless both of its fields
	// are set.
	SlowWaits SlowWaitConfig
	// MaxWait bounds each wait for a tenant slot, a pool connection or another goroutine's read
	// of a result cache entry, whatever the context's deadline; zero (the default) leaves waits
	// to the context. A slot or connection wait that reaches it fails with ErrWaitLimit, and a
	// cache wait stops waiting and reads the database itself, so work without a deadline (a
	// background job) cannot wait forever behind a leaked slot or a stuck request.
	MaxWait time.Duration

	// LoadStrategy is how a read loads its to-many relations when the query does not choose
	// (Query.LoadStrategy). The default, LoadPipelined, sends the root statement and every level in
	// one round trip where every level can read one snapshot; LoadPerLevel sends a round trip per level.
	LoadStrategy LoadStrategy
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
	// UnmarshalNumber decodes the columns tagged jsonnumber: as Unmarshal, except that a number
	// decoded into an interface value (map[string]any, []any, any) is a json.Number holding its
	// literal instead of a float64, so integers past 2^53 keep every digit. With sonic,
	// sonic.Config{UseNumber: true}.Froze().Unmarshal.
	UnmarshalNumber func(data []byte, v any) error
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
	cache    *resultCache
	tenants  *tenantLimiter
	slow     *slowWaits

	// drains counts the goroutines reading the reply to a COMMIT or ROLLBACK keel did not wait
	// for; drainFailures counts the connections closed because that reply failed.
	drains        sync.WaitGroup
	drainFailures atomic.Int64

	tablesOnce   sync.Once
	tablesByName map[string]*schema.Table
	constraints  constraintCatalog

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
	if cfg.ReadIsolation > Serializable {
		return nil, errf("Config.ReadIsolation %d is not an isolation level", uint8(cfg.ReadIsolation))
	}
	if cfg.TenantConcurrency < 0 {
		return nil, errf("Config.TenantConcurrency must not be negative")
	}
	cfg.JSON = resolveJSON(cfg.JSON)
	cfg.configure(pc)
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("keel: open pool: %w", err)
	}
	db := &DB{pool: pool, cfg: cfg, slow: newSlowWaits(cfg.SlowWaits)}
	if cfg.TenantConcurrency > 0 {
		db.tenants = newTenantLimiter(cfg.TenantConcurrency, db.slow, cfg.MaxWait)
	}
	if len(cfg.Replicas) > 0 {
		if db.replicas, err = openReplicas(ctx, &db.cfg); err != nil {
			pool.Close()
			return nil, err
		}
		db.replicas.start(db)
	}
	db.prefetchConstraints(ctx)
	if cfg.DataVersions.Enabled {
		db.versions = startVersioner(db)
	}
	if err := db.openCache(ctx); err != nil {
		db.Close()
		return nil, err
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
		registerJSON(conn.TypeMap(), codec)
		conn.PgConn().CustomData()[jsonNumberKey] = codec.UnmarshalNumber
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

// Close stops the replica lag tracker, writes the pending data versions, waits for the replies to
// read-only COMMITs keel did not wait for, and closes every connection, the replicas' included.
// Calls after the first do nothing.
func (db *DB) Close() {
	db.closeOnce.Do(func() {
		db.closeCache()
		if db.replicas != nil {
			db.replicas.stop()
		}
		if db.versions != nil {
			db.versions.close()
		}
		db.drains.Wait()
		if db.replicas != nil {
			db.replicas.close()
		}
		db.pool.Close()
	})
}

// TenantColumns returns Config.TenantColumns: the tenant columns, in the order the values of a
// tenant (Config.Tenant) and of a tenant key (TenantKey) come in. The slice is a copy.
func (db *DB) TenantColumns() []string { return slices.Clone(db.cfg.TenantColumns) }

// TenantWaits returns how many terminals and transactions have waited for a tenant slot
// (Config.TenantConcurrency) since Open.
func (db *DB) TenantWaits() int64 {
	if db.tenants == nil {
		return 0
	}
	return db.tenants.waits.Load()
}

// ReadCommitFailures returns how many connections keel closed since Open because the reply to a
// read-only COMMIT or ROLLBACK it did not wait for failed.
func (db *DB) ReadCommitFailures() int64 { return db.drainFailures.Load() }

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
