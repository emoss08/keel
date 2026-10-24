package export_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/analytics/table"
)

func writeJSON(t *testing.T, r table.Reader, opts export.Options) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := export.JSON(context.Background(), &buf, r, opts); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var runMeta = export.Meta{
	Title:       "Margin by lane",
	GeneratedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	RequestedBy: "dispatch@example.com",
	Params:      []export.Param{{Name: "window", Value: 30}, {Name: "customer", Value: "ACME"}},
}

func TestJSONEnvelope(t *testing.T) {
	out := writeJSON(t, sampleTable(t).Reader(), export.Options{Meta: runMeta, Location: chicago})
	var doc struct {
		Meta    map[string]any    `json:"meta"`
		Schema  []json.RawMessage `json:"schema"`
		Rows    [][]any           `json:"rows"`
		Levels  [][2]int          `json:"levels"`
		Totals  []any             `json:"totals"`
		Summary map[string]any    `json:"summary"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc.Meta["timezone"] != "America/Chicago" || doc.Meta["title"] != "Margin by lane" {
		t.Errorf("meta %v", doc.Meta)
	}
	params, _ := json.Marshal(doc.Meta["params"])
	if string(params) != `[{"name":"window","value":30},{"name":"customer","value":"ACME"}]` {
		t.Errorf("params in declared order: %s", params)
	}
	first, _ := json.Marshal(doc.Rows[0])
	if want := `["ACME, Inc.","InTransit","1082.0672643987443142",12,0.1834,"2026-03-04T01:30:00Z",true,1772587800,94200,{"a":1}]`; string(first) != want {
		t.Errorf("first row\n got %s\nwant %s", first, want)
	}
	nulls, _ := json.Marshal(doc.Rows[2])
	if string(nulls) != `["Globex",null,null,null,null,null,null,null,null,null]` {
		t.Errorf("null row %s", nulls)
	}
	if len(doc.Rows) != 4 || len(doc.Levels) != 1 || doc.Levels[0] != [2]int{3, 1} {
		t.Errorf("rows %d, levels %v", len(doc.Rows), doc.Levels)
	}
	if doc.Totals[2] != "1169.5672643987443142" {
		t.Errorf("totals %v", doc.Totals)
	}
	if doc.Summary["rowCount"] != float64(3) || doc.Summary["truncated"] != true || doc.Summary["truncatedBy"] != "rows" {
		t.Errorf("summary %v", doc.Summary)
	}
	if !strings.Contains(string(out), `"display":{"style":"currency","decimals":2`) {
		t.Errorf("the schema carries the display: %s", doc.Schema[2])
	}
}

func TestJSONRoundTrip(t *testing.T) {
	first := writeJSON(t, sampleTable(t).Reader(), export.Options{Meta: runMeta, Location: chicago})
	env, err := export.ReadJSON(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Title != runMeta.Title || !env.Meta.GeneratedAt.Equal(runMeta.GeneratedAt) || env.Location.String() != "America/Chicago" {
		t.Errorf("meta %+v %v", env.Meta, env.Location)
	}
	if env.RowCount != 3 || env.Truncated != export.TruncatedRows || !env.Table.Truncated {
		t.Errorf("summary %d %v", env.RowCount, env.Truncated)
	}
	if n := env.Table.Len(); n != 5 {
		t.Errorf("table holds %d rows", n)
	}
	second := writeJSON(t, env.Table.Reader(), export.Options{Meta: env.Meta, Location: env.Location})
	if !bytes.Equal(first, second) {
		t.Errorf("round trip changed the envelope:\n%s\n%s", first, second)
	}
}

func TestJSONStopsAtTheByteLimit(t *testing.T) {
	out := writeJSON(t, newGenReader(5000, 512), export.Options{MaxBytes: 8192})
	env, err := export.ReadJSON(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("a truncated envelope is still a document: %v", err)
	}
	if env.Truncated != export.TruncatedBytes || env.RowCount == 0 || env.RowCount >= 5000 || int64(env.Table.Len()) != env.RowCount {
		t.Errorf("truncated %v, %d rows, table %d", env.Truncated, env.RowCount, env.Table.Len())
	}
}

func TestReadJSONRefusesMalformedEnvelopes(t *testing.T) {
	cases := map[string]string{
		"no schema":     `{"meta":{"title":"x","generatedAt":"2026-01-01T00:00:00Z"},"rows":[]}`,
		"rows first":    `{"rows":[[1]],"schema":[{"id":"a","label":"A","type":"int","display":{}}]}`,
		"short row":     `{"schema":[{"id":"a","label":"A","type":"int","display":{}},{"id":"b","label":"B","type":"int","display":{}}],"rows":[[1]]}`,
		"wrong value":   `{"schema":[{"id":"a","label":"A","type":"int","display":{}}],"rows":[["x"]]}`,
		"empty schema":  `{"schema":[],"rows":[]}`,
		"bad level":     `{"schema":[{"id":"a","label":"A","type":"int","display":{}}],"rows":[[1]],"levels":[[5,1]]}`,
		"unknown type":  `{"schema":[{"id":"a","label":"A","type":"money","display":{}}],"rows":[]}`,
		"not an object": `[]`,
	}
	for name, doc := range cases {
		if _, err := export.ReadJSON(strings.NewReader(doc)); !errors.Is(err, export.ErrEnvelope) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
