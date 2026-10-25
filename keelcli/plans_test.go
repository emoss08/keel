package keelcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/keelcli/internal/plans"
)

// writeShapes writes shapes to a file as keel.RecordShapes writes them and returns its path.
func writeShapes(t *testing.T, shapes ...keel.Shape) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shapes.jsonl")
	obs, closeFn, err := keel.RecordShapesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range shapes {
		e := keel.QueryEvent{SQL: s.SQL, Op: s.Op, Table: s.Table, Args: s.Args}
		obs.AfterQuery(obs.BeforeQuery(t.Context(), &e), &e)
	}
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	return path
}

// plansSample is a migrated sample database with a 20,000-row analyzed table, big, keyed on id
// and with no index on name.
func plansSample(t *testing.T) *sample {
	t.Helper()
	s := newSample(t)
	if v := dbScalar[int](t, s.dsn, "SELECT current_setting('server_version_num')::int"); v < plans.MinServerVersion {
		t.Skipf("EXPLAIN (GENERIC_PLAN) needs PostgreSQL 16, the server is %d", v)
	}
	s.run(t, "migrate", "up", "-plain").expect(t, ExitOK)
	dbExec(t, s.dsn, `CREATE TABLE big AS SELECT g AS id, md5(g::text) AS name FROM generate_series(1, 20000) AS g;
ALTER TABLE big ADD PRIMARY KEY (id);
ANALYZE big`)
	return s
}

func TestPlansCommand(t *testing.T) {
	s := plansSample(t)
	byID := keel.Shape{SQL: "SELECT id, name FROM big WHERE id = $1", Op: "select", Table: "big", Args: 1}
	byName := keel.Shape{SQL: "SELECT id, name FROM big WHERE name = $1", Op: "select", Table: "big", Args: 1}
	page := keel.Shape{SQL: "SELECT id, name FROM big ORDER BY name LIMIT $1", Op: "select", Table: "big", Args: 1}
	keyPage := keel.Shape{SQL: "SELECT id, name FROM big WHERE id > $1 ORDER BY id LIMIT $2", Op: "select", Table: "big", Args: 2}
	missing := keel.Shape{SQL: "SELECT nope FROM missing_table WHERE id = $1", Op: "select", Table: "missing_table", Args: 1}
	update := keel.Shape{SQL: "UPDATE big SET name = $2 WHERE id = $1", Op: "update", Table: "big", Args: 2}

	clean := writeShapes(t, byID, keyPage, update)
	help := s.run(t, "help")
	help.expect(t, ExitOK, "plans", "explain recorded statement shapes")
	s.run(t, "plans", "-plain", "-shapes", clean).expect(t, ExitOK, "No findings: 3 shapes planned.")

	all := writeShapes(t, byID, byName, page, keyPage, missing, update)
	r := s.run(t, "plans", "-plain", "-shapes", all)
	r.expect(t, ExitProblems,
		"findings select big",
		"SELECT id, name FROM big WHERE name = $1",
		"seq-scan sequential scan of public.big (about 20,000 rows)",
		"unplanned select missing_table",
		`relation "missing_table" does not exist (SQLSTATE 42P01)`,
		"2 findings in 2 of 5 shapes planned; 1 shape could not be planned.",
		"keelcli: plans found 2 findings")
	r.lacks(t, "sort-under-limit", "SELECT id, name FROM big WHERE id = $1", "UPDATE big")

	s.run(t, "plans", "-plain", "-sort-limit", "-shapes", all).expect(t, ExitProblems,
		"sort-under-limit sort by big.name under a limit", "3 findings in 2 of 5 shapes planned")
	s.run(t, "plans", "-plain", "-seq-scan-rows", "50000", "-shapes", all).
		expect(t, ExitOK, "No findings: 5 shapes planned; 1 shape could not be planned.")
	s.run(t, "plans", "-plain", "-seq-scan-rows", "50000", "-max-cost", "0.01", "-shapes", clean).
		expect(t, ExitProblems, "cost total cost ", " over 0.01")

	var doc struct {
		Shapes, Findings, Unplanned int
		Reports                     []struct {
			SQL, Op, Table, Error string
			Args                  int
			Cost, Rows            float64
			Findings              []struct {
				Kind, Relation, Detail string
				Rows                   float64
			}
		}
	}
	out := s.run(t, "plans", "-json", "-shapes", all)
	if out.code != ExitProblems {
		t.Fatalf("exit %d:\n%s%s", out.code, out.stdout, out.stderr)
	}
	jsonDoc(t, out.stdout, &doc)
	if doc.Shapes != 6 || doc.Findings != 2 || doc.Unplanned != 1 || len(doc.Reports) != 6 {
		t.Fatalf("json %+v", doc)
	}
	name := doc.Reports[1]
	if name.SQL != byName.SQL || name.Op != "select" || name.Table != "big" || name.Args != 1 || name.Cost <= 0 ||
		len(name.Findings) != 1 || name.Findings[0].Kind != "seq-scan" || name.Findings[0].Relation != "public.big" ||
		name.Findings[0].Rows != 20000 {
		t.Fatalf("report %+v", name)
	}
	if doc.Reports[0].Findings == nil || len(doc.Reports[0].Findings) != 0 || doc.Reports[4].Error == "" {
		t.Fatalf("reports %+v", doc.Reports)
	}
}

func TestPlansCommandInput(t *testing.T) {
	s := plansSample(t)
	s.run(t, "plans", "-plain").expect(t, ExitUsage, "plans needs -shapes")
	s.run(t, "plans", "-plain", "-shapes", filepath.Join(t.TempDir(), "none.jsonl")).
		expect(t, ExitFailure, "keelcli: plans: open ")
	s.run(t, "plans", "-plain", "-shapes", writeShapes(t), "extra").expect(t, ExitUsage, "plans takes no arguments")

	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.run(t, "plans", "-plain", "-shapes", empty).expect(t, ExitOK, "No shapes to plan.")

	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(bad, []byte(`{"sql":"SELECT 1"}`+"\n{"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.run(t, "plans", "-plain", "-shapes", bad).expect(t, ExitFailure, "read shapes: shape 2")

	two := writeShapes(t, keel.Shape{SQL: "SELECT 1; DELETE FROM big", Op: "query"})
	s.run(t, "plans", "-plain", "-shapes", two).
		expect(t, ExitOK, "the shape holds more than one statement", "1 shape could not be planned")
	if n := dbScalar[int](t, s.dsn, "SELECT count(*) FROM big"); n != 20000 {
		t.Fatalf("a shape with two statements ran: %d rows left", n)
	}

	stdin := runApp(t, s.app, false, strings.NewReader(`{"sql":"SELECT id FROM big WHERE id = $1","op":"select","table":"big","args":1}`),
		"plans", "-plain", "-shapes", "-")
	stdin.expect(t, ExitOK, "No findings: 1 shape planned.")
}
