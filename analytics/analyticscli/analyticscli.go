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
// go:generate program, and Messages, WriteMessages, CheckMessages and Describe are the plain
// functions behind i18n and describe.
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
	// Tables are the application's generated table schemas (fleetdb.ShipmentsSchema(), …); the
	// reportable ones make the catalog that i18n and describe write.
	Tables []*schema.Table
	// Rollups are the application's declared rollups.
	Rollups []*rollup.Rollup
	// Keys name the schema the way the catalog does; zero fields default to rollup.DefaultKeys.
	Keys rollup.Keys
	// Open opens a database for rollups refresh and rollups pending, which close it when done.
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
			return en.run(ctx, args)
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
			return keelcli.Usagef("analytics: %v", err)
		}
		return nil
	}
	en.json = func() bool { return asJSON }
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

// env is one run of an analytics command: its configuration, output and flag handling, which
// keelcli supplies when it runs the command and Run supplies otherwise.
type env struct {
	cfg   Config
	out   io.Writer
	flags func(name string) *flag.FlagSet
	parse func(fs *flag.FlagSet, args []string) error
	json  func() bool
}

// run dispatches args to a command.
func (e *env) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return keelcli.Usagef("analytics: name a command\n\n%s", usage)
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
	return keelcli.Usagef("analytics: unknown command %q\n\n%s", args[0], usage)
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
		return "", false, keelcli.Usagef("analytics %s: unexpected arguments %s", name, strings.Join(fs.Args(), " "))
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
