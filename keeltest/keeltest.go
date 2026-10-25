// Package keeltest holds test helpers for code that builds statements with keel: golden snapshots
// of rendered statements, and plan assertions that an index serves a query.
package keeltest

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/keellog"
)

// UpdateEnv names the environment variable that, set to 1, makes Snapshot rewrite the snapshots
// instead of comparing with them. A test binary's own -update flag does the same.
const UpdateEnv = "KEEL_UPDATE"

// Snapshot compares a rendered statement, formatted with keellog.Format and followed by its
// bound values, with testdata/keel/<test name>.sql, and fails the test with the first line that
// differs. Run the tests with KEEL_UPDATE=1 (or -update, where the test binary defines it) to
// write the snapshots. A snapshot shows the whole statement in a reviewable form, so a change to
// how keel renders is one regenerated diff rather than an edit to every assertion.
func Snapshot(t testing.TB, sql string, args []any) {
	t.Helper()
	SnapshotNamed(t, t.Name(), sql, args)
}

// SnapshotNamed is Snapshot with a name of its own, for a test that snapshots several statements.
func SnapshotNamed(t testing.TB, name, sql string, args []any) {
	t.Helper()
	got := render(sql, args)
	path := filepath.Join("testdata", "keel", fileName(name)+".sql")
	if updating() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // a checked-in test fixture
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot %s: %v; write it with %s=1", path, err, UpdateEnv)
	}
	if string(want) == got {
		return
	}
	t.Fatalf("statement differs from %s %s; rewrite it with %s=1 if the change is intended\ngot:\n%s",
		path, firstDiff(string(want), got), UpdateEnv, got)
}

func render(sql string, args []any) string {
	var b strings.Builder
	b.WriteString(keellog.Format(sql))
	b.WriteByte('\n')
	for i, a := range args {
		b.WriteString("-- $")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(" = ")
		b.WriteString(keellog.Literal(a, 1<<10, 1<<10))
		b.WriteByte('\n')
	}
	return b.String()
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func fileName(name string) string {
	return unsafeName.ReplaceAllString(strings.ReplaceAll(name, "/", "__"), "_")
}

func updating() bool {
	if os.Getenv(UpdateEnv) == "1" {
		return true
	}
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(w), len(g)) {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "at line " + strconv.Itoa(i+1) + ":\n  want: " + wl + "\n  got:  " + gl
		}
	}
	return ""
}

// Explainer is a query keel can explain: a typed Query or a DynQuery.
type Explainer interface {
	Explain(ctx context.Context, db *keel.DB, opts keel.ExplainOptions) (*keel.Plan, error)
}

// UsesIndex fails the test unless the generic plan of q (the plan a prepared statement settles on,
// with its parameters unknown) reads index. Sequential scans are switched off for the plan, so a
// small test table cannot hide a predicate no index can serve, and a predicate that reaches a
// partial or expression index only through a parameter's value fails. ctx carries the scope the
// query runs under.
func UsesIndex(t testing.TB, ctx context.Context, db *keel.DB, q Explainer, index string) {
	t.Helper()
	var plan *keel.Plan
	err := keel.Read(ctx, db, func(ctx context.Context) error {
		var err error
		plan, err = q.Explain(ctx, db, keel.ExplainOptions{GenericPlan: true})
		return err
	}, keel.Setting("enable_seqscan", "off"))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !slices.Contains(plan.Indexes(), index) {
		t.Fatalf("the generic plan does not read %s; it reads %v and scans %v\n%s",
			index, plan.Indexes(), plan.SeqScans(), plan.JSON)
	}
}
