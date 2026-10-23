package migratecli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/migrate"
)

// events is one of each event kind.
func events() []migrate.Event {
	m := &migrate.Migration{Version: "20260101000000", Name: "index", Source: "core", Kind: migrate.KindVersioned,
		Directives: migrate.Directives{NoTx: true}}
	b := []migrate.Blocker{{PID: 42, Application: "psql", State: "idle in transaction", Mode: "AccessShareLock",
		Age: 3 * time.Second, Query: "SELECT *\n  FROM shipments"}}
	return []migrate.Event{
		{Kind: migrate.LockWait, Duration: time.Second, Blockers: b},
		{Kind: migrate.LockAcquired, Duration: 1500 * time.Millisecond},
		{Kind: migrate.MigrationStart, Migration: m, Total: 2},
		{Kind: migrate.Statement, Migration: m, Total: 2, Statement: 1, Statements: 2, SQL: "CREATE INDEX CONCURRENTLY a ON t (a)"},
		{Kind: migrate.Retry, Migration: m, Total: 2, Statement: 1, Statements: 2, Attempt: 2, Blockers: b, Err: errors.New("lock timeout")},
		{Kind: migrate.MigrationEnd, Migration: m, Total: 2, Duration: 20 * time.Millisecond},
		{Kind: migrate.BaselineMarked, Migration: m, Index: 1, Total: 2},
		{Kind: migrate.After, Index: 2, Total: 2, SQL: "SELECT reconcile()"},
		{Kind: migrate.RunEnd, Total: 2, Duration: 2 * time.Second},
	}
}

func TestPrinterPlain(t *testing.T) {
	var buf bytes.Buffer
	p := Printer(&buf, false)
	for _, e := range events() {
		p(e)
	}
	want := `waiting for the migration lock (1s)
  held by pid 42 (psql, idle in transaction, AccessShareLock, 3s): SELECT * FROM shipments
migration lock acquired after 1.5s
[1/2] applying 20260101000000_index (core) [notx]
[1/2]   statement 1/2: CREATE INDEX CONCURRENTLY a ON t (a)
[1/2] lock timeout; retrying, attempt 2
  blocked by pid 42 (psql, idle in transaction, AccessShareLock, 3s): SELECT * FROM shipments
[1/2] applied 20260101000000_index (core) in 20ms
[2/2] marked 20260101000000_index (core) as applied
after: SELECT reconcile()
applied 1 migration and marked 1 baseline in 2s
`
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestPrinterJSON(t *testing.T) {
	var buf bytes.Buffer
	p := Printer(&buf, true)
	for _, e := range events() {
		p(e)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(events()) {
		t.Fatalf("%d lines", len(lines))
	}
	var retry struct {
		Event     string
		Attempt   int
		Statement int
		Error     string
		Migration struct {
			Version    string
			Kind       string
			Directives struct{ NoTx bool }
		}
		Blockers []struct {
			PID   int32
			Mode  string
			AgeMS float64 `json:"age_ms"`
		}
	}
	if err := json.Unmarshal([]byte(lines[4]), &retry); err != nil {
		t.Fatal(err)
	}
	if retry.Event != "retry" || retry.Attempt != 2 || retry.Error != "lock timeout" || retry.Migration.Kind != "versioned" ||
		retry.Blockers[0].PID != 42 || retry.Blockers[0].AgeMS != 3000 {
		t.Fatalf("retry %+v", retry)
	}
	if !strings.Contains(lines[5], `"duration_ms":20`) || strings.Contains(lines[2], "duration_ms") {
		t.Fatalf("durations:\n%s\n%s", lines[2], lines[5])
	}
}

func TestRunEndLines(t *testing.T) {
	cases := map[string][]migrate.Event{
		"nothing to apply\n":                      {{Kind: migrate.RunEnd}},
		"failed after 1s: 0 migrations applied\n": {{Kind: migrate.RunEnd, Duration: time.Second, Err: errors.New("x")}},
	}
	for want, evs := range cases {
		var buf bytes.Buffer
		p := Printer(&buf, false)
		for _, e := range evs {
			p(e)
		}
		if buf.String() != want {
			t.Errorf("got %q, want %q", buf.String(), want)
		}
	}
}
