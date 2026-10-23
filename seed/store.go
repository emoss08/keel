package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/emoss08/keel"
	"github.com/jackc/pgx/v5"
)

// lockKey is the advisory lock key every seed run, prune and environment mark holds.
const lockKey = "keel:seed"

// ddl creates the bookkeeping tables. They hold no tenant data, so they are not tenant tables.
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS keel_seeds (
    name text PRIMARY KEY,
    kind text NOT NULL,
    fingerprint text NOT NULL,
    inputs jsonb NOT NULL DEFAULT '{}',
    version text NOT NULL DEFAULT '',
    code text NOT NULL DEFAULT '',
    applied_at timestamptz NOT NULL,
    duration_ms bigint NOT NULL,
    rows bigint NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS keel_seed_tenants (
    name text NOT NULL,
    tenant text NOT NULL,
    fingerprint text NOT NULL,
    applied_at timestamptz NOT NULL,
    PRIMARY KEY (name, tenant)
)`,
	`CREATE TABLE IF NOT EXISTS keel_meta (
    key text PRIMARY KEY,
    value text NOT NULL
)`,
}

const (
	insertMeta = `INSERT INTO keel_meta (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`
	upsertMeta = `INSERT INTO keel_meta (key, value) VALUES ($1, $2)
    ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`
	upsertSeed = `INSERT INTO keel_seeds (name, kind, fingerprint, inputs, version, code, applied_at, duration_ms, rows)
    VALUES ($1, $2, $3, $4::text::jsonb, $5, $6, now(), $7, $8)
    ON CONFLICT (name) DO UPDATE SET kind = EXCLUDED.kind, fingerprint = EXCLUDED.fingerprint,
        inputs = EXCLUDED.inputs, version = EXCLUDED.version, code = EXCLUDED.code,
        applied_at = EXCLUDED.applied_at, duration_ms = EXCLUDED.duration_ms, rows = EXCLUDED.rows`
	upsertTenant = `INSERT INTO keel_seed_tenants (name, tenant, fingerprint, applied_at) VALUES ($1, $2, $3, now())
    ON CONFLICT (name, tenant) DO UPDATE SET fingerprint = EXCLUDED.fingerprint, applied_at = EXCLUDED.applied_at`
	selectTables = `SELECT to_regclass('keel_seeds') IS NOT NULL, to_regclass('keel_seed_tenants') IS NOT NULL,
    to_regclass('keel_meta') IS NOT NULL`
	selectSeeds = `SELECT name, kind, fingerprint, inputs::text, version, code, applied_at, duration_ms, rows
    FROM keel_seeds`
	selectTenantCounts = `SELECT name, fingerprint, count(*) FROM keel_seed_tenants GROUP BY name, fingerprint`
	selectDoneTenants  = `SELECT tenant FROM keel_seed_tenants WHERE name = $1 AND fingerprint = $2`
	selectEnv          = `SELECT value FROM keel_meta WHERE key = $1`
	deleteSeeds        = `DELETE FROM keel_seeds WHERE name = ANY($1::text[])`
	deleteSeedTenants  = `DELETE FROM keel_seed_tenants WHERE name = ANY($1::text[])`
)

// createTables creates the bookkeeping tables in the transaction on ctx, or in one statement each.
// Callers hold the seed lock, so concurrent creation cannot race.
func createTables(ctx context.Context, db *keel.DB) error {
	for _, q := range ddl {
		if _, err := keel.Exec(ctx, db, q); err != nil {
			return fmt.Errorf("seed: create bookkeeping tables: %w", err)
		}
	}
	return nil
}

// querier reads rows: a pool or one of its connections.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// stored is a seed's bookkeeping row.
type stored struct {
	name        string
	kind        string
	fingerprint string
	inputs      map[string]string
	version     string
	code        string
	appliedAt   time.Time
	duration    time.Duration
	rows        int64
}

// tenantKey identifies the tenants a seed applied with one fingerprint.
type tenantKey struct{ name, fingerprint string }

// state is what the database records about seeds. names lists every seed with a row in
// keel_seeds or keel_seed_tenants; ready reports that every bookkeeping table exists.
type state struct {
	seeds   map[string]*stored
	names   []string
	tenants map[tenantKey]int
	env     Env
	ready   bool
}

// loadState reads the bookkeeping tables. Tables that do not exist yet read as empty.
func loadState(ctx context.Context, q querier) (*state, error) {
	st := &state{seeds: map[string]*stored{}, tenants: map[tenantKey]int{}}
	var seeds, tenants, meta bool
	err := queryRows(ctx, q, selectTables, nil, func(rows pgx.Rows) error {
		return rows.Scan(&seeds, &tenants, &meta)
	})
	if err != nil {
		return nil, err
	}
	st.ready = seeds && tenants && meta
	if seeds {
		if err := queryRows(ctx, q, selectSeeds, nil, st.scanSeed); err != nil {
			return nil, err
		}
	}
	if tenants {
		if err := queryRows(ctx, q, selectTenantCounts, nil, st.scanTenantCount); err != nil {
			return nil, err
		}
	}
	if meta {
		err := queryRows(ctx, q, selectEnv, []any{envKey}, func(rows pgx.Rows) error {
			return rows.Scan(&st.env)
		})
		if err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (st *state) scanSeed(rows pgx.Rows) error {
	s := &stored{}
	var inputs string
	var ms int64
	if err := rows.Scan(&s.name, &s.kind, &s.fingerprint, &inputs, &s.version, &s.code, &s.appliedAt,
		&ms, &s.rows); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(inputs), &s.inputs); err != nil {
		return fmt.Errorf("seed: decode inputs of %q: %w", s.name, err)
	}
	s.duration = time.Duration(ms) * time.Millisecond
	st.seeds[s.name] = s
	st.names = append(st.names, s.name)
	return nil
}

func (st *state) scanTenantCount(rows pgx.Rows) error {
	var k tenantKey
	var n int
	if err := rows.Scan(&k.name, &k.fingerprint, &n); err != nil {
		return err
	}
	st.tenants[k] = n
	if _, ok := st.seeds[k.name]; !ok && !slices.Contains(st.names, k.name) {
		st.names = append(st.names, k.name)
	}
	return nil
}

// doneTenants returns the tenants that applied a seed with its current fingerprint.
func doneTenants(ctx context.Context, q querier, p *prepared) (map[string]bool, error) {
	done := map[string]bool{}
	err := queryRows(ctx, q, selectDoneTenants, []any{p.seed.Name, p.fingerprint}, func(rows pgx.Rows) error {
		var t string
		if err := rows.Scan(&t); err != nil {
			return err
		}
		done[t] = true
		return nil
	})
	return done, err
}

// queryRows runs sql and calls scan for each row.
func queryRows(ctx context.Context, q querier, sql string, args []any, scan func(pgx.Rows) error) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("seed: read bookkeeping: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("seed: read bookkeeping: %w", err)
	}
	return nil
}

// writeSeed records a seed as applied, in the transaction on ctx.
func writeSeed(ctx context.Context, db *keel.DB, p *prepared, d time.Duration, rows int64) error {
	inputs, err := json.Marshal(p.inputs)
	if err != nil {
		return err
	}
	_, err = keel.Exec(ctx, db, upsertSeed, p.seed.Name, p.seed.Kind.String(), p.fingerprint, string(inputs),
		p.seed.Version, p.code, d.Milliseconds(), rows)
	if err != nil {
		return fmt.Errorf("record the run: %w", err)
	}
	return nil
}

// writeTenant records a tenant of a PerTenant seed as applied, in the transaction on ctx.
func writeTenant(ctx context.Context, db *keel.DB, p *prepared, tenant string) error {
	if _, err := keel.Exec(ctx, db, upsertTenant, p.seed.Name, tenant, p.fingerprint); err != nil {
		return fmt.Errorf("record the tenant's run: %w", err)
	}
	return nil
}
