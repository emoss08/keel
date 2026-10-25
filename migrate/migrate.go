// Package migrate applies SQL and Go migrations to a Postgres database.
//
// A migration is a file named <version>_<name>.sql, where version is 14 digits ordered as a number
// (Create writes a UTC timestamp such as 20261023143000), or a Func registered with a Source. Directives at the top of a file change how
// it runs:
//
//	--keel:notx                  run outside a transaction, statement by statement, split at --keel:split lines
//	--keel:lock_timeout 2s       override Options.LockTimeout (0 disables it)
//	--keel:statement_timeout 1m  bound each statement
//	--keel:no_retry              fail on the first lock timeout
//	--keel:baseline              a squashed schema: recorded without running on a database that has history
//	--keel:lint_ignore a,b       skip lint rules
//
// R_<name>.sql files are repeatable: they run after the versioned migrations whenever their
// checksum changes, in name order. There are no down migrations.
//
// Up serialises runs across processes with an advisory lock, applies each transactional migration
// in its own transaction together with its bookkeeping row, and bounds every lock wait so a
// migration never queues an exclusive lock behind a long query; a lock timeout is retried with
// backoff. An applied migration whose file changed is ErrChanged, and a pending migration older than
// the newest applied one is ErrOutOfOrder.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Table is the bookkeeping table.
const Table = "keel_migrations"

// Defaults for Options.
const (
	DefaultLockTimeout  = 5 * time.Second
	DefaultRetries      = 5
	DefaultRetryBackoff = time.Second
)

// lockKey is the advisory lock that serialises runs; lockKey+1 serialises creating the table.
const lockKey int64 = 7372631836457123681

// maxBackoff caps the wait between retries.
const maxBackoff = 30 * time.Second

var (
	// ErrChanged is returned when an applied migration's file no longer matches what was applied.
	ErrChanged = errors.New("migrate: an applied migration was edited")
	// ErrOutOfOrder is returned when a pending migration is older than the newest applied one and
	// Options.AllowOutOfOrder is not set.
	ErrOutOfOrder = errors.New("migrate: a pending migration is older than the newest applied one")
	// ErrBaselineGap is returned when a database has history that ends before a baseline, so the
	// migrations the baseline replaced are missing from it.
	ErrBaselineGap = errors.New("migrate: the database's history ends before the baseline")
)

// Options configure a Migrator.
type Options struct {
	// Schema holds keel_migrations; default public. Up creates it when missing.
	Schema string
	// LockTimeout bounds each lock wait of a migration; default DefaultLockTimeout, negative for
	// none. A file's --keel:lock_timeout overrides it.
	LockTimeout time.Duration
	// Retries is how many times a migration that hit its lock timeout is retried; default
	// DefaultRetries, negative for none.
	Retries int
	// RetryBackoff is the wait before the first retry, doubled for each one after it up to 30s;
	// default DefaultRetryBackoff.
	RetryBackoff time.Duration
	// AllowOutOfOrder applies pending migrations older than the newest applied one instead of
	// returning ErrOutOfOrder.
	AllowOutOfOrder bool
	// After lists statements run once, in the last migration's transaction, after a run that
	// applied anything; when the last migration is notx, or a run fails after applying some, they
	// run in a transaction of their own.
	After []string
	// AfterFunc runs once after a run that applied anything, outside every transaction and after
	// the migration lock is released, with the migrations the run applied (also when it failed
	// part-way, joining its error to the run's). Use it for work SQL cannot do, such as telling a
	// data layer's caches that migrated tables changed (keel.MarkChanged).
	AfterFunc func(ctx context.Context, applied []Migration) error
	// Observer receives the run's progress. It is called synchronously on the goroutine running
	// Up, Baseline or Repair, so it must not block.
	Observer func(Event)
}

// lockTimeout returns the lock timeout for a migration with directives d; zero means none.
func (o Options) lockTimeout(d Directives) time.Duration {
	switch {
	case d.LockTimeout < 0:
		return 0
	case d.LockTimeout > 0:
		return d.LockTimeout
	case o.LockTimeout < 0:
		return 0
	case o.LockTimeout > 0:
		return o.LockTimeout
	}
	return DefaultLockTimeout
}

// retries returns how many retries a migration with directives d gets.
func (o Options) retries(d Directives) int {
	switch {
	case d.NoRetry || o.Retries < 0:
		return 0
	case o.Retries > 0:
		return o.Retries
	}
	return DefaultRetries
}

// backoff returns the wait before retry attempt+1, with up to 25% jitter.
func (o Options) backoff(attempt int) time.Duration {
	base := o.RetryBackoff
	if base <= 0 {
		base = DefaultRetryBackoff
	}
	d := base << min(attempt-1, 20)
	if d <= 0 || d > maxBackoff {
		d = maxBackoff
	}
	return d + time.Duration(rand.Int64N(int64(d)/4+1))
}

// table returns the quoted, schema-qualified bookkeeping table.
func (o Options) table() string { return pgx.Identifier{o.schema(), Table}.Sanitize() }

// schema returns the bookkeeping schema.
func (o Options) schema() string {
	if o.Schema == "" {
		return "public"
	}
	return o.Schema
}

// Record is one row of keel_migrations.
type Record struct {
	Version   string
	Name      string
	Source    string
	Kind      Kind
	Checksum  string
	AppliedAt time.Time
	Duration  time.Duration
}

// ID is the record's version and name, or R_<name> for a repeatable migration.
func (r Record) ID() string {
	if r.Kind == KindRepeatable {
		return "R_" + r.Name
	}
	return r.Version + "_" + r.Name
}

// Migrator applies a Set to one database. It is not safe for concurrent use; concurrent runs in
// other goroutines or processes serialise on the database's advisory lock.
type Migrator struct {
	conn       *pgx.Conn
	owned      bool
	opts       Options
	migrations []Migration
}

// Open connects to dsn and returns a Migrator for set. The connection's application_name defaults
// to "keel migrate", so other sessions can see who holds a lock.
func Open(ctx context.Context, dsn string, set Set, opts Options) (*Migrator, error) {
	if set.err != nil {
		return nil, set.err
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "keel migrate"
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("migrate: connect: %w", err)
	}
	return &Migrator{conn: conn, owned: true, opts: opts, migrations: set.migrations}, nil
}

// New returns a Migrator on an open connection, which the caller keeps ownership of.
func New(conn *pgx.Conn, set Set, opts Options) (*Migrator, error) {
	if set.err != nil {
		return nil, set.err
	}
	return &Migrator{conn: conn, opts: opts, migrations: set.migrations}, nil
}

// Close closes the connection Open made; it does nothing for a Migrator made by New.
func (m *Migrator) Close() error {
	if !m.owned {
		return nil
	}
	return m.conn.Close(context.Background())
}

// Migrations returns the set's migrations, in order.
func (m *Migrator) Migrations() []Migration { return m.migrations }

// Conn returns the Migrator's connection.
func (m *Migrator) Conn() *pgx.Conn { return m.conn }

// emit sends e to the observer.
func (m *Migrator) emit(e Event) {
	if m.opts.Observer != nil {
		m.opts.Observer(e)
	}
}

// tableExists reports whether keel_migrations exists.
func (m *Migrator) tableExists(ctx context.Context) (bool, error) {
	var ok bool
	err := m.conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", m.opts.table()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("migrate: %w", err)
	}
	return ok, nil
}

// ensureTable creates the bookkeeping schema and table when missing. Concurrent creators
// serialise on a transaction-scoped advisory lock, since CREATE … IF NOT EXISTS is not safe
// against itself.
func (m *Migrator) ensureTable(ctx context.Context) error {
	if ok, err := m.tableExists(ctx); err != nil || ok {
		return err
	}
	err := pgx.BeginFunc(ctx, m.conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey+1); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL",
			pgx.Identifier{m.opts.schema()}.Sanitize()).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{m.opts.schema()}.Sanitize()); err != nil {
				return err
			}
		}
		t := m.opts.table()
		_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+t+` (
	version text NOT NULL,
	name text NOT NULL,
	source text NOT NULL,
	kind text NOT NULL,
	checksum text NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now(),
	duration_ms bigint NOT NULL DEFAULT 0,
	PRIMARY KEY (version, name)
);
CREATE UNIQUE INDEX IF NOT EXISTS keel_migrations_version_key ON `+t+` (version) WHERE version <> ''`)
		return err
	})
	if err != nil {
		return fmt.Errorf("migrate: create %s: %w", m.opts.table(), err)
	}
	return nil
}

// records reads keel_migrations, oldest first; a missing table has none.
func (m *Migrator) records(ctx context.Context) ([]Record, error) {
	if ok, err := m.tableExists(ctx); err != nil || !ok {
		return nil, err
	}
	rows, err := m.conn.Query(ctx, `SELECT version, name, source, kind, checksum, applied_at, duration_ms
FROM `+m.opts.table()+` ORDER BY applied_at, version, name`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", Table, err)
	}
	recs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Record, error) {
		var r Record
		var kind string
		var ms int64
		err := row.Scan(&r.Version, &r.Name, &r.Source, &kind, &r.Checksum, &r.AppliedAt, &ms)
		r.Kind, r.Duration = parseKind(kind), time.Duration(ms)*time.Millisecond
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", Table, err)
	}
	return recs, nil
}

// History returns every applied migration, oldest first, including those no longer in the set.
func (m *Migrator) History(ctx context.Context) ([]Record, error) { return m.records(ctx) }

// lock takes the session-level migration lock, sending LockWait about every second while another
// session holds it. It polls pg_try_advisory_lock rather than waiting in pg_advisory_lock: a
// waiting statement holds a transaction open, and CREATE INDEX CONCURRENTLY in the run that holds
// the lock waits for every open transaction to finish.
func (m *Migrator) lock(ctx context.Context) (func(), error) {
	start := time.Now()
	wait := 20 * time.Millisecond
	var reported time.Time
	for {
		var got bool
		if err := m.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&got); err != nil {
			return nil, fmt.Errorf("migrate: lock: %w", err)
		}
		if got {
			break
		}
		if reported.IsZero() || time.Since(reported) >= time.Second {
			blockers, _ := advisoryBlockers(ctx, m.conn)
			m.emit(Event{Kind: LockWait, Blockers: blockers, Duration: time.Since(start)})
			reported = time.Now()
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("migrate: lock: %w", err)
		}
		wait = min(wait*2, 250*time.Millisecond)
	}
	m.emit(Event{Kind: LockAcquired, Duration: time.Since(start)})
	return func() {
		_, _ = m.conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockKey)
	}, nil
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isLockTimeout reports whether err is lock_not_available (55P03).
func isLockTimeout(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55P03"
}

// UpOptions configure one Up.
type UpOptions struct {
	// To stops after this version; repeatable migrations then run only when no versioned
	// migration remains pending.
	To string
	// DryRun returns the plan without changing anything.
	DryRun bool
}

// Result is what Up did.
type Result struct {
	Plan     Plan
	Applied  []Migration
	Marked   []Migration
	Duration time.Duration
}

// Up applies the plan: it records the baselines a database with history already has, runs every
// pending migration in version order, then the repeatable migrations that are due, then
// Options.After. It returns what it applied, also when it fails part-way.
func (m *Migrator) Up(ctx context.Context, o UpOptions) (Result, error) {
	if o.DryRun {
		p, err := m.Plan(ctx, o)
		return Result{Plan: p}, err
	}
	start := time.Now()
	r := &run{m: m, watch: newWatcher(m.conn)}
	defer r.watch.close()
	res, err := r.up(ctx, o)
	if len(res.Applied) > 0 && m.opts.AfterFunc != nil {
		if afterErr := m.opts.AfterFunc(ctx, res.Applied); afterErr != nil {
			err = errors.Join(err, fmt.Errorf("migrate: after: %w", afterErr))
		}
	}
	res.Duration = time.Since(start)
	m.emit(Event{Kind: RunEnd, Total: res.Plan.Total(), Duration: res.Duration, Err: err})
	return res, err
}

// Plan returns what Up would do, without changing anything. It returns the errors Up would:
// ErrChanged, ErrOutOfOrder, ErrBaselineGap or an unknown To.
func (m *Migrator) Plan(ctx context.Context, o UpOptions) (Plan, error) {
	recs, err := m.records(ctx)
	if err != nil {
		return Plan{}, err
	}
	return plan(m.migrations, recs, m.opts, o)
}

// Status returns every migration of the set with its state, followed by the applied migrations
// no longer in it.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	recs, err := m.records(ctx)
	if err != nil {
		return nil, err
	}
	return analyze(m.migrations, recs).statuses, nil
}

// Check compares the set with the database's history.
func (m *Migrator) Check(ctx context.Context) (CheckReport, error) {
	recs, err := m.records(ctx)
	if err != nil {
		return CheckReport{}, err
	}
	return analyze(m.migrations, recs).report(), nil
}

// Baseline records every migration of the set up to and including version as applied, without
// running it, and returns the ones it recorded. It is how an existing database adopts keel.
func (m *Migrator) Baseline(ctx context.Context, version string) ([]Migration, error) {
	if !hasVersion(m.migrations, version) {
		return nil, fmt.Errorf("migrate: baseline: no migration has version %s", version)
	}
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	recs, err := m.records(ctx)
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, r := range recs {
		done[r.Version] = true
	}
	var out []Migration
	for _, mg := range m.migrations {
		if mg.Kind != KindRepeatable && mg.Version <= version && !done[mg.Version] {
			out = append(out, mg)
		}
	}
	err = pgx.BeginFunc(ctx, m.conn, func(tx pgx.Tx) error {
		for _, mg := range out {
			if err := record(ctx, tx, m.opts, mg, 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: baseline: %w", err)
	}
	for i := range out {
		m.emit(Event{Kind: BaselineMarked, Migration: &out[i], Index: i, Total: len(out)})
	}
	return out, nil
}

// Repaired is the outcome of Repair.
type Repaired struct {
	Migration Migration
	Old       string
	New       string
}

// Repair accepts the current checksum of the applied migration with version, after an edit that
// does not change what it does, such as a comment. Old and New are equal when nothing changed.
func (m *Migrator) Repair(ctx context.Context, version string) (Repaired, error) {
	var mg *Migration
	for i := range m.migrations {
		if m.migrations[i].Kind != KindRepeatable && m.migrations[i].Version == version {
			mg = &m.migrations[i]
		}
	}
	if mg == nil {
		return Repaired{}, fmt.Errorf("migrate: repair: no migration has version %s", version)
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return Repaired{}, err
	}
	defer unlock()
	out := Repaired{Migration: *mg, New: mg.Checksum}
	err = m.conn.QueryRow(ctx, "SELECT checksum FROM "+m.opts.table()+" WHERE version = $1", version).Scan(&out.Old)
	if errors.Is(err, pgx.ErrNoRows) || isUndefinedTable(err) {
		return out, fmt.Errorf("migrate: repair: %s is not applied", mg.ID())
	}
	if err != nil {
		return out, fmt.Errorf("migrate: repair: %w", err)
	}
	if out.Old == out.New {
		return out, nil
	}
	_, err = m.conn.Exec(ctx, "UPDATE "+m.opts.table()+" SET checksum = $2, name = $3, source = $4 WHERE version = $1",
		version, mg.Checksum, mg.Name, mg.Source)
	if err != nil {
		return out, fmt.Errorf("migrate: repair: %w", err)
	}
	return out, nil
}

// isUndefinedTable reports whether err is undefined_table (42P01).
func isUndefinedTable(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42P01"
}
