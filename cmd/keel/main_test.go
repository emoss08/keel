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

// keel runs the command line and returns its exit code and output.
func keel(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// ok runs the command line and fails the test on a non-zero exit.
func ok(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := keel(t, args...)
	if code != 0 {
		t.Fatalf("keel %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"migrate"}, {"migrate", "nope"}, {"migrate", "status", "extra"}, {"migrate", "repair"}} {
		if code, _, errOut := keel(t, args...); code != 2 || !strings.Contains(errOut, "usage:") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if code, _, errOut := keel(t, "migrate", "status", "-dsn", "", "-dir", t.TempDir()); code != 1 || !strings.Contains(errOut, "KEEL_DSN") {
		t.Errorf("no dsn: exit %d %s", code, errOut)
	}
}

func TestMigrateCommands(t *testing.T) {
	dsn := database(t)
	dir := t.TempDir()
	d := "core=" + dir

	out := ok(t, "migrate", "create", "items", "-dir", d)
	items := strings.TrimSpace(strings.TrimPrefix(out, "created "))
	write(t, items, "CREATE TABLE items (id int PRIMARY KEY, name text);")
	out = ok(t, "migrate", "create", "items_name", "-notx", "-dir", d, "-json")
	var created map[string]string
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatal(err)
	}
	write(t, created["path"], "--keel:notx\nCREATE INDEX CONCURRENTLY items_name ON items (name);")
	view := strings.TrimSpace(strings.TrimPrefix(ok(t, "migrate", "create", "views", "-repeatable", "-dir", d), "created "))
	write(t, view, "CREATE OR REPLACE VIEW item_names AS SELECT name FROM items;")
	itemsVersion := strings.SplitN(filepath.Base(items), "_", 2)[0]

	out = ok(t, "migrate", "plan", "-dsn", dsn, "-dir", d, "-after", "SELECT 1")
	if !strings.Contains(out, "apply   R_views (core)") || !strings.Contains(out, "after   SELECT 1") || !strings.Contains(out, "3 migrations to apply") {
		t.Fatalf("plan:\n%s", out)
	}
	out = ok(t, "migrate", "up", "-dry-run", "-dsn", dsn, "-dir", d)
	if !strings.Contains(out, "        CREATE TABLE items") {
		t.Fatalf("dry run:\n%s", out)
	}
	out = ok(t, "migrate", "up", "-dsn", dsn, "-dir", d, "-to", itemsVersion)
	if !strings.Contains(out, "[1/1] applied "+itemsVersion+"_items (core)") || !strings.Contains(out, "applied 1 migration in") {
		t.Fatalf("up -to:\n%s", out)
	}

	out = ok(t, "migrate", "up", "-json", "-dsn", dsn, "-dir", d, "-after", "SELECT 1")
	var kinds []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var e struct {
			Event     string
			Migration *struct{ Name, Kind string }
			Total     int
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%s: %v", sc.Text(), err)
		}
		kinds = append(kinds, e.Event)
	}
	if strings.Join(kinds, ",") != "lock_acquired,migration_start,statement,migration_end,migration_start,after,migration_end,run_end" {
		t.Fatalf("events %v\n%s", kinds, out)
	}

	out = ok(t, "migrate", "status", "-json", "-dsn", dsn, "-dir", d)
	var st []struct{ Name, State, Kind string }
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if len(st) != 3 || st[0].State != "applied" || st[2].Kind != "repeatable" || st[2].State != "applied" {
		t.Fatalf("status %+v", st)
	}
	if out := ok(t, "migrate", "status", "-dsn", dsn, "-dir", d); !strings.Contains(out, "applied  R ") {
		t.Fatalf("status:\n%s", out)
	}
	if out := ok(t, "migrate", "history", "-dsn", dsn, "-dir", d); strings.Count(out, "\n") != 4 {
		t.Fatalf("history:\n%s", out)
	}
	if out := ok(t, "migrate", "plan", "-dsn", dsn, "-dir", d); out != "nothing to apply\n" {
		t.Fatalf("plan:\n%s", out)
	}
	if out := ok(t, "migrate", "check", "-dsn", dsn, "-dir", d); !strings.HasPrefix(out, "ok:") {
		t.Fatalf("check:\n%s", out)
	}

	write(t, items, "-- the items\nCREATE TABLE items (id int PRIMARY KEY, name text);")
	code, out, errOut := keel(t, "migrate", "check", "-dsn", dsn, "-dir", d)
	if code != 1 || !strings.Contains(out, "changed       "+itemsVersion+"_items") || !strings.Contains(errOut, "1 problem") {
		t.Fatalf("check changed: %d\n%s%s", code, out, errOut)
	}
	code, out, _ = keel(t, "migrate", "check", "-json", "-dsn", dsn, "-dir", d)
	var rep struct {
		OK      bool
		Changed []struct{ Name string }
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 1 || rep.OK || len(rep.Changed) != 1 {
		t.Fatalf("check json %d %+v %v", code, rep, err)
	}
	if out := ok(t, "migrate", "repair", itemsVersion, "-dsn", dsn, "-dir", d); !strings.Contains(out, "repaired "+itemsVersion+"_items (core): checksum") {
		t.Fatalf("repair:\n%s", out)
	}
	ok(t, "migrate", "check", "-dsn", dsn, "-dir", d)

	lint := filepath.Join(dir, "20991231000000_risky.sql")
	write(t, lint, "CREATE INDEX items_id ON items (id);")
	code, out, _ = keel(t, "migrate", "lint", "-dsn", dsn, "-dir", d)
	if code != 0 || !strings.Contains(out, "core/20991231000000_risky.sql:1: warning: index-concurrently") || !strings.Contains(out, "1 migration linted") {
		t.Fatalf("lint: %d\n%s", code, out)
	}
	write(t, lint, "ALTER TABLE items ADD COLUMN code text NOT NULL;")
	code, out, _ = keel(t, "migrate", "lint", "-json", "-dir", d)
	var findings []struct{ Rule, Severity string }
	if err := json.Unmarshal([]byte(out), &findings); err != nil || code != 1 || len(findings) != 1 || findings[0].Rule != "not-null-default" {
		t.Fatalf("lint json %d %s %v", code, out, err)
	}
	if err := os.Remove(lint); err != nil {
		t.Fatal(err)
	}

	fresh := database(t)
	out = ok(t, "migrate", "baseline", itemsVersion, "-dsn", fresh, "-dir", d)
	if !strings.Contains(out, "1 migration marked as applied") {
		t.Fatalf("baseline:\n%s", out)
	}
	if out := ok(t, "migrate", "plan", "-json", "-dsn", fresh, "-dir", d); !strings.Contains(out, `"items_name"`) || strings.Contains(out, `"name": "items"`) {
		t.Fatalf("plan after baseline:\n%s", out)
	}

	stamp := filepath.Join(t.TempDir(), "version_gen.go")
	if out := ok(t, "migrate", "stamp", "-dir", d, "-package", "schema", "-out", stamp); !strings.Contains(out, "wrote "+stamp) {
		t.Fatalf("stamp:\n%s", out)
	}
	if out := ok(t, "migrate", "stamp", "-dir", d, "-out", stamp, "-check"); !strings.Contains(out, "up to date") {
		t.Fatalf("stamp -check:\n%s", out)
	}
	ok(t, "migrate", "create", "more", "-dir", d)
	if code, _, errOut := keel(t, "migrate", "stamp", "-dir", d, "-out", stamp, "-check"); code != 1 || !strings.Contains(errOut, "out of date") {
		t.Fatalf("stale stamp -check: %d %s", code, errOut)
	}

	if out := ok(t, "migrate", "templates", "-json", "-dsn", dsn); !strings.HasPrefix(out, "[") {
		t.Fatalf("templates:\n%s", out)
	}
	if out := ok(t, "migrate", "templates", "-dsn", dsn); !strings.HasPrefix(out, "NAME") {
		t.Fatalf("templates:\n%s", out)
	}
}

func TestCheckAgainstBase(t *testing.T) {
	base, head := t.TempDir(), t.TempDir()
	write(t, filepath.Join(base, "20260102000000_b.sql"), "SELECT 1;")
	write(t, filepath.Join(head, "20260102000000_b.sql"), "SELECT 1;")
	write(t, filepath.Join(head, "20260101000000_a.sql"), "SELECT 1;")
	code, out, _ := keel(t, "migrate", "check", "-dir", "core="+head, "-base-dir", "core="+base)
	if code != 1 || !strings.Contains(out, "out of order  20260101000000_a (core)") {
		t.Fatalf("check: %d\n%s", code, out)
	}
	write(t, filepath.Join(head, "20260101000000_a.sql"), "")
	if err := os.Rename(filepath.Join(head, "20260101000000_a.sql"), filepath.Join(head, "20260103000000_a.sql")); err != nil {
		t.Fatal(err)
	}
	if out := ok(t, "migrate", "check", "-dir", "core="+head, "-base-dir", "core="+base); !strings.Contains(out, "ok: 1 migration pending") {
		t.Fatalf("check:\n%s", out)
	}
}

// write writes a file.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
