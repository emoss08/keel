package adapt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/seed"
)

// DefaultStamp is the schema version file stamp writes when the application names none.
const DefaultStamp = "schema/version_gen.go"

// MigrationsConfig is what the migration adapter works from.
//
// Set returns the migrations, read again on every call when they come from Dirs so a file written
// by Create shows at once. Dirs are the sources' directories on disk, which Create, Squash and
// Stamp write to. Seeds, when set, are the seeds whose reference data a template carries;
// SeedOptions supplies their tenants, system scope and parallelism.
type MigrationsConfig struct {
	Name        string
	DB          *Database
	Set         func() migrate.Set
	Options     migrate.Options
	Dirs        []migrate.Dir
	Stamp       string
	Dump        []string
	Lint        []migrate.LintRule
	Seeds       *seed.Registry
	SeedOptions seed.Options
	Now         func() time.Time
}

// Migrations implements migrateport.Engine over keel/migrate. It remembers the template key, which
// depends only on the seeds the binary was built with.
type Migrations struct {
	c MigrationsConfig

	mu      sync.Mutex
	seedKey string
}

// Migrations implements every migration port.
var _ migrateport.Engine = (*Migrations)(nil)

// NewMigrations returns the migration adapter.
func NewMigrations(c MigrationsConfig) *Migrations {
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Migrations{c: c}
}

// options are the migrator options with observe in front of the application's own observer.
func (x *Migrations) options(observe func(migrate.Event)) migrate.Options {
	o := x.c.Options
	own := o.Observer
	o.Observer = nil
	if observe != nil {
		o.Observer = func(e migrate.Event) {
			observe(e)
			if own != nil {
				own(e)
			}
		}
	}
	return o
}

// set returns the migrations with the error they were read with.
func (x *Migrations) set() (migrate.Set, error) {
	s := x.c.Set()
	return s, s.Err()
}

// read runs fn with a migrator on a pooled connection, for calls that take no lock.
func (x *Migrations) read(ctx context.Context, fn func(m *migrate.Migrator) error) error {
	set, err := x.set()
	if err != nil {
		return err
	}
	db, err := x.c.DB.DB(ctx)
	if err != nil {
		return err
	}
	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("keelcli: connect: %w", err)
	}
	defer conn.Release()
	m, err := migrate.New(conn.Conn(), set, x.options(nil))
	if err != nil {
		return err
	}
	return fn(m)
}

// write runs fn with a migrator on a connection of its own, for calls that take the migration
// lock and set session timeouts.
func (x *Migrations) write(ctx context.Context, observe func(migrate.Event), fn func(m *migrate.Migrator) error) error {
	set, err := x.set()
	if err != nil {
		return err
	}
	conn, err := x.c.DB.Connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	m, err := migrate.New(conn, set, x.options(observe))
	if err != nil {
		return err
	}
	return fn(m)
}

// Status lists every migration with its state.
func (x *Migrations) Status(ctx context.Context) ([]migrateport.Status, error) {
	var out []migrateport.Status
	err := x.read(ctx, func(m *migrate.Migrator) error {
		st, err := m.Status(ctx)
		out = statuses(st)
		return err
	})
	return out, err
}

// History lists the bookkeeping rows, oldest first.
func (x *Migrations) History(ctx context.Context) ([]migrateport.Record, error) {
	var out []migrateport.Record
	err := x.read(ctx, func(m *migrate.Migrator) error {
		rs, err := m.History(ctx)
		out = records(rs)
		return err
	})
	return out, err
}

// Plan reports what Up would do, with each migration's SQL.
func (x *Migrations) Plan(ctx context.Context, opts migrateport.PlanOptions) (migrateport.Plan, error) {
	var out migrateport.Plan
	err := x.read(ctx, func(m *migrate.Migrator) error {
		p, err := m.Plan(ctx, migrate.UpOptions{To: opts.To})
		out = plan(p)
		return err
	})
	return out, err
}

// Up applies pending migrations on a connection of its own, translating the engine's events. A
// run that applied anything refreshes the shared pool, whose prepared statements may describe
// tables the run changed.
func (x *Migrations) Up(ctx context.Context, opts migrateport.UpOptions, observe migrateport.Observer) error {
	t := newMigrationEvents(observe, len(x.c.Options.After), x.c.Now)
	return x.write(ctx, t.event, func(m *migrate.Migrator) error {
		res, err := m.Up(ctx, migrate.UpOptions{To: opts.To})
		if len(res.Applied) > 0 || len(res.Marked) > 0 {
			x.c.DB.Refresh()
		}
		return err
	})
}

// dir returns the directory of source, or of the first source when it is empty.
func (x *Migrations) dir(source string) (migrate.Dir, error) {
	if len(x.c.Dirs) == 0 {
		return migrate.Dir{}, errors.New("keelcli: no migration directory: set App.Dirs to the directories of the migration sources")
	}
	if source == "" {
		return x.c.Dirs[0], nil
	}
	for _, d := range x.c.Dirs {
		if d.Name == source {
			return d, nil
		}
	}
	return migrate.Dir{}, fmt.Errorf("keelcli: no migration directory for source %q", source)
}

// Create writes an empty migration file into the source's directory.
func (x *Migrations) Create(_ context.Context, req migrateport.CreateRequest) (migrateport.Created, error) {
	d, err := x.dir(req.Source)
	if err != nil {
		return migrateport.Created{}, err
	}
	path, err := migrate.Create(d.Path, req.Name, migrate.CreateOptions{
		NoTx: req.Kind == migrateport.NoTx, Repeatable: req.Kind == migrateport.RepeatableFile, Now: x.c.Now(),
	})
	if err != nil {
		return migrateport.Created{}, err
	}
	return migrateport.Created{Path: path, Migration: created(path, d.Name, req.Kind)}, nil
}

// created describes the migration file Create wrote at path.
func created(path, source string, kind migrateport.CreateKind) migrateport.Migration {
	base := strings.TrimSuffix(filepath.Base(path), ".sql")
	m := migrateport.Migration{Source: source, Directives: migrateport.Directives{NoTx: kind == migrateport.NoTx}}
	if name, ok := strings.CutPrefix(base, "R_"); ok && kind == migrateport.RepeatableFile {
		m.Name, m.Kind = name, migrateport.Repeatable
		return m
	}
	m.Version, m.Name, _ = strings.Cut(base, "_")
	return m
}

// Squash replaces the migrations up to opts.To with one baseline, dumping the schema with the
// configured pg_dump command.
func (x *Migrations) Squash(ctx context.Context, opts migrateport.SquashOptions) (migrateport.Squashed, error) {
	if _, err := x.dir(""); err != nil {
		return migrateport.Squashed{}, err
	}
	admin, err := x.c.DB.Admin()
	if err != nil {
		return migrateport.Squashed{}, err
	}
	dump := opts.Dump
	if len(dump) == 0 {
		dump = x.c.Dump
	}
	res, err := migrate.Squash(ctx, migrate.SquashOptions{Admin: admin, Dirs: x.c.Dirs, To: opts.To, Dump: dump, Options: x.options(nil)})
	if err != nil {
		return migrateport.Squashed{}, err
	}
	return migrateport.Squashed{Path: res.Baseline, Version: res.Version, Replaced: len(res.Squashed)}, nil
}

// Stamp writes the schema version file, or with opts.Check reports whether it is current. The
// file's package is the one it already declares, or that of the Go files beside it, or its
// directory's name.
func (x *Migrations) Stamp(_ context.Context, opts migrateport.StampOptions) (migrateport.Stamp, error) {
	set, err := x.set()
	if err != nil {
		return migrateport.Stamp{}, err
	}
	path := firstNonEmpty(opts.Out, x.c.Stamp, DefaultStamp)
	src, err := migrate.Stamp(set, stampPackage(path))
	if err != nil {
		return migrateport.Stamp{}, err
	}
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return migrateport.Stamp{}, fmt.Errorf("keelcli: stamp: %w", err)
	}
	out := migrateport.Stamp{Path: path, Version: set.Version(), Fingerprint: set.Fingerprint(), Current: bytes.Equal(cur, src)}
	if opts.Check || out.Current {
		return out, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return out, fmt.Errorf("keelcli: stamp: %w", err)
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		return out, fmt.Errorf("keelcli: stamp: %w", err)
	}
	out.Current = true
	return out, nil
}

// stampPackage is the package of the schema version file at path.
func stampPackage(path string) string {
	if pkg := packageOf(path); pkg != "" {
		return pkg
	}
	dir := filepath.Dir(path)
	if files, err := filepath.Glob(filepath.Join(dir, "*.go")); err == nil {
		for _, f := range files {
			if pkg := packageOf(f); pkg != "" && !strings.HasSuffix(f, "_test.go") {
				return pkg
			}
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Base(dir)
	}
	return filepath.Base(abs)
}

// packageOf is the package a Go file declares, or "" when it cannot be read.
func packageOf(file string) string {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.PackageClauseOnly)
	if err != nil {
		return ""
	}
	return f.Name.Name
}

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Check reports ordering and checksum problems against the database's history, or against the
// base branch's directories when opts.Base names them, which needs no database.
func (x *Migrations) Check(ctx context.Context, opts migrateport.CheckOptions) ([]migrateport.Problem, error) {
	set, err := x.set()
	if err != nil {
		return nil, err
	}
	if len(opts.Base) > 0 {
		rep, err := migrate.CheckAgainst(set, baseSet(opts.Base))
		return problems(rep, true), err
	}
	var rep migrate.CheckReport
	err = x.read(ctx, func(m *migrate.Migrator) error {
		rep, err = m.Check(ctx)
		return err
	})
	return problems(rep, false), err
}

// DirSet reads the migration directories, each as the source it names.
func DirSet(dirs []migrate.Dir) migrate.Set {
	sources := make([]migrate.Source, len(dirs))
	for i, d := range dirs {
		sources[i] = migrate.Source{Name: d.Name, FS: os.DirFS(d.Path)}
	}
	return migrate.Sources(sources...)
}

// baseSet reads the base branch's directories.
func baseSet(dirs []migrateport.Dir) migrate.Set {
	out := make([]migrate.Dir, len(dirs))
	for i, d := range dirs {
		out[i] = migrate.Dir{Name: d.Name, Path: d.Path}
	}
	return DirSet(out)
}

// problems converts a check report, worded for the database's history or the base branch.
func problems(rep migrate.CheckReport, base bool) []migrateport.Problem {
	where := "applied"
	if base {
		where = "on the base branch"
	}
	var out []migrateport.Problem
	add := func(kind migrateport.ProblemKind, ss []migrate.Status, message func(migrate.Status) string) {
		for _, s := range ss {
			out = append(out, migrateport.Problem{Kind: kind, Migration: migration(s.Migration), Message: message(s)})
		}
	}
	add(migrateport.OutOfOrder, rep.OutOfOrder, func(migrate.Status) string {
		return "pending, older than the newest migration " + where + "; give it a newer version"
	})
	add(migrateport.ChangedFile, rep.Changed, func(s migrate.Status) string {
		return fmt.Sprintf("checksum %s differs from the %s %s; add a new migration, or repair after a comment-only edit",
			short(s.Checksum), where, short(s.AppliedChecksum))
	})
	add(migrateport.MissingFile, rep.Missing, func(migrate.Status) string {
		return where + ", but no longer in any source"
	})
	add(migrateport.BaselineGap, rep.Gaps, func(migrate.Status) string {
		return "the history " + where + " ends before this baseline; apply the migrations it replaced first"
	})
	return out
}

// short shortens a checksum for a message.
func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// Lint lints the pending migrations, or every migration with opts.All or without a database,
// with the built-in rules and the application's.
func (x *Migrations) Lint(ctx context.Context, opts migrateport.LintOptions) ([]migrateport.Finding, error) {
	set, err := x.set()
	if err != nil {
		return nil, err
	}
	all, _ := set.Migrations()
	targets := all
	if !opts.All {
		targets, err = x.pending(ctx)
		if errors.Is(err, ErrNoDatabase) {
			targets, err = all, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return findings(migrate.Lint(targets, x.c.Lint...)), nil
}

// pending lists the pending and due repeatable migrations of the database.
func (x *Migrations) pending(ctx context.Context) ([]migrate.Migration, error) {
	var out []migrate.Migration
	err := x.read(ctx, func(m *migrate.Migrator) error {
		st, err := m.Status(ctx)
		for _, s := range st {
			if s.State == migrate.StatePending || s.State == migrate.StateRepeatableDue {
				out = append(out, s.Migration)
			}
		}
		return err
	})
	return out, err
}

// Baseline records every migration up to version as applied without running it.
func (x *Migrations) Baseline(ctx context.Context, version string) (migrateport.Baselined, error) {
	var out migrateport.Baselined
	err := x.write(ctx, nil, func(m *migrate.Migrator) error {
		marked, err := m.Baseline(ctx, version)
		out.Marked = migrations(marked)
		return err
	})
	return out, err
}

// Repair accepts the current checksum of the applied migration with version.
func (x *Migrations) Repair(ctx context.Context, version string) (migrateport.Repaired, error) {
	var out migrateport.Repaired
	err := x.write(ctx, nil, func(m *migrate.Migrator) error {
		r, err := m.Repair(ctx, version)
		out = migrateport.Repaired{Migration: migration(r.Migration), OldChecksum: r.Old, NewChecksum: r.New}
		return err
	})
	return out, err
}
