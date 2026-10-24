package ir

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func canned(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/trenova/canned.json")
	if err != nil {
		t.Fatal(err)
	}
	var reports map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reports); err != nil {
		t.Fatal(err)
	}
	return reports
}

func refPaths(v any, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["ref"].(map[string]any); ok && ref["field"] != "" {
			parts := []string{}
			if p, ok := ref["path"].([]any); ok {
				for _, s := range p {
					parts = append(parts, s.(string))
				}
			}
			out[strings.Join(append(parts, ref["field"].(string)), ".")] = true
		}
		for _, child := range x {
			refPaths(child, out)
		}
	case []any:
		for _, child := range x {
			refPaths(child, out)
		}
	}
}

func TestUpgradesEveryCannedReport(t *testing.T) {
	reports := canned(t)
	if len(reports) < 40 {
		t.Fatalf("expected every canned report, found %d", len(reports))
	}
	for key, raw := range reports {
		t.Run(key, func(t *testing.T) {
			def, err := UpgradeFromTrenovaV1(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(&def, Limits{}); err != nil {
				t.Fatalf("upgraded definition is invalid: %v", err)
			}
			viaDecode, err := Decode(raw)
			if err != nil || !reflect.DeepEqual(*viaDecode, def) {
				t.Fatalf("Decode upgrades the same way: %v", err)
			}
			encoded, _ := Encode(&def)
			again, err := Decode(encoded)
			if err != nil {
				t.Fatalf("re-decode: %v\n%s", err, encoded)
			}
			if err := Validate(again, Limits{}); err != nil {
				t.Fatalf("re-decoded definition is invalid: %v", err)
			}
			var old map[string]any
			_ = json.Unmarshal(raw, &old)
			want := map[string]bool{}
			refPaths(old, want)
			text := string(encoded)
			for path := range want {
				if !strings.Contains(text, `"`+path+`"`) {
					t.Errorf("field %s was lost", path)
				}
			}
			if n := len(old["columns"].([]any)); n != len(def.Columns) {
				t.Errorf("%d columns became %d", n, len(def.Columns))
			}
		})
	}
}

func TestUpgradeRevenuePerMile(t *testing.T) {
	def, err := UpgradeFromTrenovaV1(canned(t)["revenue-per-mile"])
	if err != nil {
		t.Fatal(err)
	}
	want := `{
	  "version": 2,
	  "entity": "shipment",
	  "columns": [
	    {"id": "customer", "label": "Customer", "dimension": {"field": "customer.name"}},
	    {"id": "service_type", "label": "Service Type", "dimension": {"field": "serviceType.code"}},
	    {"id": "shipment_count", "label": "Shipments", "measure": {"field": "id", "agg": "count"},
	     "display": {"style": "number", "decimals": 0, "grouping": true}},
	    {"id": "total_revenue", "label": "Total Revenue", "measure": {"field": "totalChargeAmount", "agg": "sum"},
	     "display": {"style": "currency", "decimals": 2, "negative": "parentheses"}},
	    {"id": "freight_revenue", "label": "Freight Revenue", "measure": {"field": "freightChargeAmount", "agg": "sum"},
	     "display": {"style": "currency", "decimals": 2, "negative": "parentheses"}},
	    {"id": "total_miles", "label": "Total Miles", "measure": {"field": "moves.distance", "agg": "sum"},
	     "transform": {"op": "round", "precision": 0}, "display": {"style": "number", "decimals": 0, "suffix": " mi"}},
	    {"id": "avg_move_miles", "label": "Average Miles per Move", "measure": {"field": "moves.distance", "agg": "avg"},
	     "transform": {"op": "round", "precision": 1}, "display": {"style": "number", "decimals": 1, "suffix": " mi"}},
	    {"id": "revenue_per_mile", "label": "Revenue per Mile", "calc": "[total_revenue] / [total_miles]",
	     "transform": {"op": "round", "precision": 2}, "display": {"style": "currency", "decimals": 2, "negative": "parentheses"}},
	    {"id": "miles_per_shipment", "label": "Average Length of Haul", "calc": "[total_miles] / [shipment_count]",
	     "transform": {"op": "round", "precision": 1}, "display": {"style": "number", "decimals": 1, "suffix": " mi"}}
	  ],
	  "filters": {"and": [
	    {"field": "createdAt", "op": "last_n_days", "param": "windowDays"},
	    {"field": "status", "op": "not_in", "value": ["Canceled"]}
	  ]},
	  "sort": [{"column": "revenue_per_mile", "desc": true}],
	  "parameters": [{"name": "windowDays", "label": "Window (days)", "type": "int", "required": true, "default": 90}]
	}`
	expected := decode(t, want)
	if !jsonEqual(t, expected, &def) {
		got, _ := Encode(&def)
		t.Fatalf("got\n%s", got)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var va, vb any
	_ = json.Unmarshal(ja, &va)
	_ = json.Unmarshal(jb, &vb)
	return reflect.DeepEqual(va, vb)
}

func TestUpgradeShapesTheCannedLibraryLacks(t *testing.T) {
	v1 := `{
	  "irVersion": 1,
	  "entity": "shipment",
	  "columns": [
	    {"id": "customer", "ref": {"path": ["customer"], "field": "name"}, "kind": "dimension"},
	    {"id": "charge_band", "ref": {"field": "totalCharge"}, "kind": "dimension", "band": {"edges": [0, 1000, 5000]}},
	    {"id": "revenue", "ref": {"field": "totalCharge"}, "kind": "measure", "agg": "sum"},
	    {"id": "rounded", "ref": {"field": "weight"}, "kind": "measure", "agg": "sum", "transform": {"op": "round", "precision": 0}},
	    {"id": "loaded_miles", "ref": {"path": ["moves"], "field": "distance"}, "kind": "measure", "agg": "sum",
	     "filter": {"op": "and", "filters": [{"ref": {"path": ["moves"], "field": "loaded"}, "operator": "eq", "value": true}]}},
	    {"id": "vs_goal", "ref": {"field": ""}, "kind": "computed", "label": "Against goal",
	     "computed": {"op": "divide", "leftId": "revenue", "rightValue": 250000, "format": "percent"}},
	    {"id": "offset", "ref": {"field": ""}, "kind": "computed", "label": "Offset",
	     "computed": {"op": "add", "leftValue": -1.5, "rightId": "revenue", "format": "none"}},
	    {"id": "tiny", "ref": {"field": ""}, "kind": "computed", "label": "Tiny",
	     "computed": {"op": "multiply", "leftId": "revenue", "rightValue": 1e-7}}
	  ],
	  "filters": {"op": "and", "filters": [
	    {"ref": {"field": "shippedAt"}, "operator": "daterange", "value": [1767225600, 1769904000]},
	    {"ref": {"path": ["customer"], "field": "code"}, "operator": "eq", "value": "acme", "transform": {"op": "upper"}}
	  ], "groups": [{"op": "and", "filters": []}]},
	  "having": {"op": "or", "filters": [
	    {"ref": {"field": "totalCharge"}, "operator": "gt", "agg": "sum", "value": 1000},
	    {"ref": {"field": "weight"}, "operator": "lt", "agg": "sum", "param": "maxWeight"},
	    {"ref": {"field": "weight"}, "operator": "gte", "agg": "max", "value": 10}
	  ], "groups": [{"op": "and", "filters": [{"ref": {"path": ["moves"], "field": "distance"}, "operator": "gt", "agg": "avg", "value": 5}]}]},
	  "pivot": {"ref": {"field": "status"}, "values": ["New", "Completed"], "labels": ["Fresh", "Done"], "measureIds": ["revenue"], "includeOther": true},
	  "sort": [{"columnId": "revenue:New", "direction": "asc"}],
	  "limit": 250,
	  "totals": true,
	  "parameters": [
	    {"name": "maxWeight", "type": "int", "required": false, "allowedValues": ["100", "200"]},
	    {"name": "customer", "type": "ref", "required": true, "refEntity": "customer"},
	    {"name": "flags", "type": "bool", "required": false, "allowedValues": ["true"]}
	  ],
	  "charts": [
	    {"id": "c1", "type": "bar", "xColumnId": "customer", "seriesIds": ["revenue:New", "revenue:__other__"], "stacked": true, "limit": 10},
	    {"id": "c2", "type": "line", "xColumnId": "customer", "seriesIds": ["rounded"], "stacked": true, "curved": true},
	    {"id": "c3", "type": "kpi", "seriesIds": ["rounded"], "compareId": "loaded_miles", "goal": {"value": 5000, "label": "Target"}},
	    {"id": "c4", "type": "map", "latColumnId": "rounded", "lngColumnId": "loaded_miles", "labelColumnId": "customer", "goal": {}}
	  ]
	}`
	def, err := UpgradeFromTrenovaV1([]byte(v1))
	if err != nil {
		t.Fatal(err)
	}
	calcs := map[string]string{}
	for _, c := range def.Columns {
		if c.Calc != nil {
			calcs[c.ID] = c.Calc.Text + "|" + c.Format
		}
	}
	wantCalcs := map[string]string{"vs_goal": "[revenue] / 250000|percent", "offset": "-1.5 + [revenue]|", "tiny": "[revenue] * 0.0000001|"}
	if !reflect.DeepEqual(calcs, wantCalcs) {
		t.Errorf("computed specs: %v", calcs)
	}
	hidden := []string{}
	for _, c := range def.Columns {
		if c.Hidden {
			hidden = append(hidden, c.ID+"="+string(c.Measure.Agg)+"("+c.Measure.Field+")")
		}
	}
	sort.Strings(hidden)
	wantHidden := []string{"having_avg_moves_distance=avg(moves.distance)", "having_max_weight=max(weight)", "having_sum_weight=sum(weight)"}
	if !reflect.DeepEqual(hidden, wantHidden) {
		t.Errorf("hidden measures: %v", hidden)
	}
	wantHaving := Or(
		Filter{Column: "revenue", Op: OpGt, Value: json.Number("1000")},
		Filter{Column: "having_sum_weight", Op: OpLt, Param: "maxWeight"},
		Filter{Column: "having_max_weight", Op: OpGte, Value: json.Number("10")},
		*And(Filter{Column: "having_avg_moves_distance", Op: OpGt, Value: json.Number("5")}),
	)
	if !reflect.DeepEqual(def.Having, wantHaving) {
		t.Errorf("having: %+v", def.Having)
	}
	wantFilters := And(
		Filter{Field: "shippedAt", Op: OpBetween, Value: []any{json.Number("1767225600"), json.Number("1769904000")}},
		Filter{Field: "customer.code", Op: OpEq, Value: "acme", Transform: &Transform{Op: TransformUpper}},
	)
	if !reflect.DeepEqual(def.Filters, wantFilters) {
		t.Errorf("filters: %+v", def.Filters)
	}
	if def.Groupings == nil || !def.Groupings.Total || def.Limit != N(250) || def.Sort[0] != (Sort{Column: "revenue:New"}) {
		t.Errorf("layout: %+v %+v %+v", def.Groupings, def.Limit, def.Sort)
	}
	if p := def.Pivot; p.Field != "status" || !p.Other || !reflect.DeepEqual(p.Labels, []string{"Fresh", "Done"}) || !reflect.DeepEqual(p.Columns, []string{"revenue"}) {
		t.Errorf("pivot: %+v", p)
	}
	if d := def.Columns[1].Dimension; d.Band == nil || len(d.Band.Edges) != 3 {
		t.Errorf("band: %+v", d)
	}
	params := def.Parameters
	if !reflect.DeepEqual(params[0].Allowed, []any{json.Number("100"), json.Number("200")}) || params[1].Entity != "customer" || params[2].Allowed[0] != true {
		t.Errorf("parameters: %+v", params)
	}
	charts := def.Charts
	if !charts[0].Stacked || charts[1].Stacked || charts[2].Compare != "loaded_miles" || *charts[2].Goal.Value != 5000 || charts[3].PointLabel != "customer" || charts[3].Goal != nil {
		t.Errorf("charts: %+v", charts)
	}
	if err := Validate(&def, Limits{}); err == nil || !strings.Contains(err.Error(), "must be pivoted too") {
		t.Errorf("the vs_goal calc over the pivoted revenue is reported, not hidden: %v", err)
	}
}

func TestUpgradeHavingNameCollision(t *testing.T) {
	v1 := `{"irVersion":1,"entity":"shipment","columns":[
	  {"id":"having_sum_weight","ref":{"field":"proNumber"},"kind":"dimension"},
	  {"id":"n","ref":{"field":"id"},"kind":"measure","agg":"count"}],
	  "having":{"op":"and","filters":[{"ref":{"field":"weight"},"operator":"gt","agg":"sum","value":1}]}}`
	def, err := UpgradeFromTrenovaV1([]byte(v1))
	if err != nil {
		t.Fatal(err)
	}
	if def.Having.And[0].Column != "having_sum_weight_2" || def.Columns[2].ID != "having_sum_weight_2" {
		t.Fatalf("collision: %+v", def.Columns)
	}
	if err := Validate(&def, Limits{}); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeErrors(t *testing.T) {
	cases := map[string]string{
		`{"irVersion":2,"entity":"x","columns":[]}`:                                                                                                "Trenova IR version 2 is not version 1",
		`{"irVersion":1,"entity":"x","columns":[],"bogus":1}`:                                                                                      `unknown field "bogus"`,
		`{"irVersion":1,"entity":"x","columns":[{"id":"a","kind":"weird"}]}`:                                                                       `unknown column kind "weird"`,
		`{"irVersion":1,"entity":"x","columns":[{"id":"a","kind":"computed"}]}`:                                                                    "has no computed spec",
		`{"irVersion":1,"entity":"x","columns":[{"id":"a","kind":"computed","computed":{"op":"pow","leftId":"b","rightId":"c"}}]}`:                 `unknown computed operator "pow"`,
		`{"irVersion":1,"entity":"x","columns":[{"id":"a","kind":"computed","computed":{"op":"add","leftId":"b","leftValue":1,"rightId":"c"}}]}`:   "not both",
		`{"irVersion":1,"entity":"x","columns":[{"id":"a","kind":"computed","computed":{"op":"add","rightId":"c"}}]}`:                              "needs a column or a value",
		`{"irVersion":1,"entity":"x","columns":[{"id":"a","ref":{"field":"f"},"kind":"dimension","agg":"sum"}]}`:                                   "carries an aggregation",
		`{"irVersion":1,"entity":"x","columns":[],"filters":{"op":"and","filters":[{"ref":{"field":"f"},"operator":"countgt","value":1}]}}`:        `operator "countgt" has no equivalent`,
		`{"irVersion":1,"entity":"x","columns":[],"filters":{"op":"and","filters":[{"ref":{"field":"f"},"operator":"eq","agg":"sum","value":1}]}}`: "a row filter carries an aggregation",
		`{"irVersion":1,"entity":"x","columns":[],"filters":{"op":"xor","filters":[{"ref":{"field":"f"},"operator":"eq","value":1}]}}`:             `unknown group operator "xor"`,
		`{"irVersion":1} x`: "not a Trenova IR v1 definition",
	}
	for src, want := range cases {
		_, err := UpgradeFromTrenovaV1([]byte(src))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", src, err, want)
		}
	}
}
