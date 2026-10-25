package keelcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/emoss08/keel/keelcli/internal/render"
)

type runner func(ctx context.Context, s *session, args []string) error

type builtin struct {
	name    string
	args    string
	summary string
	setup   func(fs *flag.FlagSet) runner
}

func (b builtin) group() string {
	g, _, _ := strings.Cut(b.name, " ")
	return g
}

func builtins(s *session) []builtin {
	up := "apply pending migrations: the deploy step"
	if s.hasSeeds() {
		up = "migrate, then apply seeds: the deploy step"
	}
	out := []builtin{
		{"up", "[-to version] [-dry-run] [-env name]", up, setupUp},
		{"migrate up", "[-to version] [-dry-run]", "apply pending migrations", setupMigrateUp},
		{"migrate plan", "[-to version]", "show what migrate up would do", setupMigratePlan},
		{"migrate status", "", "list migrations with their state", setupMigrateStatus},
		{"migrate history", "", "list applied migrations", setupMigrateHistory},
		{"migrate create", "[-kind tx|notx|repeatable] [-source name] [name]", "write a new migration file", setupMigrateCreate},
		{"migrate squash", "[-to version] [-dump command] [-admin dsn]", "replace applied migrations with one baseline", setupMigrateSquash},
		{"migrate stamp", "[-check] [-out path]", "write the schema version file, or check it", setupMigrateStamp},
		{"migrate check", "[-base-dir [name=]path]...", "check ordering and checksums", setupMigrateCheck},
		{"migrate lint", "[-all]", "lint pending migrations", setupMigrateLint},
		{"migrate baseline", "<version>", "record migrations up to version as applied", setupMigrateBaseline},
		{"migrate repair", "<version>", "accept the current checksum of a changed migration", setupMigrateRepair},
	}
	if s.hasSeeds() {
		out = append(out,
			builtin{"seed apply", "[-force] [-dry-run] [names...]", "apply seeds that changed, or the named ones", setupSeedApply},
			builtin{"seed plan", "[-force] [names...]", "show which seeds would run and why", setupSeedPlan},
			builtin{"seed status", "", "list seeds with their state", setupSeedStatus},
			builtin{"seed prune", "", "delete records of seeds no longer registered", setupSeedPrune},
		)
	}
	out = append(out,
		builtin{"reset", "[-admin dsn]", "recreate the database from the current template", setupReset},
		builtin{"template", "[-admin dsn]", "list template databases", setupTemplate},
		builtin{"template build", "[-admin dsn]", "build the current template", setupTemplateBuild},
		builtin{"template prune", "[-admin dsn]", "drop stale templates", setupTemplatePrune},
	)
	if s.hasFacts() {
		out = append(out, builtin{"doctor", "", "check the database, its settings and what holds locks", setupDoctor})
	}
	if s.app.Tables != nil {
		out = append(out, builtin{"verify", "", "compare the models with the database's tables", setupVerify})
	}
	if s.app.Config != nil {
		out = append(out, builtin{"plans", "-shapes file [-seq-scan-rows n] [-max-cost c] [-sort-limit]",
			"explain recorded statement shapes and flag scans, costs and sorted pages", setupPlans})
	}
	return append(out, builtin{"version", "", "print the version", setupVersion})
}

func (s *session) route(ctx context.Context, args []string) error {
	if err := s.checkCommands(); err != nil {
		return err
	}
	root := s.flagSet(s.app.Name)
	if err := s.parseHead(root, args); err != nil {
		if errors.Is(err, errHelp) {
			s.help(s.st.out)
			return nil
		}
		return err
	}
	rest := root.Args()
	if len(rest) == 0 {
		if s.interactive() {
			if err := s.setup(); err != nil {
				return err
			}
			return s.console(ctx)
		}
		s.help(s.st.err)
		return Usagef("keelcli: no command: name one, or run %s in a terminal for the console", s.app.Name)
	}
	if rest[0] == "help" {
		return s.helpFor(rest[1:])
	}
	for _, c := range s.app.Commands {
		if c.Name == rest[0] {
			if err := s.setup(); err != nil {
				return err
			}
			return c.Run(ctx, &Env{App: s.app, Stdin: s.st.in, Stdout: s.st.out, Stderr: s.st.err, s: s}, rest[1:])
		}
	}
	b, sub, err := s.lookup(rest)
	if err != nil {
		return err
	}
	fs := s.flagSet(s.app.Name + " " + b.name)
	run := b.setup(fs)
	if err := s.parse(fs, sub); err != nil {
		if errors.Is(err, errHelp) {
			s.commandHelp(s.st.out, b, fs)
			return nil
		}
		return err
	}
	if err := s.setup(); err != nil {
		return err
	}
	return run(ctx, s, fs.Args())
}

func (s *session) lookup(args []string) (builtin, []string, error) {
	all := builtins(s)
	if len(args) > 1 {
		for _, b := range all {
			if b.name == args[0]+" "+args[1] {
				return b, args[2:], nil
			}
		}
	}
	for _, b := range all {
		if b.name == args[0] {
			return b, args[1:], nil
		}
	}
	if slices.ContainsFunc(all, func(b builtin) bool { return b.group() == args[0] }) {
		if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			return builtin{}, nil, Usagef("keelcli: unknown command %q; run %s help %s", args[0]+" "+args[1], s.app.Name, args[0])
		}
		s.groupHelp(s.st.err, args[0])
		return builtin{}, nil, Usagef("keelcli: %s needs a subcommand", args[0])
	}
	return builtin{}, nil, Usagef("keelcli: unknown command %q; run %s help", args[0], s.app.Name)
}

func (s *session) help(w io.Writer) {
	name := s.app.Name
	if s.app.Description != "" {
		fmt.Fprintf(w, "%s: %s\n\n", name, s.app.Description)
	}
	fmt.Fprintf(w, "Usage: %s [flags] <command> [arguments]\n\n", name)
	fmt.Fprintf(w, "Run %s with no command in a terminal to open the console.\n\nCommands:\n", name)
	t := render.Table{NoHeader: true, Indent: 2, Columns: []render.Column{{Title: "COMMAND"}, {Title: "SUMMARY"}}}
	for _, b := range builtins(s) {
		t.Rows = append(t.Rows, []render.Cell{render.C(b.name, render.Normal), render.C(b.summary, render.Normal)})
	}
	t.Rows = append(t.Rows, []render.Cell{render.C("help [command]", render.Normal), render.C("show help for a command", render.Normal)})
	io.WriteString(w, t.String(render.Plain))
	if len(s.app.Commands) > 0 {
		fmt.Fprintf(w, "\n%s commands:\n", name)
		t.Rows = nil
		for _, c := range s.app.Commands {
			t.Rows = append(t.Rows, []render.Cell{render.C(c.Name, render.Normal), render.C(c.Summary, render.Normal)})
		}
		io.WriteString(w, t.String(render.Plain))
	}
	fmt.Fprintf(w, "\nGlobal flags, before or after the command:\n")
	s.flagDefaults(w, s.flagSet(name), true)
	fmt.Fprintf(w, "\nExit codes: 0 ok, 1 failure, 2 usage or refusal, 3 a check found problems.\n")
}

func (s *session) helpFor(args []string) error {
	if len(args) == 0 {
		s.help(s.st.out)
		return nil
	}
	for _, c := range s.app.Commands {
		if c.Name == args[0] {
			fmt.Fprintf(s.st.out, "Usage: %s %s %s\n\n%s\n", s.app.Name, c.Name, c.Usage, c.Summary)
			return nil
		}
	}
	b, _, err := s.lookup(args)
	if err != nil {
		return err
	}
	fs := s.flagSet(s.app.Name + " " + b.name)
	b.setup(fs)
	s.commandHelp(s.st.out, b, fs)
	return nil
}

func (s *session) groupHelp(w io.Writer, group string) {
	fmt.Fprintf(w, "Usage: %s %s <command>\n\nCommands:\n", s.app.Name, group)
	t := render.Table{NoHeader: true, Indent: 2, Columns: []render.Column{{Title: "COMMAND"}, {Title: "SUMMARY"}}}
	for _, b := range builtins(s) {
		if b.group() == group {
			t.Rows = append(t.Rows, []render.Cell{render.C(b.name+" "+b.args, render.Normal), render.C(b.summary, render.Normal)})
		}
	}
	io.WriteString(w, t.String(render.Plain))
}

func (s *session) commandHelp(w io.Writer, b builtin, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage: %s %s %s\n\n%s.\n", s.app.Name, b.name, b.args, strings.ToUpper(b.summary[:1])+b.summary[1:])
	var buf strings.Builder
	s.flagDefaults(&buf, fs, false)
	if buf.Len() > 0 {
		fmt.Fprintf(w, "\nFlags:\n%s", buf.String())
	}
	fmt.Fprintf(w, "\nGlobal flags: %s\n", strings.Join(s.globalNames(), " "))
}

// globalNames lists the global flags, keelcli's and the application's.
func (s *session) globalNames() []string {
	names := []string{"-json", "-plain", "-no-color", "-yes", "-yes-production"}
	s.appFlags.VisitAll(func(f *flag.Flag) { names = append(names, "-"+f.Name) })
	return names
}

func (s *session) flagDefaults(w io.Writer, fs *flag.FlagSet, globalsOnly bool) {
	global := map[string]bool{"json": true, "plain": true, "no-color": true, "yes": true, "yes-production": true}
	s.appFlags.VisitAll(func(f *flag.Flag) { global[f.Name] = true })
	t := render.Table{NoHeader: true, Indent: 2, Columns: []render.Column{{Title: "FLAG"}, {Title: "USAGE"}}}
	fs.VisitAll(func(f *flag.Flag) {
		if global[f.Name] != globalsOnly {
			return
		}
		name, usage := flag.UnquoteUsage(f)
		flagText := "-" + f.Name
		if name != "" {
			flagText += " " + name
		}
		t.Rows = append(t.Rows, []render.Cell{render.C(flagText, render.Normal), render.C(usage, render.Normal)})
	})
	io.WriteString(w, t.String(render.Plain))
}
