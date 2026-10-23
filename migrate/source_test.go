package migrate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDirectives(t *testing.T) {
	set := one(files(
		"20260101000000_a.sql", "-- comment\n--keel:notx\n--keel:lock_timeout 2s\n--keel:statement_timeout 1m\n--keel:no_retry\n--keel:lint_ignore lock-heavy, notx-mixed\n\nCREATE INDEX CONCURRENTLY i ON t (a);\n--keel:split\nCREATE INDEX CONCURRENTLY j ON t (b);\n",
		"20260102000000_b.sql", "--keel:lock_timeout 0\nSELECT 1;",
		"20251231000000_c.sql", "--keel:baseline\nCREATE TABLE t (a int);",
	))
	all, err := set.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Kind != KindBaseline || all[0].Kind.String() != "baseline" {
		t.Fatalf("kind %v", all[0].Kind)
	}
	ms := all[1:]
	a := ms[0]
	want := Directives{NoTx: true, LockTimeout: 2 * time.Second, StatementTimeout: time.Minute, NoRetry: true,
		LintIgnore: []string{"lock-heavy", "notx-mixed"}}
	if fmt.Sprint(a.Directives) != fmt.Sprint(want) {
		t.Fatalf("directives %+v", a.Directives)
	}
	if got := a.Statements(); len(got) != 2 || !strings.HasSuffix(got[0], "ON t (a);") || got[1] != "CREATE INDEX CONCURRENTLY j ON t (b);" {
		t.Fatalf("statements %q", got)
	}
	if ms[1].Directives.LockTimeout != -1 {
		t.Fatalf("lock_timeout 0 = %v", ms[1].Directives.LockTimeout)
	}
	if a.Source != "core" || a.File != "20260101000000_a.sql" || a.Checksum != checksum(a.SQL) || a.ID() != "20260101000000_a" {
		t.Fatalf("migration %+v", a)
	}
}

func TestDirectiveErrors(t *testing.T) {
	cases := map[string]string{
		"unknown":            "--keel:notxx\nSELECT 1;",
		"misplaced":          "SELECT 1;\n--keel:notx\nSELECT 2;",
		"twice":              "--keel:notx\n--keel:notx\nSELECT 1;",
		"split without notx": "SELECT 1;\n--keel:split\nSELECT 2;",
		"argument":           "--keel:notx yes\nSELECT 1;",
		"no duration":        "--keel:lock_timeout\nSELECT 1;",
		"bad duration":       "--keel:statement_timeout soon\nSELECT 1;",
		"empty ignore":       "--keel:lint_ignore\nSELECT 1;",
		"empty name":         "--keel:\nSELECT 1;",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := one(files("20260101000000_a.sql", body)).Err(); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestDirectivesIgnoreLiterals(t *testing.T) {
	body := "CREATE FUNCTION f() RETURNS text LANGUAGE sql AS $$\n--keel:notx\nSELECT 'x'\n$$;\n"
	set := one(files("20260101000000_f.sql", body))
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	notx := "--keel:notx\nCREATE FUNCTION f() RETURNS text LANGUAGE sql AS $body$\nSELECT 1;\n--keel:split\nSELECT 2;\n$body$;\n--keel:split\nSELECT 3;\n"
	ms, err := one(files("20260101000000_f.sql", notx)).Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if got := ms[0].Statements(); len(got) != 2 || got[1] != "SELECT 3;" {
		t.Fatalf("statements %q", got)
	}
}

func TestReadErrors(t *testing.T) {
	cases := map[string]Set{
		"bad name":          one(files("items.sql", "x")),
		"short version":     one(files("1_items.sql", "x")),
		"not a timestamp":   one(files("20261399000000_items.sql", "x")),
		"down file":         one(files("20260101000000_items.down.sql", "x")),
		"up file":           one(files("20260101000000_items.up.sql", "x")),
		"empty repeatable":  one(files("R_.sql", "x")),
		"duplicate version": one(files("20260101000000_a.sql", "x", "20260101000000_b.sql", "x")),
		"duplicate across sources": Sources(
			Source{Name: "core", FS: files("20260101000000_a.sql", "x")},
			Source{Name: "cloud", FS: files("20260101000000_b.sql", "x")}),
		"duplicate repeatable": Sources(
			Source{Name: "core", FS: files("R_v.sql", "x")},
			Source{Name: "cloud", FS: files("R_v.sql", "y")}),
		"duplicate source":    Sources(Source{Name: "core"}, Source{Name: "core"}),
		"older than baseline": one(files("20260102000000_base.sql", "--keel:baseline\nSELECT 1;", "20260101000000_a.sql", "x")),
		"repeatable baseline": one(files("R_v.sql", "--keel:baseline\nSELECT 1;")),
		"go without func":     one(nil, Func{Version: "20260101000000", Name: "g"}),
		"go with both": one(nil, Func{Version: "20260101000000", Name: "g",
			Up:     func(context.Context, pgx.Tx) error { return nil },
			UpNoTx: func(context.Context, *pgx.Conn) error { return nil }}),
		"go bad version": one(nil, Func{Version: "1", Name: "g", Up: func(context.Context, pgx.Tx) error { return nil }}),
		"go no name":     one(nil, Func{Version: "20260101000000", Up: func(context.Context, pgx.Tx) error { return nil }}),
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			if set.Err() == nil {
				t.Fatal("no error")
			}
			if _, err := set.Migrations(); err == nil || !strings.HasPrefix(err.Error(), "migrate:") {
				t.Fatalf("error %v", err)
			}
		})
	}
}

func TestSourcesMerge(t *testing.T) {
	noop := func(context.Context, pgx.Tx) error { return nil }
	set := Sources(
		Source{Name: "core", FS: files("20260103000000_c.sql", "x", "20260101000000_a.sql", "x", "R_b.sql", "x", "README.md", "ignored")},
		Source{Name: "cloud", FS: files("20260102000000_b.sql", "x", "R_a.sql", "x"), Funcs: []Func{{Version: "20260104000000", Name: "backfill", Up: noop}}},
		Source{FS: files("20260105000000_e.sql", "x")},
	)
	ms, err := set.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	got := migrationIDs(ms)
	if fmt.Sprint(got) != "[20260101000000_a 20260102000000_b 20260103000000_c 20260104000000_backfill 20260105000000_e R_a R_b]" {
		t.Fatalf("order %v", got)
	}
	if ms[1].Source != "cloud" || ms[3].Kind != KindGo || ms[3].SQL != "" || ms[4].Source != "default" || ms[5].Kind != KindRepeatable {
		t.Fatalf("migrations %+v", ms)
	}
	if set.Version() != "20260105000000" {
		t.Fatalf("version %s", set.Version())
	}
	if ms[3].Statements() != nil {
		t.Fatal("Go migration has statements")
	}
}

func TestFingerprint(t *testing.T) {
	a := one(files("20260101000000_a.sql", "x", "R_v.sql", "y"))
	b := one(files("R_v.sql", "y", "20260101000000_a.sql", "x"))
	if a.Fingerprint() == "" || a.Fingerprint() != b.Fingerprint() {
		t.Fatal("fingerprint is not stable")
	}
	for name, other := range map[string]Set{
		"content": one(files("20260101000000_a.sql", "x ", "R_v.sql", "y")),
		"name":    one(files("20260101000000_b.sql", "x", "R_v.sql", "y")),
		"source":  Sources(Source{Name: "other", FS: files("20260101000000_a.sql", "x", "R_v.sql", "y")}),
		"go": one(files("20260101000000_a.sql", "x", "R_v.sql", "y"),
			Func{Version: "20260102000000", Name: "g", Up: func(context.Context, pgx.Tx) error { return nil }}),
	} {
		if other.Fingerprint() == a.Fingerprint() {
			t.Errorf("%s change keeps the fingerprint", name)
		}
	}
}

func TestLex(t *testing.T) {
	src := "SELECT 'a;b', E'c\\'d;', $$e;f$$, $t$g;$$h$t$, \"i;j\" -- k;l\n/* m; /* n; */ o; */ p; q"
	l := lex(src)
	if len(l.masked) != len(src) {
		t.Fatal("masked length differs")
	}
	sts := l.statements()
	if len(sts) != 2 || !strings.HasPrefix(sts[1].text, "q") || !strings.Contains(sts[0].text, `"i;j"`) {
		t.Fatalf("statements %+v", sts)
	}
	for _, leaked := range []string{"a;b", "c", "e;f", "g;", "k;l", "m;", "o;"} {
		if strings.Contains(sts[0].text, leaked) {
			t.Errorf("%q leaked into %q", leaked, sts[0].text)
		}
	}
	if l.line(strings.Index(src, "q")) != 2 {
		t.Fatal("line")
	}
}
