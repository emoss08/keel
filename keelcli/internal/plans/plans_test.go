package plans

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixture parses testdata/name.json.
func fixture(t *testing.T, name string) Node {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParse(t *testing.T) {
	n := fixture(t, "page_sorted")
	if n.Type != "Limit" || n.TotalCost != 1580.85 || n.PlanRows != 10 || len(n.Plans) != 1 {
		t.Fatalf("root %+v", n)
	}
	sort := n.Plans[0]
	if sort.Type != "Sort" || !slices.Equal(sort.SortKey, []string{"big.name"}) {
		t.Fatalf("sort %+v", sort)
	}
	scan := sort.Plans[0]
	if scan.Type != "Seq Scan" || scan.Relation != "big" || scan.Schema != "public" {
		t.Fatalf("scan %+v", scan)
	}

	for _, c := range []struct{ in, want string }{
		{"", "parse EXPLAIN output"},
		{"{}", "parse EXPLAIN output"},
		{"[]", "no plan"},
		{`[{"Planning": {}}]`, "no plan"},
	} {
		if _, err := Parse([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.in, err, c.want)
		}
	}
}

func TestSeqScans(t *testing.T) {
	cases := []struct {
		fixture string
		want    []Relation
	}{
		{"page_sorted", []Relation{{"public", "big"}}},
		{"index_scan", nil},
		{"parallel_page", []Relation{{"tms", "shipments"}}},
		{"incremental", nil},
		{"joins", []Relation{{"public", "settings"}, {"tms", "stops"}}},
		{"aggregate_page", nil},
	}
	for _, c := range cases {
		if got := fixture(t, c.fixture).SeqScans(); !slices.Equal(got, c.want) {
			t.Errorf("%s: seq scans %v, want %v", c.fixture, got, c.want)
		}
	}
	if s := (Relation{Name: "t"}).String(); s != "t" {
		t.Fatalf("relation without schema %q", s)
	}
}

func TestSortsUnderLimit(t *testing.T) {
	cases := []struct {
		fixture string
		keys    [][]string
	}{
		{"page_sorted", [][]string{{"big.name"}}},
		{"parallel_page", [][]string{{"shipments.created_at DESC", "shipments.id DESC"}}},
		{"incremental", nil},
		{"aggregate_page", nil},
		{"aggregate_ranked", [][]string{{"(sum(shipments.total_charge_amount)) DESC"}}},
		{"union_ranked", [][]string{{"shipments.updated_at DESC", "shipments.id DESC"}}},
		{"index_scan", nil},
		{"joins", nil},
	}
	for _, c := range cases {
		var keys [][]string
		for _, s := range fixture(t, c.fixture).SortsUnderLimit() {
			keys = append(keys, s.SortKey)
		}
		if !slices.EqualFunc(keys, c.keys, slices.Equal[[]string]) {
			t.Errorf("%s: sorts under a limit %v, want %v", c.fixture, keys, c.keys)
		}
	}
}

func TestRelations(t *testing.T) {
	sorts := fixture(t, "page_sorted").SortsUnderLimit()
	if len(sorts) != 1 || !slices.Equal(sorts[0].Relations(), []Relation{{"public", "big"}}) {
		t.Fatalf("sorts %+v", sorts)
	}
	if got := fixture(t, "joins").Relations(); !slices.Equal(got, []Relation{{"public", "settings"}, {"tms", "stops"}}) {
		t.Fatalf("relations %v", got)
	}
	ranked := fixture(t, "aggregate_ranked").SortsUnderLimit()
	if len(ranked) != 1 {
		t.Fatalf("ranked sorts %+v", ranked)
	}
	if rels, aggRows := ranked[0].SortInput(); len(rels) != 0 || aggRows != 300 {
		t.Fatalf("sort input of a ranked aggregate: %v %v", rels, aggRows)
	}
	union := fixture(t, "union_ranked").SortsUnderLimit()
	if len(union) != 1 {
		t.Fatalf("union sorts %+v", union)
	}
	if rels, rows := union[0].SortInput(); len(rels) != 0 || rows != 400 {
		t.Fatalf("sort input of a ranked union: %v %v", rels, rows)
	}
	if rels, aggRows := sorts[0].SortInput(); !slices.Equal(rels, []Relation{{"public", "big"}}) || aggRows != 0 {
		t.Fatalf("sort input of a sorted page: %v %v", rels, aggRows)
	}
	if rel, rows := largest(nil, nil); rel != (Relation{}) || rows != 0 {
		t.Fatalf("largest of none: %v %v", rel, rows)
	}
}

func TestCheck(t *testing.T) {
	tuples := map[Relation]float64{
		{"public", "big"}:      20000,
		{"public", "settings"}: -1,
		{"tms", "stops"}:       120000,
		{"tms", "shipments"}:   1000000,
	}
	kinds := func(fs []Finding) string {
		var out []string
		for _, f := range fs {
			k := string(f.Kind)
			if f.Relation != "" {
				k += ":" + f.Relation
			}
			out = append(out, k)
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		fixture string
		rules   Rules
		want    string
	}{
		{"page_sorted", Rules{SeqScanRows: 10000}, "seq-scan:public.big"},
		{"page_sorted", Rules{SeqScanRows: 20000}, ""},
		{"page_sorted", Rules{SeqScanRows: 10000, SortLimit: true}, "seq-scan:public.big sort-under-limit:public.big"},
		{"page_sorted", Rules{SeqScanRows: 20000, SortLimit: true}, ""},
		{"page_sorted", Rules{SeqScanRows: -1, SortLimit: true}, "seq-scan:public.big sort-under-limit:public.big"},
		{"page_sorted", Rules{SeqScanRows: 1e9, MaxCost: 1000}, "cost"},
		{"page_sorted", Rules{SeqScanRows: 1e9, MaxCost: 2000}, ""},
		{"index_scan", Rules{SeqScanRows: -1, MaxCost: 1, SortLimit: true}, "cost"},
		{"index_scan", Rules{SeqScanRows: -1, SortLimit: true}, ""},
		{"parallel_page", Rules{SeqScanRows: 10000, SortLimit: true}, "seq-scan:tms.shipments sort-under-limit:tms.shipments"},
		{"parallel_page", Rules{SeqScanRows: 2e6, SortLimit: true}, ""},
		{"incremental", Rules{SeqScanRows: -1, SortLimit: true}, ""},
		{"joins", Rules{SeqScanRows: 10000}, "seq-scan:tms.stops"},
		{"joins", Rules{SeqScanRows: -1}, "seq-scan:public.settings seq-scan:tms.stops"},
		{"aggregate_page", Rules{SeqScanRows: -1, SortLimit: true}, ""},
		{"aggregate_ranked", Rules{SeqScanRows: 10000, SortLimit: true}, ""},
		{"aggregate_ranked_wide", Rules{SeqScanRows: 10000, SortLimit: true}, "sort-under-limit"},
		{"union_ranked", Rules{SeqScanRows: 10000, SortLimit: true}, ""},
	}
	for _, c := range cases {
		if got := kinds(Check(fixture(t, c.fixture), c.rules, tuples)); got != c.want {
			t.Errorf("%s %+v: findings %q, want %q", c.fixture, c.rules, got, c.want)
		}
	}

	fs := Check(fixture(t, "page_sorted"), Rules{SeqScanRows: 0, MaxCost: 1000, SortLimit: true}, tuples)
	want := []string{
		"sequential scan of public.big (about 20,000 rows)",
		"total cost 1580.85 over 1000",
		"sort by big.name under a limit over public.big (about 20,000 rows): the page is sorted in full, no index gives its order",
	}
	if len(fs) != len(want) {
		t.Fatalf("findings %+v", fs)
	}
	for i, f := range fs {
		if f.Detail != want[i] {
			t.Errorf("finding %d: %q, want %q", i, f.Detail, want[i])
		}
	}
	if fs[0].Rows != 20000 || fs[1].Cost != 1580.85 || !slices.Equal(fs[2].SortKey, []string{"big.name"}) ||
		fs[2].Relation != "public.big" || fs[2].Rows != 20000 {
		t.Fatalf("finding fields %+v", fs)
	}

	unknown := Check(fixture(t, "page_sorted"), Rules{SeqScanRows: 0, SortLimit: true}, nil)
	if len(unknown) != 0 {
		t.Fatalf("a relation of unknown size counted as non-empty: %+v", unknown)
	}
}

func TestSingleStatement(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT 1", true},
		{"SELECT 1;", true},
		{"SELECT 1;  \n", true},
		{"SELECT 1; DROP TABLE t", false},
		{"SELECT 1;SELECT 2;", false},
		{"SELECT ';' FROM t", true},
		{"SELECT 'it''s; fine' FROM t", true},
		{`SELECT E'a\'; b' FROM t`, true},
		{`SELECT E'a\\'; DROP TABLE t`, false},
		{`SELECT "odd;name" FROM t`, true},
		{`SELECT "a""b;" FROM t; DELETE FROM t`, false},
		{"SELECT 1 -- trailing; comment\nFROM t", true},
		{"SELECT 1 -- comment at the end;", true},
		{"SELECT /* a; /* nested; */ still; */ 1", true},
		{"SELECT /* never closed; ", false},
		{"SELECT $$a;b$$, $tag$ x; $$ y $tag$ FROM t", true},
		{"SELECT $1, $2 FROM t WHERE a = $1; DROP TABLE t", false},
		{"SELECT $1::int; ", true},
		{"SELECT a$b FROM t", true},
		{"SELECT $$never closed;", false},
		{"SELECT 'never closed;", false},
	}
	for _, c := range cases {
		if got := SingleStatement(c.sql); got != c.want {
			t.Errorf("%q: %v, want %v", c.sql, got, c.want)
		}
	}
}
