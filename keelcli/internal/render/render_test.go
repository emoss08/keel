package render

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                     "0ms",
		420 * time.Microsecond:                "<1ms",
		42 * time.Millisecond:                 "42ms",
		3 * time.Second:                       "3s",
		1800 * time.Millisecond:               "1.8s",
		time.Minute:                           "1m",
		4*time.Minute + 12*time.Second:        "4m12s",
		2 * time.Hour:                         "2h",
		2*time.Hour + 5*time.Minute:           "2h05m",
		19 * 24 * time.Hour:                   "19d",
		-time.Second:                          "0ms",
		59*time.Second + 999*time.Millisecond: "60.0s",
	}
	for d, want := range cases {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestCountSizePlural(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 9000: "9,000", 1234567: "1,234,567", -4213: "-4,213"} {
		if got := Count(n); got != want {
			t.Errorf("Count(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int64]string{512: "512 B", 2048: "2.0 KB", 48 << 20: "48 MB", 3 << 30: "3.0 GB"} {
		if got := Size(n); got != want {
			t.Errorf("Size(%d) = %q, want %q", n, got, want)
		}
	}
	if Plural(1, "seed", "seeds") != "1 seed" || Plural(9000, "tenant", "tenants") != "9,000 tenants" {
		t.Error("Plural")
	}
}

func TestDSN(t *testing.T) {
	cases := map[string]string{
		"postgres://trenova:s3cret@localhost:5432/trenova?sslmode=disable": "postgres://trenova:***@localhost:5432/trenova?sslmode=disable",
		"postgres://localhost/trenova?password=hunter2":                    "postgres://localhost/trenova?password=***",
		"host=localhost user=trenova password=s3cret dbname=trenova":       "host=localhost user=trenova password=*** dbname=trenova",
		"postgres://trenova@localhost/trenova":                             "postgres://trenova@localhost/trenova",
	}
	for in, want := range cases {
		if got := DSN(in); got != want {
			t.Errorf("DSN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWrap(t *testing.T) {
	got := Wrap("refusing to prune: pass -yes-production to proceed", 20)
	want := []string{"refusing to prune:", "pass -yes-production", "to proceed"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Wrap = %q", got)
	}
	if got := Wrap("abcdefghij", 4); strings.Join(got, "|") != "abcd|efgh|ij" {
		t.Fatalf("long word: %q", got)
	}
	if got := Wrap("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty: %q", got)
	}
}

func TestTableFitsAndDropsColumns(t *testing.T) {
	tbl := Table{Columns: []Column{
		{Title: "VERSION", Min: 14},
		{Title: "NAME", Min: 8, Flex: true},
		{Title: "SOURCE", Drop: 2},
		{Title: "DETAIL", Min: 6, Flex: true, Drop: 1},
	}, Rows: [][]Cell{
		{C("20261025110000", Muted), C("edi_partner_index_with_a_long_name", Normal), C("core", Muted), C("notx · lock_timeout 2s", Faint)},
	}}
	full := tbl.Lines(Plain)
	if !strings.Contains(full[1], "edi_partner_index_with_a_long_name") || !strings.Contains(full[1], "notx · lock_timeout 2s") {
		t.Fatalf("unbounded table truncated: %q", full)
	}
	for _, width := range []int{80, 60, 40, 30, 20} {
		tbl.Width = width
		for _, line := range tbl.Lines(Plain) {
			if w := ansi.StringWidth(line); w > width {
				t.Fatalf("width %d: %d cells: %q", width, w, line)
			}
		}
	}
	tbl.Width = 40
	lines := tbl.Lines(Plain)
	if strings.Contains(lines[0], "DETAIL") || strings.Contains(lines[0], "SOURCE") {
		t.Fatalf("optional columns kept at 40: %q", lines)
	}
	if !strings.Contains(lines[1], "20261025110000") {
		t.Fatalf("required column lost: %q", lines)
	}
}

func TestTableRightAlignAndIndent(t *testing.T) {
	tbl := Table{NoHeader: true, Indent: 2, Columns: []Column{{Title: "A"}, {Title: "N", Right: true}}, Rows: [][]Cell{
		{C("x", Normal), C("5", Normal)}, {C("y", Normal), C("1,000", Normal)},
	}}
	got := tbl.String(Plain)
	want := "  x      5\n  y  1,000\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinesThrottlesTenants(t *testing.T) {
	var buf bytes.Buffer
	l := NewLines(&buf)
	s := seedport.Seed{Name: "perms", Kind: seedport.Reference, PerTenant: true}
	l.Seed(seedport.Event{Kind: seedport.SeedStart, Seed: &s, Index: 1, Total: 1, Tenants: 1000})
	for i := 1; i <= 1000; i++ {
		l.Seed(seedport.Event{Kind: seedport.TenantEnd, Seed: &s, Index: 1, Total: 1, Tenants: 1000, TenantsDone: i})
	}
	if n := strings.Count(buf.String(), "tenants "); n != 10 {
		t.Fatalf("%d progress lines, want 10:\n%s", n, buf.String())
	}
	l.Seed(seedport.Event{Kind: seedport.TenantEnd, Seed: &s, Index: 1, Total: 1, Tenant: "org_9", Err: errors.New("boom")})
	if !strings.Contains(buf.String(), "tenant org_9 failed: boom") {
		t.Fatalf("tenant failure missing:\n%s", buf.String())
	}
}

func TestMigrationLines(t *testing.T) {
	var buf bytes.Buffer
	l := NewLines(&buf)
	m := migrateport.Migration{Version: "20261025110000", Name: "edi_partner_index", Source: "core",
		Directives: migrateport.Directives{NoTx: true, LockTimeout: 2 * time.Second, NoRetry: true}}
	l.Migration(migrateport.Event{Kind: migrateport.MigrationStart, Migration: &m, Index: 3, Total: 12})
	l.Migration(migrateport.Event{Kind: migrateport.BaselineMarked, Migration: &m})
	l.Migration(migrateport.Event{Kind: migrateport.RunEnd, Total: 1, Duration: time.Second})
	want := "migrate [ 3/12] 20261025110000_edi_partner_index  core · notx · lock_timeout 2s · no_retry\n" +
		"migrate baseline 20261025110000_edi_partner_index recorded as applied\n" +
		"migrate done: 1 migration applied in 1s\n"
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestPlanTextEmpty(t *testing.T) {
	if got := PlanText(migrateport.Plan{}, false, Plain, 80); !strings.Contains(got, "up to date") {
		t.Fatalf("got %q", got)
	}
	p := migrateport.Plan{Baseline: []migrateport.Migration{{Version: "1", Name: "baseline"}}}
	if got := PlanText(p, false, Plain, 80); !strings.Contains(got, "record as applied without running:\n  1_baseline") {
		t.Fatalf("got %q", got)
	}
}
