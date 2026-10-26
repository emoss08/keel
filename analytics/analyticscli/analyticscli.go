// Package analyticscli adds the analytics commands to a keelcli database binary:
//
//	analytics ts [-check] -out path           the TypeScript module of analytics/ir
//	analytics i18n [-check] -out path         the catalog's labels as a JSON message catalog
//	analytics describe [-out path]            the reportable entities, for a language model
//	analytics rollups migration [-check] -out dir
//	analytics rollups refresh [-rollup a,b] [-batch n] [-max-batches n] [-rebuild]
//	analytics rollups pending
//
// Command returns them as one keelcli.Command; Run runs the same commands without keelcli, for a
// program run by go generate, and WriteMessages, CheckMessages and Describe are the plain functions
// behind i18n and describe. The keel command itself runs ts, which needs no application tables;
// an application registers the rest in its database binary:
//
//	keelcli.Main(keelcli.App{
//	    Name: "fleet-db", Config: dbConfig, Migrations: migrations,
//	    Commands: []keelcli.Command{analyticscli.Command(analyticscli.Config{
//	        Tables: keeltables.All(), Rollups: []*rollup.Rollup{ShipmentsDaily},
//	    })},
//	})
package analyticscli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/keelcli"
	"github.com/emoss08/keel/schema"
)

// Config is what the analytics commands work on.
type Config struct {
	// Tables are every generated table of the application, keeltables.All() from the package
	// keel gen writes with -tables; the reportable ones make the catalog (analytics.Build) that
	// i18n and describe write.
	Tables []*schema.Table
	// Rollups are the application's declared rollups.
	Rollups []*rollup.Rollup
	// Open opens a database for rollups refresh and rollups pending when they run through Run,
	// which closes it when done. Under keelcli they use the App's database instead.
	Open func(ctx context.Context) (*keel.DB, error)
	// Refresh are the defaults of rollups refresh; its flags override the batch sizes.
	Refresh rollup.RefreshOptions
}

// usage lists the analytics commands.
const usage = `Usage: analytics <command> [arguments]

Commands:
  ts [-check] -out path                 write the TypeScript module of analytics/ir, or check it
  i18n [-check] -out path               write the catalog's labels as a JSON message catalog, or check it
  describe [-out path]                  describe the reportable entities for a language model
  rollups migration [-check] -out dir   write migrations for new and changed rollups, or check them
  rollups refresh [-rollup a,b] [-batch n] [-max-batches n] [-rebuild]
                                        recompute the slices writes marked dirty
  rollups pending                       count the dirty slices waiting for each rollup
`

// Command returns the analytics commands as one keelcli command named analytics.
func Command(cfg Config) keelcli.Command {
	return keelcli.Command{
		Name:    "analytics",
		Usage:   "<ts|i18n|describe|rollups> [arguments]",
		Summary: "generate the analytics TypeScript, messages and rollup migrations; refresh rollups",
		Run: func(ctx context.Context, e *keelcli.Env, args []string) error {
			en := &env{cfg: cfg, out: e.Stdout, flags: e.FlagSet, parse: e.Parse, json: e.JSON}
			en.db = func(ctx context.Context) (*keel.DB, func(), error) {
				db, err := e.DB(ctx)
				return db, func() {}, err
			}
			return en.run(ctx, args)
		},
	}
}

// standaloneUsage lists the analytics commands of the keel command itself.
const standaloneUsage = `Usage: keel analytics <command> [arguments]

Commands:
  ts [-check] -out path   write the TypeScript module of analytics/ir, or check it

i18n, describe and rollups need the application's tables and rollups: register
analyticscli.Command(analyticscli.Config{Tables: keeltables.All(), ...}) in the
application's database binary.
`

// Standalone returns the analytics commands that need no application tables, as the keel command
// registers them: ts. The others answer with how to register Command in the application's binary.
func Standalone() keelcli.Command {
	return keelcli.Command{
		Name:    "analytics",
		Usage:   "ts [-check] -out path",
		Summary: "generate the analytics TypeScript module (applications register the rest)",
		Run: func(_ context.Context, e *keelcli.Env, args []string) error {
			en := &env{out: e.Stdout, flags: e.FlagSet, parse: e.Parse, json: e.JSON}
			if len(args) == 0 {
				return keelcli.Usagef("analyticscli: name a command\n\n%s", standaloneUsage)
			}
			switch args[0] {
			case "help", "-h", "-help", "--help":
				_, err := io.WriteString(en.out, standaloneUsage)
				return err
			case "ts":
				return en.ts(args[1:])
			}
			return keelcli.Usagef("analyticscli: %q is not a command of keel analytics\n\n%s", args[0], standaloneUsage)
		},
	}
}

// Run runs one analytics command, args without the leading "analytics", writing its output to
// out. A go:generate program calls it with its own arguments:
//
//	//go:generate go run ./cmd/analyticsgen ts -out ../client/packages/analytics/index.ts
func Run(ctx context.Context, cfg Config, args []string, out io.Writer) error {
	var asJSON bool
	en := &env{cfg: cfg, out: out}
	en.flags = func(name string) *flag.FlagSet {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.BoolVar(&asJSON, "json", false, "write JSON")
		return fs
	}
	en.parse = func(fs *flag.FlagSet, args []string) error {
		if err := fs.Parse(args); err != nil {
			return keelcli.Usagef("analyticscli: %v", err)
		}
		return nil
	}
	en.json = func() bool { return asJSON }
	en.db = func(ctx context.Context) (*keel.DB, func(), error) {
		if cfg.Open == nil {
			return nil, nil, errors.New("analyticscli: rollups: Config.Open is not set")
		}
		db, err := cfg.Open(ctx)
		if err != nil {
			return nil, nil, err
		}
		return db, db.Close, nil
	}
	return en.run(ctx, args)
}

// Main is Run with the process's arguments and stdout, exiting 2 on a usage error and 1 on any
// other, for a go:generate program's main function.
func Main(cfg Config) {
	err := Run(context.Background(), cfg, os.Args[1:], os.Stdout)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, err)
	if errors.Is(err, keelcli.ErrUsage) {
		os.Exit(keelcli.ExitUsage)
	}
	os.Exit(keelcli.ExitFailure)
}

// env is one run of an analytics command: its configuration, output, flag handling and database,
// which keelcli supplies when it runs the command and Run supplies otherwise. db returns the
// database with the function that releases it.
type env struct {
	cfg   Config
	out   io.Writer
	flags func(name string) *flag.FlagSet
	parse func(fs *flag.FlagSet, args []string) error
	json  func() bool
	db    func(ctx context.Context) (*keel.DB, func(), error)
}

// run dispatches args to a command.
func (e *env) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return keelcli.Usagef("analyticscli: name a command\n\n%s", usage)
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		_, err := io.WriteString(e.out, usage)
		return err
	case "ts":
		return e.ts(args[1:])
	case "i18n":
		return e.i18n(args[1:])
	case "describe":
		return e.describe(args[1:])
	case "rollups":
		return e.rollups(ctx, args[1:])
	}
	return keelcli.Usagef("analyticscli: unknown command %q\n\n%s", args[0], usage)
}

// outputFlags parses a command that writes a file: -out and, when check is set, -check.
func (e *env) outputFlags(name string, args []string, check bool) (string, bool, error) {
	fs := e.flags("analytics " + name)
	out := fs.String("out", "", "the file to write")
	var doCheck *bool
	if check {
		doCheck = fs.Bool("check", false, "fail when the file is out of date instead of writing it")
	}
	if err := e.parse(fs, args); err != nil {
		return "", false, err
	}
	if fs.NArg() > 0 {
		return "", false, keelcli.Usagef("analyticscli: %s: unexpected arguments %s", name, strings.Join(fs.Args(), " "))
	}
	return *out, doCheck != nil && *doCheck, nil
}

// wrote reports a written or checked file.
func (e *env) wrote(path string, checked bool) error {
	verb := "wrote"
	if checked {
		verb = "up to date:"
	}
	_, err := fmt.Fprintf(e.out, "%s %s\n", verb, path)
	return err
}

// problems marks a failed check, so keelcli exits with ExitProblems.
func problems(err error) error { return fmt.Errorf("%w: %w", keelcli.ErrProblems, err) }
