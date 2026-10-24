package ir

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics/formula"
)

type fakeEntity struct {
	fields map[string]FieldInfo
	edges  map[string]EdgeInfo
}

type fakeCatalog map[string]fakeEntity

func (c fakeCatalog) HasEntity(key string) bool { _, ok := c[key]; return ok }

func (c fakeCatalog) EntityKeys() []string { return sortedKeys(c) }

func (c fakeCatalog) Field(entity, key string) (FieldInfo, bool) {
	f, ok := c[entity].fields[key]
	return f, ok
}

func (c fakeCatalog) FieldKeys(entity string) []string { return sortedKeys(c[entity].fields) }

func (c fakeCatalog) Edge(entity, name string) (EdgeInfo, bool) {
	e, ok := c[entity].edges[name]
	return e, ok
}

func (c fakeCatalog) EdgeNames(entity string) []string { return sortedKeys(c[entity].edges) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

var (
	numericAggs = []Agg{AggCount, AggCountDistinct, AggSum, AggAvg, AggMin, AggMax}
	orderedAggs = []Agg{AggCount, AggCountDistinct, AggMin, AggMax}
	plainAggs   = []Agg{AggCount, AggCountDistinct}
)

func dim(t formula.Type, aggs []Agg) FieldInfo {
	return FieldInfo{Type: t, Aggs: aggs, Filterable: true, Groupable: true}
}

func measureField(t formula.Type) FieldInfo {
	return FieldInfo{Type: t, Aggs: numericAggs, Filterable: true}
}

func catalog() fakeCatalog {
	return fakeCatalog{
		"shipment": {
			fields: map[string]FieldInfo{
				"id":          dim(formula.String, plainAggs),
				"proNumber":   dim(formula.String, orderedAggs),
				"status":      {Type: formula.Enum, Aggs: plainAggs, Filterable: true, Groupable: true, Enum: []string{"New", "InTransit", "Completed", "Canceled"}},
				"totalCharge": measureField(formula.Decimal),
				"weight":      measureField(formula.Int),
				"pieces":      dim(formula.Int, numericAggs),
				"shippedAt":   dim(formula.Epoch, orderedAggs),
				"deliveredOn": dim(formula.Date, orderedAggs),
				"pickedUpAt":  dim(formula.Timestamp, orderedAggs),
				"hazmat":      dim(formula.Bool, plainAggs),
				"customerId":  dim(formula.Ref, plainAggs),
				"meta":        {Type: formula.JSON},
				"notes":       {Type: formula.String, Aggs: plainAggs},
				"lat":         measureField(formula.Float),
				"lng":         measureField(formula.Float),
			},
			edges: map[string]EdgeInfo{
				"customer": {Target: "customer"},
				"moves":    {Target: "move", ToMany: true},
			},
		},
		"customer": {
			fields: map[string]FieldInfo{
				"id":   dim(formula.String, plainAggs),
				"name": dim(formula.String, orderedAggs),
				"code": dim(formula.String, orderedAggs),
				"tier": {Type: formula.Enum, Aggs: plainAggs, Filterable: true, Groupable: true, Enum: []string{"Gold", "Silver"}},
			},
			edges: map[string]EdgeInfo{"shipments": {Target: "shipment", ToMany: true}},
		},
		"move": {
			fields: map[string]FieldInfo{
				"id":       dim(formula.String, plainAggs),
				"distance": measureField(formula.Decimal),
				"loaded":   dim(formula.Bool, plainAggs),
				"sequence": dim(formula.Int, numericAggs),
			},
			edges: map[string]EdgeInfo{"stops": {Target: "stop", ToMany: true}},
		},
		"stop": {
			fields: map[string]FieldInfo{
				"city":      dim(formula.String, orderedAggs),
				"arrivedAt": dim(formula.Timestamp, orderedAggs),
			},
		},
	}
}

const designExample = `{
  "version": 2,
  "entity": "shipment",
  "columns": [
    {"id": "customer", "dimension": {"field": "customer.name"}},
    {"id": "month", "dimension": {"field": "shippedAt", "bucket": "month"}},
    {"id": "revenue", "measure": {"field": "totalCharge", "agg": "sum"}},
    {"id": "miles", "measure": {"field": "moves.distance", "agg": "sum"}},
    {"id": "rpm", "calc": "round([revenue] / nullif([miles], 0), 2)", "format": "money"},
    {"id": "share", "window": {"of": "revenue", "fn": "percent_of_total", "partition": ["month"]}},
    {"id": "mom", "window": {"of": "revenue", "fn": "change", "over": "month", "offset": 1}}
  ],
  "filters": {"and": [
    {"field": "shippedAt", "op": "last_n_days", "param": "window"},
    {"field": "status", "op": "not_in", "value": ["Canceled"]}
  ]},
  "having": {"and": [{"column": "revenue", "op": "gt", "value": 1000}]},
  "groupings": {"subtotals": ["customer"], "total": true},
  "top": {"n": 5, "by": "revenue", "per": ["month"]},
  "sort": [{"column": "revenue", "desc": true}],
  "limit": 500,
  "parameters": [{"name": "window", "type": "int", "default": 30}]
}`

func decode(t *testing.T, src string) *Definition {
	t.Helper()
	def, err := Decode([]byte(src))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return def
}

func TestDesignExampleDecodesValidatesAndResolves(t *testing.T) {
	def := decode(t, designExample)
	if err := Validate(def, Limits{}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	r, err := Resolve(def, catalog(), Limits{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := map[string]Output{
		"customer": {ID: "customer", Type: formula.String, Level: formula.Row},
		"month":    {ID: "month", Type: formula.Epoch, Level: formula.Row},
		"revenue":  {ID: "revenue", Type: formula.Decimal, Level: formula.Aggregate},
		"miles":    {ID: "miles", Type: formula.Decimal, Level: formula.Aggregate},
		"rpm":      {ID: "rpm", Type: formula.Decimal, Level: formula.Aggregate},
		"share":    {ID: "share", Type: formula.Decimal, Level: formula.Window},
		"mom":      {ID: "mom", Type: formula.Decimal, Level: formula.Window},
	}
	for id, w := range want {
		got, ok := r.Output(id)
		if !ok || got != w {
			t.Errorf("output %s = %+v, want %+v", id, got, w)
		}
	}
	if !r.Aggregated || r.Fields["moves.distance"] == nil || !r.Fields["moves.distance"].ToMany || r.Fields["customer.name"].Entity != "customer" {
		t.Errorf("resolved fields: %+v", r.Fields)
	}
	if r.Calcs["rpm"] == nil || r.Calcs["rpm"].Func.Name != "round" {
		t.Errorf("checked calc: %+v", r.Calcs["rpm"])
	}
	if _, ok := r.Output("nope"); ok {
		t.Error("unknown output")
	}
}

func TestJSONRoundTripIsStable(t *testing.T) {
	def := decode(t, designExample)
	out, err := Encode(def)
	if err != nil {
		t.Fatal(err)
	}
	again := decode(t, string(out))
	out2, _ := Encode(again)
	if string(out) != string(out2) {
		t.Fatalf("encoding is not stable:\n%s\n%s", out, out2)
	}
	for _, want := range []string{
		`"calc": "round([revenue] / nullif([miles], 0), 2)"`,
		`"bucket": "month"`,
		`"limit": 500`,
		`{"field": "status", "op": "not_in", "value": ["Canceled"]}`,
	} {
		compact := strings.Join(strings.Fields(string(out)), " ")
		compact = strings.NewReplacer("{ ", "{", " }", "}", "[ ", "[", " ]", "]").Replace(compact)
		if !strings.Contains(compact, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("encoding lacks %s:\n%s", want, out)
		}
	}
	if v, ok := again.Filters.And[0].Value.(json.Number); ok || again.Parameters[0].Default != json.Number("30") {
		t.Errorf("numbers decode exactly: %T %v", v, again.Parameters[0].Default)
	}
}

func TestParamShapesRoundTrip(t *testing.T) {
	src := `{"version":2,"entity":"shipment","columns":[
	  {"id":"d","dimension":{"field":"shippedAt","bucket":{"param":"grain"}}},
	  {"id":"r","measure":{"agg":"count"}},
	  {"id":"r2","measure":{"agg":"sum","field":"weight"}},
	  {"id":"ast","calc":{"op":"*","args":[{"column":"r2"},{"number":"2"}]}}],
	  "pivot":{"field":"status","values":{"param":"statuses"},"columns":["r"]},
	  "top":{"n":{"param":"n"},"by":"r2"},
	  "limit":{"param":"n"},
	  "page":{"size":50,"after":"abc"},
	  "parameters":[{"name":"grain","type":"string","default":"month","allowed":["day","month"]},
	    {"name":"statuses","type":"enum","multi":true},{"name":"n","type":"int","required":true}]}`
	def := decode(t, src)
	if def.Columns[0].Dimension.Bucket != GrainParam("grain") || def.Limit != CountParam("n") || def.Pivot.Values.Param != "statuses" || def.Columns[3].Calc.Node == nil {
		t.Fatalf("param shapes: %+v", def)
	}
	if err := Validate(def, Limits{}); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(def)
	for _, want := range []string{`"bucket":{"param":"grain"}`, `"limit":{"param":"n"}`, `"values":{"param":"statuses"}`, `"n":{"param":"n"}`, `"calc":{"op":"*"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := map[string]string{
		`[]`:                                     "not a JSON object",
		`{"entity":"x"}`:                         "has no version",
		`{"version":9}`:                          "version 9 is not supported",
		`{"version":2,"entity":"x","colums":[]}`: `unknown field "colums"`,
		`{"version":2,"entity":"x","columns":[],"filters":{"and":[],"field":"x"}}`:               "group",
		`{"version":2,"entity":"x","columns":[],"limit":"x"}`:                                    "whole number",
		`{"version":2,"entity":"x","columns":[{"id":"a","dimension":{"field":"f","bucket":3}}]}`: "bucket name",
		`{"version":2,"entity":"x","columns":[],"pivot":{"field":"f","values":3,"columns":[]}}`:  "array of values",
		`{"version":2,"entity":"x","columns":[{"id":"a","calc":7}]}`:                             "formula",
		`{"version":2,"entity":"x","columns":[],"limit":{"param":"n","x":1}}`:                    "unknown field",
		`{"version":2,"entity":"x","columns":[]} {}`:                                             "not a JSON object",
	}
	for src, want := range cases {
		_, err := Decode([]byte(src))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", src, err, want)
		}
		var es Errors
		if err != nil && !errors.As(err, &es) {
			t.Errorf("%s: error is %T, want Errors", src, err)
		}
	}
}

func TestFilterHelpers(t *testing.T) {
	f := And(Where("status", OpEq, "New"), *Or(WhereParam("shippedAt", OpLastNDays, "w"), Filter{Calc: formula.Of("{hazmat}")}), *And())
	count := 0
	f.Walk(func(*Filter) { count++ })
	if count != 3 || f.Empty() || !And(*And()).Empty() || !(*Filter)(nil).Empty() {
		t.Errorf("walk/empty: %d", count)
	}
	c := f.Clone()
	c.And[0].Value = "Changed"
	c.And[1].Or[0].Param = "x"
	if f.And[0].Value != "New" || f.And[1].Or[0].Param != "w" {
		t.Error("clone shares nodes")
	}
	out, _ := json.Marshal(f)
	want := `{"and":[{"field":"status","op":"eq","value":"New"},{"or":[{"field":"shippedAt","op":"last_n_days","param":"w"},{"calc":"{hazmat}"}]},{"and":[]}]}`
	if string(out) != want {
		t.Errorf("filter JSON\n%s\nwant\n%s", out, want)
	}
}

func TestSchemaIsEmbedded(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal(Schema(), &s); err != nil || s["$ref"] != "#/$defs/Definition" {
		t.Fatalf("schema: %v", err)
	}
}

func TestOperatorTable(t *testing.T) {
	cases := map[formula.Type][]Op{
		formula.Bool: {OpEq, OpNe, OpIsNull, OpIsNotNull},
		formula.JSON: {OpIsNull, OpIsNotNull},
		formula.Enum: {OpEq, OpNe, OpIn, OpNotIn, OpIsNull, OpIsNotNull},
	}
	for typ, want := range cases {
		if got := OpsFor(typ); !slices.Equal(got, want) {
			t.Errorf("OpsFor(%s) = %v", typ, got)
		}
	}
	if !opAllowed(OpLastNDays, formula.Epoch) || opAllowed(OpLastNDays, formula.Int) || opAllowed(OpContains, formula.Enum) || !opAllowed(OpBetween, formula.Decimal) {
		t.Error("operator rules")
	}
	if len(OpsFor(formula.Unknown)) != len(Ops()) || len(OpTable()) != len(formula.Types()) {
		t.Error("unknown types accept every operator")
	}
	if _, ok := Op("nope").Info(); ok {
		t.Error("unknown op has info")
	}
}

func TestEnumsAndRules(t *testing.T) {
	if TransformRound.ResultType(formula.Int) != formula.Decimal || TransformAbs.ResultType(formula.Int) != formula.Int || TransformUpper.ResultType(formula.Enum) != formula.String {
		t.Error("transform result types")
	}
	kpi := chartRule(ChartKPI)
	if kpi.Dimension || !kpi.SingleSeries || chartRule(ChartBar).SingleSeries || !chartRule(ChartMap).Coordinates || !chartRule(ChartArea).Stacking {
		t.Error("chart rules")
	}
	if !(&Band{Width: 5}).whole() || (&Band{Edges: []float64{0, 1.5}}).whole() {
		t.Error("whole bands")
	}
}
