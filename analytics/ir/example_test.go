package ir_test

import (
	"fmt"

	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
)

func ExampleValidate() {
	def := &ir.Definition{
		Version: ir.Version,
		Entity:  "shipment",
		Columns: []ir.Column{
			{ID: "customer", Dimension: &ir.Dimension{Field: "customer.name"}},
			{ID: "month", Dimension: &ir.Dimension{Field: "shippedAt", Bucket: ir.Every(ir.BucketMonth)}},
			{ID: "revenue", Measure: &ir.Measure{Field: "totalCharge", Agg: ir.AggSum}},
			{ID: "loads", Measure: &ir.Measure{Agg: ir.AggCount}},
			{ID: "per_load", Calc: formula.Of("[revenue] / [loads]")},
		},
		Filters: ir.And(
			ir.WhereParam("shippedAt", ir.OpLastNDays, "window"),
			ir.Where("status", ir.OpNotIn, []string{"Canceled"}),
		),
		Top:        &ir.Top{N: ir.CountParam("n"), By: "revenue"},
		Parameters: []ir.Parameter{{Name: "window", Type: formula.Int, Default: 30}, {Name: "n", Type: formula.Int, Default: 10}},
	}
	fmt.Println(ir.Validate(def, ir.Limits{}))
	def.Columns[4].Calc = formula.Of("[revnue] / [loads]")
	fmt.Println(ir.Validate(def, ir.Limits{}))
	// Output:
	// <nil>
	// /columns/4/calc: 1:1: unknown column [revnue]; did you mean [revenue]?
}

func ExampleApplyFilters() {
	def := &ir.Definition{
		Version: ir.Version,
		Entity:  "shipment",
		Columns: []ir.Column{{ID: "loads", Measure: &ir.Measure{Agg: ir.AggCount}}},
		Filters: ir.And(ir.Where("status", ir.OpEq, "Completed")),
	}
	status := ir.DashboardFilter{ID: "status", Entity: "shipment", Field: "status", Op: ir.OpIn}
	tile, _ := ir.ApplyFilters(def, []ir.FilterValue{{Filter: status, Value: []string{"New", "InTransit"}}}, ir.Params{})
	out, _ := tile.Filters.MarshalJSON()
	fmt.Println(string(out))
	// Output: {"and":[{"field":"status","op":"in","value":["New","InTransit"]}]}
}
