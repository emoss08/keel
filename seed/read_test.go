package seed_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/seed"
)

// offlineDB returns a DB that never connects: pgxpool connects lazily.
func offlineDB(t *testing.T, codec *keel.JSONCodec) *keel.DB {
	t.Helper()
	cfg := testConfig("postgres://keel:keel@127.0.0.1:1/none?sslmode=disable")
	cfg.JSON = codec
	return must(keel.Open(context.Background(), cfg))(t)
}

func newRun(t *testing.T, db *keel.DB, fsys fstest.MapFS, paths ...string) *seed.Run {
	t.Helper()
	s := &seed.Seed{Name: "reader", Kind: seed.Reference, Inputs: seed.Files(fsys, paths...),
		Run: func(context.Context, *seed.Run) error { return nil }}
	return must(seed.NewRunForTest(db, s))(t)
}

type state struct {
	Code string `yaml:"abbreviation" json:"abbreviation"`
	Name string `yaml:"name" json:"name"`
	Pop  int    `yaml:"population" json:"population"`
}

func TestYAML(t *testing.T) {
	fsys := fstest.MapFS{
		"data/states.yaml": {Data: []byte("- abbreviation: TX\n  name: Texas\n  population: 31000000\n" +
			"- abbreviation: OH\n  name: Ohio\n")},
		"data/dup.yaml":   {Data: []byte("abbreviation: TX\nabbreviation: OH\n")},
		"data/other.yaml": {Data: []byte("[]")},
	}
	r := newRun(t, offlineDB(t, nil), fsys, "data/states.yaml", "data/dup.yaml")
	states := must(seed.YAML[[]state](r, "data/states.yaml"))(t)
	eq(t, len(states), 2)
	eq(t, states[0], state{Code: "TX", Name: "Texas", Pop: 31000000})
	eq(t, states[1].Name, "Ohio")

	_, err := seed.YAML[state](r, "data/dup.yaml")
	if err == nil || !strings.Contains(err.Error(), "data/dup.yaml") {
		t.Fatalf("duplicate key: got %v", err)
	}
	_, err = seed.YAML[[]state](r, "data/other.yaml")
	if !errors.Is(err, seed.ErrUndeclaredInput) {
		t.Fatalf("undeclared input: got %v", err)
	}
}

func TestJSONUsesTheDBCodec(t *testing.T) {
	calls := 0
	codec := &keel.JSONCodec{
		Marshal: json.Marshal,
		Unmarshal: func(b []byte, v any) error {
			calls++
			return json.Unmarshal(b, v)
		},
	}
	fsys := fstest.MapFS{"data/states.json": {Data: []byte(`[{"abbreviation":"CA","name":"California"}]`)}}
	r := newRun(t, offlineDB(t, codec), fsys, "data/states.json")
	states := must(seed.JSON[[]state](r, "data/states.json"))(t)
	eq(t, len(states), 1)
	eq(t, states[0].Code, "CA")
	eq(t, calls, 1)

	_, err := seed.JSON[[]state](r, "data/missing.json")
	if !errors.Is(err, seed.ErrUndeclaredInput) {
		t.Fatalf("undeclared input: got %v", err)
	}

	plain := newRun(t, offlineDB(t, nil), fsys, "data/states.json")
	states = must(seed.JSON[[]state](plain, "data/states.json"))(t)
	eq(t, states[0].Name, "California")
}

type level int

type base struct {
	ID string
}

type csvRow struct {
	base
	ProNumber string
	Count     int16
	Big       uint64
	Ratio     float64
	Hazmat    bool
	Seen      time.Time
	Day       time.Time
	Wait      time.Duration
	Level     level
	Weight    *int64
	Note      *string
	Addr      netip.Addr
	AddrPtr   *netip.Addr
	Renamed   string `csv:"label"`
	Skipped   string `csv:"-"`
	UserID    string
}

func TestCSVTypes(t *testing.T) {
	data := "\ufeffid,pro_number,count,big,ratio,hazmat,seen,day,wait,level,weight,note,addr,addr_ptr,label,user_id\n" +
		"a1,PRO-1,-12,18446744073709551615,0.25,true,2026-10-23T14:30:00Z,2026-10-23,90s,3,1200,hi,10.0.0.1,::1,L,u1\n" +
		"a2,PRO-2,,,,,,,,,,,,,,\n"
	fsys := fstest.MapFS{"rows.csv": {Data: []byte(data)}}
	r := newRun(t, offlineDB(t, nil), fsys, "rows.csv")
	rows := must(seed.CSV[csvRow](r, "rows.csv"))(t)
	eq(t, len(rows), 2)
	a := rows[0]
	eq(t, a.ID, "a1")
	eq(t, a.ProNumber, "PRO-1")
	eq(t, a.Count, int16(-12))
	eq(t, a.Big, uint64(18446744073709551615))
	eq(t, a.Ratio, 0.25)
	eq(t, a.Hazmat, true)
	eq(t, a.Seen, time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC))
	eq(t, a.Day, time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	eq(t, a.Wait, 90*time.Second)
	eq(t, a.Level, level(3))
	eq(t, *a.Weight, int64(1200))
	eq(t, *a.Note, "hi")
	eq(t, a.Addr, netip.MustParseAddr("10.0.0.1"))
	eq(t, *a.AddrPtr, netip.MustParseAddr("::1"))
	eq(t, a.Renamed, "L")
	eq(t, a.UserID, "u1")
	b := rows[1]
	eq(t, b.Count, int16(0))
	eq(t, b.Weight == nil, true)
	eq(t, b.Note == nil, true)
	eq(t, b.AddrPtr == nil, true)
	eq(t, b.Seen.IsZero(), true)
}

func TestCSVErrors(t *testing.T) {
	fsys := fstest.MapFS{
		"unknown.csv": {Data: []byte("id,colour\na,red\n")},
		"bad.csv":     {Data: []byte("id,count\na,1\nb,many\n")},
		"dup.csv":     {Data: []byte("id,id\na,b\n")},
		"empty.csv":   {Data: []byte("")},
	}
	r := newRun(t, offlineDB(t, nil), fsys, "unknown.csv", "bad.csv", "dup.csv", "empty.csv")

	_, err := seed.CSV[csvRow](r, "unknown.csv")
	if err == nil || !strings.Contains(err.Error(), `"colour"`) {
		t.Fatalf("unknown column: got %v", err)
	}
	_, err = seed.CSV[csvRow](r, "bad.csv")
	if err == nil || !strings.Contains(err.Error(), "bad.csv line 3") || !strings.Contains(err.Error(), `"count"`) {
		t.Fatalf("bad value: got %v", err)
	}
	_, err = seed.CSV[csvRow](r, "dup.csv")
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate column: got %v", err)
	}
	rows := must(seed.CSV[csvRow](r, "empty.csv"))(t)
	eq(t, len(rows), 0)
	_, err = seed.CSV[string](r, "bad.csv")
	if err == nil || !strings.Contains(err.Error(), "struct") {
		t.Fatalf("non-struct: got %v", err)
	}
	err = seed.CSVEach(r, "nope.csv", func(csvRow) error { return nil })
	if !errors.Is(err, seed.ErrUndeclaredInput) {
		t.Fatalf("undeclared input: got %v", err)
	}
	stop := errors.New("stop")
	n := 0
	err = seed.CSVEach(r, "bad.csv", func(csvRow) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("stop: got %v after %d rows", err, n)
	}
}

func TestGlobInputsAndOpen(t *testing.T) {
	fsys := fstest.MapFS{
		"data/a.csv":   {Data: []byte("x\n")},
		"data/b.csv":   {Data: []byte("y\n")},
		"data/c.yaml":  {Data: []byte("z\n")},
		"data/raw.txt": {Data: []byte("raw")},
	}
	r := newRun(t, offlineDB(t, nil), fsys, "data/*.csv", "data/raw.txt")
	got := r.Inputs()
	eq(t, strings.Join(got, ","), "data/a.csv,data/b.csv,data/raw.txt")
	b := must(r.ReadFile("data/raw.txt"))(t)
	eq(t, string(b), "raw")
	f := must(r.Open("data/b.csv"))(t)
	ok(t, f.Close())
	_, err := r.Open("data/c.yaml")
	if !errors.Is(err, seed.ErrUndeclaredInput) {
		t.Fatalf("undeclared input: got %v", err)
	}
}
