package keel_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// observe sends e through obs as keel would.
func observe(obs keel.Observer, e keel.QueryEvent) {
	ctx := obs.BeforeQuery(context.Background(), &e)
	obs.AfterQuery(ctx, &e)
}

func TestRecordShapesWritesEachPlannableTextOnce(t *testing.T) {
	var buf bytes.Buffer
	obs := keel.RecordShapes(&buf)
	for _, e := range []keel.QueryEvent{
		{Op: "select", Table: "t", SQL: "SELECT a FROM t WHERE b = $1", Args: 1},
		{Op: "select", Table: "t", SQL: "SELECT a FROM t WHERE b = $1", Args: 1},
		{Op: "savepoint", SQL: "SAVEPOINT keel_sp_1"},
		{Op: "lock", SQL: "SELECT pg_advisory_xact_lock($1)", Args: 1},
		{Op: "explain", Table: "explain", SQL: "EXPLAIN (FORMAT JSON) SELECT 1"},
		{Op: "exec", SQL: "CREATE TABLE x (id int)"},
		{Op: "copy", Table: "t", SQL: "COPY t (a) FROM STDIN"},
		{Op: "query", SQL: "  /* set */ -- operation\n (SELECT 1) UNION (SELECT 2)"},
		{Op: "update", Table: "t", SQL: "WITH x AS (SELECT 1) UPDATE t SET a = $1 WHERE a < 3 AND b <> '&'", Args: 1},
		{Op: "insert", Table: "t", SQL: "insert into t (a) values ($1)", Args: 1},
		{Op: "select", Table: "t", SQL: "SELECT a FROM t WHERE b = $1", Args: 1},
	} {
		observe(obs, e)
	}
	want := `{"sql":"SELECT a FROM t WHERE b = $1","op":"select","table":"t","args":1}
{"sql":"  /* set */ -- operation\n (SELECT 1) UNION (SELECT 2)","op":"query","table":"","args":0}
{"sql":"WITH x AS (SELECT 1) UPDATE t SET a = $1 WHERE a < 3 AND b <> '&'","op":"update","table":"t","args":1}
{"sql":"insert into t (a) values ($1)","op":"insert","table":"t","args":1}
`
	if buf.String() != want {
		t.Fatalf("recorded:\n%s\nwant:\n%s", buf.String(), want)
	}
	shapes := must(keel.ReadShapes(bytes.NewReader(buf.Bytes())))(t)
	eq(t, len(shapes), 4)
	eq(t, shapes[0], keel.Shape{SQL: "SELECT a FROM t WHERE b = $1", Op: "select", Table: "t", Args: 1})
	eq(t, shapes[3].Op, "insert")
}

func TestRecordShapesConcurrent(t *testing.T) {
	var buf bytes.Buffer
	obs := keel.RecordShapes(&buf)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 100 {
				observe(obs, keel.QueryEvent{Op: "select", Table: "t", SQL: fmt.Sprintf("SELECT %d FROM t", (i+g)%100)})
			}
		})
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	eq(t, len(lines), 100)
	shapes := must(keel.ReadShapes(&buf))(t)
	eq(t, len(shapes), 100)
}

func TestRecordShapesFileAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shapes.jsonl")
	first, closeFirst, err := keel.RecordShapesFile(path)
	ok(t, err)
	second, closeSecond, err := keel.RecordShapesFile(path)
	ok(t, err)
	observe(first, keel.QueryEvent{Op: "select", Table: "a", SQL: "SELECT 1 FROM a"})
	observe(second, keel.QueryEvent{Op: "select", Table: "a", SQL: "SELECT 1 FROM a"})
	observe(second, keel.QueryEvent{Op: "delete", Table: "b", SQL: "DELETE FROM b WHERE id = $1", Args: 1})
	observe(first, keel.QueryEvent{Op: "update", Table: "c", SQL: "UPDATE c SET n = n + 1"})
	ok(t, closeFirst())
	ok(t, closeSecond())

	raw := must(os.ReadFile(path))(t)
	eq(t, strings.Count(string(raw), "\n"), 4)
	f := must(os.Open(path))(t)
	defer f.Close()
	shapes := must(keel.ReadShapes(f))(t)
	got := make([]string, len(shapes))
	for i, s := range shapes {
		got[i] = s.SQL
	}
	eq(t, strings.Join(got, " | "), "SELECT 1 FROM a | DELETE FROM b WHERE id = $1 | UPDATE c SET n = n + 1")

	if _, _, err := keel.RecordShapesFile(filepath.Join(t.TempDir(), "missing", "shapes.jsonl")); err == nil {
		t.Fatal("RecordShapesFile opened a file in a missing directory")
	}
}

func TestReadShapes(t *testing.T) {
	shapes := must(keel.ReadShapes(strings.NewReader("\n" +
		`{"sql":"SELECT 1","op":"select","table":"t","args":0}` + "\n\n" +
		`{"sql":"SELECT 1","op":"select","table":"t","args":0}` + "\n" +
		`{"sql":"SELECT 2","op":"select","table":"u","args":2}`)))(t)
	eq(t, len(shapes), 2)
	eq(t, shapes[1], keel.Shape{SQL: "SELECT 2", Op: "select", Table: "u", Args: 2})

	empty := must(keel.ReadShapes(strings.NewReader("")))(t)
	eq(t, len(empty), 0)

	for _, c := range []struct{ in, want string }{
		{`{"sql":"SELECT 1"}` + "\n" + `{"sql":`, "shape 2"},
		{`{"op":"select"}`, "shape 1 has no sql"},
		{`not json`, "shape 1"},
	} {
		_, err := keel.ReadShapes(strings.NewReader(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want an error naming %q", c.in, err, c.want)
		}
	}
}

func TestRecordShapesFromStatements(t *testing.T) {
	var buf bytes.Buffer
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{keel.RecordShapes(&buf)} })
	ctx := newTenant()
	c := fleetdb.Customers

	for i := range 3 {
		cust := &fleet.Customer{Name: "Shaped", Code: fmt.Sprintf("SH%d", i)}
		ok(t, c.Create(ctx, d, cust))
		must(c.Get(ctx, d, cust.ID))(t)
	}
	must(c.Where(c.Code.Eq("SH1")).List(ctx, d))(t)
	must(c.Where(c.Code.Eq("SH2")).List(ctx, d))(t)
	must(c.Where(c.Code.Eq("SH1")).Explain(ctx, d, keel.ExplainOptions{}))(t)
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		_, err := keel.Exec(ctx, d, "SAVEPOINT shapes_test")
		return err
	}))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	shapes := must(keel.ReadShapes(bytes.NewReader(buf.Bytes())))(t)
	eq(t, len(shapes), len(lines))
	ops := map[string]int{}
	for _, s := range shapes {
		if s.Table == "customers" {
			ops[s.Op]++
		}
		if strings.HasPrefix(s.SQL, "EXPLAIN") || strings.HasPrefix(s.SQL, "SAVEPOINT") {
			t.Fatalf("recorded a statement EXPLAIN cannot plan: %s", s.SQL)
		}
		if s.Args == 0 && s.Op == "insert" {
			t.Fatalf("insert shape without arguments: %+v", s)
		}
	}
	eq(t, ops["insert"], 1)
	if ops["select"] < 2 {
		t.Fatalf("want the Get and the List shapes, got %d select shapes:\n%s", ops["select"], buf.String())
	}
	list, _, err := c.Where(c.Code.Eq("SH9")).ToSQL(ctx, d)
	ok(t, err)
	found := false
	for _, s := range shapes {
		found = found || s.SQL == list
	}
	if !found {
		t.Fatalf("the List shape %q was not recorded:\n%s", list, buf.String())
	}
}
