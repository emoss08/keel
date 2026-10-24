package ir

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics/formula"
)

func drillBase() *Definition {
	return &Definition{
		Version: Version,
		Entity:  "shipment",
		Columns: []Column{
			{ID: "customer", Label: "Customer", Dimension: &Dimension{Field: "customer.name"}},
			{ID: "month", Dimension: &Dimension{Field: "shippedAt", Bucket: Every(BucketMonth)}},
			{ID: "day", Dimension: &Dimension{Field: "deliveredOn", Bucket: Every(BucketDay)}},
			{ID: "week", Dimension: &Dimension{Field: "pickedUpAt", Bucket: GrainParam("grain")}},
			{ID: "charge", Dimension: &Dimension{Field: "totalCharge", Band: &Band{Edges: []float64{0, 1000, 5000}}}},
			{ID: "pcs", Dimension: &Dimension{Field: "pieces", Band: &Band{Width: 10}}},
			{ID: "code", Transform: &Transform{Op: TransformUpper}, Dimension: &Dimension{Field: "customer.code"}},
			{ID: "late", Calc: formula.Of("date_diff('day', {pickedUpAt}, {deliveredOn}) > 2")},
			{ID: "revenue", Measure: &Measure{Field: "totalCharge", Agg: AggSum, Filter: And(Where("hazmat", OpEq, true))}},
			{ID: "loads", Measure: &Measure{Agg: AggCount}},
			{ID: "share", Window: &Window{Of: "revenue", Fn: WindowPercentOfTotal}},
		},
		Filters:    And(Where("status", OpNotIn, []any{"Canceled"})),
		Parameters: []Parameter{{Name: "grain", Type: formula.String, Default: "week", Allowed: []any{"day", "week"}}},
		Pivot:      &Pivot{Field: "status", Values: Values{List: []string{"New", "Completed"}}, Columns: []string{"loads"}, Other: true},
	}
}

func recordColumns() []Column {
	return []Column{{ID: "pro", Dimension: &Dimension{Field: "proNumber"}}}
}

func drill(t *testing.T, req DrillRequest) *Definition {
	t.Helper()
	if req.Columns == nil {
		req.Columns = recordColumns()
	}
	out, err := Drill(drillBase(), req, CatalogTypes(catalog(), "shipment"))
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if err := Validate(out, Limits{}); err != nil {
		t.Fatalf("drilled definition is invalid: %v", err)
	}
	return out
}

func conditions(d *Definition) []Filter { return d.Filters.And }

func TestDrillInvertsBucketsInTheReportTimeZone(t *testing.T) {
	chicago, _ := time.LoadLocation("America/Chicago")
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, chicago)
	out := drill(t, DrillRequest{Location: chicago, Row: []DrillValue{
		{Column: "month", Value: start.Unix()},
		{Column: "day", Value: time.Date(2026, 3, 8, 0, 0, 0, 0, chicago).Format(time.RFC3339)},
		{Column: "week", Value: json.Number("1772949600")},
	}})
	got := conditions(out)
	want := []Filter{
		*And(Where("status", OpNotIn, []any{"Canceled"})),
		{Field: "shippedAt", Op: OpGte, Value: start.Unix()},
		{Field: "shippedAt", Op: OpLt, Value: time.Date(2026, 4, 1, 0, 0, 0, 0, chicago).Unix()},
		{Field: "deliveredOn", Op: OpGte, Value: "2026-03-08"},
		{Field: "deliveredOn", Op: OpLt, Value: "2026-03-09"},
		{Field: "pickedUpAt", Op: OpGte, Value: "2026-03-08T00:00:00-06:00"},
		{Field: "pickedUpAt", Op: OpLt, Value: "2026-03-15T00:00:00-05:00"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%+v\nwant\n%+v", got, want)
	}
}

func TestDrillBucketParameter(t *testing.T) {
	out := drill(t, DrillRequest{Params: map[string]any{"grain": "day"}, Row: []DrillValue{{Column: "week", Value: "2026-03-08T00:00:00Z"}}})
	if got := conditions(out)[2].Value; got != "2026-03-09T00:00:00Z" {
		t.Fatalf("a day bucket from the parameter ends a day later: %v", got)
	}
}

func TestDrillInvertsBands(t *testing.T) {
	cases := []struct {
		column string
		value  any
		want   []Filter
	}{
		{"charge", 0, []Filter{{Field: "totalCharge", Op: OpLt, Value: json.Number("1000")}}},
		{"charge", json.Number("1000"), []Filter{{Field: "totalCharge", Op: OpGte, Value: json.Number("1000")}, {Field: "totalCharge", Op: OpLt, Value: json.Number("5000")}}},
		{"charge", "5000", []Filter{{Field: "totalCharge", Op: OpGte, Value: json.Number("5000")}}},
		{"pcs", 20, []Filter{{Field: "pieces", Op: OpGte, Value: int64(20)}, {Field: "pieces", Op: OpLt, Value: int64(30)}}},
	}
	for _, c := range cases {
		got := conditions(drill(t, DrillRequest{Row: []DrillValue{{Column: c.column, Value: c.value}}}))[1:]
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s=%v: got %+v, want %+v", c.column, c.value, got, c.want)
		}
	}
	two := &Band{Edges: []float64{0, 10}}
	if f := (&driller{}).bandRange(&Dimension{Field: "x", Band: &Band{Edges: []float64{5, 10}}}, 5, ""); len(f) != 1 || f[0].Op != OpLt {
		t.Errorf("lowest band keeps only its upper bound: %+v", f)
	}
	if lo, hi, bounded := two.Bounds(10); lo != 10 || hi != 0 || bounded {
		t.Errorf("top band is open: %v %v %v", lo, hi, bounded)
	}
	if f := (&driller{}).bandRange(&Dimension{Field: "x", Band: &Band{Edges: []float64{5}}}, 1, ""); len(f) != 1 || f[0].Op != OpIsNotNull {
		t.Errorf("a single open band matches every value: %+v", f)
	}
}

func TestDrillValuesNullsTransformsAndCalcs(t *testing.T) {
	got := conditions(drill(t, DrillRequest{Row: []DrillValue{
		{Column: "customer", Value: nil},
		{Column: "code", Value: "ACME"},
		{Column: "late", Value: true},
		{Column: "month", Value: nil},
	}}))[1:]
	want := []Filter{
		{Field: "customer.name", Op: OpIsNull},
		{Field: "customer.code", Op: OpEq, Value: "ACME", Transform: &Transform{Op: TransformUpper}},
		{Calc: formula.Of("date_diff('day', {pickedUpAt}, {deliveredOn}) > 2"), Op: OpEq, Value: true},
		{Field: "shippedAt", Op: OpIsNull},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestDrillCarriesMeasureFilterAndPivotScope(t *testing.T) {
	got := conditions(drill(t, DrillRequest{Column: "revenue"}))
	if len(got) != 2 || !reflect.DeepEqual(got[1], *And(Where("hazmat", OpEq, true))) {
		t.Fatalf("measure filter: %+v", got)
	}
	got = conditions(drill(t, DrillRequest{Column: "share"}))
	if len(got) != 2 || got[1].And[0].Field != "hazmat" {
		t.Fatalf("a window carries its column's measure filter: %+v", got)
	}
	got = conditions(drill(t, DrillRequest{Column: "loads:Completed"}))
	if !reflect.DeepEqual(got[1], Where("status", OpEq, "Completed")) {
		t.Fatalf("pivot cell: %+v", got)
	}
	got = conditions(drill(t, DrillRequest{Column: "loads:" + PivotOther}))
	want := *Or(Filter{Field: "status", Op: OpIsNull}, Where("status", OpNotIn, []any{"New", "Completed"}))
	if !reflect.DeepEqual(got[1], want) {
		t.Fatalf("Other inverts to the same or the compiler emits: %+v", got[1])
	}
	if got := conditions(drill(t, DrillRequest{Column: "customer"})); len(got) != 1 {
		t.Fatalf("a dimension adds no scope: %+v", got)
	}
}

func TestDrillDiscoveredPivotOther(t *testing.T) {
	def := drillBase()
	def.Pivot = &Pivot{Field: "weight", Top: &PivotTop{N: N(2), By: "revenue"}, Columns: []string{"loads"}, Other: true}
	out, err := Drill(def, DrillRequest{Columns: recordColumns(), Column: "loads:" + PivotOther, PivotValues: []string{"3", "5"}}, CatalogTypes(catalog(), "shipment"))
	if err != nil {
		t.Fatal(err)
	}
	if got := conditions(out)[1].Or[1].Value; !reflect.DeepEqual(got, []any{json.Number("3"), json.Number("5")}) {
		t.Fatalf("discovered values typed by the field: %#v", got)
	}
	out, _ = Drill(def, DrillRequest{Columns: recordColumns(), Column: "loads:" + PivotOther}, nil)
	if got := conditions(out)[1]; got.Op != OpIsNull {
		t.Fatalf("no discovered values: %+v", got)
	}
}

func TestDrillColumnsAndLimit(t *testing.T) {
	out := drill(t, DrillRequest{Limit: 50})
	ids := []string{}
	for _, c := range out.Columns {
		ids = append(ids, c.ID+"="+c.Dimension.Field)
	}
	if strings.Join(ids, ",") != "pro=proNumber,d1=customer.name,d2=customer.code" || out.Limit != N(50) || out.Columns[1].Label != "Customer" {
		t.Fatalf("columns %v limit %v", ids, out.Limit)
	}
	if len(out.Parameters) != 1 || out.Entity != "shipment" || out.Version != Version {
		t.Fatal("drilled definition keeps the parameters")
	}
	clash := drill(t, DrillRequest{Columns: []Column{{ID: "d1", Dimension: &Dimension{Field: "proNumber"}}}})
	if clash.Columns[1].ID != "d1_1" {
		t.Fatalf("appended ids stay unique: %+v", clash.Columns)
	}
}

func TestDrillErrors(t *testing.T) {
	cases := []struct {
		req DrillRequest
		msg string
	}{
		{DrillRequest{Columns: recordColumns(), Row: []DrillValue{{Column: "revenue", Value: 1}}}, `/row/0: "revenue" is not a grouping column`},
		{DrillRequest{Columns: recordColumns(), Row: []DrillValue{{Column: "nope", Value: 1}}}, "not a grouping column"},
		{DrillRequest{Columns: recordColumns(), Column: "nope"}, `/column: "nope" is not a column`},
		{DrillRequest{Columns: recordColumns(), Row: []DrillValue{{Column: "month", Value: "March"}}}, "expected the instant"},
		{DrillRequest{Columns: recordColumns(), Row: []DrillValue{{Column: "charge", Value: "lots"}}}, "expected a number"},
		{DrillRequest{Columns: recordColumns(), Params: map[string]any{"grain": "fortnight"}, Row: []DrillValue{{Column: "week", Value: 0}}}, "has no value"},
	}
	for _, c := range cases {
		_, err := Drill(drillBase(), c.req, nil)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("got %v, want %q", err, c.msg)
		}
	}
	def := drillBase()
	def.Columns = def.Columns[1:2]
	def.Pivot = nil
	if _, err := Drill(def, DrillRequest{}, nil); err == nil || !strings.Contains(err.Error(), "needs at least one column") {
		t.Errorf("no columns: %v", err)
	}
	def.Version = 1
	if _, err := Drill(def, DrillRequest{Columns: recordColumns()}, nil); err == nil {
		t.Error("an invalid report does not drill")
	}
}
