// Command keel generates typed data-access packages for keel models and manages migrations.
//
//	keel gen [-out {dir}/{name}db] [-kinds keelkinds] [-tables keeltables] [-check] ./internal/...
//	keel migrate <command> [flags] [args]
//	keel analytics ts [-check] -out path
//
// Every command but gen is keelcli's: in a terminal runs draw live views and keel with no
// arguments opens the console; elsewhere output is plain lines, or JSON with -json. Run keel help
// for the commands and flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/analyticscli"
	"github.com/emoss08/keel/gen"
	"github.com/emoss08/keel/keelcli"
	"github.com/emoss08/keel/migrate"
)

// genUsage is keel gen's arguments, for keel help gen.
const genUsage = "[-out {dir}/{name}db] [-kinds keelkinds] [-tables keeltables] [-tags a,b] [-acronyms PRO,BOL] [-check] packages..."

// main runs the command line until it ends or is interrupted; a second signal kills the process.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run runs the command line and returns the exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return app(&migrateFlags{}).Run(ctx, args, stdin, stdout, stderr)
}

// app is the keel command: keelcli's commands over the migrations the flags name, and gen.
func app(f *migrateFlags) keelcli.App {
	return keelcli.App{
		Name:        "keel",
		Description: "typed data access for Postgres: gen writes the packages, analytics ts the analytics TypeScript module, the other commands manage migrations.",
		Version:     version(),
		Config:      f.config,
		Flags:       f.bind,
		Setup:       f.setup,
		Commands:    []keelcli.Command{genCommand(), analyticscli.Standalone()},
	}
}

// version is the module version keel was built from.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return ""
}

// genCommand is keel gen.
func genCommand() keelcli.Command {
	return keelcli.Command{
		Name:    "gen",
		Usage:   genUsage,
		Summary: "generate typed data-access packages for keel models",
		Run: func(_ context.Context, env *keelcli.Env, args []string) error {
			err := runGen(args, env.Stdout, env.Stderr)
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		},
	}
}

// runGen runs keel gen.
func runGen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "{dir}/{name}db", "generated package directory, relative to the module root")
	kinds := fs.String("kinds", "keelkinds", "shared column kinds package directory, relative to the module root")
	tables := fs.String("tables", "keeltables", "package directory listing every generated table, relative to the module root")
	tags := fs.String("tags", "", "comma-separated build tags")
	check := fs.Bool("check", false, "fail when generated files are out of date instead of writing them")
	acronyms := fs.String("acronyms", "", "comma-separated words labels write in capitals (default ID,URL,API,JSON,UUID)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	patterns := fs.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	o := gen.Options{Patterns: patterns, Out: *out, Kinds: *kinds, Tables: *tables}
	if *tags != "" {
		o.Tags = strings.Split(*tags, ",")
	}
	if *acronyms != "" {
		o.Acronyms = strings.Split(*acronyms, ",")
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

// migrateFlags are keel's global flags, which say where the migrations and the database are.
type migrateFlags struct {
	dsn         string
	dirs        listFlag
	schema      string
	lockTimeout time.Duration
	retries     int
	outOfOrder  bool
	after       listFlag
}

// bind declares the flags.
func (f *migrateFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.dsn, "dsn", os.Getenv("KEEL_DSN"), "database connection `string` (default $KEEL_DSN)")
	fs.Var(&f.dirs, "dir", "migration source as `[name=]path`, repeatable (default migrations)")
	fs.StringVar(&f.schema, "schema", "", "`schema` of keel_migrations (default public)")
	fs.DurationVar(&f.lockTimeout, "lock-timeout", migrate.DefaultLockTimeout, "lock timeout per migration, 0 for none")
	fs.IntVar(&f.retries, "retries", migrate.DefaultRetries, "retries after a lock timeout, 0 for none")
	fs.BoolVar(&f.outOfOrder, "allow-out-of-order", false, "apply migrations older than the newest applied one")
	fs.Var(&f.after, "after", "`statement` run after a run that applied anything, repeatable")
}

// setup points the app at the -dir sources with the migrator options the flags set.
func (f *migrateFlags) setup(a *keelcli.App) error {
	a.Dirs = f.sources()
	a.Migrate = f.options()
	return nil
}

// sources returns the -dir sources, or migrations as the default source.
func (f *migrateFlags) sources() []migrate.Dir {
	if len(f.dirs) == 0 {
		return []migrate.Dir{{Name: "default", Path: "migrations"}}
	}
	out := make([]migrate.Dir, len(f.dirs))
	for i, d := range f.dirs {
		out[i] = keelcli.ParseDir(d)
	}
	return out
}

// options returns the migrator options.
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

// config returns the database configuration, or an error wrapping keelcli.ErrNoDatabase without
// -dsn and KEEL_DSN.
func (f *migrateFlags) config() (keel.Config, error) {
	if f.dsn == "" {
		return keel.Config{}, fmt.Errorf("%w: pass -dsn or set KEEL_DSN", keelcli.ErrNoDatabase)
	}
	return keel.Config{DSN: f.dsn, SkipEnums: true}, nil
}
