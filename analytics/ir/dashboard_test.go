package ir

import (
	"reflect"
	"testing"
)

func dashFilter(id, entity, field string, op Op) DashboardFilter {
	return DashboardFilter{ID: id, Entity: entity, Field: field, Op: op}
}

func picked(f DashboardFilter, v any) FilterValue { return FilterValue{Filter: f, Value: v} }

func shipmentDef(filters *Filter) *Definition {
	return &Definition{Version: Version, Entity: "shipment", Filters: filters}
}

func TestApplyFiltersSkipsOtherEntities(t *testing.T) {
	def := shipmentDef(nil)
	got, _ := ApplyFilters(def, []FilterValue{picked(dashFilter("f1", "invoice", "status", OpEq), "Posted")}, Params{})
	if got != def {
		t.Fatal("an inapplicable filter leaves the report untouched")
	}
}

func TestApplyFiltersIgnoresEmptyValues(t *testing.T) {
	got, _ := ApplyFilters(shipmentDef(nil), []FilterValue{
		picked(dashFilter("f1", "shipment", "status", OpEq), nil),
		picked(dashFilter("f2", "shipment", "proNumber", OpEq), ""),
		picked(dashFilter("f3", "shipment", "bol", OpIn), []string{}),
	}, Params{})
	if got.Filters != nil {
		t.Fatalf("empty values filter nothing: %+v", got.Filters)
	}
}

func TestHasValue(t *testing.T) {
	for _, v := range []any{nil, "", []string{}, []any{}} {
		if hasValue(v) {
			t.Errorf("%#v counts as a value", v)
		}
	}
	for _, v := range []any{"Completed", 0, false, []string{"a"}} {
		if !hasValue(v) {
			t.Errorf("%#v is a value", v)
		}
	}
}

func TestApplyFiltersOverridesAMatchingCondition(t *testing.T) {
	def := shipmentDef(And(Where("status", OpEq, "Completed")))
	got, _ := ApplyFilters(def, []FilterValue{picked(dashFilter("f1", "shipment", "status", OpNe), "New")}, Params{})
	if len(got.Filters.And) != 1 || got.Filters.And[0].Value != "New" || got.Filters.And[0].Op != OpNe || got.Filters.And[0].IsGroup() {
		t.Fatalf("override: %+v", got.Filters)
	}
	if def.Filters.And[0].Value != "Completed" || def.Filters.And[0].Op != OpEq {
		t.Fatal("the stored report was modified")
	}
}

func TestApplyFiltersSkipsParameterBoundConditions(t *testing.T) {
	def := shipmentDef(And(WhereParam("status", OpEq, "statusParam")))
	got, _ := ApplyFilters(def, []FilterValue{picked(dashFilter("f1", "shipment", "status", OpEq), "New")}, Params{})
	and := got.Filters.And
	if len(and) != 2 || and[0].Value != "New" || and[1].And[0].Param != "statusParam" {
		t.Fatalf("parameter-bound condition is nested, not rewritten: %+v", got.Filters)
	}
}

func TestApplyFiltersNestsRatherThanFlattens(t *testing.T) {
	def := shipmentDef(Or(Where("proNumber", OpEq, "A"), Where("bol", OpEq, "B")))
	got, _ := ApplyFilters(def, []FilterValue{picked(dashFilter("f1", "shipment", "status", OpEq), "New")}, Params{})
	and := got.Filters.And
	if len(and) != 2 || and[0].Field != "status" || len(and[1].Or) != 2 {
		t.Fatalf("the report's or survives intact: %+v", got.Filters)
	}
}

func TestApplyFiltersOverridesInsideNestedGroups(t *testing.T) {
	def := shipmentDef(And(*Or(Where("status", OpEq, "Completed"))))
	got, _ := ApplyFilters(def, []FilterValue{picked(dashFilter("f1", "shipment", "status", OpEq), "New")}, Params{})
	if len(got.Filters.And) != 1 || got.Filters.And[0].Or[0].Value != "New" {
		t.Fatalf("a nested match counts: %+v", got.Filters)
	}
	if def.Filters.And[0].Or[0].Value != "Completed" {
		t.Fatal("original modified")
	}
}

func TestApplyFiltersMatchesWholePaths(t *testing.T) {
	def := shipmentDef(And(Where("customer.name", OpEq, "Acme"), Where("name", OpEq, "x")))
	got, _ := ApplyFilters(def, []FilterValue{picked(dashFilter("f1", "shipment", "customer.name", OpIn), []string{"Beta"})}, Params{})
	if got.Filters.And[0].Op != OpIn || got.Filters.And[1].Value != "x" || len(got.Filters.And) != 2 {
		t.Fatalf("path matching: %+v", got.Filters)
	}
}

func TestApplyFiltersAddsAndOverrides(t *testing.T) {
	def := shipmentDef(And(Where("status", OpEq, "Completed")))
	got, _ := ApplyFilters(def, []FilterValue{
		picked(dashFilter("f1", "shipment", "status", OpEq), "New"),
		picked(dashFilter("f2", "shipment", "customer.name", OpEq), "Acme"),
	}, Params{})
	want := And(Where("customer.name", OpEq, "Acme"), *And(Where("status", OpEq, "New")))
	if !reflect.DeepEqual(got.Filters, want) {
		t.Fatalf("got %+v", got.Filters)
	}
}

func TestApplyFiltersOnAReportWithoutFilters(t *testing.T) {
	got, _ := ApplyFilters(shipmentDef(nil), []FilterValue{picked(dashFilter("f1", "shipment", "status", OpEq), "New")}, Params{})
	if !reflect.DeepEqual(got.Filters, And(Where("status", OpEq, "New"))) {
		t.Fatalf("got %+v", got.Filters)
	}
}

func TestResolveParamsPrefersExplicitBindings(t *testing.T) {
	def := &Definition{Entity: "shipment", Parameters: []Parameter{{Name: "windowDays"}, {Name: "region"}}}
	_, got := ApplyFilters(def, nil, Params{
		Values:   map[string]any{"lookback": 90, "windowDays": 30, "region": "West"},
		Bindings: map[string]string{"windowDays": "lookback"},
	})
	if got["windowDays"] != 90 || got["region"] != "West" {
		t.Fatalf("got %v", got)
	}
}

func TestResolveParamsIgnoresUndeclaredValues(t *testing.T) {
	def := &Definition{Entity: "shipment", Parameters: []Parameter{{Name: "windowDays"}}}
	_, got := ApplyFilters(def, nil, Params{Values: map[string]any{"windowDays": 30, "somethingElse": "x"}})
	if !reflect.DeepEqual(got, map[string]any{"windowDays": 30}) {
		t.Fatalf("got %v", got)
	}
}

func TestResolveParamsReturnsNilWhenNothingApplies(t *testing.T) {
	cases := []struct {
		params []Parameter
		values map[string]any
	}{
		{nil, map[string]any{"a": 1}},
		{[]Parameter{{Name: "windowDays"}}, map[string]any{}},
		{[]Parameter{{Name: "windowDays"}}, map[string]any{"windowDays": nil}},
	}
	for _, c := range cases {
		_, got := ApplyFilters(&Definition{Parameters: c.params}, nil, Params{Values: c.values})
		if got != nil {
			t.Errorf("%v: got %v", c.values, got)
		}
	}
	if def, got := ApplyFilters(nil, nil, Params{}); def != nil || got != nil {
		t.Error("nil definition")
	}
}
