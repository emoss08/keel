package render

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

var update = flag.Bool("update", false, "rewrite golden files")

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

func migrationEvents() []migrateport.Event {
	m := &migrateport.Migration{Version: "20260101000000", Name: "index", Source: "core", Kind: migrateport.Versioned,
		Directives: migrateport.Directives{NoTx: true, LockTimeout: -1}}
	r := &migrateport.Migration{Name: "views", Source: "core", Kind: migrateport.Repeatable}
	b := &migrateport.Migration{Version: "20251231000000", Name: "baseline", Source: "core", Kind: migrateport.Baseline}
	holder := []migrateport.Blocker{{PID: 42, Application: "psql", State: "idle in transaction", Mode: "AccessShareLock",
		Age: 3 * time.Second, Query: "SELECT *\n  FROM shipments"}}
	timeout := errors.New("ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)")
	return []migrateport.Event{
		{Kind: migrateport.LockWait, Attempt: 1, Blockers: holder},
		{Kind: migrateport.LockWait, Attempt: 2, Duration: time.Second, Blockers: holder},
		{Kind: migrateport.LockAcquired, Duration: 1500 * time.Millisecond},
		{Kind: migrateport.BaselineMarked, Migration: b},
		{Kind: migrateport.MigrationStart, Migration: m, Index: 1, Total: 2},
		{Kind: migrateport.Statement, Migration: m, Index: 1, Total: 2, Statement: 1, Statements: 2},
		{Kind: migrateport.Retry, Migration: m, Index: 1, Total: 2, Statement: 1, Statements: 2, Attempt: 1, Err: timeout, Blockers: holder},
		{Kind: migrateport.MigrationEnd, Migration: m, Index: 1, Total: 2, Duration: 20 * time.Millisecond},
		{Kind: migrateport.MigrationStart, Migration: r, Index: 2, Total: 2},
		{Kind: migrateport.MigrationEnd, Migration: r, Index: 2, Total: 2, Duration: 3 * time.Millisecond},
		{Kind: migrateport.Retry, Attempt: 1, Err: timeout, Blockers: holder},
		{Kind: migrateport.After, Statement: 1, Statements: 1, Duration: 4 * time.Millisecond},
		{Kind: migrateport.RunEnd, Total: 2, Duration: 2 * time.Second},
		{Kind: migrateport.LockAcquired},
		{Kind: migrateport.RunEnd, Duration: time.Second, Err: errors.New("migrate: 20260101000000_index: boom")},
	}
}

func seedEvents() []seedport.Event {
	ref := &seedport.Seed{Name: "states", Kind: seedport.Reference}
	per := &seedport.Seed{Name: "settings", Kind: seedport.Reference, PerTenant: true}
	fix := &seedport.Seed{Name: "demo", Kind: seedport.Fixture}
	return []seedport.Event{
		{Kind: seedport.LockWait, Duration: time.Second},
		{Kind: seedport.LockAcquired, Duration: 1200 * time.Millisecond},
		{Kind: seedport.Planned, Seed: ref, Reason: seedport.Reason{Kind: seedport.InputChanged, Inputs: []string{"data/states.yaml"}}, Index: 1, Total: 3},
		{Kind: seedport.Planned, Seed: per, Reason: seedport.Reason{Kind: seedport.UpToDate}, Index: 2, Total: 3},
		{Kind: seedport.Planned, Seed: fix, Reason: seedport.Reason{Kind: seedport.Refused}, Index: 3, Total: 3},
		{Kind: seedport.SeedStart, Seed: ref, Index: 1, Total: 2},
		{Kind: seedport.SeedEnd, Seed: ref, Index: 1, Total: 2, Rows: 56, Duration: 30 * time.Millisecond},
		{Kind: seedport.SeedStart, Seed: per, Index: 2, Total: 2, Tenants: 2},
		{Kind: seedport.TenantStart, Seed: per, Index: 2, Total: 2, Tenant: "t1", Tenants: 2},
		{Kind: seedport.TenantEnd, Seed: per, Index: 2, Total: 2, Tenant: "t1", Tenants: 2, TenantsDone: 1, Rows: 4},
		{Kind: seedport.TenantEnd, Seed: per, Index: 2, Total: 2, Tenant: "t2", Tenants: 2, TenantsDone: 1, Rows: 4, Err: errors.New("boom")},
		{Kind: seedport.SeedEnd, Seed: per, Index: 2, Total: 2, Tenants: 2, Rows: 4, Duration: time.Second, Err: errors.New("seed: settings: tenant \"t2\": boom")},
		{Kind: seedport.RunEnd, Total: 1, Duration: 2 * time.Second, Err: errors.New("seed: settings: tenant \"t2\": boom")},
	}
}

func TestEventLinesGolden(t *testing.T) {
	var buf bytes.Buffer
	l := NewLines(&buf)
	for _, e := range migrationEvents() {
		l.Migration(e)
	}
	for _, e := range seedEvents() {
		l.Seed(e)
	}
	golden(t, "events_plain", buf.String())
}

func TestEventJSONGolden(t *testing.T) {
	var buf bytes.Buffer
	j := NewJSON(&buf)
	for _, e := range migrationEvents() {
		j.Migration(e)
	}
	for _, e := range seedEvents() {
		j.Seed(e)
	}
	golden(t, "events_json", buf.String())
}
