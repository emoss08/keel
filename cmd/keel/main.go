// Command keel generates typed data-access packages for keel models and manages migrations.
//
//	keel gen [-out {dir}/{name}db] [-kinds keelkinds] [-check] ./internal/...
//	keel migrate <command> [flags] [args]
//
// Run keel with no arguments for the migrate commands and flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/emoss08/keel/gen"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/migrate/migratecli"
)

// usageText is printed for a missing or unknown command.
const usageText = `usage:
  keel gen [-out {dir}/{name}db] [-kinds keelkinds] [-tags a,b] [-check] packages...
  keel migrate up [-to VERSION] [-dry-run]     apply pending migrations
  keel migrate plan [-to VERSION]              show what up would do
  keel migrate status                          every migration with its state
  keel migrate history                         applied migrations, oldest first
  keel migrate create NAME [-notx] [-repeatable]
  keel migrate check [-base-dir name=path]...  pending, out-of-order, changed, missing; exit 1 on problems
  keel migrate lint [-all]                     lint pending migrations (every one without a database)
  keel migrate baseline VERSION                record migrations up to VERSION as applied
  keel migrate repair VERSION                  accept an applied migration's new checksum
  keel migrate squash [-to VERSION] [-admin DSN] [-dump "pg_dump ..."]
  keel migrate stamp [-package schema] [-out schema/version_gen.go] [-check]
  keel migrate templates [prune] [-key KEY] [-keep NAME]...

migrate flags, accepted by every command:
  -dsn DSN               database (default $KEEL_DSN)
  -dir [name=]path       a migration source, repeatable (default migrations)
  -schema NAME           schema of keel_migrations (default public)
  -lock-timeout D        lock timeout per migration, 0 for none (default 5s)
  -retries N             retries after a lock timeout, 0 for none (default 5)
  -allow-out-of-order    apply migrations older than the newest applied one
  -after SQL             statement run after a run that applied anything, repeatable
  -json                  JSON output (JSON lines of events for up)`

// errUsage is returned for a malformed command line.
var errUsage = errors.New("usage")

// main runs the command line until it ends or is interrupted.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run runs the command line and returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var err error
	switch {
	case len(args) == 0:
		err = errUsage
	case args[0] == "gen":
		err = runGen(args[1:], stdout, stderr)
	case args[0] == "migrate":
		err = runMigrate(ctx, args[1:], stdout, stderr)
	default:
		err = errUsage
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage) || errors.Is(err, flag.ErrHelp):
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	fmt.Fprintln(stderr, "keel:", err)
	return 1
}

// runGen runs keel gen.
func runGen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "{dir}/{name}db", "generated package directory, relative to the module root")
	kinds := fs.String("kinds", "keelkinds", "shared column kinds package directory, relative to the module root")
	tags := fs.String("tags", "", "comma-separated build tags")
	check := fs.Bool("check", false, "fail when generated files are out of date instead of writing them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	patterns := fs.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	o := gen.Options{Patterns: patterns, Out: *out, Kinds: *kinds}
	if *tags != "" {
		o.Tags = strings.Split(*tags, ",")
	}
	files, err := gen.Generate(o)
	if err != nil {
		return err
	}
	if *check {
		if stale := gen.Stale(files); len(stale) > 0 {
			return fmt.Errorf("generated files are out of date; run keel gen:\n  %s", strings.Join(stale, "\n  "))
		}
		return nil
	}
	changed, err := gen.Write(files)
	for _, c := range changed {
		fmt.Fprintln(stdout, "wrote", c)
	}
	return err
}

// listFlag is a repeatable string flag.
type listFlag []string

// String joins the values.
func (l *listFlag) String() string { return strings.Join(*l, ",") }

// Set appends a value.
func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// migrateFlags are the flags of a migrate command.
type migrateFlags struct {
	fs          *flag.FlagSet
	dsn         string
	dirs        listFlag
	schema      string
	lockTimeout time.Duration
	retries     int
	outOfOrder  bool
	after       listFlag
	json        bool

	to, admin, dump, pkg, out, key string
	dryRun, noTx, repeatable       bool
	check, all                     bool
	baseDirs, keep                 listFlag
}

// newMigrateFlags declares the flags every migrate command takes, and those of cmd.
func newMigrateFlags(cmd string, stderr io.Writer) *migrateFlags {
	f := &migrateFlags{fs: flag.NewFlagSet("migrate "+cmd, flag.ContinueOnError)}
	fs := f.fs
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVar(&f.dsn, "dsn", os.Getenv("KEEL_DSN"), "database connection string")
	fs.Var(&f.dirs, "dir", "migration source as [name=]path, repeatable")
	fs.StringVar(&f.schema, "schema", "", "schema of keel_migrations")
	fs.DurationVar(&f.lockTimeout, "lock-timeout", migrate.DefaultLockTimeout, "lock timeout per migration, 0 for none")
	fs.IntVar(&f.retries, "retries", migrate.DefaultRetries, "retries after a lock timeout, 0 for none")
	fs.BoolVar(&f.outOfOrder, "allow-out-of-order", false, "apply migrations older than the newest applied one")
	fs.Var(&f.after, "after", "statement run after a run that applied anything, repeatable")
	fs.BoolVar(&f.json, "json", false, "JSON output")
	switch cmd {
	case "up", "plan":
		fs.StringVar(&f.to, "to", "", "stop after this version")
		if cmd == "up" {
			fs.BoolVar(&f.dryRun, "dry-run", false, "print the plan and its SQL without applying it")
		}
	case "create":
		fs.BoolVar(&f.noTx, "notx", false, "run outside a transaction")
		fs.BoolVar(&f.repeatable, "repeatable", false, "create a repeatable R_<name>.sql migration")
	case "squash":
		fs.StringVar(&f.to, "to", "", "newest version to squash (default the newest)")
		fs.StringVar(&f.admin, "admin", "", "DSN for creating scratch databases (default -dsn)")
		fs.StringVar(&f.dump, "dump", "pg_dump", `pg_dump command; {dsn} and {db} are replaced, as in "docker exec keelpg pg_dump -U keel -d {db}"`)
	case "stamp":
		fs.StringVar(&f.pkg, "package", "schema", "package of the generated file")
		fs.StringVar(&f.out, "out", "schema/version_gen.go", "generated file")
		fs.BoolVar(&f.check, "check", false, "fail when the file is out of date instead of writing it")
	case "check":
		fs.Var(&f.baseDirs, "base-dir", "a source of the base branch as [name=]path, repeatable; checks without a database")
	case "lint":
		fs.BoolVar(&f.all, "all", false, "lint every migration, not only pending ones")
	case "templates":
		fs.StringVar(&f.key, "key", "", "template key of the current template, kept by prune")
		fs.Var(&f.keep, "keep", "template name prune keeps, repeatable")
	}
	return f
}

// parse parses args, allowing flags after positional arguments, and returns the positional ones.
func (f *migrateFlags) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.fs.Parse(args); err != nil {
			return nil, err
		}
		args = f.fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// sources returns the -dir sources, or migrations as the default source.
func sources(dirs listFlag) []migrate.Dir {
	if len(dirs) == 0 {
		return []migrate.Dir{{Name: "default", Path: "migrations"}}
	}
	out := make([]migrate.Dir, len(dirs))
	for i, d := range dirs {
		name, path, ok := strings.Cut(d, "=")
		if !ok {
			name, path = "default", d
		}
		out[i] = migrate.Dir{Name: name, Path: path}
	}
	return out
}

// set reads dirs.
func set(dirs []migrate.Dir) migrate.Set {
	sources := make([]migrate.Source, len(dirs))
	for i, d := range dirs {
		sources[i] = migrate.Source{Name: d.Name, FS: os.DirFS(d.Path)}
	}
	return migrate.Sources(sources...)
}

// options returns the Migrator options.
func (f *migrateFlags) options() migrate.Options {
	o := migrate.Options{Schema: f.schema, LockTimeout: f.lockTimeout, Retries: f.retries,
		AllowOutOfOrder: f.outOfOrder, After: f.after}
	if f.lockTimeout <= 0 {
		o.LockTimeout = -1
	}
	if f.retries <= 0 {
		o.Retries = -1
	}
	return o
}

// config returns the command configuration.
func (f *migrateFlags) config() migratecli.Config {
	return migratecli.Config{DSN: f.dsn, Sources: set(sources(f.dirs)), Options: f.options(), JSON: f.json}
}

// runMigrate runs a migrate command.
func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	cmd := args[0]
	f := newMigrateFlags(cmd, stderr)
	pos, err := f.parse(args[1:])
	if err != nil {
		return err
	}
	arg := func() (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("migrate %s needs one argument: %w", cmd, errUsage)
		}
		return pos[0], nil
	}
	if len(pos) > 0 && cmd != "create" && cmd != "baseline" && cmd != "repair" && cmd != "templates" {
		return fmt.Errorf("migrate %s takes no arguments: %w", cmd, errUsage)
	}
	c := f.config()
	switch cmd {
	case "up":
		return migratecli.Up(ctx, stdout, c, migrate.UpOptions{To: f.to, DryRun: f.dryRun})
	case "plan":
		return migratecli.Plan(ctx, stdout, c, migrate.UpOptions{To: f.to})
	case "status":
		return migratecli.Status(ctx, stdout, c)
	case "history":
		return migratecli.History(ctx, stdout, c)
	case "check":
		return runCheck(ctx, stdout, f, c)
	case "lint":
		return migratecli.Lint(ctx, stdout, c, f.all)
	case "baseline", "repair":
		v, err := arg()
		if err != nil {
			return err
		}
		if cmd == "baseline" {
			return migratecli.Baseline(ctx, stdout, c, v)
		}
		return migratecli.Repair(ctx, stdout, c, v)
	case "create":
		name, err := arg()
		if err != nil {
			return err
		}
		return migratecli.Create(stdout, sources(f.dirs)[0].Path, name,
			migrate.CreateOptions{NoTx: f.noTx, Repeatable: f.repeatable}, f.json)
	case "squash":
		return runSquash(ctx, stdout, f)
	case "stamp":
		return migratecli.Stamp(stdout, c.Sources, migratecli.StampOptions{Package: f.pkg, Out: f.out, Check: f.check}, f.json)
	case "templates":
		return runTemplates(ctx, stdout, f, c, pos)
	}
	return fmt.Errorf("unknown migrate command %q: %w", cmd, errUsage)
}

// runCheck runs migrate check against the database, or against -base-dir sources.
func runCheck(ctx context.Context, stdout io.Writer, f *migrateFlags, c migratecli.Config) error {
	if len(f.baseDirs) == 0 {
		return migratecli.Check(ctx, stdout, c, nil)
	}
	base := set(sources(f.baseDirs))
	c.DSN = ""
	return migratecli.Check(ctx, stdout, c, &base)
}

// runSquash runs migrate squash.
func runSquash(ctx context.Context, stdout io.Writer, f *migrateFlags) error {
	admin := f.admin
	if admin == "" {
		admin = f.dsn
	}
	if admin == "" {
		return errors.New("migrate squash needs -admin, -dsn or KEEL_DSN")
	}
	return migratecli.Squash(ctx, stdout, migrate.SquashOptions{Admin: admin, Dirs: sources(f.dirs), To: f.to,
		Dump: strings.Fields(f.dump), Options: f.options()}, f.json)
}

// runTemplates runs migrate templates and migrate templates prune.
func runTemplates(ctx context.Context, stdout io.Writer, f *migrateFlags, c migratecli.Config, pos []string) error {
	if f.dsn == "" {
		return errors.New("migrate templates needs -dsn or KEEL_DSN")
	}
	switch {
	case len(pos) == 0:
		return migratecli.Templates(ctx, stdout, f.dsn, f.json)
	case len(pos) == 1 && pos[0] == "prune":
		name, err := migrate.TemplateName(c.Sources, f.key)
		if err != nil {
			return err
		}
		return migratecli.PruneTemplates(ctx, stdout, f.dsn, append([]string{name}, f.keep...), f.json)
	}
	return fmt.Errorf("migrate templates takes prune or nothing: %w", errUsage)
}
