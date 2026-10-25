package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// adminDSN is the test server's DSN.
func adminDSN() string {
	if v := os.Getenv("KEEL_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
}

// database creates an empty database for one test and returns its DSN.
func database(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN())
	if err != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	name := "keel_cli_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgx.Connect(context.Background(), adminDSN())
		if err == nil {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			a.Close(context.Background())
		}
	})
	u, err := url.Parse(adminDSN())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// cli runs the command line without a terminal and returns its exit code and output.
func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// ok runs the command line and fails the test on a non-zero exit.
func ok(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := cli(t, args...)
	if code != 0 {
		t.Fatalf("keel %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

// contains fails the test unless out holds every want.
func contains(t *testing.T, what, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Fatalf("%s lacks %q:\n%s", what, w, out)
		}
	}
}

func TestUsage(t *testing.T) {
	t.Setenv("KEEL_DSN", "")
	for _, args := range [][]string{nil, {"nope"}, {"migrate"}, {"migrate", "nope"}, {"migrate", "status", "extra"}, {"migrate", "repair"}} {
		if code, _, errOut := cli(t, args...); code != 2 || !strings.Contains(errOut, "keelcli: ") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	code, _, errOut := cli(t, "migrate", "status", "-dir", t.TempDir())
	if code != 1 || !strings.Contains(errOut, "pass -dsn or set KEEL_DSN") {
		t.Errorf("no dsn: exit %d %s", code, errOut)
	}
	out := ok(t, "help")
	contains(t, "help", out, "gen ", "migrate up", "doctor", "-dsn string", "-dir [name=]path", "-lock-timeout duration",
		"-retries int", "-allow-out-of-order", "-after statement", "-schema schema", "-json")
	if strings.Contains(out, "seed apply") {
		t.Fatalf("help lists seed commands without seeds:\n%s", out)
	}
	contains(t, "help gen", ok(t, "help", "gen"), "Usage: keel gen [-out {dir}/{name}db]")
}

func TestAnalyticsTS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client", "analytics.ts")
	if code, _, _ := cli(t, "analytics", "ts", "-check", "-out", path); code != 3 {
		t.Fatalf("a missing module passed the check: exit %d", code)
	}
	contains(t, "analytics ts", ok(t, "analytics", "ts", "-out", path), "wrote "+path)
	contains(t, "analytics ts -check", ok(t, "analytics", "ts", "-check", "-out", path), "up to date")
	code, _, errOut := cli(t, "analytics", "i18n", "-out", path)
	if code != 2 || !strings.Contains(errOut, "keeltables.All()") {
		t.Fatalf("analytics i18n without tables: exit %d %s", code, errOut)
	}
	contains(t, "help", ok(t, "help"), "analytics")
}

func TestMigrateCommands(t *testing.T) {
	dsn := database(t)
	dir := t.TempDir()
	d := "core=" + dir

	items := strings.TrimSpace(strings.TrimPrefix(ok(t, "migrate", "create", "items", "-dir", d), "Created "))
	write(t, items, "CREATE TABLE items (id int PRIMARY KEY, name text);")
	var created struct{ Path string }
	if err := json.Unmarshal([]byte(ok(t, "migrate", "create", "-kind", "notx", "-dir", d, "-json", "items_name")), &created); err != nil {
		t.Fatal(err)
	}
	write(t, created.Path, "--keel:notx\nCREATE INDEX CONCURRENTLY items_name ON items (name);")
	view := strings.TrimSpace(strings.TrimPrefix(ok(t, "migrate", "create", "views", "-kind", "repeatable", "-dir", d), "Created "))
	write(t, view, "CREATE OR REPLACE VIEW item_names AS SELECT name FROM items;")
	itemsVersion := strings.SplitN(filepath.Base(items), "_", 2)[0]

	contains(t, "plan", ok(t, "migrate", "plan", "-dsn", dsn, "-dir", d, "-after", "SELECT 1"),
		"Would apply 3 migrations:", "R               views", "Then, in the last migration's transaction:", "SELECT 1")
	contains(t, "dry run", ok(t, "migrate", "up", "-dry-run", "-dsn", dsn, "-dir", d), "CREATE TABLE items")
	contains(t, "up -to", ok(t, "migrate", "up", "-dsn", dsn, "-dir", d, "-to", itemsVersion),
		"[1/1] "+itemsVersion+"_items  applied in", "done: 1 migration applied in")

	out := ok(t, "up", "-json", "-dsn", dsn, "-dir", d, "-after", "SELECT 1")
	var kinds []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var e struct {
			Kind  string
			Index int
			Total int
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%s: %v", sc.Text(), err)
		}
		kinds = append(kinds, e.Kind)
	}
	want := "lock_acquired,migration_start,statement,migration_end,migration_start,after,migration_end,run_end"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("events %v\n%s", kinds, out)
	}
	if !strings.Contains(out, `"kind":"run_end","total":2`) {
		t.Fatalf("run_end does not count the migrations applied:\n%s", out)
	}

	var st struct {
		Migrations []struct{ Name, State, Kind string }
	}
	if err := json.Unmarshal([]byte(ok(t, "migrate", "status", "-json", "-dsn", dsn, "-dir", d)), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Migrations) != 3 || st.Migrations[0].State != "applied" || st.Migrations[2].Kind != "repeatable" || st.Migrations[2].State != "applied" {
		t.Fatalf("status %+v", st)
	}
	contains(t, "status", ok(t, "migrate", "status", "-dsn", dsn, "-dir", d), "R               views", "3 applied")
	if out := ok(t, "migrate", "history", "-dsn", dsn, "-dir", d); strings.Count(out, "\n") != 4 {
		t.Fatalf("history:\n%s", out)
	}
	contains(t, "plan", ok(t, "migrate", "plan", "-dsn", dsn, "-dir", d), "Nothing to apply")
	contains(t, "check", ok(t, "migrate", "check", "-dsn", dsn, "-dir", d), "No problems")

	write(t, items, "-- the items\nCREATE TABLE items (id int PRIMARY KEY, name text);")
	code, out, errOut := cli(t, "migrate", "check", "-dsn", dsn, "-dir", d)
	if code != 3 || !strings.Contains(out, itemsVersion+"_items") || !strings.Contains(errOut, "1 problem") {
		t.Fatalf("check changed: %d\n%s%s", code, out, errOut)
	}
	code, out, _ = cli(t, "migrate", "check", "-json", "-dsn", dsn, "-dir", d)
	var rep struct {
		Problems []struct{ Kind string }
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 3 || len(rep.Problems) != 1 || rep.Problems[0].Kind != "changed" {
		t.Fatalf("check json %d %s %v", code, out, err)
	}
	contains(t, "repair", ok(t, "migrate", "repair", itemsVersion, "-yes", "-dsn", dsn, "-dir", d), "Repaired "+itemsVersion+"_items: checksum")
	ok(t, "migrate", "check", "-dsn", dsn, "-dir", d)

	lint := filepath.Join(dir, "20991231000000_risky.sql")
	write(t, lint, "CREATE INDEX items_id ON items (id);")
	code, out, _ = cli(t, "migrate", "lint", "-dsn", dsn, "-dir", d)
	if code != 0 || !strings.Contains(out, "index-concurrently") || !strings.Contains(out, "20991231000000_risky:1") {
		t.Fatalf("lint: %d\n%s", code, out)
	}
	write(t, lint, "ALTER TABLE items ADD COLUMN code text NOT NULL;")
	t.Setenv("KEEL_DSN", "")
	code, out, _ = cli(t, "migrate", "lint", "-json", "-dir", d)
	var found struct {
		Findings []struct{ Rule, Severity string }
	}
	if err := json.Unmarshal([]byte(out), &found); err != nil || code != 3 || len(found.Findings) != 1 || found.Findings[0].Rule != "not-null-default" {
		t.Fatalf("lint without a database %d %s %v", code, out, err)
	}
	if err := os.Remove(lint); err != nil {
		t.Fatal(err)
	}

	fresh := database(t)
	contains(t, "baseline", ok(t, "migrate", "baseline", itemsVersion, "-yes", "-dsn", fresh, "-dir", d), "Recorded 1 migration as applied")
	if out := ok(t, "migrate", "plan", "-json", "-dsn", fresh, "-dir", d); !strings.Contains(out, `"items_name"`) || strings.Contains(out, `"name":"items"`) {
		t.Fatalf("plan after baseline:\n%s", out)
	}

	stamp := filepath.Join(t.TempDir(), "schema", "version_gen.go")
	contains(t, "stamp", ok(t, "migrate", "stamp", "-dir", d, "-out", stamp), "Wrote "+stamp)
	if src, err := os.ReadFile(stamp); err != nil || !strings.Contains(string(src), "package schema") {
		t.Fatalf("stamp file %v:\n%s", err, src)
	}
	contains(t, "stamp -check", ok(t, "migrate", "stamp", "-dir", d, "-out", stamp, "-check"), "matches the migrations")
	ok(t, "migrate", "create", "more", "-dir", d)
	if code, out, _ := cli(t, "migrate", "stamp", "-dir", d, "-out", stamp, "-check"); code != 3 || !strings.Contains(out, "is stale") {
		t.Fatalf("stale stamp -check: %d %s", code, out)
	}

	if out := ok(t, "template", "-json", "-dsn", dsn, "-dir", d); !strings.HasPrefix(out, `{"templates":`) {
		t.Fatalf("templates:\n%s", out)
	}
}

func TestCheckAgainstBase(t *testing.T) {
	base, head := t.TempDir(), t.TempDir()
	write(t, filepath.Join(base, "20260102000000_b.sql"), "SELECT 1;")
	write(t, filepath.Join(head, "20260102000000_b.sql"), "SELECT 1;")
	write(t, filepath.Join(head, "20260101000000_a.sql"), "SELECT 1;")
	code, out, _ := cli(t, "migrate", "check", "-dir", "core="+head, "-base-dir", "core="+base)
	if code != 3 || !strings.Contains(out, "out-of-order") || !strings.Contains(out, "20260101000000_a") {
		t.Fatalf("check: %d\n%s", code, out)
	}
	if err := os.Rename(filepath.Join(head, "20260101000000_a.sql"), filepath.Join(head, "20260103000000_a.sql")); err != nil {
		t.Fatal(err)
	}
	contains(t, "check", ok(t, "migrate", "check", "-dir", "core="+head, "-base-dir", "core="+base), "No problems")
}

// write writes a file.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlans(t *testing.T) {
	contains(t, "help", ok(t, "help"), "plans", "explain recorded statement shapes")
	dsn := database(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 160000 {
		t.Skipf("EXPLAIN (GENERIC_PLAN) needs PostgreSQL 16, the server is %d", version)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE orders (id int PRIMARY KEY, customer text NOT NULL);
INSERT INTO orders SELECT g, 'c' || (g % 100) FROM generate_series(1, 5000) AS g;
ANALYZE orders`); err != nil {
		t.Fatal(err)
	}
	shapes := filepath.Join(t.TempDir(), "shapes.jsonl")
	write(t, shapes, `{"sql":"SELECT id FROM orders WHERE id = $1","op":"select","table":"orders","args":1}
{"sql":"SELECT id FROM orders WHERE customer = $1","op":"select","table":"orders","args":1}
`)

	contains(t, "plans", ok(t, "plans", "-dsn", dsn, "-shapes", shapes), "No findings: 2 shapes planned.")
	code, out, errOut := cli(t, "plans", "-dsn", dsn, "-shapes", shapes, "-seq-scan-rows", "-1")
	if code != 3 || !strings.Contains(out, "seq-scan sequential scan of public.orders (about 5,000 rows)") ||
		!strings.Contains(errOut, "plans found 1 finding") {
		t.Fatalf("plans -seq-scan-rows -1: exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = cli(t, "plans", "-json", "-dsn", dsn, "-shapes", shapes, "-seq-scan-rows", "-1")
	var doc struct {
		Shapes, Findings int
		Reports          []struct {
			SQL      string
			Findings []struct{ Kind, Relation string }
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 3 || doc.Shapes != 2 || doc.Findings != 1 ||
		len(doc.Reports) != 2 || len(doc.Reports[1].Findings) != 1 || doc.Reports[1].Findings[0].Relation != "public.orders" {
		t.Fatalf("plans json: exit %d %v\n%s", code, err, out)
	}
	t.Setenv("KEEL_DSN", "")
	if code, _, errOut := cli(t, "plans", "-shapes", shapes); code != 1 || !strings.Contains(errOut, "pass -dsn or set KEEL_DSN") {
		t.Fatalf("plans without a database: exit %d %s", code, errOut)
	}
}
