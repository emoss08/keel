package keelcli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/fake"
	"github.com/emoss08/keel/keelcli/seedport"
)

var update = flag.Bool("update", false, "rewrite golden files")

var fixedNow = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

type result struct {
	code   int
	stdout string
	stderr string
}

type harness struct {
	app    App
	engine *fake.Engine
}

func newHarness(opts fake.Options) harness {
	opts.Now = func() time.Time { return fixedNow }
	e := fake.New(opts)
	return harness{engine: e, app: App{
		Name:        "trenova-db",
		Description: "Trenova's database: migrations and reference seeds.",
		Version:     "1.4.0",
		Migrations:  e.Migrations(),
		Seeds:       e.Seeds(),
		Facts:       e.Facts(),
		Extensions:  []string{"pg_trgm", "pgcrypto"},
	}}
}

func (h harness) run(t *testing.T, args ...string) result {
	t.Helper()
	return h.exec(t, false, "", args...)
}

func (h harness) exec(t *testing.T, tty bool, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := h.app.run(ctx, args, streams{
		in: &typist{chunks: strings.Split(stdin, "\x00")}, out: &out, err: &errb, inTTY: tty, outTTY: tty, width: 100,
		environ: []string{"TERM=xterm-256color"}, now: func() time.Time { return fixedNow },
	})
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

type typist struct {
	chunks []string
}

func (t *typist) Read(p []byte) (int, error) {
	if len(t.chunks) == 0 {
		return 0, io.EOF
	}
	time.Sleep(400 * time.Millisecond)
	n := copy(p, t.chunks[0])
	t.chunks[0] = t.chunks[0][n:]
	if t.chunks[0] == "" {
		t.chunks = t.chunks[1:]
	}
	return n, nil
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from %s:\n--- got ---\n%s\n--- want ---\n%s", name, path, got, want)
	}
}

func TestGoldenPlain(t *testing.T) {
	cases := []struct {
		name string
		opts fake.Options
		args []string
		code int
	}{
		{"help", fake.Options{}, []string{"help"}, ExitOK},
		{"help_migrate_up", fake.Options{}, []string{"help", "migrate", "up"}, ExitOK},
		{"migrate_status", fake.Options{}, []string{"migrate", "status"}, ExitOK},
		{"migrate_status_drift", fake.Options{Drift: true}, []string{"migrate", "status"}, ExitOK},
		{"migrate_history", fake.Options{}, []string{"migrate", "history"}, ExitOK},
		{"migrate_plan", fake.Options{}, []string{"migrate", "plan"}, ExitOK},
		{"migrate_up_dry_run", fake.Options{}, []string{"migrate", "up", "-dry-run", "-to", "20261025110000"}, ExitOK},
		{"migrate_lint", fake.Options{}, []string{"migrate", "lint"}, ExitProblems},
		{"migrate_check_drift", fake.Options{Drift: true}, []string{"migrate", "check"}, ExitProblems},
		{"migrate_stamp_check", fake.Options{}, []string{"migrate", "stamp", "-check"}, ExitOK},
		{"up", fake.Options{}, []string{"up"}, ExitOK},
		{"up_fail_migration", fake.Options{FailMigration: true}, []string{"up"}, ExitFailure},
		{"seed_apply_fail", fake.Options{FailSeed: true, NoLockWait: true}, []string{"seed", "apply"}, ExitFailure},
		{"seed_status", fake.Options{}, []string{"seed", "status"}, ExitOK},
		{"seed_plan", fake.Options{}, []string{"seed", "plan", "organization_role_permissions"}, ExitOK},
		{"seed_plan_production", fake.Options{Environment: seedport.Production}, []string{"seed", "plan"}, ExitOK},
		{"template", fake.Options{}, []string{"template"}, ExitOK},
		{"doctor", fake.Options{}, []string{"doctor"}, ExitOK},
		{"version", fake.Options{}, []string{"version"}, ExitOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHarness(c.opts).run(t, c.args...)
			if r.code != c.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, c.code, r.stdout, r.stderr)
			}
			golden(t, c.name, r.stdout+stderrSection(r.stderr))
		})
	}
}

func stderrSection(s string) string {
	if s == "" {
		return ""
	}
	return "--- stderr ---\n" + s
}

func TestGoldenJSON(t *testing.T) {
	cases := []struct {
		name string
		opts fake.Options
		args []string
		code int
	}{
		{"json_migrate_status", fake.Options{Drift: true}, []string{"migrate", "status", "-json"}, ExitOK},
		{"json_migrate_plan", fake.Options{}, []string{"-json", "migrate", "plan"}, ExitOK},
		{"json_up", fake.Options{Tenants: 20}, []string{"up", "-json"}, ExitOK},
		{"json_seed_status", fake.Options{}, []string{"seed", "status", "-json"}, ExitOK},
		{"json_lint", fake.Options{}, []string{"migrate", "lint", "-json"}, ExitProblems},
		{"json_doctor", fake.Options{}, []string{"doctor", "-json"}, ExitOK},
		{"json_templates", fake.Options{}, []string{"template", "-json"}, ExitOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHarness(c.opts).run(t, c.args...)
			if r.code != c.code {
				t.Fatalf("exit %d, want %d\nstderr:\n%s", r.code, c.code, r.stderr)
			}
			for i, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("line %d is not JSON: %s", i+1, line)
				}
			}
			golden(t, c.name, r.stdout)
		})
	}
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		opts   fake.Options
		args   []string
		code   int
		stderr string
	}{
		{"no command without a terminal", fake.Options{}, nil, ExitUsage, "no command"},
		{"unknown command", fake.Options{}, []string{"frobnicate"}, ExitUsage, `unknown command "frobnicate"`},
		{"unknown subcommand", fake.Options{}, []string{"migrate", "sideways"}, ExitUsage, `unknown command "migrate sideways"`},
		{"group without subcommand", fake.Options{}, []string{"seed"}, ExitUsage, "seed needs a subcommand"},
		{"bad flag", fake.Options{}, []string{"migrate", "up", "-sideways"}, ExitUsage, "flag provided but not defined"},
		{"stray argument", fake.Options{}, []string{"migrate", "status", "now"}, ExitUsage, "takes no arguments"},
		{"baseline needs a version", fake.Options{}, []string{"migrate", "baseline"}, ExitUsage, "needs one version"},
		{"bad create kind", fake.Options{}, []string{"migrate", "create", "-kind", "sometimes", "x"}, ExitUsage, `-kind "sometimes"`},
		{"bad migration name", fake.Options{}, []string{"migrate", "create", "Add Holds"}, ExitUsage, "migration name"},
		{"changed migration blocks up", fake.Options{Drift: true}, []string{"migrate", "up"}, ExitFailure, "applied file changed"},
		{"doctor passes with warnings", fake.Options{}, []string{"doctor"}, ExitOK, ""},
		{"lint errors are problems", fake.Options{}, []string{"migrate", "lint"}, ExitProblems, "found 2 errors"},
		{"help flag", fake.Options{}, []string{"-h"}, ExitOK, ""},
		{"command help flag", fake.Options{}, []string{"seed", "apply", "-h"}, ExitOK, ""},
		{"env conflict", fake.Options{}, []string{"up", "-env", "production"}, ExitFailure, "marked development, not production"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHarness(c.opts).run(t, c.args...)
			if r.code != c.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, c.code, r.stdout, r.stderr)
			}
			if !strings.Contains(r.stderr, c.stderr) {
				t.Fatalf("stderr %q does not contain %q", r.stderr, c.stderr)
			}
		})
	}
}

func TestDoctorFailsOnMissingExtension(t *testing.T) {
	h := newHarness(fake.Options{})
	h.app.Extensions = []string{"postgis"}
	r := h.run(t, "doctor")
	if r.code != ExitProblems || !strings.Contains(r.stdout, "missing postgis") {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
}

func TestPromptRefusedWithoutTerminal(t *testing.T) {
	for _, args := range [][]string{
		{"reset"},
		{"seed", "prune"},
		{"template", "prune"},
		{"migrate", "squash"},
		{"migrate", "baseline", "20261025110000"},
		{"migrate", "repair", "20261024150000"},
		{"migrate", "create"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := newHarness(fake.Options{Drift: true}).run(t, args...)
			if r.code != ExitUsage {
				t.Fatalf("exit %d, want %d\nstderr: %s", r.code, ExitUsage, r.stderr)
			}
			if !strings.Contains(r.stderr, "pass -yes") && !strings.Contains(r.stderr, "needs a name") {
				t.Fatalf("stderr: %s", r.stderr)
			}
		})
	}
}

func TestYesSkipsPrompt(t *testing.T) {
	h := newHarness(fake.Options{Drift: true})
	if r := h.run(t, "migrate", "repair", "-yes", "20261024150000"); r.code != ExitOK {
		t.Fatalf("repair: exit %d: %s", r.code, r.stderr)
	}
	if r := h.run(t, "template", "prune", "-yes"); r.code != ExitOK || !strings.Contains(r.stdout, "Dropped 2 templates") {
		t.Fatalf("template prune: exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	r := h.run(t, "-yes", "seed", "prune")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Pruned 1 seed record") {
		t.Fatalf("seed prune: exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if r := h.run(t, "seed", "prune"); r.code != ExitOK || !strings.Contains(r.stdout, "Nothing to prune") {
		t.Fatalf("second prune: exit %d: %s", r.code, r.stdout)
	}
}

func TestProductionRefusal(t *testing.T) {
	prod := fake.Options{Environment: seedport.Production, Drift: true}
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"template", "prune", "-yes"}, ExitUsage, "marked production; pass -yes-production"},
		{[]string{"migrate", "repair", "-yes", "20261024150000"}, ExitUsage, "marked production; pass -yes-production"},
		{[]string{"template", "prune", "-yes", "-yes-production"}, ExitOK, ""},
		{[]string{"reset", "-yes", "-yes-production"}, ExitUsage, "where it never runs"},
		{[]string{"-yes-production", "seed", "prune"}, ExitUsage, "pass -yes"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			r := newHarness(prod).run(t, c.args...)
			if r.code != c.code || !strings.Contains(r.stderr, c.want) {
				t.Fatalf("exit %d (want %d), stderr %q, want %q", r.code, c.code, r.stderr, c.want)
			}
		})
	}
}

func TestEnvironmentMark(t *testing.T) {
	h := newHarness(fake.Options{})
	if err := h.engine.Seeds().MarkEnvironment(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	r := h.run(t, "up", "-env", "staging", "-dry-run")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Marked the database as staging.") {
		t.Fatalf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if r := h.run(t, "up", "-env", "staging", "-dry-run"); r.code != ExitOK || strings.Contains(r.stdout, "Marked") {
		t.Fatalf("second mark: exit %d\n%s", r.code, r.stdout)
	}
}

func TestApplicationCommand(t *testing.T) {
	h := newHarness(fake.Options{})
	var got []string
	h.app.Commands = []Command{{
		Name: "volume", Usage: "[-orgs n]", Summary: "seed volume data",
		Run: func(ctx context.Context, env *Env, args []string) error {
			fs := env.FlagSet("volume")
			orgs := fs.Int("orgs", 1, "organizations")
			if err := env.Parse(fs, args); err != nil {
				return err
			}
			got = append(got, fs.Args()...)
			if *orgs < 1 {
				return Usagef("keelcli: -orgs must be positive")
			}
			return env.Confirm(ctx, "seed volume data", "Seed volume data?")
		},
	}}
	if r := h.run(t, "volume", "-yes", "-orgs", "3", "extra"); r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if len(got) != 1 || got[0] != "extra" {
		t.Fatalf("args %v", got)
	}
	if r := h.run(t, "volume", "-orgs", "0"); r.code != ExitUsage {
		t.Fatalf("exit %d", r.code)
	}
	if r := h.run(t, "help"); !strings.Contains(r.stdout, "volume") {
		t.Fatalf("help lacks the command:\n%s", r.stdout)
	}
	h.app.Commands = append(h.app.Commands, Command{Name: "doctor"})
	if r := h.run(t, "version"); r.code != ExitFailure || !strings.Contains(r.stderr, "reuses a built-in name") {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
}

func TestWithoutSeedsOrFacts(t *testing.T) {
	h := newHarness(fake.Options{})
	h.app.Seeds, h.app.Facts = nil, nil
	if r := h.run(t, "seed", "status"); r.code != ExitUsage {
		t.Fatalf("seed status without seeds: exit %d", r.code)
	}
	if r := h.run(t, "doctor"); r.code != ExitUsage {
		t.Fatalf("doctor without facts: exit %d", r.code)
	}
	r := h.run(t, "up")
	if r.code != ExitOK || strings.Contains(r.stdout, "seed") {
		t.Fatalf("up without seeds: exit %d\n%s", r.code, r.stdout)
	}
	if r := h.run(t, "up", "-env", "production"); r.code != ExitUsage {
		t.Fatalf("-env without seeds: exit %d", r.code)
	}
	h.app.Migrations = nil
	if r := h.run(t, "version"); r.code != ExitFailure {
		t.Fatalf("no migrations: exit %d", r.code)
	}
}

func TestPlainCancellationListsWhatIsLeft(t *testing.T) {
	h := newHarness(fake.Options{Speed: 1, NoLockWait: true})
	var out, errb bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(600 * time.Millisecond)
		cancel()
	}()
	code := h.app.run(ctx, []string{"up"}, streams{
		in: strings.NewReader(""), out: &out, err: &errb, now: func() time.Time { return fixedNow },
	})
	if code != ExitFailure {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "left undone: 20261025110000_edi_partner_index, 20261025120000_rate_tables") || !strings.Contains(out.String(), "seeds not started") {
		t.Fatalf("output:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "keelcli: cancelled") {
		t.Fatalf("stderr: %s", errb.String())
	}
}

func TestLiveViewsRunToCompletion(t *testing.T) {
	cases := []struct {
		name string
		opts fake.Options
		args []string
		code int
		want []string
	}{
		{"up", fake.Options{Tenants: 300}, []string{"up"}, ExitOK, []string{"Summary", "4 migrations applied", "7 seeds applied"}},
		{"migrate up", fake.Options{}, []string{"migrate", "up", "-to", "20261025120000"}, ExitOK, []string{"2 migrations applied"}},
		{"seed apply", fake.Options{Tenants: 300}, []string{"seed", "apply", "organization_role_permissions"}, ExitOK, []string{"2 seeds applied"}},
		{"seed failure", fake.Options{Tenants: 300, FailSeed: true}, []string{"seed", "apply"}, ExitFailure, []string{"failed at tenant org_00142", "Left undone", "document_template_starters"}},
		{"migration failure", fake.Options{FailMigration: true}, []string{"up"}, ExitFailure, []string{"failed after 2 migrations", "seeds: not started", "violates not-null constraint"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHarness(c.opts).exec(t, true, "", c.args...)
			out := ansi.Strip(r.stdout)
			if r.code != c.code {
				t.Fatalf("exit %d, want %d\n%s\nstderr: %s", r.code, c.code, out, r.stderr)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Fatalf("output lacks %q:\n%s", w, out)
				}
			}
			if r.stderr != "" {
				t.Fatalf("live view repeated its error on stderr: %s", r.stderr)
			}
		})
	}
}

func TestPromptsInATerminal(t *testing.T) {
	h := newHarness(fake.Options{})
	if r := h.exec(t, true, "n", "template", "prune"); r.code != ExitFailure || !strings.Contains(r.stderr, "not confirmed: prune templates") {
		t.Fatalf("declined: exit %d: %s", r.code, r.stderr)
	}
	r := h.exec(t, true, "y", "template", "prune")
	if r.code != ExitOK || !strings.Contains(ansi.Strip(r.stdout), "Dropped 2 templates") {
		t.Fatalf("confirmed: exit %d: %s%s", r.code, ansi.Strip(r.stdout), r.stderr)
	}
	if !strings.Contains(ansi.Strip(r.stdout), "Environment: development") {
		t.Fatalf("confirmation did not name the environment:\n%s", ansi.Strip(r.stdout))
	}
	r = h.exec(t, true, "add_holds\t\x00j\x00\r", "migrate", "create")
	if r.code != ExitOK || !strings.Contains(ansi.Strip(r.stdout), "Created schema/migrations/20261027093000_add_holds.sql") {
		t.Fatalf("create: exit %d: %s%s", r.code, ansi.Strip(r.stdout), r.stderr)
	}
	rows, _ := h.engine.Migrations().Status(context.Background())
	if last := rows[len(rows)-1]; last.Migration.Name != "add_holds" || !last.Migration.Directives.NoTx {
		t.Fatalf("created %+v", last.Migration)
	}
	r = h.exec(t, true, "y", "reset")
	if out := ansi.Strip(r.stdout); r.code != ExitOK || !strings.Contains(out, "Recreate trenova on localhost:5432") ||
		!strings.Contains(out, "Reset trenova from keel_tpl_9f2c41d0 in 180ms.") {
		t.Fatalf("reset: exit %d: %s%s", r.code, out, r.stderr)
	}
	r = h.exec(t, true, "", "template", "build")
	if out := ansi.Strip(r.stdout); r.code != ExitOK || !strings.Contains(out, "Building the current template") ||
		!strings.Contains(out, "Template ready: keel_tpl_9f2c41d0 (48 MB).") {
		t.Fatalf("template build: exit %d: %s%s", r.code, out, r.stderr)
	}
}

func TestConsoleQuits(t *testing.T) {
	r := newHarness(fake.Options{}).exec(t, true, "q", nil...)
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
}

func TestPlainFlagInTerminal(t *testing.T) {
	r := newHarness(fake.Options{}).exec(t, true, "", "migrate", "status", "-plain")
	if r.code != ExitOK || strings.Contains(r.stdout, "\x1b[") || !strings.HasPrefix(r.stdout, "VERSION") {
		t.Fatalf("exit %d\n%q", r.code, r.stdout)
	}
}

func TestNoColor(t *testing.T) {
	h := newHarness(fake.Options{})
	var out bytes.Buffer
	code := h.app.run(context.Background(), []string{"migrate", "status"}, streams{
		in: strings.NewReader(""), out: &out, err: &out, inTTY: true, outTTY: true, width: 120,
		environ: []string{"TERM=xterm-256color", "NO_COLOR=1"}, now: func() time.Time { return fixedNow },
	})
	if code != ExitOK || strings.Contains(out.String(), "\x1b[3") || !strings.Contains(out.String(), "✓") {
		t.Fatalf("exit %d\n%q", code, out.String())
	}
}
