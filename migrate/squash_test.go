package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/emoss08/keel/internal/testdump"
)

// dumpCommand returns the test server's pg_dump command (see testdump.Command).
func dumpCommand(t *testing.T) []string {
	t.Helper()
	requirePostgres(t)
	return testdump.Command(t)
}

// writeFiles writes migration files into dir.
func writeFiles(t *testing.T, dir string, kv ...string) {
	t.Helper()
	for i := 0; i < len(kv); i += 2 {
		if err := os.WriteFile(filepath.Join(dir, kv[i]), []byte(kv[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// dirSet reads dir as the core source.
func dirSet(dir string) Set { return Sources(Source{Name: "core", FS: os.DirFS(dir)}) }

func TestSquash(t *testing.T) {
	dump := dumpCommand(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFiles(t, dir,
		"20260101000000_init.sql", `CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TYPE status AS ENUM ('new', 'done');
CREATE TABLE shipments (id text PRIMARY KEY DEFAULT gen_random_uuid()::text, status status NOT NULL DEFAULT 'new', weight int);
CREATE FUNCTION shipment_count() RETURNS bigint LANGUAGE plpgsql SET search_path = public AS $$
BEGIN
	RETURN (SELECT count(*) FROM shipments);
END
$$;`,
		"20260102000000_index.sql", "--keel:notx\nCREATE INDEX CONCURRENTLY shipments_status ON shipments (status);",
		"20260103000000_stops.sql", "CREATE TABLE stops (id serial PRIMARY KEY, shipment_id text REFERENCES shipments (id), note text);\nCOMMENT ON TABLE stops IS 'stops; with a semicolon';",
		"R_view.sql", "CREATE OR REPLACE VIEW shipment_weights AS SELECT id, weight FROM shipments;",
	)
	after := []string{"CREATE TABLE IF NOT EXISTS after_marker (id int)"}

	old := database(t)
	if _, err := open(t, old, dirSet(dir), Options{After: after}).Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}

	res, err := Squash(ctx, SquashOptions{Admin: adminDSN(), Dirs: []Dir{{Name: "core", Path: dir}}, Dump: dump, Options: Options{After: after}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "20260103000000" || res.Baseline != filepath.Join(dir, "20260103000000_baseline.sql") ||
		len(res.Squashed) != 3 || len(res.Removed) != 3 {
		t.Fatalf("result %+v", res)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[20260103000000_baseline.sql R_view.sql]" {
		t.Fatalf("files %v", names)
	}
	body, _ := os.ReadFile(res.Baseline)
	text := string(body)
	if !strings.HasPrefix(text, "--keel:baseline\n") || strings.Contains(text, `\restrict`) || strings.Contains(text, "keel_migrations") ||
		strings.Contains(text, "set_config('search_path'") || strings.Contains(text, "Dumped from") || !strings.Contains(text, "CREATE TABLE public.stops") ||
		strings.Contains(text, "shipment_weights") {
		t.Fatalf("baseline:\n%s", text)
	}

	writeFiles(t, dir, "20260104000000_more.sql", "ALTER TABLE stops ADD COLUMN arrived_at timestamptz;")
	set := dirSet(dir)

	fresh := database(t)
	got, err := open(t, fresh, set, Options{After: after}).Up(ctx, UpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(migrationIDs(got.Applied)) != "[20260103000000_baseline 20260104000000_more R_view]" {
		t.Fatalf("fresh applied %v", migrationIDs(got.Applied))
	}
	fc := connect(t, fresh)
	if n := scalar[int64](t, fc, "SELECT shipment_count()"); n != 0 {
		t.Fatal("function missing")
	}
	if !scalar[bool](t, fc, "SELECT to_regclass('shipments_status') IS NOT NULL AND to_regclass('after_marker') IS NOT NULL") {
		t.Fatal("index or After table missing")
	}

	rec := newRecorder()
	m := open(t, old, set, Options{After: after, Observer: rec.observe})
	got, err = m.Up(ctx, UpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(migrationIDs(got.Marked)) != "[20260103000000_baseline]" || fmt.Sprint(migrationIDs(got.Applied)) != "[20260104000000_more]" {
		t.Fatalf("old: marked %v applied %v", migrationIDs(got.Marked), migrationIDs(got.Applied))
	}
	if !slices.Contains(rec.kinds(), BaselineMarked) {
		t.Fatalf("events %v", rec.kinds())
	}
	st, _ := m.Status(ctx)
	for _, s := range st {
		if s.State != StateApplied {
			t.Fatalf("status %+v", s)
		}
	}
	if len(st) != 3 {
		t.Fatalf("%d statuses", len(st))
	}
	if _, err := Squash(ctx, SquashOptions{Admin: adminDSN(), Dirs: []Dir{{Name: "core", Path: dir}}, Dump: dump, To: "20260103000000"}); err == nil {
		t.Fatal("squashed a lone baseline")
	}
}

func TestCleanDump(t *testing.T) {
	in := `--
-- PostgreSQL database dump
--

\restrict abc123

-- Dumped from database version 18.0
-- Dumped by pg_dump version 18.0

SET statement_timeout = 0;
SET client_encoding = 'UTF8';
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET row_security = off;
COMMENT ON EXTENSION pgcrypto IS 'cryptographic functions';


CREATE FUNCTION public.f() RETURNS text
    LANGUAGE sql
    SET search_path TO 'public'
    AS $$
SELECT 'x'
\not a meta command
$$;

\unrestrict abc123
`
	want := `SET LOCAL client_encoding = 'UTF8';
SET LOCAL check_function_bodies = false;

CREATE FUNCTION public.f() RETURNS text
    LANGUAGE sql
    SET search_path TO 'public'
    AS $$
SELECT 'x'
\not a meta command
$$;
`
	if got := cleanDump(in); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if d := firstDiff("a\nb", "a\nc"); !strings.Contains(d, "line 2") {
		t.Fatal(d)
	}
}
