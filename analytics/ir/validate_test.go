package ir

import (
	"errors"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics/formula"
)

func base() *Definition {
	return &Definition{
		Version: Version,
		Entity:  "shipment",
		Columns: []Column{
			{ID: "customer", Dimension: &Dimension{Field: "customer.name"}},
			{ID: "month", Dimension: &Dimension{Field: "shippedAt", Bucket: Every(BucketMonth)}},
			{ID: "revenue", Measure: &Measure{Field: "totalCharge", Agg: AggSum}},
			{ID: "loads", Measure: &Measure{Agg: AggCount}},
			{ID: "rpl", Calc: formula.Of("[revenue] / [loads]")},
		},
		Parameters: []Parameter{
			{Name: "window", Type: formula.Int, Default: 30},
			{Name: "statuses", Type: formula.Enum, Multi: true},
			{Name: "label", Type: formula.String},
		},
	}
}

func listReport() *Definition {
	return &Definition{Version: Version, Entity: "shipment", Columns: []Column{
		{ID: "pro", Dimension: &Dimension{Field: "proNumber"}},
		{ID: "weight", Dimension: &Dimension{Field: "weight"}},
	}}
}

func withPivot(d *Definition) {
	d.Columns[0] = Column{ID: "customer", Dimension: &Dimension{Field: "customer.name"}}
	d.Pivot = &Pivot{Field: "status", Values: Values{List: []string{"New", "Completed"}}, Columns: []string{"revenue"}, Other: true}
}

func addColumn(c Column) func(*Definition) {
	return func(d *Definition) { d.Columns = append(d.Columns, c) }
}

func where(f Filter) func(*Definition) {
	return func(d *Definition) { d.Filters = And(f) }
}

func having(f Filter) func(*Definition) {
	return func(d *Definition) { d.Having = And(f) }
}

func chart(c Chart) func(*Definition) {
	return func(d *Definition) { d.Charts = []Chart{c} }
}

func intp(n int) *int { return &n }

func floatp(f float64) *float64 { return &f }

type ruleCase struct {
	name   string
	mutate func(*Definition)
	cat    bool
	lim    Limits
	ptr    string
	msg    string
	start  func() *Definition
}

func runCases(t *testing.T, cases []ruleCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := c.start
			if start == nil {
				start = base
			}
			d := start()
			if c.mutate != nil {
				c.mutate(d)
			}
			var err error
			if c.cat {
				_, err = Resolve(d, catalog(), c.lim)
			} else {
				err = Validate(d, c.lim)
			}
			if c.msg == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var es Errors
			if !errors.As(err, &es) {
				t.Fatalf("want %s: %q, got %v", c.ptr, c.msg, err)
			}
			for _, e := range es {
				if e.Pointer == c.ptr && strings.Contains(e.Message, c.msg) {
					return
				}
			}
			t.Fatalf("want %s: %q, got\n%v", c.ptr, c.msg, err)
		})
	}
}

func TestBaseIsValid(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "structure"},
		{name: "catalog", cat: true},
		{name: "list report", start: listReport, cat: true},
		{name: "nil definition", start: func() *Definition { return nil }, ptr: "", msg: "a definition is required"},
	})
}

func TestEnvelopeRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "version", mutate: func(d *Definition) { d.Version = 1 }, ptr: "/version", msg: "version 1 is not supported"},
		{name: "entity required", mutate: func(d *Definition) { d.Entity = "" }, ptr: "/entity", msg: "choose the entity"},
		{name: "entity key", mutate: func(d *Definition) { d.Entity = "ship-ment" }, ptr: "/entity", msg: "not an entity key"},
		{name: "entity unknown", cat: true, mutate: func(d *Definition) { d.Entity = "shipmnt" }, ptr: "/entity", msg: `unknown entity "shipmnt"; did you mean "shipment"?`},
		{name: "no columns", mutate: func(d *Definition) { d.Columns = nil }, ptr: "/columns", msg: "add at least one column"},
		{name: "too many columns", lim: Limits{MaxColumns: 3}, ptr: "/columns", msg: "at most 3 columns"},
	})
}

func TestColumnRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "id required", mutate: func(d *Definition) { d.Columns[0].ID = "" }, ptr: "/columns/0/id", msg: "give the column an id"},
		{name: "id chars", mutate: func(d *Definition) { d.Columns[0].ID = "a b" }, ptr: "/columns/0/id", msg: "not a column id"},
		{name: "id length", mutate: func(d *Definition) { d.Columns[0].ID = strings.Repeat("a", 65) }, ptr: "/columns/0/id", msg: "at most 64"},
		{name: "id duplicate", mutate: func(d *Definition) { d.Columns[1].ID = "customer" }, ptr: "/columns/1/id", msg: `"customer" is used twice`},
		{name: "no kind", mutate: func(d *Definition) { d.Columns[0].Dimension = nil }, ptr: "/columns/0", msg: "give it one"},
		{name: "two kinds", mutate: func(d *Definition) { d.Columns[0].Measure = &Measure{Agg: AggCount} }, ptr: "/columns/0", msg: "exactly one of"},
		{name: "all hidden", start: listReport, mutate: func(d *Definition) { d.Columns[0].Hidden, d.Columns[1].Hidden = true, true }, ptr: "/columns", msg: "at least one column must be visible"},
		{name: "label length", mutate: func(d *Definition) { d.Columns[0].Label = strings.Repeat("x", 201) }, ptr: "/columns/0/label", msg: "at most 200"},
		{name: "format hint", mutate: func(d *Definition) { d.Columns[4].Format = "money!" }, ptr: "/columns/4/format", msg: "not a format hint"},
		{name: "display object", mutate: func(d *Definition) { d.Columns[2].Display = []byte(`[1]`) }, ptr: "/columns/2/display", msg: "a display spec is an object"},
		{name: "display ok", mutate: func(d *Definition) { d.Columns[2].Display = []byte(`{"style":"currency"}`) }},
		{name: "transform unknown", mutate: func(d *Definition) { d.Columns[2].Transform = &Transform{Op: "rond"} }, ptr: "/columns/2/transform/op", msg: `did you mean "round"?`},
		{name: "precision not taken", mutate: func(d *Definition) { d.Columns[2].Transform = &Transform{Op: TransformAbs, Precision: intp(2)} }, ptr: "/columns/2/transform/precision", msg: "abs does not take a precision"},
		{name: "precision range", mutate: func(d *Definition) { d.Columns[2].Transform = &Transform{Op: TransformRound, Precision: intp(9)} }, ptr: "/columns/2/transform/precision", msg: "from 0 to 6"},
		{name: "scale factor required", mutate: func(d *Definition) { d.Columns[2].Transform = &Transform{Op: TransformScale} }, ptr: "/columns/2/transform/factor", msg: "scale needs a factor"},
		{name: "scale factor zero", mutate: func(d *Definition) { d.Columns[2].Transform = &Transform{Op: TransformScale, Factor: floatp(0)} }, ptr: "/columns/2/transform/factor", msg: "non-zero finite"},
		{name: "factor not taken", mutate: func(d *Definition) { d.Columns[2].Transform = &Transform{Op: TransformRound, Factor: floatp(2)} }, ptr: "/columns/2/transform/factor", msg: "round does not take a factor"},
		{name: "text transform on number", cat: true, mutate: func(d *Definition) { d.Columns[2].Transform = &Transform{Op: TransformUpper} }, ptr: "/columns/2/transform/op", msg: "upper applies to text, and this column is decimal"},
		{name: "number transform on text", cat: true, mutate: func(d *Definition) { d.Columns[0].Transform = &Transform{Op: TransformRound} }, ptr: "/columns/0/transform/op", msg: "round applies to numbers, and this column is string"},
		{name: "transform ok", cat: true, mutate: func(d *Definition) {
			d.Columns[0].Transform = &Transform{Op: TransformTitle}
			d.Columns[2].Transform = &Transform{Op: TransformScale, Factor: floatp(0.01)}
		}},
	})
}

func TestDimensionRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "field required", mutate: func(d *Definition) { d.Columns[0].Dimension.Field = "" }, ptr: "/columns/0/dimension/field", msg: "choose a field"},
		{name: "field path", mutate: func(d *Definition) { d.Columns[0].Dimension.Field = "customer..name" }, ptr: "/columns/0/dimension/field", msg: "not a field path"},
		{name: "path depth", lim: Limits{MaxPathDepth: 1}, mutate: func(d *Definition) { d.Columns[0].Dimension.Field = "moves.stops.city" }, ptr: "/columns/0/dimension/field", msg: "crosses more than 1 relations"},
		{name: "unknown field", cat: true, mutate: func(d *Definition) { d.Columns[0].Dimension.Field = "customer.nam" }, ptr: "/columns/0/dimension/field", msg: `unknown field "nam" on customer; did you mean "customer.name"?`},
		{name: "unknown relation", cat: true, mutate: func(d *Definition) { d.Columns[0].Dimension.Field = "custmer.name" }, ptr: "/columns/0/dimension/field", msg: `unknown relation "custmer" on shipment; did you mean "customer.name"?`},
		{name: "bucket unknown", mutate: func(d *Definition) { d.Columns[1].Dimension.Bucket = Every("mnth") }, ptr: "/columns/1/dimension/bucket", msg: `did you mean "month"?`},
		{name: "bucket and band", mutate: func(d *Definition) { d.Columns[1].Dimension.Band = &Band{Width: 5} }, ptr: "/columns/1/dimension/band", msg: "not both"},
		{name: "bucket on text", cat: true, mutate: func(d *Definition) { d.Columns[0].Dimension.Bucket = Every(BucketDay) }, ptr: "/columns/0/dimension/bucket", msg: "only dates and times are bucketed, and this field is string"},
		{name: "bucket unit and param", mutate: func(d *Definition) { d.Columns[1].Dimension.Bucket = Grain{Unit: BucketDay, Param: "label"} }, ptr: "/columns/1/dimension/bucket", msg: "not both"},
		{name: "bucket param type", mutate: func(d *Definition) { d.Columns[1].Dimension.Bucket = GrainParam("window") }, ptr: "/columns/1/dimension/bucket", msg: "values are buckets"},
		{name: "bucket param values", mutate: func(d *Definition) {
			d.Parameters[2].Allowed = []any{"day", "fortnight"}
			d.Columns[1].Dimension.Bucket = GrainParam("label")
		}, ptr: "/columns/1/dimension/bucket", msg: "values are buckets"},
		{name: "bucket param unknown", mutate: func(d *Definition) { d.Columns[1].Dimension.Bucket = GrainParam("grain") }, ptr: "/columns/1/dimension/bucket", msg: `unknown parameter "grain"`},
		{name: "bucket param ok", cat: true, mutate: func(d *Definition) {
			d.Parameters[2].Allowed = []any{"day", "month"}
			d.Columns[1].Dimension.Bucket = GrainParam("label")
		}},
		{name: "band order", start: listReport, mutate: func(d *Definition) { d.Columns[1].Dimension.Band = &Band{Edges: []float64{5, 1}} }, ptr: "/columns/1/dimension/band", msg: "must ascend"},
		{name: "band width", start: listReport, mutate: func(d *Definition) { d.Columns[1].Dimension.Band = &Band{Width: -1} }, ptr: "/columns/1/dimension/band", msg: "positive, finite"},
		{name: "band edges", start: listReport, mutate: func(d *Definition) { d.Columns[1].Dimension.Band = &Band{Edges: []float64{1}} }, ptr: "/columns/1/dimension/band", msg: "at least two boundaries"},
		{name: "band both", start: listReport, mutate: func(d *Definition) { d.Columns[1].Dimension.Band = &Band{Width: 1, Edges: []float64{1, 2}} }, ptr: "/columns/1/dimension/band", msg: "not both"},
		{name: "band on text", cat: true, start: listReport, mutate: func(d *Definition) { d.Columns[0].Dimension.Band = &Band{Width: 5} }, ptr: "/columns/0/dimension/band", msg: "only numbers are banded"},
		{name: "band fraction on int", cat: true, start: listReport, mutate: func(d *Definition) { d.Columns[1].Dimension.Band = &Band{Edges: []float64{0, 1.5}} }, ptr: "/columns/1/dimension/band", msg: "whole numbers"},
		{name: "to-many dimension", cat: true, mutate: func(d *Definition) { d.Columns[0].Dimension.Field = "moves.loaded" }, ptr: "/columns/0/dimension/field", msg: "crosses a to-many relation"},
		{name: "not groupable", cat: true, mutate: func(d *Definition) { d.Columns[0].Dimension.Field = "totalCharge" }, ptr: "/columns/0/dimension/field", msg: "cannot group an aggregated report"},
		{name: "banded measure field groups", cat: true, mutate: func(d *Definition) {
			d.Columns[0].Dimension = &Dimension{Field: "totalCharge", Band: &Band{Edges: []float64{0, 1000, 5000}}}
		}},
	})
}

func TestMeasureRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "agg required", mutate: func(d *Definition) { d.Columns[2].Measure.Agg = "" }, ptr: "/columns/2/measure/agg", msg: "choose an aggregation"},
		{name: "agg unknown", mutate: func(d *Definition) { d.Columns[2].Measure.Agg = "summ" }, ptr: "/columns/2/measure/agg", msg: `did you mean "sum"?`},
		{name: "field required", mutate: func(d *Definition) { d.Columns[2].Measure.Field = "" }, ptr: "/columns/2/measure/field", msg: "only count works without one"},
		{name: "agg not supported", cat: true, mutate: func(d *Definition) { d.Columns[2].Measure.Field = "customer.name" }, ptr: "/columns/2/measure/agg", msg: "sum is not available for customer.name"},
		{name: "to-many filter scope", cat: true, mutate: func(d *Definition) {
			d.Columns[2].Measure = &Measure{Field: "moves.distance", Agg: AggSum, Filter: And(Where("status", OpEq, "New"))}
		}, ptr: "/columns/2/measure/filter/and/0/field", msg: "aggregates move rows, so its filter can only use their fields"},
		{name: "to-many filter on its rows", cat: true, mutate: func(d *Definition) {
			d.Columns[2].Measure = &Measure{Field: "moves.distance", Agg: AggSum, Filter: And(Where("moves.loaded", OpEq, true))}
		}},
		{name: "to-many calc filter scope", cat: true, mutate: func(d *Definition) {
			d.Columns[2].Measure = &Measure{Field: "moves.distance", Agg: AggSum, Filter: And(Filter{Calc: formula.Of("{hazmat}")})}
		}, ptr: "/columns/2/measure/filter/and/0/calc", msg: "aggregates move rows"},
		{name: "column in measure filter", mutate: func(d *Definition) {
			d.Columns[2].Measure.Filter = And(Filter{Column: "customer", Op: OpEq, Value: "x"})
		}, ptr: "/columns/2/measure/filter/and/0/column", msg: "a measure filter compares fields"},
		{name: "measure filter ok", cat: true, mutate: func(d *Definition) {
			d.Columns[2].Measure.Filter = Or(Where("status", OpIn, []any{"New"}), Where("customer.tier", OpEq, "Gold"))
		}},
	})
}

func TestCalcRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "syntax", mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("[revenue] /") }, ptr: "/columns/4/calc", msg: "1:12: expected a value"},
		{name: "unknown column", mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("[revnue] / [loads]") }, ptr: "/columns/4/calc", msg: "unknown column [revnue]; did you mean [revenue]?"},
		{name: "cycle", mutate: func(d *Definition) {
			d.Columns[4].Calc = formula.Of("[other] + 1")
			d.Columns = append(d.Columns, Column{ID: "other", Calc: formula.Of("[rpl] * 2")})
		}, ptr: "/columns/5/calc", msg: "[rpl] refers back to this column"},
		{name: "self", mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("[rpl] + 1") }, ptr: "/columns/4/calc", msg: "[rpl] refers back"},
		{name: "multi param", mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("@statuses") }, ptr: "/columns/4/calc", msg: "takes several values"},
		{name: "unknown param", mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("@windw") }, ptr: "/columns/4/calc", msg: "did you mean @window?"},
		{name: "field types", cat: true, mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("{customer.name} + 1") }, ptr: "/columns/4/calc", msg: "+ needs numbers, but this is string"},
		{name: "unknown field", cat: true, mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("{customer.nme}") }, ptr: "/columns/4/calc", msg: "did you mean {customer.name}?"},
		{name: "field depth", lim: Limits{MaxPathDepth: 1}, mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("{moves.stops.city}") }, ptr: "/columns/4/calc", msg: "unknown field"},
		{name: "always null", mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("null") }, ptr: "/columns/4/calc", msg: "always null"},
		{name: "levels", cat: true, mutate: func(d *Definition) { d.Columns[4].Calc = formula.Of("[revenue] + {weight}") }, ptr: "/columns/4/calc", msg: "{weight} is a value per row"},
		{name: "AST calc", mutate: func(d *Definition) {
			d.Columns[4].Calc = formula.OfNode(formula.Binary("/", formula.ColumnRef("revenue"), formula.ColumnRef("nope")))
		}, ptr: "/columns/4/calc", msg: "unknown column [nope]"},
		{name: "AST shape", mutate: func(d *Definition) {
			d.Columns[4].Calc = formula.OfNode(formula.Binary("/", formula.ColumnRef("revenue"), formula.Number("-1")))
		}, ptr: "/columns/4/calc/args/1", msg: "canonical form"},
		{name: "formula limits", lim: Limits{Formula: formula.Limits{MaxNodes: 2}}, ptr: "/columns/4/calc", msg: "at most 2 terms"},
		{name: "row calc in aggregated report", cat: true, mutate: func(d *Definition) {
			d.Columns[4].Calc = formula.Of("date_diff('day', {pickedUpAt}, {deliveredOn}) > 2")
		}},
		{name: "calc over a dimension and measures", cat: true, mutate: func(d *Definition) {
			d.Columns[4].Calc = formula.Of("if([customer] = 'Acme', [revenue] * 0.9, [revenue])")
		}},
	})
}

func TestWindowRules(t *testing.T) {
	win := func(w Window) func(*Definition) { return addColumn(Column{ID: "w", Window: &w}) }
	runCases(t, []ruleCase{
		{name: "fn unknown", mutate: win(Window{Of: "revenue", Fn: "rnk"}), ptr: "/columns/5/window/fn", msg: `did you mean "rank"?`},
		{name: "of required", mutate: win(Window{Fn: WindowRank}), ptr: "/columns/5/window/of", msg: "choose the window's column"},
		{name: "of unknown", mutate: win(Window{Of: "revenu", Fn: WindowRank}), ptr: "/columns/5/window/of", msg: `did you mean "revenue"?`},
		{name: "of window", mutate: func(d *Definition) {
			d.Columns = append(d.Columns, Column{ID: "w", Window: &Window{Of: "revenue", Fn: WindowRank}}, Column{ID: "w2", Window: &Window{Of: "w", Fn: WindowRank}})
		}, ptr: "/columns/6/window/of", msg: "not another window"},
		{name: "over required", mutate: win(Window{Of: "revenue", Fn: WindowRunningTotal}), ptr: "/columns/5/window/over", msg: "choose the ordering column"},
		{name: "over not taken", mutate: win(Window{Of: "revenue", Fn: WindowPercentOfTotal, Over: "month"}), ptr: "/columns/5/window/over", msg: "does not take an ordering column"},
		{name: "over window", mutate: func(d *Definition) {
			d.Columns = append(d.Columns, Column{ID: "w", Window: &Window{Of: "revenue", Fn: WindowRank}}, Column{ID: "w2", Window: &Window{Of: "revenue", Fn: WindowFirst, Over: "w"}})
		}, ptr: "/columns/6/window/over", msg: "not another window"},
		{name: "over unordered", cat: true, start: listReport, mutate: func(d *Definition) {
			d.Columns = append(d.Columns, Column{ID: "h", Dimension: &Dimension{Field: "hazmat"}}, Column{ID: "w", Window: &Window{Of: "weight", Fn: WindowRunningTotal, Over: "h"}})
		}, ptr: "/columns/3/window/over", msg: "has no order"},
		{name: "partition level", mutate: win(Window{Of: "revenue", Fn: WindowRank, Partition: []string{"loads"}}), ptr: "/columns/5/window/partition/0", msg: "not one"},
		{name: "partition duplicate", mutate: win(Window{Of: "revenue", Fn: WindowRank, Partition: []string{"month", "month"}}), ptr: "/columns/5/window/partition/1", msg: "listed twice"},
		{name: "partition over", mutate: win(Window{Of: "revenue", Fn: WindowFirst, Over: "month", Partition: []string{"month"}}), ptr: "/columns/5/window/partition/0", msg: "cannot also partition"},
		{name: "offset not taken", mutate: win(Window{Of: "revenue", Fn: WindowRank, Offset: 2}), ptr: "/columns/5/window/offset", msg: "rank does not take an offset"},
		{name: "offset range", mutate: win(Window{Of: "revenue", Fn: WindowChange, Over: "month", Offset: -1}), ptr: "/columns/5/window/offset", msg: "from 1 to 366"},
		{name: "size range", mutate: win(Window{Of: "revenue", Fn: WindowMovingAvg, Over: "month", Size: 1}), ptr: "/columns/5/window/size", msg: "from 2 to 366"},
		{name: "size not taken", mutate: win(Window{Of: "revenue", Fn: WindowRank, Size: 3}), ptr: "/columns/5/window/size", msg: "rank does not take a size"},
		{name: "desc not taken", mutate: win(Window{Of: "revenue", Fn: WindowChange, Over: "month", Desc: true}), ptr: "/columns/5/window/desc", msg: "only rank and dense_rank"},
		{name: "numeric of", cat: true, mutate: win(Window{Of: "customer", Fn: WindowRunningTotal, Over: "month"}), ptr: "/columns/5/window/of", msg: "running_total needs a numeric column"},
		{name: "too many", lim: Limits{MaxWindows: 1}, mutate: func(d *Definition) {
			d.Columns = append(d.Columns, Column{ID: "w", Window: &Window{Of: "revenue", Fn: WindowRank}}, Column{ID: "w2", Window: &Window{Of: "revenue", Fn: WindowDenseRank}})
		}, ptr: "/columns", msg: "at most 1 window columns"},
		{name: "of pivoted", mutate: func(d *Definition) {
			withPivot(d)
			d.Columns[1].Dimension.Field = "pickedUpAt"
			d.Columns = append(d.Columns, Column{ID: "w", Window: &Window{Of: "revenue", Fn: WindowRank}})
		}, ptr: "/columns/5/window/of", msg: "spread across the pivot"},
		{name: "every fn", cat: true, mutate: func(d *Definition) {
			for i, w := range []Window{
				{Of: "revenue", Fn: WindowRunningTotal, Over: "month", Partition: []string{"customer"}},
				{Of: "revenue", Fn: WindowPercentOfTotal},
				{Of: "revenue", Fn: WindowRank, Desc: true},
				{Of: "loads", Fn: WindowDenseRank},
				{Of: "revenue", Fn: WindowMovingAvg, Over: "month", Size: 3},
				{Of: "revenue", Fn: WindowChange, Over: "month"},
				{Of: "revenue", Fn: WindowPercentChange, Over: "month", Offset: 12},
				{Of: "customer", Fn: WindowFirst, Over: "month"},
			} {
				d.Columns = append(d.Columns, Column{ID: "w" + string(rune('a'+i)), Window: &w})
			}
		}},
	})
}

func TestFilterRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "no subject", mutate: where(Filter{Op: OpEq, Value: 1}), ptr: "/filters/and/0", msg: "compares a field, a calc or a column"},
		{name: "two subjects", mutate: where(Filter{Field: "status", Column: "customer", Op: OpEq, Value: 1}), ptr: "/filters/and/0", msg: "exactly one of"},
		{name: "and and or", mutate: func(d *Definition) { d.Filters = &Filter{And: []Filter{}, Or: []Filter{}} }, ptr: "/filters", msg: "and or or, not both"},
		{name: "group with leaf", mutate: func(d *Definition) { d.Filters = &Filter{And: []Filter{}, Field: "status"} }, ptr: "/filters", msg: "cannot also be one"},
		{name: "depth", lim: Limits{MaxFilterDepth: 1}, mutate: func(d *Definition) { d.Filters = And(*Or(Where("status", OpEq, "New"))) }, ptr: "/filters/and/0", msg: "nest at most 1 levels"},
		{name: "conditions", lim: Limits{MaxConditions: 1}, mutate: func(d *Definition) {
			d.Filters = And(Where("status", OpEq, "New"), Where("status", OpEq, "New"))
		}, ptr: "/filters", msg: "at most 1 conditions"},
		{name: "op required", mutate: where(Filter{Field: "status", Value: 1}), ptr: "/filters/and/0/op", msg: "choose how the value is compared"},
		{name: "op unknown", mutate: where(Filter{Field: "status", Op: "not_inn", Value: []any{"x"}}), ptr: "/filters/and/0/op", msg: `did you mean "not_in"?`},
		{name: "value and param", mutate: where(Filter{Field: "weight", Op: OpEq, Value: 1, Param: "window"}), ptr: "/filters/and/0", msg: "a value or a parameter, not both"},
		{name: "value required", mutate: where(Filter{Field: "weight", Op: OpEq}), ptr: "/filters/and/0/value", msg: "eq needs a value"},
		{name: "no value taken", mutate: where(Filter{Field: "weight", Op: OpIsNull, Value: 1}), ptr: "/filters/and/0", msg: "is_null takes no value"},
		{name: "in scalar", mutate: where(Where("status", OpIn, "New")), ptr: "/filters/and/0/value", msg: "non-empty list"},
		{name: "in empty", mutate: where(Where("status", OpIn, []string{})), ptr: "/filters/and/0/value", msg: "non-empty list"},
		{name: "between shape", mutate: where(Where("weight", OpBetween, []int{1, 2, 3})), ptr: "/filters/and/0/value", msg: "between takes [from, to]"},
		{name: "days zero", mutate: where(Where("shippedAt", OpLastNDays, 0)), ptr: "/filters/and/0/value", msg: "from 1 to 3650"},
		{name: "days large", mutate: where(Where("shippedAt", OpNextNDays, 4000)), ptr: "/filters/and/0/value", msg: "from 1 to 3650"},
		{name: "text operand", mutate: where(Where("proNumber", OpContains, 5)), ptr: "/filters/and/0/value", msg: "contains compares with text"},
		{name: "list value scalar op", mutate: where(Where("weight", OpEq, []int{1})), ptr: "/filters/and/0/value", msg: "got a list"},
		{name: "param unknown", mutate: where(WhereParam("shippedAt", OpLastNDays, "windw")), ptr: "/filters/and/0/param", msg: `did you mean "window"?`},
		{name: "param multi for eq", mutate: where(WhereParam("status", OpEq, "statuses")), ptr: "/filters/and/0/param", msg: "a single value (for eq)"},
		{name: "param single for in", mutate: where(WhereParam("status", OpIn, "label")), ptr: "/filters/and/0/param", msg: "a multi-value parameter (for in)"},
		{name: "param days type", mutate: where(WhereParam("shippedAt", OpLastNDays, "label")), ptr: "/filters/and/0/param", msg: "an int day count"},
		{name: "param text type", mutate: where(WhereParam("proNumber", OpContains, "window")), ptr: "/filters/and/0/param", msg: "a string pattern"},
		{name: "param type", cat: true, mutate: where(WhereParam("weight", OpEq, "label")), ptr: "/filters/and/0/param", msg: "a int value"},
		{name: "aggregate column in where", mutate: where(Filter{Column: "revenue", Op: OpGt, Value: 1}), ptr: "/filters/and/0/column", msg: "compare it in having"},
		{name: "unknown column", mutate: where(Filter{Column: "custmer", Op: OpEq, Value: "x"}), ptr: "/filters/and/0/column", msg: `did you mean "customer"?`},
		{name: "window column", mutate: func(d *Definition) {
			d.Columns = append(d.Columns, Column{ID: "w", Window: &Window{Of: "revenue", Fn: WindowRank}})
			d.Filters = And(Filter{Column: "w", Op: OpLte, Value: 3})
		}, ptr: "/filters/and/0/column", msg: "use top"},
		{name: "aggregate calc in where", mutate: where(Filter{Calc: formula.Of("[revenue] > 10")}), ptr: "/filters/and/0/calc", msg: "must be a value per row"},
		{name: "calc syntax", mutate: where(Filter{Calc: formula.Of("{a} >")}), ptr: "/filters/and/0/calc", msg: "expected a value"},
		{name: "calc not bool", cat: true, mutate: where(Filter{Calc: formula.Of("{weight} + 1")}), ptr: "/filters/and/0/calc", msg: "must be true or false"},
		{name: "calc compared", cat: true, mutate: where(Filter{Calc: formula.Of("date_diff('day', {pickedUpAt}, {deliveredOn})"), Op: OpGt, Value: 2})},
		{name: "calc op type", cat: true, mutate: where(Filter{Calc: formula.Of("{weight} + 1"), Op: OpContains, Value: "x"}), ptr: "/filters/and/0/op", msg: "contains does not apply to int values"},
		{name: "not filterable", cat: true, mutate: where(Where("notes", OpEq, "x")), ptr: "/filters/and/0/field", msg: "notes cannot be filtered on"},
		{name: "op for type", cat: true, mutate: where(Where("status", OpContains, "N")), ptr: "/filters/and/0/op", msg: `contains does not apply to enum values; use one of "eq" or "ne"`},
		{name: "value type", cat: true, mutate: where(Where("weight", OpEq, "heavy")), ptr: "/filters/and/0/value", msg: "heavy is not a int value"},
		{name: "list value type", cat: true, mutate: where(Where("weight", OpIn, []any{1, "x"})), ptr: "/filters/and/0/value/1", msg: "x is not a int value"},
		{name: "enum value", cat: true, mutate: where(Where("status", OpNotIn, []any{"Canceld"})), ptr: "/filters/and/0/value/0", msg: `unknown value "Canceld"; did you mean "Canceled"?`},
		{name: "date values", cat: true, mutate: func(d *Definition) {
			d.Filters = And(Where("deliveredOn", OpBetween, []any{"2026-01-01", "2026-02-01"}), Where("pickedUpAt", OpGte, "2026-01-01T00:00:00Z"), Where("shippedAt", OpLt, 1767225600))
		}},
		{name: "bad date", cat: true, mutate: where(Where("deliveredOn", OpEq, "Jan 1")), ptr: "/filters/and/0/value", msg: "not a date value"},
		{name: "transform type", cat: true, mutate: where(Filter{Field: "status", Op: OpEq, Value: "New", Transform: &Transform{Op: TransformRound}}), ptr: "/filters/and/0/transform/op", msg: "round applies to numbers"},
		{name: "transform shape", mutate: where(Filter{Field: "status", Op: OpEq, Value: "New", Transform: &Transform{Op: "x"}}), ptr: "/filters/and/0/transform/op", msg: `unknown transform "x"`},
		{name: "row column in where", cat: true, mutate: where(Filter{Column: "month", Op: OpGte, Value: 1767225600})},
	})
}

func TestHavingRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "needs measure", start: listReport, mutate: having(Filter{Column: "pro", Op: OpEq, Value: "x"}), ptr: "/having", msg: "needs a measure"},
		{name: "field", mutate: having(Where("weight", OpGt, 1)), ptr: "/having/and/0/field", msg: "add a measure column for weight"},
		{name: "window", mutate: func(d *Definition) {
			d.Columns = append(d.Columns, Column{ID: "w", Window: &Window{Of: "revenue", Fn: WindowRank}})
			d.Having = And(Filter{Column: "w", Op: OpLte, Value: 3})
		}, ptr: "/having/and/0/column", msg: "use top"},
		{name: "pivoted", mutate: func(d *Definition) {
			withPivot(d)
			d.Having = And(Filter{Column: "revenue", Op: OpGt, Value: 1})
		}, ptr: "/having/and/0/column", msg: "spread across the pivot"},
		{name: "op", mutate: having(Filter{Column: "customer", Op: OpContains, Value: "x"}), ptr: "/having/and/0/op", msg: "having compares with"},
		{name: "transform", mutate: having(Filter{Column: "revenue", Op: OpGt, Value: 1, Transform: &Transform{Op: TransformAbs}}), ptr: "/having/and/0/transform", msg: "cannot carry a transform"},
		{name: "ungrouped field calc", mutate: having(Filter{Calc: formula.Of("{weight} > 1")}), ptr: "/having/and/0/calc", msg: "fields must be aggregated, as in sum({weight})"},
		{name: "window calc", mutate: func(d *Definition) {
			d.Columns = append(d.Columns, Column{ID: "w", Window: &Window{Of: "revenue", Fn: WindowRank}})
			d.Having = And(Filter{Calc: formula.Of("[w] < 3")})
		}, ptr: "/having/and/0/calc", msg: "cannot use them"},
		{name: "aggregate calc", cat: true, mutate: having(Filter{Calc: formula.Of("sum({weight}) > 10 and [revenue] > [rpl]")})},
		{name: "column compare", cat: true, mutate: having(Filter{Column: "revenue", Op: OpBetween, Value: []any{100, 200}})},
		{name: "dimension in having", cat: true, mutate: having(Filter{Column: "customer", Op: OpIn, Value: []any{"Acme"}})},
	})
}

func TestGroupingAndTopRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "groupings need measure", start: listReport, mutate: func(d *Definition) { d.Groupings = &Groupings{Total: true} }, ptr: "/groupings", msg: "need a measure"},
		{name: "subtotal unknown", mutate: func(d *Definition) { d.Groupings = &Groupings{Subtotals: []string{"custmer"}} }, ptr: "/groupings/subtotals/0", msg: `did you mean "customer"?`},
		{name: "subtotal measure", mutate: func(d *Definition) { d.Groupings = &Groupings{Subtotals: []string{"revenue"}} }, ptr: "/groupings/subtotals/0", msg: "visible dimension columns"},
		{name: "subtotal last", mutate: func(d *Definition) { d.Groupings = &Groupings{Subtotals: []string{"month"}} }, ptr: "/groupings/subtotals/0", msg: "last grouping column"},
		{name: "subtotal duplicate", mutate: func(d *Definition) { d.Groupings = &Groupings{Subtotals: []string{"customer", "customer"}} }, ptr: "/groupings/subtotals/1", msg: "listed twice"},
		{name: "subtotals limit", lim: Limits{MaxSubtotals: 1}, mutate: func(d *Definition) { d.Groupings = &Groupings{Subtotals: []string{"customer", "month"}} }, ptr: "/groupings/subtotals", msg: "at most 1 subtotal"},
		{name: "groupings ok", cat: true, mutate: func(d *Definition) { d.Groupings = &Groupings{Subtotals: []string{"customer"}, Total: true} }},
		{name: "top n", mutate: func(d *Definition) { d.Top = &Top{N: N(0), By: "revenue"} }, ptr: "/top/n", msg: "from 1 to 1000"},
		{name: "top n param", mutate: func(d *Definition) { d.Top = &Top{N: CountParam("label"), By: "revenue"} }, ptr: "/top/n", msg: "a single int"},
		{name: "top by required", mutate: func(d *Definition) { d.Top = &Top{N: N(3)} }, ptr: "/top/by", msg: "choose the ranking column"},
		{name: "top by window", mutate: func(d *Definition) {
			d.Columns = append(d.Columns, Column{ID: "w", Window: &Window{Of: "revenue", Fn: WindowRank}})
			d.Top = &Top{N: N(3), By: "w"}
		}, ptr: "/top/by", msg: "not a window"},
		{name: "top by unordered", cat: true, mutate: func(d *Definition) {
			d.Columns[0] = Column{ID: "customer", Dimension: &Dimension{Field: "hazmat"}}
			d.Top = &Top{N: N(3), By: "customer"}
		}, ptr: "/top/by", msg: "has no order"},
		{name: "top per measure", mutate: func(d *Definition) { d.Top = &Top{N: N(3), By: "revenue", Per: []string{"loads"}} }, ptr: "/top/per/0", msg: "not one"},
		{name: "top per duplicate", mutate: func(d *Definition) { d.Top = &Top{N: N(3), By: "revenue", Per: []string{"month", "month"}} }, ptr: "/top/per/1", msg: "listed twice"},
		{name: "top by pivoted", mutate: func(d *Definition) {
			withPivot(d)
			d.Top = &Top{N: N(3), By: "revenue"}
		}, ptr: "/top/by", msg: "spread across the pivot"},
		{name: "top ok", cat: true, mutate: func(d *Definition) {
			d.Top = &Top{N: CountParam("window"), By: "revenue", Per: []string{"month"}, Bottom: true}
		}},
	})
}

func TestPivotRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "needs measure", start: listReport, mutate: func(d *Definition) {
			d.Pivot = &Pivot{Field: "status", Values: Values{List: []string{"New"}}, Columns: []string{"weight"}}
		}, ptr: "/pivot", msg: "needs one"},
		{name: "listed and discovered", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Top = &PivotTop{N: N(3), By: "loads"}
		}, ptr: "/pivot", msg: "exactly one of the two"},
		{name: "neither", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values = Values{}
		}, ptr: "/pivot", msg: "exactly one of the two"},
		{name: "values empty", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values = Values{List: []string{}}
		}, ptr: "/pivot/values", msg: "at least one pivot value"},
		{name: "values duplicate", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values.List = []string{"New", "New"}
		}, ptr: "/pivot/values/1", msg: "listed twice"},
		{name: "values reserved", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values.List = []string{PivotOther}
		}, ptr: "/pivot/values/0", msg: "reserved"},
		{name: "values and param", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values.Param = "statuses"
		}, ptr: "/pivot/values", msg: "not both"},
		{name: "labels", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Labels = []string{"One"}
		}, ptr: "/pivot/labels", msg: "one label per pivot value"},
		{name: "values limit", lim: Limits{MaxPivotValues: 1}, mutate: withPivot, ptr: "/pivot/values", msg: "at most 1 values"},
		{name: "top labels", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values, d.Pivot.Top, d.Pivot.Labels = Values{}, &PivotTop{N: N(3), By: "loads"}, []string{"x"}
		}, ptr: "/pivot/labels", msg: "discovered values are labelled by themselves"},
		{name: "top by dimension", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values, d.Pivot.Top = Values{}, &PivotTop{N: N(3), By: "month"}
		}, ptr: "/pivot/top/by", msg: "not one"},
		{name: "top n", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values, d.Pivot.Top = Values{}, &PivotTop{N: N(0), By: "loads"}
		}, ptr: "/pivot/top/n", msg: "from 1 to 50"},
		{name: "values param single", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values = Values{Param: "label"}
		}, ptr: "/pivot/values", msg: "multi-value parameter"},
		{name: "values param labels", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Values, d.Pivot.Labels = Values{Param: "statuses"}, []string{"x"}
		}, ptr: "/pivot/labels", msg: "not values a parameter supplies"},
		{name: "columns required", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Columns = nil
		}, ptr: "/pivot/columns", msg: "at least one measure"},
		{name: "column dimension", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Columns = []string{"month"}
		}, ptr: "/pivot/columns/0", msg: "not one"},
		{name: "column hidden", mutate: func(d *Definition) {
			withPivot(d)
			d.Columns[2].Hidden = true
		}, ptr: "/pivot/columns/0", msg: "hidden"},
		{name: "field is a dimension", mutate: func(d *Definition) {
			withPivot(d)
			d.Columns[0].Dimension.Field = "status"
		}, ptr: "/pivot/field", msg: "already a dimension column"},
		{name: "width", lim: Limits{MaxPivotColumns: 2}, mutate: withPivot, ptr: "/pivot", msg: "would produce 3 columns"},
		{name: "to-many field", cat: true, mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Field = "moves.loaded"
			d.Pivot.Values.List = []string{"true"}
		}, ptr: "/pivot/field", msg: "to-many"},
		{name: "not groupable", cat: true, mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Field = "notes"
		}, ptr: "/pivot/field", msg: "cannot be pivoted"},
		{name: "calc over pivoted", mutate: func(d *Definition) { withPivot(d) }, ptr: "/columns/4/calc", msg: "must be pivoted too"},
		{name: "pivoted calc", cat: true, mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Columns = append(d.Pivot.Columns, "rpl", "loads")
		}},
		{name: "discovered", cat: true, mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot = &Pivot{Field: "status", Top: &PivotTop{N: N(5), By: "loads"}, Columns: []string{"revenue", "rpl", "loads"}, Other: true}
		}},
		{name: "param values", cat: true, mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot = &Pivot{Field: "status", Values: Values{Param: "statuses"}, Columns: []string{"revenue", "rpl"}}
		}},
	})
}

func TestSortLimitPageRules(t *testing.T) {
	sortBy := func(cols ...string) func(*Definition) {
		return func(d *Definition) {
			for _, c := range cols {
				d.Sort = append(d.Sort, Sort{Column: c})
			}
		}
	}
	pivotSort := func(col string) func(*Definition) {
		return func(d *Definition) {
			withPivot(d)
			d.Pivot.Columns = append(d.Pivot.Columns, "rpl")
			d.Sort = []Sort{{Column: col, Desc: true}}
		}
	}
	runCases(t, []ruleCase{
		{name: "unknown", mutate: sortBy("revnue"), ptr: "/sort/0/column", msg: `did you mean "revenue"?`},
		{name: "duplicate", mutate: sortBy("revenue", "revenue"), ptr: "/sort/1/column", msg: "already sorts by"},
		{name: "limit", lim: Limits{MaxSort: 1}, mutate: sortBy("revenue", "loads"), ptr: "/sort", msg: "at most 1 columns"},
		{name: "cell of unpivoted", mutate: sortBy("revenue:New"), ptr: "/sort/0/column", msg: "not spread across the pivot"},
		{name: "cell value", mutate: pivotSort("revenue:Gone"), ptr: "/sort/0/column", msg: `no value "Gone"`},
		{name: "other cell", mutate: func(d *Definition) {
			pivotSort("revenue:" + PivotOther)(d)
			d.Pivot.Other = false
		}, ptr: "/sort/0/column", msg: "no Other column"},
		{name: "pivoted base", mutate: pivotSort("revenue"), ptr: "/sort/0/column", msg: "sort by one of its cells"},
		{name: "cell ok", mutate: pivotSort("revenue:New")},
		{name: "hidden column ok", mutate: func(d *Definition) {
			d.Columns[3].Hidden = true
			d.Sort = []Sort{{Column: "loads"}}
		}},
		{name: "negative limit", mutate: func(d *Definition) { d.Limit = N(-1) }, ptr: "/limit", msg: "cannot be negative"},
		{name: "limit max", lim: Limits{MaxLimit: 10}, mutate: func(d *Definition) { d.Limit = N(11) }, ptr: "/limit", msg: "from 1 to 10"},
		{name: "limit param", mutate: func(d *Definition) { d.Limit = CountParam("statuses") }, ptr: "/limit", msg: "a single int"},
		{name: "page size", mutate: func(d *Definition) { d.Page = &Page{} }, ptr: "/page/size", msg: "from 1 to 1000"},
		{name: "cursor", mutate: func(d *Definition) { d.Page = &Page{Size: 10, After: "a b"} }, ptr: "/page/after", msg: "not one this report produced"},
		{name: "page ok", mutate: func(d *Definition) { d.Page = &Page{Size: 100, After: "eyJrIjoxfQ=="} }},
	})
}

func TestParameterRules(t *testing.T) {
	param := func(p Parameter) func(*Definition) {
		return func(d *Definition) { d.Parameters = append(d.Parameters, p) }
	}
	runCases(t, []ruleCase{
		{name: "name", mutate: param(Parameter{Type: formula.Int}), ptr: "/parameters/3/name", msg: "name the parameter"},
		{name: "name chars", mutate: param(Parameter{Name: "1x", Type: formula.Int}), ptr: "/parameters/3/name", msg: "not a parameter name"},
		{name: "duplicate", mutate: param(Parameter{Name: "window", Type: formula.Int}), ptr: "/parameters/3/name", msg: "declared twice"},
		{name: "label", mutate: param(Parameter{Name: "p", Type: formula.Int, Label: strings.Repeat("x", 201)}), ptr: "/parameters/3/label", msg: "at most 200"},
		{name: "type", mutate: param(Parameter{Name: "p", Type: "strin"}), ptr: "/parameters/3/type", msg: `did you mean "string"?`},
		{name: "json type", mutate: param(Parameter{Name: "p", Type: formula.JSON}), ptr: "/parameters/3/type", msg: "unknown parameter type"},
		{name: "ref entity", mutate: param(Parameter{Name: "p", Type: formula.Ref}), ptr: "/parameters/3/entity", msg: "names the entity"},
		{name: "entity on non-ref", mutate: param(Parameter{Name: "p", Type: formula.String, Entity: "customer"}), ptr: "/parameters/3/entity", msg: "only a ref parameter"},
		{name: "ref allowed", mutate: param(Parameter{Name: "p", Type: formula.Ref, Entity: "customer", Allowed: []any{"x"}}), ptr: "/parameters/3/allowed", msg: "cannot list allowed values"},
		{name: "ref unknown entity", cat: true, mutate: param(Parameter{Name: "p", Type: formula.Ref, Entity: "custmer"}), ptr: "/parameters/3/entity", msg: `did you mean "customer"?`},
		{name: "allowed type", mutate: param(Parameter{Name: "p", Type: formula.Int, Allowed: []any{1, "x"}}), ptr: "/parameters/3/allowed/1", msg: "x is not a int value"},
		{name: "allowed duplicate", mutate: param(Parameter{Name: "p", Type: formula.String, Allowed: []any{"a", "a"}}), ptr: "/parameters/3/allowed/1", msg: "allowed twice"},
		{name: "default type", mutate: param(Parameter{Name: "p", Type: formula.Bool, Default: "yes"}), ptr: "/parameters/3/default", msg: "yes is not a bool value"},
		{name: "multi default", mutate: param(Parameter{Name: "p", Type: formula.String, Multi: true, Default: "a"}), ptr: "/parameters/3/default", msg: "default is a list"},
		{name: "default allowed", mutate: param(Parameter{Name: "p", Type: formula.String, Multi: true, Default: []any{"c"}, Allowed: []any{"a", "b"}}), ptr: "/parameters/3/default", msg: "not one of the allowed values"},
		{name: "limit", lim: Limits{MaxParameters: 2}, ptr: "/parameters", msg: "at most 2 parameters"},
		{name: "ok", cat: true, mutate: func(d *Definition) {
			d.Parameters = append(d.Parameters,
				Parameter{Name: "cust", Type: formula.Ref, Entity: "customer", Required: true},
				Parameter{Name: "since", Type: formula.Date, Default: "2026-01-01"},
				Parameter{Name: "rate", Type: formula.Decimal, Default: "1.25", Allowed: []any{"1.25", 2}},
				Parameter{Name: "tiers", Type: formula.Enum, Multi: true, Default: []string{"Gold"}, Allowed: []any{"Gold", "Silver"}})
		}},
	})
}

func TestChartRules(t *testing.T) {
	runCases(t, []ruleCase{
		{name: "too many", mutate: func(d *Definition) { d.Charts = make([]Chart, 7) }, ptr: "/charts", msg: "at most 6 charts"},
		{name: "id", mutate: chart(Chart{Type: ChartBar}), ptr: "/charts/0/id", msg: "give the chart an id"},
		{name: "id length", mutate: chart(Chart{ID: strings.Repeat("c", 65), Type: ChartBar}), ptr: "/charts/0/id", msg: "at most 64"},
		{name: "duplicate", mutate: func(d *Definition) {
			d.Charts = []Chart{{ID: "c", Type: ChartKPI, Series: []string{"revenue"}}, {ID: "c", Type: ChartKPI, Series: []string{"revenue"}}}
		}, ptr: "/charts/1/id", msg: "used twice"},
		{name: "type", mutate: chart(Chart{ID: "c", Type: "bars"}), ptr: "/charts/0/type", msg: `did you mean "bar"?`},
		{name: "x required", mutate: chart(Chart{ID: "c", Type: ChartBar, Series: []string{"revenue"}}), ptr: "/charts/0/x", msg: "choose the column"},
		{name: "x unknown", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "custmer", Series: []string{"revenue"}}), ptr: "/charts/0/x", msg: `did you mean "customer"?`},
		{name: "x measure", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "revenue", Series: []string{"loads"}}), ptr: "/charts/0/x", msg: "groups by a dimension"},
		{name: "x hidden", mutate: func(d *Definition) {
			d.Columns[0].Hidden = true
			d.Charts = []Chart{{ID: "c", Type: ChartBar, X: "customer", Series: []string{"revenue"}}}
		}, ptr: "/charts/0/x", msg: "unknown column"},
		{name: "kpi x", mutate: chart(Chart{ID: "c", Type: ChartKPI, X: "customer", Series: []string{"revenue"}}), ptr: "/charts/0/x", msg: "has no category axis"},
		{name: "scatter x", mutate: chart(Chart{ID: "c", Type: ChartScatter, X: "customer", Series: []string{"revenue"}}), ptr: "/charts/0/x", msg: "cannot be drawn as a value"},
		{name: "map lat", mutate: chart(Chart{ID: "c", Type: ChartMap, Lng: "loads"}), ptr: "/charts/0/lat", msg: "needs a latitude column"},
		{name: "map same", mutate: chart(Chart{ID: "c", Type: ChartMap, Lat: "loads", Lng: "loads"}), ptr: "/charts/0/lng", msg: "cannot be the same column"},
		{name: "map label", mutate: chart(Chart{ID: "c", Type: ChartMap, Lat: "loads", Lng: "revenue", PointLabel: "nope"}), ptr: "/charts/0/pointLabel", msg: "unknown column"},
		{name: "coordinates on bar", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "customer", Series: []string{"revenue"}, Lat: "loads"}), ptr: "/charts/0", msg: "only a map"},
		{name: "series required", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "customer"}), ptr: "/charts/0/series", msg: "at least one value"},
		{name: "series limit", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "customer", Series: make([]string, 13)}), ptr: "/charts/0/series", msg: "at most 12"},
		{name: "single series", mutate: chart(Chart{ID: "c", Type: ChartPie, X: "customer", Series: []string{"revenue", "loads"}}), ptr: "/charts/0/series", msg: "exactly one value"},
		{name: "series duplicate", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "customer", Series: []string{"revenue", "revenue"}}), ptr: "/charts/0/series/1", msg: "listed twice"},
		{name: "series dimension", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "customer", Series: []string{"month"}}), ptr: "/charts/0/series/0", msg: "cannot be drawn as a value"},
		{name: "stacked pie", mutate: chart(Chart{ID: "c", Type: ChartPie, X: "customer", Series: []string{"revenue"}, Stacked: true}), ptr: "/charts/0/stacked", msg: "cannot be stacked"},
		{name: "limit", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "customer", Series: []string{"revenue"}, Limit: 500}), ptr: "/charts/0/limit", msg: "from 0 to 200"},
		{name: "compare on bar", mutate: chart(Chart{ID: "c", Type: ChartBar, X: "customer", Series: []string{"revenue"}, Compare: "loads"}), ptr: "/charts/0/compare", msg: "only a KPI tile"},
		{name: "compare dimension", mutate: chart(Chart{ID: "c", Type: ChartKPI, Series: []string{"revenue"}, Compare: "month"}), ptr: "/charts/0/compare", msg: "cannot be drawn as a value"},
		{name: "goal both", mutate: chart(Chart{ID: "c", Type: ChartKPI, Series: []string{"revenue"}, Goal: &Goal{Value: floatp(1), Column: "loads"}}), ptr: "/charts/0/goal", msg: "not both"},
		{name: "goal column", mutate: chart(Chart{ID: "c", Type: ChartKPI, Series: []string{"revenue"}, Goal: &Goal{Column: "nope"}}), ptr: "/charts/0/goal/column", msg: "unknown column"},
		{name: "every chart", mutate: func(d *Definition) {
			withPivot(d)
			d.Pivot.Columns = append(d.Pivot.Columns, "rpl")
			d.Charts = []Chart{
				{ID: "a", Type: ChartBar, X: "customer", Series: []string{"revenue:New", "revenue:" + PivotOther, "loads"}, Stacked: true},
				{ID: "b", Type: ChartLine, X: "month", Series: []string{"revenue"}, Curved: true},
				{ID: "c", Type: ChartKPI, Series: []string{"loads"}, Compare: "rpl:New", Goal: &Goal{Value: floatp(250000)}},
				{ID: "d", Type: ChartScatter, X: "loads", Series: []string{"rpl:Completed"}},
				{ID: "e", Type: ChartMap, Lat: "loads", Lng: "loads2", PointLabel: "customer"},
				{ID: "f", Type: ChartDonut, X: "customer", Series: []string{"loads"}, Goal: &Goal{Column: "loads2"}},
			}
			d.Columns = append(d.Columns, Column{ID: "loads2", Measure: &Measure{Agg: AggCount}})
		}},
	})
}
