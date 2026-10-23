// Package migrate applies SQL-file migrations.
//
// A migration is a pair of files named <version>_<name>.up.sql and <version>_<name>.down.sql,
// where version is a sortable number such as a UTC timestamp (20260101120000). An up file runs in
// one transaction together with the bookkeeping row. A file named *.notx.up.sql runs outside a
// transaction, statement by statement, for work Postgres refuses inside one (CREATE INDEX
// CONCURRENTLY, ALTER TYPE … ADD VALUE); separate its statements with a line holding only
// "--keel:split".
//
// Runs are serialised across processes with an advisory lock, so concurrent deploys never apply a
// migration twice, and an applied file whose content changed is reported instead of ignored.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Table is the bookkeeping table.
const Table = "keel_migrations"

const lockKey = 7372631836457123681

// Migration is one migration found in the source.
type Migration struct {
	Version string
	Name    string
	Up      string
	Down    string
	NoTx    bool
	Sum     string
}

// ID is the migration's version and name.
func (m Migration) ID() string { return m.Version + "_" + m.Name }

// Status is a migration with its state in the database.
type Status struct {
	Migration
	Applied   bool
	AppliedAt time.Time
	Changed   bool
}

// Migrator applies migrations from a source to one database.
type Migrator struct {
	conn       *pgx.Conn
	migrations []Migration
}

// Open connects to dsn and reads the migrations in fsys.
func Open(ctx context.Context, dsn string, fsys fs.FS) (*Migrator, error) {
	ms, err := Read(fsys)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("migrate: connect: %w", err)
	}
	return &Migrator{conn: conn, migrations: ms}, nil
}

// New uses an open connection, which the caller keeps ownership of.
func New(conn *pgx.Conn, fsys fs.FS) (*Migrator, error) {
	ms, err := Read(fsys)
	if err != nil {
		return nil, err
	}
	return &Migrator{conn: conn, migrations: ms}, nil
}

// Close closes the connection Open made.
func (m *Migrator) Close() error { return m.conn.Close(context.Background()) }

// Migrations returns the migrations found in the source, in order.
func (m *Migrator) Migrations() []Migration { return m.migrations }

// Read reads the migrations in fsys.
func Read(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read source: %w", err)
	}
	byID := map[string]*Migration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		var dir string
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			dir = "up"
		case strings.HasSuffix(name, ".down.sql"):
			dir = "down"
		default:
			return nil, fmt.Errorf("migrate: %s must end in .up.sql or .down.sql", name)
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".sql"), "."+dir)
		notx := strings.HasSuffix(base, ".notx")
		base = strings.TrimSuffix(base, ".notx")
		version, label, ok := strings.Cut(base, "_")
		if !ok || version == "" || label == "" || strings.Trim(version, "0123456789") != "" {
			return nil, fmt.Errorf("migrate: %s must be named <digits>_<name>.%s.sql", name, dir)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		mg := byID[base]
		if mg == nil {
			mg = &Migration{Version: version, Name: label}
			byID[base] = mg
		}
		if dir == "up" {
			mg.Up = string(body)
			mg.NoTx = notx
			sum := sha256.Sum256(body)
			mg.Sum = hex.EncodeToString(sum[:])
		} else {
			mg.Down = string(body)
		}
	}
	out := make([]Migration, 0, len(byID))
	for id, mg := range byID {
		if mg.Up == "" {
			return nil, fmt.Errorf("migrate: %s has a down file but no up file", id)
		}
		out = append(out, *mg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			if len(out[i].Version) != len(out[j].Version) {
				return len(out[i].Version) < len(out[j].Version)
			}
			return out[i].Version < out[j].Version
		}
		return out[i].Name < out[j].Name
	})
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("migrate: %s and %s share a version", out[i-1].ID(), out[i].ID())
		}
	}
	return out, nil
}

func (m *Migrator) ensureTable(ctx context.Context) error {
	_, err := m.conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+Table+` (
	version text PRIMARY KEY,
	name text NOT NULL,
	checksum text NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now()
)`)
	return err
}

func (m *Migrator) lock(ctx context.Context) (func(), error) {
	if _, err := m.conn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(lockKey)); err != nil {
		return nil, fmt.Errorf("migrate: lock: %w", err)
	}
	return func() {
		_, _ = m.conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", int64(lockKey))
	}, nil
}

type applied struct {
	sum string
	at  time.Time
}

func (m *Migrator) applied(ctx context.Context) (map[string]applied, error) {
	rows, err := m.conn.Query(ctx, "SELECT version, checksum, applied_at FROM "+Table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]applied{}
	for rows.Next() {
		var v string
		var a applied
		if err := rows.Scan(&v, &a.sum, &a.at); err != nil {
			return nil, err
		}
		out[v] = a
	}
	return out, rows.Err()
}

// Status reports every migration in the source and whether it is applied.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	done, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, len(m.migrations))
	for i, mg := range m.migrations {
		a, ok := done[mg.Version]
		out[i] = Status{Migration: mg, Applied: ok, AppliedAt: a.at, Changed: ok && a.sum != mg.Sum}
	}
	return out, nil
}

// ErrChanged is returned when an applied migration's up file no longer matches what was applied.
var ErrChanged = errors.New("migrate: an applied migration was edited")

// Up applies every pending migration in order and returns the ones it applied.
func (m *Migrator) Up(ctx context.Context) ([]string, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	done, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	for _, mg := range m.migrations {
		if a, ok := done[mg.Version]; ok && a.sum != mg.Sum {
			return nil, fmt.Errorf("%w: %s", ErrChanged, mg.ID())
		}
	}
	var out []string
	for _, mg := range m.migrations {
		if _, ok := done[mg.Version]; ok {
			continue
		}
		if err := m.apply(ctx, mg); err != nil {
			return out, fmt.Errorf("migrate: %s: %w", mg.ID(), err)
		}
		out = append(out, mg.ID())
	}
	return out, nil
}

func (m *Migrator) apply(ctx context.Context, mg Migration) error {
	if mg.NoTx {
		for _, s := range Split(mg.Up) {
			if _, err := m.conn.Exec(ctx, s); err != nil {
				return err
			}
		}
		_, err := m.conn.Exec(ctx, "INSERT INTO "+Table+" (version, name, checksum) VALUES ($1, $2, $3)",
			mg.Version, mg.Name, mg.Sum)
		return err
	}
	tx, err := m.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, mg.Up); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+Table+" (version, name, checksum) VALUES ($1, $2, $3)",
		mg.Version, mg.Name, mg.Sum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Down reverts the last n applied migrations, newest first, and returns the ones it reverted.
func (m *Migrator) Down(ctx context.Context, n int) ([]string, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	done, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := len(m.migrations) - 1; i >= 0 && len(out) < n; i-- {
		mg := m.migrations[i]
		if _, ok := done[mg.Version]; !ok {
			continue
		}
		if mg.Down == "" {
			return out, fmt.Errorf("migrate: %s has no down file", mg.ID())
		}
		if err := m.revert(ctx, mg); err != nil {
			return out, fmt.Errorf("migrate: revert %s: %w", mg.ID(), err)
		}
		out = append(out, mg.ID())
	}
	return out, nil
}

func (m *Migrator) revert(ctx context.Context, mg Migration) error {
	if mg.NoTx {
		for _, s := range Split(mg.Down) {
			if _, err := m.conn.Exec(ctx, s); err != nil {
				return err
			}
		}
		_, err := m.conn.Exec(ctx, "DELETE FROM "+Table+" WHERE version = $1", mg.Version)
		return err
	}
	tx, err := m.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, mg.Down); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM "+Table+" WHERE version = $1", mg.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Split splits a non-transactional migration at lines holding only "--keel:split".
func Split(sql string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(sql, "\n") {
		if strings.TrimSpace(line) == "--keel:split" {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
			continue
		}
		cur.WriteString(line)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// Create writes an empty up and down migration named name in dir, versioned with the current UTC
// time, and returns their paths.
func Create(dir, name string, notx bool) ([]string, error) {
	name = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "_"))
	if name == "" {
		return nil, errors.New("migrate: empty name")
	}
	version := time.Now().UTC().Format("20060102150405")
	suffix := ""
	if notx {
		suffix = ".notx"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var paths []string
	for _, d := range []string{"up", "down"} {
		p := filepath.Join(dir, fmt.Sprintf("%s_%s%s.%s.sql", version, name, suffix, d))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			return paths, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}
