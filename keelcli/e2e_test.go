package keelcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel/internal/testdump"
	"github.com/emoss08/keel/migrate"
)

func jsonDoc(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
}

func jsonKinds(t *testing.T, out string) []string {
	t.Helper()
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var e struct{ Source, Kind string }
		jsonDoc(t, line, &e)
		kinds = append(kinds, e.Source+":"+e.Kind)
	}
	return kinds
}

func TestSampleDeploy(t *testing.T) {
	s := newSample(t)

	up := s.run(t, "up", "-plain", "-env", "development")
	up.expect(t, ExitOK,
		"Marked the database as development.",
		"migrate [1/4] 20260101000000_tenants  core\n",
		"migrate [2/4] 20260102000000_item_indexes  core · notx · lock_timeout 2s\n",
		"migrate [2/4] 20260102000000_item_indexes  statement 2/2\n",
		"migrate [3/4] 20260103000000_backfill_item_states  core · go\n",
		"migrate [4/4] R_views  core · repeatable\n",
		"migrate after statement 1/1 in ",
		"migrate done: 4 migrations applied in ",
		"seed    states runs: new\n",
		"seed    [1/3] states  reference\n",
		"seed    [1/3] states  3 rows in ",
		"seed    [2/3] tenant_settings  reference · 3 tenants\n",
		"seed    [2/3] tenant_settings  tenants 3/3\n",
		"seed    [2/3] tenant_settings  3 rows in ",
		"seed    [3/3] demo_items  3 rows in ",
		"seed    done: 3 seeds applied in ",
	)
	up.lacks(t, "lock acquired after 0", "left undone")
	if n := dbScalar[int](t, s.dsn, "SELECT count(*) FROM tenant_settings"); n != 3 {
		t.Fatalf("%d tenant settings", n)
	}

	s.run(t, "migrate", "status", "-plain").expect(t, ExitOK,
		"20260102000000  item_indexes          core    applied", "R               views", "4 applied")
	s.run(t, "migrate", "plan").expect(t, ExitOK, "Nothing to apply")
	s.run(t, "migrate", "history").expect(t, ExitOK, "backfill_item_states")
	s.run(t, "migrate", "check").expect(t, ExitOK, "No problems")
	s.run(t, "migrate", "lint").expect(t, ExitOK, "No lint findings.")
	s.run(t, "migrate", "lint", "-all").expect(t, ExitOK, "no-serial", "20260101000000_tenants:19", "use identity columns")
	if *update {
		s.run(t, "migrate", "stamp").expect(t, ExitOK)
	}
	s.run(t, "migrate", "stamp", "-check").expect(t, ExitOK, "matches the migrations (version 20260103000000)")

	s.run(t, "seed", "status").expect(t, ExitOK, "states", "applied", "tenant_settings", "demo_items")
	s.run(t, "seed", "plan").expect(t, ExitOK, "up to date", "fixture applied", "0 seeds run.")
	s.run(t, "seed", "plan", "-force", "states").expect(t, ExitOK, "forced", "1 seed runs.")
	s.run(t, "seed", "apply", "-force", "states").expect(t, ExitOK, "seed    [1/1] states  3 rows in ", "done: 1 seed applied")

	again := s.run(t, "up", "-json")
	again.expect(t, ExitOK)
	kinds := strings.Join(jsonKinds(t, again.stdout), ",")
	if !strings.HasPrefix(kinds, "migrate:lock_acquired,migrate:run_end,seed:lock_acquired,seed:planned") || !strings.HasSuffix(kinds, "seed:run_end") {
		t.Fatalf("second up events: %s", kinds)
	}

	var st struct {
		Migrations []struct{ State string }
	}
	jsonDoc(t, s.run(t, "migrate", "status", "-json").stdout, &st)
	if len(st.Migrations) != 4 {
		t.Fatalf("status %+v", st)
	}
	var seeds struct {
		Seeds []struct {
			State string
			Rows  int64
		}
	}
	jsonDoc(t, s.run(t, "seed", "status", "-json").stdout, &seeds)
	if len(seeds.Seeds) != 3 || seeds.Seeds[1].Rows != 3 {
		t.Fatalf("seed status %+v", seeds)
	}
}

func TestSampleCheckFindsDrift(t *testing.T) {
	s := newSample(t)
	s.run(t, "migrate", "up").expect(t, ExitOK)
	dbExec(t, s.dsn, "UPDATE keel_migrations SET checksum = 'abc' WHERE version = '20260101000000'")
	s.run(t, "migrate", "check").expect(t, ExitProblems, "changed", "20260101000000_tenants", "checksum")
	s.run(t, "migrate", "status").expect(t, ExitOK, "changed")
	s.run(t, "migrate", "up").expect(t, ExitFailure, "an applied migration was edited")
	s.run(t, "migrate", "repair", "-yes", "20260101000000").expect(t, ExitOK, "Repaired 20260101000000_tenants: checksum abc → ")
	s.run(t, "migrate", "check").expect(t, ExitOK, "No problems")
}

func TestSampleSeedPrune(t *testing.T) {
	s := newSample(t)
	s.run(t, "up").expect(t, ExitOK)
	dbExec(t, s.dsn, `INSERT INTO keel_seeds (name, kind, fingerprint, applied_at, duration_ms, rows)
VALUES ('retired_codes', 'reference', 'x', now(), 5, 12)`)
	s.run(t, "seed", "status").expect(t, ExitOK, "retired_codes", "orphaned")
	s.run(t, "seed", "prune").expect(t, ExitUsage, "pass -yes")
	s.run(t, "seed", "prune", "-yes").expect(t, ExitOK, "Pruned 1 seed record.")
	s.run(t, "seed", "prune").expect(t, ExitOK, "Nothing to prune")
}

func TestSampleTemplatesAndReset(t *testing.T) {
	s := newSample(t)
	s.run(t, "up", "-env", "development").expect(t, ExitOK)
	s.run(t, "template").expect(t, ExitOK, "No templates")
	s.run(t, "template", "build").expect(t, ExitOK, "Template ready: keel_tpl_")

	stale := s.app
	stale.Migrations = migrate.Sources(migrate.Source{Name: "core", FS: os.DirFS(sampleDir)})
	runApp(t, stale, false, nil, "template", "build").expect(t, ExitOK, "Template ready")
	s.run(t, "template").expect(t, ExitOK, "current", "stale")
	s.run(t, "template", "prune", "-yes").expect(t, ExitOK, "Dropped 1 template.")
	s.run(t, "template", "prune").expect(t, ExitOK, "Nothing to prune")

	dbExec(t, s.dsn, "INSERT INTO tenants (id, name) VALUES ('t9', 'Stray')")
	s.run(t, "reset").expect(t, ExitUsage, "pass -yes")
	name := strings.TrimPrefix(s.dsn[strings.LastIndex(s.dsn, "/"):], "/")
	name, _, _ = strings.Cut(name, "?")
	s.run(t, "reset", "-yes").expect(t, ExitOK, "Reset "+name+" from keel_tpl_")
	if n := dbScalar[int](t, s.dsn, "SELECT count(*) FROM tenants"); n != 3 {
		t.Fatalf("reset kept %d tenants", n)
	}
	if n := dbScalar[int](t, s.dsn, "SELECT count(*) FROM items"); n != 0 {
		t.Fatalf("reset kept %d fixture items", n)
	}
	s.run(t, "seed", "status").expect(t, ExitOK, "demo_items", "due")
	s.run(t, "doctor").expect(t, ExitOK, "environment      pass    development", "templates        pass    keel_tpl_")
	s.run(t, "migrate", "status").expect(t, ExitOK, "4 applied")
}

func TestSampleDoctor(t *testing.T) {
	s := newSample(t)
	s.run(t, "up").expect(t, ExitOK)
	r := s.run(t, "doctor")
	r.expect(t, ExitOK, "connection       pass", "server           pass    PostgreSQL", "plan_cache_mode  pass    auto",
		"extensions       pass    plpgsql", "migrations       pass    up to date at 20260103000000", "environment      info    unmarked")
	var doc struct {
		Checks []struct{ Name, Level string }
	}
	jsonDoc(t, s.run(t, "doctor", "-json").stdout, &doc)
	if len(doc.Checks) < 8 {
		t.Fatalf("doctor json %+v", doc)
	}
	s.app.Extensions = []string{"postgis_never_installed"}
	s.run(t, "doctor").expect(t, ExitProblems, "missing postgis_never_installed")
}

func TestSampleProduction(t *testing.T) {
	s := newSample(t)
	s.run(t, "up", "-env", "production").expect(t, ExitFailure, "fixtures refused on a production database: demo_items",
		"migrate done: 4 migrations applied")
	s.run(t, "up", "-env", "staging").expect(t, ExitFailure, "marked production, not staging")
	s.run(t, "seed", "plan").expect(t, ExitOK, "refused")
	s.run(t, "seed", "apply", "states").expect(t, ExitOK, "done: 1 seed applied")
	s.run(t, "seed", "apply").expect(t, ExitFailure, "fixtures refused")
	s.run(t, "reset", "-yes", "-yes-production").expect(t, ExitUsage, "where it never runs")
	stale := s.app
	stale.Migrations = migrate.Sources(migrate.Source{Name: "core", FS: os.DirFS(sampleDir)})
	runApp(t, stale, false, nil, "template", "build").expect(t, ExitOK, "Template ready")
	s.run(t, "template", "prune", "-yes").expect(t, ExitUsage, "marked production; pass -yes-production")
	s.run(t, "migrate", "repair", "-yes", "20260101000000").expect(t, ExitUsage, "marked production; pass -yes-production")
	s.run(t, "seed", "prune", "-yes").expect(t, ExitOK, "Nothing to prune")
	s.run(t, "doctor").expect(t, ExitOK, "production: fixtures and destructive commands refuse")
}

func TestSampleLockWait(t *testing.T) {
	s := newSample(t)
	s.run(t, "migrate", "up").expect(t, ExitOK)
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(s.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = "other-deploy"
	holder, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock(7372631836457123681)"); err != nil {
		t.Fatal(err)
	}
	pid := holder.PgConn().PID()
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(1500 * time.Millisecond)
		_, _ = holder.Exec(ctx, "SELECT pg_advisory_unlock(7372631836457123681)")
	}()
	r := s.run(t, "migrate", "up", "-plain")
	<-released
	r.expect(t, ExitOK,
		"lock    waiting for the migration lock (attempt 1",
		"lock    waiting for the migration lock (attempt 2",
		"blocked by pid "+strconv.Itoa(int(pid))+" other-deploy, ",
		"ExclusiveLock: SELECT pg_advisory_lock(7372631836457123681)",
		"lock    migration lock acquired after 1.",
		"migrate done: 0 migrations applied",
	)
}

func TestSampleLiveView(t *testing.T) {
	s := newSample(t)
	r := runApp(t, s.app, true, nil, "up")
	out := ansi.Strip(r.stdout)
	if r.code != ExitOK || r.stderr != "" {
		t.Fatalf("exit %d\n%s\nstderr: %s", r.code, out, r.stderr)
	}
	for _, w := range []string{"Summary", "Migrations  4 migrations applied", "Seeds       3 seeds applied", "item_indexes", "tenant_settings"} {
		if !strings.Contains(out, w) {
			t.Fatalf("live view lacks %q:\n%s", w, out)
		}
	}
}

func sqlOnlySample(t *testing.T) (*sample, string) {
	t.Helper()
	s := newSample(t)
	dir := t.TempDir()
	for _, f := range []string{"20260101000000_tenants.sql", "20260102000000_item_indexes.sql", "R_views.sql"} {
		body, err := os.ReadFile(filepath.Join(sampleDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.app.Dirs = []migrate.Dir{{Name: "core", Path: dir}}
	s.app.Migrations = migrate.Set{}
	s.app.Stamp = filepath.Join(dir, "schema", "version_gen.go")
	return s, dir
}

func TestSampleWritesFiles(t *testing.T) {
	s, dir := sqlOnlySample(t)
	s.run(t, "migrate", "stamp").expect(t, ExitOK, "Wrote "+s.app.Stamp)
	s.run(t, "migrate", "create", "-kind", "notx", "add_codes").expect(t, ExitOK, "Created "+dir+"/")
	s.run(t, "migrate", "status").expect(t, ExitOK, "add_codes", "3 pending")
	s.run(t, "migrate", "stamp", "-check").expect(t, ExitProblems, "is stale")
	s.run(t, "migrate", "create", "-source", "cloud", "x").expect(t, ExitFailure, `no migration directory for source "cloud"`)
}

func TestSampleSquash(t *testing.T) {
	requirePostgres(t)
	dump := testdump.Command(t)
	s, dir := sqlOnlySample(t)
	s.app.Dump = dump
	s.app.Seeds = nil
	s.run(t, "migrate", "up").expect(t, ExitOK, "done: 3 migrations applied")
	s.run(t, "migrate", "squash").expect(t, ExitUsage, "pass -yes")
	s.run(t, "migrate", "squash", "-yes").expect(t, ExitOK, "Wrote "+filepath.Join(dir, "20260102000000_baseline.sql")+", replacing 2 migrations.")
	s.run(t, "migrate", "status").expect(t, ExitOK, "baseline", "applied")
	s.run(t, "migrate", "plan").expect(t, ExitOK, "Would record as applied without running:", "20260102000000_baseline")
	s.run(t, "migrate", "up").expect(t, ExitOK, "baseline 20260102000000_baseline recorded as applied", "done: 0 migrations applied")
	s.run(t, "migrate", "check").expect(t, ExitOK, "No problems")
}

func TestSampleApplicationCommand(t *testing.T) {
	s := newSample(t)
	s.app.Commands = []Command{{
		Name: "tenants", Summary: "count the tenants",
		Run: func(ctx context.Context, env *Env, args []string) error {
			db, err := env.DB(ctx)
			if err != nil {
				return err
			}
			var n int
			if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&n); err != nil {
				return err
			}
			_, err = fmt.Fprintf(env.Stdout, "%d tenants\n", n)
			return err
		},
	}}
	s.run(t, "migrate", "up").expect(t, ExitOK)
	s.run(t, "tenants").expect(t, ExitOK, "3 tenants")
}
