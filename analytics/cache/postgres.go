package cache

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Defaults for PostgresOptions.
const (
	DefaultPostgresTable = "keel_analytics_cache"
	DefaultMaxValueBytes = 4 << 20
	DefaultCleanupEvery  = time.Minute
	DefaultCleanupBatch  = 1000
)

// cleanupTimeout bounds one background cleanup.
const cleanupTimeout = 30 * time.Second

// identifier is what a table name may be.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Querier is the part of a pgx pool the Postgres store uses; *pgxpool.Pool satisfies it, and a
// keel application passes db.Pool().
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PostgresOptions configure a Postgres store.
type PostgresOptions struct {
	// Table is the unlogged table values live in; default DefaultPostgresTable. The store creates
	// it on first use.
	Table string
	// MaxValueBytes caps a value; a larger one is ErrTooLarge. Default DefaultMaxValueBytes.
	MaxValueBytes int
	// CleanupEvery is how often a Set starts a background deletion of expired rows; default
	// DefaultCleanupEvery, negative to leave cleanup to explicit Cleanup calls.
	CleanupEvery time.Duration
	// CleanupBatch is how many expired rows one cleanup statement deletes; default
	// DefaultCleanupBatch.
	CleanupBatch int
	// OnError receives the errors of background cleanups.
	OnError func(err error)
}

// Postgres is a Store every process of an application shares: an UNLOGGED table, which skips the
// write-ahead log because a cache can be lost on a crash. Expired rows are never returned and are
// deleted in batches in the background, at most every CleanupEvery per process, with SKIP LOCKED
// so that processes cleaning at once do not wait on each other.
type Postgres struct {
	q    Querier
	name string
	opts PostgresOptions

	getSQL     string
	setSQL     string
	deleteSQL  string
	cleanupSQL string

	readyMu sync.Mutex
	ready   atomic.Bool

	lastCleanup atomic.Int64
	cleaning    atomic.Bool
	wg          sync.WaitGroup
}

// NewPostgres returns a Postgres store over q.
func NewPostgres(q Querier, o PostgresOptions) (*Postgres, error) {
	if o.Table == "" {
		o.Table = DefaultPostgresTable
	}
	if !identifier.MatchString(o.Table) {
		return nil, fmt.Errorf("cache: table name %q must match %s", o.Table, identifier)
	}
	if o.MaxValueBytes <= 0 {
		o.MaxValueBytes = DefaultMaxValueBytes
	}
	if o.CleanupEvery == 0 {
		o.CleanupEvery = DefaultCleanupEvery
	}
	if o.CleanupBatch <= 0 {
		o.CleanupBatch = DefaultCleanupBatch
	}
	t := pgx.Identifier{o.Table}.Sanitize()
	p := &Postgres{
		q:      q,
		name:   o.Table,
		opts:   o,
		getSQL: "SELECT value FROM " + t + " WHERE key = $1 AND expires_at > now()",
		setSQL: "INSERT INTO " + t + " AS c (key, value, expires_at) VALUES ($1, $2, " +
			"coalesce(now() + $3::int8 * interval '1 microsecond', 'infinity')) " +
			"ON CONFLICT (key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at",
		deleteSQL: "DELETE FROM " + t + " WHERE key = $1",
		cleanupSQL: "DELETE FROM " + t + " WHERE ctid = ANY(ARRAY(SELECT ctid FROM " + t +
			" WHERE expires_at <= now() LIMIT $1 FOR UPDATE SKIP LOCKED))",
	}
	p.lastCleanup.Store(time.Now().UnixNano())
	return p, nil
}

// Get returns the value under key unless it expired.
func (p *Postgres) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := p.ensure(ctx); err != nil {
		return nil, false, err
	}
	var v []byte
	err := p.q.QueryRow(ctx, p.getSQL, storedKey(key)).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache: get: %w", err)
	}
	return v, true, nil
}

// Set stores value under key for ttl, measured by the database's clock.
func (p *Postgres) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := p.ensure(ctx); err != nil {
		return err
	}
	k := storedKey(key)
	if len(value) > p.opts.MaxValueBytes {
		if _, err := p.q.Exec(ctx, p.deleteSQL, k); err != nil {
			return fmt.Errorf("cache: delete: %w", err)
		}
		return fmt.Errorf("%w: %d bytes, over %d", ErrTooLarge, len(value), p.opts.MaxValueBytes)
	}
	var micros *int64
	if ttl > 0 {
		m := ttl.Microseconds()
		micros = &m
	}
	if _, err := p.q.Exec(ctx, p.setSQL, k, value, micros); err != nil {
		return fmt.Errorf("cache: set: %w", err)
	}
	p.maybeCleanup()
	return nil
}

// Delete removes the value under key.
func (p *Postgres) Delete(ctx context.Context, key string) error {
	if err := p.ensure(ctx); err != nil {
		return err
	}
	if _, err := p.q.Exec(ctx, p.deleteSQL, storedKey(key)); err != nil {
		return fmt.Errorf("cache: delete: %w", err)
	}
	return nil
}

// Cleanup deletes expired rows, CleanupBatch per statement, until none is left that another
// cleaner has not locked, and returns how many it deleted.
func (p *Postgres) Cleanup(ctx context.Context) (int64, error) {
	if err := p.ensure(ctx); err != nil {
		return 0, err
	}
	var total int64
	for {
		tag, err := p.q.Exec(ctx, p.cleanupSQL, p.opts.CleanupBatch)
		if err != nil {
			return total, fmt.Errorf("cache: cleanup: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(p.opts.CleanupBatch) {
			return total, nil
		}
	}
}

// Close waits for a background cleanup to finish.
func (p *Postgres) Close() { p.wg.Wait() }

// maybeCleanup starts a background cleanup when CleanupEvery has passed since the last one and
// none is running.
func (p *Postgres) maybeCleanup() {
	if p.opts.CleanupEvery < 0 {
		return
	}
	now := time.Now().UnixNano()
	last := p.lastCleanup.Load()
	if now-last < int64(p.opts.CleanupEvery) || !p.cleaning.CompareAndSwap(false, true) {
		return
	}
	p.lastCleanup.Store(now)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.cleaning.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if _, err := p.Cleanup(ctx); err != nil && p.opts.OnError != nil {
			p.opts.OnError(err)
		}
	}()
}

// ensure creates the table and its expiry index when they do not exist, under a transaction-scoped
// advisory lock, since concurrent CREATE TABLE IF NOT EXISTS statements can fail. A role without
// CREATE on the schema works once the table exists.
func (p *Postgres) ensure(ctx context.Context) error {
	if p.ready.Load() {
		return nil
	}
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	if p.ready.Load() {
		return nil
	}
	var exists bool
	if err := p.q.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", p.name).Scan(&exists); err != nil {
		return fmt.Errorf("cache: find %s: %w", p.name, err)
	}
	if !exists {
		create := func(tx pgx.Tx) error { return p.create(ctx, tx) }
		if err := pgx.BeginFunc(ctx, p.q, create); err != nil {
			return fmt.Errorf("cache: create %s: %w", p.name, err)
		}
	}
	p.ready.Store(true)
	return nil
}

// create creates the table and its index in tx.
func (p *Postgres) create(ctx context.Context, tx pgx.Tx) error {
	t := pgx.Identifier{p.name}.Sanitize()
	idx := pgx.Identifier{p.name + "_expires"}.Sanitize()
	stmts := []string{
		"SELECT pg_advisory_xact_lock(hashtextextended('keel_analytics_cache:' || $1::text, 0))",
		"CREATE UNLOGGED TABLE IF NOT EXISTS " + t + ` (
    key text COLLATE "C" PRIMARY KEY,
    value bytea NOT NULL,
    expires_at timestamptz NOT NULL
)`,
		"CREATE INDEX IF NOT EXISTS " + idx + " ON " + t + " (expires_at)",
	}
	if _, err := tx.Exec(ctx, stmts[0], p.name); err != nil {
		return err
	}
	for _, s := range stmts[1:] {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
