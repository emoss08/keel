// Package adapt implements keelcli's ports over the real engines: keel/migrate for migrations and
// templates, keel/seed for seeds and the environment mark, and the connection pool for the facts
// doctor and the console header read.
//
// The adapters translate the engines' events into the ports' semantics (1-based indexes, the
// count of migrations applied, retry attempts that failed, lock waits that waited) so the views
// render the real engines exactly as they render package fake.
package adapt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/emoss08/keel"
)

// ErrNoDatabase is the error of a command that needs a database when none is configured.
var ErrNoDatabase = errors.New("keelcli: no database configured")

// AdminDatabase is the database the derived admin DSN connects to.
const AdminDatabase = "postgres"

// Database opens the application's database on first use and shares it between the adapters.
// Migrations run on connections of their own, named after the application, so other sessions
// see who holds a lock.
type Database struct {
	name     string
	config   func() (keel.Config, error)
	parallel int
	admin    func() string

	mu   sync.Mutex
	cfg  *keel.Config
	pool *pgxpool.Config
	err  error
	db   *keel.DB
}

// DatabaseOptions configure a Database. Config is the application's database configuration, nil
// for none; Name is the application_name its connections default to; Parallel is the seeds'
// parallelism the pool must serve; Admin returns an admin DSN that overrides the derived one, or
// "".
type DatabaseOptions struct {
	Name     string
	Config   func() (keel.Config, error)
	Parallel int
	Admin    func() string
}

// NewDatabase returns a Database that opens nothing until it is used.
func NewDatabase(o DatabaseOptions) *Database {
	return &Database{name: o.Name, config: o.Config, parallel: o.Parallel, admin: o.Admin}
}

// resolve loads the configuration once; the caller holds mu.
func (d *Database) resolve() (keel.Config, *pgxpool.Config, error) {
	if d.cfg != nil || d.err != nil {
		return d.cfgValue(), d.pool, d.err
	}
	cfg, pc, err := d.load()
	if err != nil {
		d.err = err
		return keel.Config{}, nil, err
	}
	d.cfg, d.pool = &cfg, pc
	return cfg, pc, nil
}

// cfgValue is the loaded configuration.
func (d *Database) cfgValue() keel.Config {
	if d.cfg == nil {
		return keel.Config{}
	}
	return *d.cfg
}

// load calls the application's Config and derives the pool configuration from it.
func (d *Database) load() (keel.Config, *pgxpool.Config, error) {
	if d.config == nil {
		return keel.Config{}, nil, ErrNoDatabase
	}
	cfg, err := d.config()
	switch {
	case errors.Is(err, ErrNoDatabase):
		return cfg, nil, err
	case err != nil:
		return cfg, nil, fmt.Errorf("keelcli: database configuration: %w", err)
	}
	var pc *pgxpool.Config
	switch {
	case cfg.Pool != nil:
		pc = cfg.Pool.Copy()
	case cfg.DSN == "":
		return cfg, nil, fmt.Errorf("%w: keel.Config has neither a DSN nor a Pool", ErrNoDatabase)
	default:
		if pc, err = pgxpool.ParseConfig(cfg.DSN); err != nil {
			return cfg, nil, fmt.Errorf("keelcli: parse the DSN: %w", err)
		}
	}
	d.named(pc)
	return cfg, pc, nil
}

// named names the pool's connections after the application and sizes it for the seeds'
// parallelism plus the connection that holds the seed lock.
func (d *Database) named(pc *pgxpool.Config) {
	if pc.ConnConfig.RuntimeParams == nil {
		pc.ConnConfig.RuntimeParams = map[string]string{}
	}
	if pc.ConnConfig.RuntimeParams["application_name"] == "" && d.name != "" {
		pc.ConnConfig.RuntimeParams["application_name"] = d.name
	}
	pc.MaxConns = max(pc.MaxConns, int32(d.parallel)+2)
}

// DB returns the application's keel.DB, opening it on first use.
func (d *Database) DB(ctx context.Context) (*keel.DB, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.db != nil {
		return d.db, nil
	}
	cfg, pc, err := d.resolve()
	if err != nil {
		return nil, err
	}
	cfg.Pool = pc.Copy()
	db, err := keel.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("keelcli: open the database: %w", err)
	}
	d.db = db
	return db, nil
}

// OpenAt opens a keel.DB with the application's configuration on the database dsn names, for
// work on another database such as a template being built. The caller closes it.
func (d *Database) OpenAt(ctx context.Context, dsn string) (*keel.DB, error) {
	d.mu.Lock()
	cfg, _, err := d.resolve()
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("keelcli: parse the DSN: %w", err)
	}
	d.named(pc)
	cfg.DSN, cfg.Pool = dsn, pc
	db, err := keel.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("keelcli: open %s: %w", pc.ConnConfig.Database, err)
	}
	return db, nil
}

// Connect opens a connection of its own to the application's database. The caller closes it.
func (d *Database) Connect(ctx context.Context) (*pgx.Conn, error) {
	d.mu.Lock()
	_, pc, err := d.resolve()
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, pc.ConnConfig.Copy())
	if err != nil {
		return nil, fmt.Errorf("keelcli: connect: %w", err)
	}
	return conn, nil
}

// Name returns the name of the application's database.
func (d *Database) Name() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, pc, err := d.resolve()
	if err != nil {
		return "", err
	}
	return pc.ConnConfig.Database, nil
}

// DSN returns the connection string the application's database was configured with.
func (d *Database) DSN() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, pc, err := d.resolve()
	if err != nil {
		return "", err
	}
	return pc.ConnString(), nil
}

// Admin returns the DSN that creates and drops databases: the override when one is given,
// otherwise the application's DSN on the postgres database.
func (d *Database) Admin() (string, error) {
	if d.admin != nil {
		if dsn := d.admin(); dsn != "" {
			return dsn, nil
		}
	}
	dsn, err := d.DSN()
	if err != nil {
		return "", err
	}
	if dsn == "" {
		return "", errors.New("keelcli: no admin DSN: the pool configuration has no connection string; set KEEL_ADMIN_DSN")
	}
	return WithDatabase(dsn, AdminDatabase), nil
}

// Refresh closes the pool, so the next use opens fresh connections: after a reset, whose old
// connections are gone, or after migrations, whose prepared statements may describe old tables.
func (d *Database) Refresh() {
	d.mu.Lock()
	db := d.db
	d.db = nil
	d.mu.Unlock()
	if db != nil {
		db.Close()
	}
}

// Close closes the pool.
func (d *Database) Close() { d.Refresh() }

// WithDatabase returns dsn connecting to the database db instead: a URL's path is replaced, and a
// keyword/value string gains a dbname, which wins over an earlier one.
func WithDatabase(dsn, db string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path, u.RawPath = "/"+db, ""
		return u.String()
	}
	return strings.TrimSpace(dsn) + " dbname=" + db
}
