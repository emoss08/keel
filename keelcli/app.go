// Package keelcli turns an application's migrations and seeds into the command line its database
// binaries are made of.
//
// Every command works two ways: plain, scriptable output when stdout is not a terminal or -plain or
// -json is given (CI, Kubernetes Jobs, Compose), and an interactive interface when it is: live
// views for runs, prompts for destructive commands, and, with no command, a full-screen console.
// An application describes its migrations, seeds and database in an App and calls Main; keelcli
// builds adapters over keel/migrate and keel/seed and reaches the database only through the ports
// in migrateport, seedport and factsport, so tests and the demo run the same commands against
// package fake.
package keelcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/internal/adapt"
	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/term"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/seed"
)

// The exit codes.
const (
	ExitOK       = 0
	ExitFailure  = 1
	ExitUsage    = 2
	ExitProblems = 3
)

// ErrUsage marks an error in how a command was called; keelcli exits with ExitUsage.
var ErrUsage = errors.New("keelcli: usage")

// ErrProblems marks a check that found problems; keelcli exits with ExitProblems.
var ErrProblems = errors.New("keelcli: problems found")

type usageError string

// Error returns the message.
func (e usageError) Error() string { return string(e) }

// Is matches ErrUsage.
func (e usageError) Is(target error) bool { return target == ErrUsage }

// Usagef reports a usage error, for application commands as for keelcli's own.
func Usagef(format string, args ...any) error {
	return usageError(fmt.Sprintf(format, args...))
}

// ErrNoDatabase is the error of a command that needs a database when none is configured. An
// App's Config returns an error wrapping it when the user gave no connection, so commands that can
// work without one (migrate lint lints every migration then) do.
var ErrNoDatabase = adapt.ErrNoDatabase

// App is an application's database binary.
//
// An application describes its database with the fields below and keelcli builds the engines
// from them, opening the database the first time a command needs it. A field left unset leaves
// out what needs it: without Seeds there are no seed commands, and without Config no doctor.
// Engines replaces any of the engines, as the tests and the demo do with package fake.
type App struct {
	// Name names the binary in help and is the application_name of its connections, so other
	// sessions see who holds a lock.
	Name        string
	Description string
	Version     string

	// Config returns the database configuration. It is called the first time a command needs the
	// database, so help, migrate create and migrate stamp work without one.
	Config func() (keel.Config, error)
	// Migrations are the application's migrations, usually embedded files read with
	// migrate.Sources. When it is the zero Set they are read from Dirs, again on every call.
	Migrations migrate.Set
	// Migrate configures the migrator: the bookkeeping schema, lock timeout, retries, out-of-order
	// migrations and the After statements.
	Migrate migrate.Options
	// Dirs are the migration sources' directories on disk, named as the sources: migrate create
	// writes into the first, or the one -source names, and migrate squash rewrites them.
	Dirs []migrate.Dir
	// Stamp is the path of the schema version file migrate stamp writes; default
	// schema/version_gen.go. Its package is the one the directory's Go files declare, or the
	// directory's name.
	Stamp string
	// Dump is the pg_dump command migrate squash runs, default pg_dump. "{dsn}" and "{db}" name the
	// scratch database, so a server in a container dumps with its own pg_dump:
	// docker exec <container> pg_dump -U <user> -d {db}.
	Dump []string
	// Lint are the application's lint rules, run after the built-in ones.
	Lint []migrate.LintRule

	// Seeds are the application's seeds; nil for none.
	Seeds *seed.Registry
	// Tenants lists the tenants per-tenant seeds run for.
	Tenants func(ctx context.Context, db *keel.DB) iter.Seq2[seed.Tenant, error]
	// System returns ctx with the application's system scope, under which seeds and their
	// bookkeeping run.
	System func(ctx context.Context) context.Context
	// Parallel bounds the seed transactions in flight; default 1.
	Parallel int

	// Extensions are the extensions doctor requires.
	Extensions []string
	// Commands adds the application's own commands, which must not reuse a built-in name.
	Commands []Command
	// Flags declares the application's global flags, accepted before or after any command.
	Flags func(fs *flag.FlagSet)
	// Setup runs once the command line is parsed, before the command, to finish the App from the
	// values Flags bound. It may set any field but Name, Config, Seeds, Commands and Flags, which
	// shape the command line itself; Config reads flag values when it is called, after Setup.
	Setup func(app *App) error

	// Engines replaces the engines keelcli builds from the fields above.
	Engines Engines
}

// Engines are the ports keelcli runs on. A nil engine is built from the App's other fields.
type Engines struct {
	Migrations migrateport.Engine
	Seeds      seedport.Engine
	Facts      factsport.Facts
}

// Command is an application command. Run parses its own arguments, typically with Env.FlagSet and
// Env.Parse so the global flags work after the command name too.
type Command struct {
	Name    string
	Usage   string
	Summary string
	Run     func(ctx context.Context, env *Env, args []string) error
}

// Env is what an application command gets: the streams, the output mode and the destructive-action
// guard keelcli's own commands use.
type Env struct {
	App    *App
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	s      *session
}

// JSON reports whether -json was given.
func (e *Env) JSON() bool { return e.s.g.json }

// Interactive reports whether the command may prompt and draw live views.
func (e *Env) Interactive() bool { return e.s.interactive() }

// FlagSet returns a flag set for the command with the global flags bound.
func (e *Env) FlagSet(name string) *flag.FlagSet { return e.s.flagSet(name) }

// Parse parses args, mapping -h to help and bad flags to a usage error.
func (e *Env) Parse(fs *flag.FlagSet, args []string) error { return e.s.parse(fs, args) }

// DB returns the application's database, opened on first use and closed when the command
// returns.
func (e *Env) DB(ctx context.Context) (*keel.DB, error) { return e.s.database().DB(ctx) }

// Confirm asks before a destructive action: it names the database and its environment mark,
// refuses a production database without -yes-production, skips the prompt with -yes, and is a
// usage error when there is no terminal to ask on.
func (e *Env) Confirm(ctx context.Context, verb, title string, detail ...string) error {
	return e.s.confirm(ctx, guard.Action{Verb: verb, Title: title, Detail: detail})
}

// Main runs the app with the process's arguments and streams and exits. SIGINT and SIGTERM cancel
// the running command, which stops cleanly; a second signal kills the process.
func Main(app App) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// Run runs one command and returns its exit code.
func (a App) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return a.run(ctx, args, streams{
		in: stdin, out: stdout, err: stderr,
		inTTY: term.IsTerminal(stdin), outTTY: term.IsTerminal(stdout),
		width: term.Width(stdout), environ: os.Environ(), now: time.Now,
	})
}

type streams struct {
	in      io.Reader
	out     io.Writer
	err     io.Writer
	inTTY   bool
	outTTY  bool
	width   int
	environ []string
	now     func() time.Time
}

func (a App) run(ctx context.Context, args []string, st streams) int {
	s := newSession(&a, st)
	err := s.route(ctx, args)
	s.close()
	var r reported
	if err != nil && !errors.As(err, &r) {
		fmt.Fprintln(st.err, message(err))
	}
	return exitCode(err)
}

func message(err error) string {
	if msg := err.Error(); strings.HasPrefix(msg, "keelcli:") {
		return msg
	}
	return "keelcli: " + err.Error()
}

type reported struct{ err error }

// Error returns the wrapped message.
func (r reported) Error() string { return r.err.Error() }

// Unwrap returns the error the view already showed.
func (r reported) Unwrap() error { return r.err }

func exitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrUsage), errors.Is(err, guard.ErrRefused):
		return ExitUsage
	case errors.Is(err, ErrProblems):
		return ExitProblems
	default:
		return ExitFailure
	}
}
