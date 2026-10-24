package analytics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

func TestColumnKinds(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "pro", "dimension": {"field": "proNumber"}},
		{"id": "status", "dimension": {"field": "status"}},
		{"id": "priority", "dimension": {"field": "priority"}},
		{"id": "weight", "dimension": {"field": "weight"}},
		{"id": "total", "dimension": {"field": "total"}},
		{"id": "hazmat", "dimension": {"field": "hazmat"}},
		{"id": "labels", "dimension": {"field": "labels"}},
		{"id": "created", "dimension": {"field": "createdAt"}},
		{"id": "rank", "dimension": {"field": "stageRank"}},
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "origin", "dimension": {"field": "originStop.location.city"}},
		{"id": "arrived", "dimension": {"field": "originStop.arrivedAt"}}
	], "filters": {"field": "proNumber", "op": "in", "value": ["P02", "P03", "P05"]},
	"sort": [{"column": "pro"}]}`)
	r := runner(t, analytics.RunnerOptions{})
	res, err := r.Preview(w.ctx, analytics.Request{Definition: d})
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0)
	for _, c := range res.Columns() {
		types = append(types, c.Type.String())
	}
	if got := strings.Join(types, ","); got != "string,enum,enum,int,decimal,bool,json,epoch,int,string,string,time" {
		t.Fatalf("types %s", got)
	}
	want(t, rows(t, res),
		`P02 | Assigned | 1 | 2000 | 200.25 | true | ["y"] | 1769902200 | 2 | Acme | Austin | 2026-02-01T00:30:00Z`,
		`P03 | InTransit | 2 | NULL | 300 | false | [] | 1769914800 | 3 | Acme | Dallas | 2026-02-01T04:00:00Z`,
		`P05 | Delivered | 1 | 500 | 50.75 | true | ["x"] | 1771063200 | 4 | Globex | NULL | NULL`)
}

func TestBucketsInTimeZones(t *testing.T) {
	w := seedWorld(t)
	months := def(t, `{"entity": "shipment", "columns": [
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "n", "measure": {"agg": "count"}}], "sort": [{"column": "month"}]}`)
	want(t, preview(t, w, months, nil, nil),
		"1767225600 | 2", "1769904000 | 3", "1772323200 | 4", "1775001600 | 2", "1777593600 | 1")
	want(t, preview(t, w, months, nil, location(t, "America/Chicago")),
		"1767247200 | 3", "1769925600 | 3", "1772344800 | 3", "1775019600 | 2", "1777611600 | 1")
	days := def(t, `{"entity": "shipment", "columns": [
		{"id": "day", "dimension": {"field": "createdAt", "bucket": {"param": "grain"}}},
		{"id": "n", "measure": {"agg": "count"}}],
		"filters": {"field": "createdAt", "op": "between", "value": ["2026-03-01T00:00:00Z", "2026-03-10T00:00:00Z"]},
		"parameters": [{"name": "grain", "type": "string", "default": "day", "allowed": ["day", "week", "month"]}],
		"sort": [{"column": "day"}]}`)
	want(t, preview(t, w, days, nil, location(t, "America/New_York")), "1772341200 | 1", "1772946000 | 3")
	want(t, preview(t, w, days, nil, nil), "1772323200 | 1", "1772928000 | 2", "1773014400 | 1")
	want(t, preview(t, w, days, map[string]any{"grain": "week"}, nil), "1771804800 | 1", "1772409600 | 2", "1773014400 | 1")
	stops := def(t, `{"entity": "stop", "columns": [
		{"id": "day", "dimension": {"field": "arrivedAt", "bucket": "day"}},
		{"id": "n", "measure": {"agg": "count"}}],
		"filters": {"field": "arrivedAt", "op": "between", "value": ["2026-02-01T00:00:00Z", "2026-02-01T02:00:00Z"]}}`)
	want(t, preview(t, w, stops, nil, nil), "2026-02-01T00:00:00Z | 2")
	want(t, preview(t, w, stops, nil, location(t, "America/Chicago")), "2026-01-31T06:00:00Z | 2")
}

func TestBands(t *testing.T) {
	w := seedWorld(t)
	width := def(t, `{"entity": "shipment", "columns": [
		{"id": "band", "dimension": {"field": "weight", "band": {"width": 1000}}},
		{"id": "n", "measure": {"agg": "count"}}], "sort": [{"column": "band"}]}`)
	want(t, preview(t, w, width, nil, nil), "0 | 3", "1000 | 3", "2000 | 1", "4000 | 1", "7000 | 1", "NULL | 3")
	edges := def(t, `{"entity": "shipment", "columns": [
		{"id": "band", "dimension": {"field": "weight", "band": {"edges": [0, 1000, 5000]}}},
		{"id": "n", "measure": {"agg": "count"}}], "sort": [{"column": "band"}]}`)
	want(t, preview(t, w, edges, nil, nil), "0 | 3", "1000 | 5", "5000 | 1", "NULL | 3")
	totals := def(t, `{"entity": "shipment", "columns": [
		{"id": "band", "dimension": {"field": "total", "band": {"width": 250}}},
		{"id": "n", "measure": {"agg": "count"}}], "sort": [{"column": "band"}]}`)
	want(t, preview(t, w, totals, nil, nil), "0 | 9", "250 | 2", "500 | 1")
}

func TestTransforms(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}, "transform": {"op": "upper"}},
		{"id": "rounded", "measure": {"field": "total", "agg": "sum"}, "transform": {"op": "round", "precision": 0}},
		{"id": "cents", "measure": {"field": "total", "agg": "sum"}, "transform": {"op": "scale", "factor": 100}},
		{"id": "ceil", "measure": {"field": "total", "agg": "avg"}, "transform": {"op": "ceil", "precision": 1}}
	], "filters": {"field": "customer.code", "op": "ne", "value": "x", "transform": {"op": "lower"}},
	"sort": [{"column": "customer"}]}`)
	want(t, preview(t, w, d, nil, nil),
		"ACME | 711 | 71075 | 177.7", "GLOBEX | 571 | 57075 | 142.7", "INITECH | 883 | 88250 | 220.7")
	lower := def(t, `{"entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}],
		"filters": {"field": "customer.code", "op": "eq", "value": "a", "transform": {"op": "lower"}}}`)
	want(t, preview(t, w, lower, nil, nil), "4")
}

func TestMeasureFilters(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "all", "measure": {"agg": "count"}},
		{"id": "hazmat", "measure": {"agg": "count", "filter": {"field": "hazmat", "op": "eq", "value": true}}},
		{"id": "delivered", "measure": {"field": "total", "agg": "sum", "filter": {"field": "status", "op": "in", "value": ["Delivered"]}}},
		{"id": "routed", "measure": {"agg": "count", "filter": {"field": "moves.stops.location.city", "op": "eq", "value": "Denver"}}}
	], "sort": [{"column": "customer"}]}`)
	want(t, preview(t, w, d, nil, nil), "Acme | 4 | 1 | 110 | 3", "Globex | 4 | 1 | 570.75 | 3", "Initech | 4 | 1 | NULL | 0")
}

// manyMeasures is a report with measures over the entity and across its to-many paths.
const manyMeasures = `{"entity": "shipment", "columns": [
	{"id": "customer", "dimension": {"field": "customer.name"}},
	{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
	{"id": "moves", "measure": {"field": "moves.id", "agg": "count"}},
	{"id": "distinctMoves", "measure": {"field": "moves.id", "agg": "count_distinct"}},
	{"id": "maxSeq", "measure": {"field": "moves.sequence", "agg": "max"}},
	{"id": "stops", "measure": {"field": "moves.stops.id", "agg": "count"}},
	{"id": "avgStop", "measure": {"field": "moves.stops.sequence", "agg": "avg"}},
	{"id": "sumStop", "measure": {"field": "moves.stops.sequence", "agg": "sum"}},
	{"id": "pickups", "measure": {"field": "moves.stops.id", "agg": "count", "filter": {"field": "moves.stops.kind", "op": "eq", "value": "pickup"}}}
], "sort": [{"column": "customer"}]}`

func TestToManyMeasuresGiveTheSameResultWithEveryStrategy(t *testing.T) {
	w := seedWorld(t)
	d := def(t, manyMeasures)
	expect := []string{"Acme | 710.75 | 5 | 5 | 2 | 10 | 1.5 | 15 | 5",
		"Globex | 570.75 | 5 | 5 | 3 | 10 | 1.5 | 15 | 5",
		"Initech | 882.5 | 4 | 4 | 2 | 8 | 1.5 | 12 | 4"}
	cat := catalog(t)
	for _, s := range []analytics.Strategy{analytics.StrategyAuto, analytics.StrategyCTE, analytics.StrategyCTEAll, analytics.StrategyLateral} {
		r := runner(t, analytics.RunnerOptions{Strategy: s})
		res, err := r.Preview(w.ctx, analytics.Request{Definition: d})
		if err != nil {
			t.Fatal(err)
		}
		want(t, rows(t, res), expect...)
		comp := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, cat, nil, d, nil, analytics.CompileOptions{Strategy: s}))
		sql, _, err := comp.ToSQL(w.ctx, db(t))
		if err != nil {
			t.Fatal(err)
		}
		lateral := strings.Contains(sql, "LEFT JOIN LATERAL (SELECT")
		if lateral != (s == analytics.StrategyLateral) || strings.Contains(sql, "keel_cte") {
			t.Fatalf("strategy %d:\n%s", s, sql)
		}
		filtered := def(t, strings.Replace(manyMeasures, `"sort"`, `"filters": {"field": "status", "op": "ne", "value": "Canceled"}, "sort"`, 1))
		comp = must[*analytics.Compiled](t)(analytics.Compile(w.ctx, cat, nil, filtered, nil, analytics.CompileOptions{Strategy: s}))
		sql, _, _ = comp.ToSQL(w.ctx, db(t))
		if restricted := strings.Contains(sql, `") IN (SELECT`); restricted != (s == analytics.StrategyAuto || s == analytics.StrategyCTE) {
			t.Fatalf("strategy %d restricts the parents: %v\n%s", s, restricted, sql)
		}
		res, err = r.Preview(w.ctx, analytics.Request{Definition: filtered})
		if err != nil {
			t.Fatal(err)
		}
		want(t, rows(t, res), expect[0], "Globex | 570.75 | 4 | 4 | 3 | 8 | 1.5 | 12 | 4", expect[2])
	}
	pinned := def(t, `{"entity": "shipment", "columns": [{"id": "pro", "dimension": {"field": "proNumber"}},
		{"id": "stops", "measure": {"field": "moves.stops.id", "agg": "count"}}],
		"filters": {"field": "id", "op": "in", "value": ["shp_P01", "shp_P04"]}, "sort": [{"column": "pro"}]}`)
	comp := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, cat, nil, pinned, nil, analytics.CompileOptions{}))
	sql, _, _ := comp.ToSQL(w.ctx, db(t))
	if !strings.Contains(sql, "LEFT JOIN LATERAL") {
		t.Fatalf("a pinned key does not read laterally:\n%s", sql)
	}
	want(t, preview(t, w, pinned, nil, nil), "P01 | 4", "P04 | 6")
}

func TestCountDistinctAcrossToMany(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "cities", "measure": {"field": "moves.stops.location.city", "agg": "count_distinct"}},
		{"id": "denver", "measure": {"field": "moves.stops.location.city", "agg": "count_distinct", "filter": {"field": "moves.stops.location.city", "op": "eq", "value": "Denver"}}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "perCity", "calc": "round([revenue] / [cities], 2)"}
	], "groupings": {"total": true}, "sort": [{"column": "customer"}]}`)
	want(t, preview(t, w, d, nil, nil),
		"Acme | 3 | 1 | 710.75 | 236.92", "Globex | 3 | 1 | 570.75 | 190.25", "Initech | 2 | 0 | 882.5 | 441.25",
		"TOTAL NULL | 3 | 1 | 2164 | 721.33")
	comp := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, catalog(t), nil, d, nil, analytics.CompileOptions{}))
	sql, _, _ := comp.ToSQL(w.ctx, db(t))
	if !strings.Contains(sql, `count(DISTINCT`) || !strings.Contains(sql, `FROM "stops"`) {
		t.Fatalf("no fan-out distinct count:\n%s", sql)
	}
}

func TestFormulas(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "pro", "dimension": {"field": "proNumber"}},
		{"id": "heavy", "calc": "if({weight} > 1500, 'heavy', 'light')"},
		{"id": "tag", "calc": "concat({proNumber}, '-', {customer.code})"},
		{"id": "w", "calc": "coalesce({weight}, 0)"},
		{"id": "month", "calc": "extract('month', {createdAt})"},
		{"id": "age", "calc": "date_diff('day', {createdAt}, @asOf)"},
		{"id": "size", "calc": "case when {total} >= 300 then 'large' when {total} > 0 then 'small' else 'none' end"},
		{"id": "status", "calc": "lower({status})"}
	], "filters": {"and": [{"field": "proNumber", "op": "in", "value": ["P01", "P03", "P06"]},
		{"calc": "{total} >= 0"}]},
	"parameters": [{"name": "asOf", "type": "timestamp", "default": "2026-02-01T00:00:00Z"}],
	"sort": [{"column": "pro"}]}`)
	want(t, preview(t, w, d, nil, location(t, "America/Chicago")),
		"P01 | light | P01-A | 1000 | 1 | 16 | small | new",
		"P03 | light | P03-A | 0 | 1 | 0 | large | intransit",
		"P06 | light | P06-G | 0 | 2 | -28 | none | canceled")
	agg := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "perShipment", "calc": "sum({total}) / count()"},
		{"id": "perMove", "calc": "round(sum({total}) / sum({moves.sequence}), 2)"},
		{"id": "scaled", "calc": "[revenue] * @factor"}
	], "parameters": [{"name": "factor", "type": "decimal", "default": 2}], "sort": [{"column": "customer"}]}`)
	want(t, preview(t, w, agg, nil, nil),
		"Acme | 710.75 | 177.6875 | 118.46 | 1421.5",
		"Globex | 570.75 | 142.6875 | 71.34 | 1141.5",
		"Initech | 882.5 | 220.625 | 176.5 | 1765")
}

func TestWindows(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "running", "window": {"of": "revenue", "fn": "running_total", "over": "month"}},
		{"id": "rank", "window": {"of": "revenue", "fn": "rank", "desc": true}},
		{"id": "dense", "window": {"of": "revenue", "fn": "dense_rank"}},
		{"id": "mavg", "window": {"of": "revenue", "fn": "moving_avg", "over": "month", "size": 2}},
		{"id": "chg", "window": {"of": "revenue", "fn": "change", "over": "month"}},
		{"id": "pchg", "hidden": true, "window": {"of": "revenue", "fn": "percent_change", "over": "month"}},
		{"id": "pct", "calc": "round([pchg] * 100, 1)"},
		{"id": "last", "window": {"of": "revenue", "fn": "last", "over": "month"}},
		{"id": "share", "window": {"of": "revenue", "fn": "percent_of_total"}}
	], "sort": [{"column": "month"}]}`)
	got := preview(t, w, d, nil, nil)
	for i := range got {
		cells := strings.Split(got[i], " | ")
		cells[len(cells)-1] = cells[len(cells)-1][:min(6, len(cells[len(cells)-1]))]
		got[i] = strings.Join(cells, " | ")
	}
	want(t, got,
		"1767225600 | 300.75 | 300.75 | 3 | 3 | 300.75 | NULL | NULL | 12.4 | 0.1389",
		"1769904000 | 750.75 | 1051.5 | 2 | 4 | 525.75 | 450 | 149.6 | 12.4 | 0.3469",
		"1772323200 | 870.1 | 1921.6 | 1 | 5 | 810.425 | 119.35 | 15.9 | 12.4 | 0.4020",
		"1775001600 | 230 | 2151.6 | 4 | 2 | 550.05 | -640.1 | -73.6 | 12.4 | 0.1062",
		"1777593600 | 12.4 | 2164 | 5 | 1 | 121.2 | -217.6 | -94.6 | 12.4 | 0.0057")
}

func TestSubtotalsAndTotalMatchTheRows(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "status", "dimension": {"field": "status"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "n", "measure": {"agg": "count"}},
		{"id": "moves", "measure": {"field": "moves.id", "agg": "count"}}
	], "groupings": {"subtotals": ["customer"], "total": true}, "sort": [{"column": "customer"}, {"column": "status"}]}`)
	r := runner(t, analytics.RunnerOptions{PreviewRows: 100})
	res, err := r.Preview(w.ctx, analytics.Request{Definition: d})
	if err != nil {
		t.Fatal(err)
	}
	tbl := must[*table.Table](t)(table.Collect(w.ctx, res))
	type sums struct{ revenue, n, moves float64 }
	detail, sub := map[string]*sums{}, map[string]*sums{}
	var total, all sums
	for _, b := range tbl.Batches {
		for i := range b.Len {
			cust := b.Columns[0].String[i]
			rev, _ := b.Columns[2].Numeric[i].Float64()
			s := sums{rev, float64(b.Columns[3].Int64[i]), float64(b.Columns[4].Int64[i])}
			switch b.Level(i) {
			case table.Detail:
				if detail[cust] == nil {
					detail[cust] = &sums{}
				}
				detail[cust].revenue += s.revenue
				detail[cust].n += s.n
				detail[cust].moves += s.moves
				all.revenue, all.n, all.moves = all.revenue+s.revenue, all.n+s.n, all.moves+s.moves
			case table.Subtotal(1):
				sub[cust] = &s
			case table.Total:
				total = s
			}
		}
	}
	if len(sub) != 3 || total.n != 12 || total.moves != 14 {
		t.Fatalf("subtotals %v, total %+v", sub, total)
	}
	for cust, s := range sub {
		if d := detail[cust]; d.n != s.n || d.moves != s.moves || d.revenue-s.revenue > 1e-9 || s.revenue-d.revenue > 1e-9 {
			t.Fatalf("%s: rows %+v, subtotal %+v", cust, *d, *s)
		}
	}
	if all.n != total.n || all.moves != total.moves {
		t.Fatalf("rows %+v, total %+v", all, total)
	}
	got := rows(t, tbl.Reader())
	if got[len(got)-1] != "TOTAL NULL | NULL | 2164 | 12 | 14" || got[4] != "SUB1 Acme | NULL | 710.75 | 4 | 5" {
		t.Fatalf("rows\n%s", strings.Join(got, "\n"))
	}
}

func TestTopNPerGroup(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}}
	], "top": {"n": {"param": "n"}, "by": "revenue", "per": ["month"]},
	"parameters": [{"name": "n", "type": "int", "default": 1}], "sort": [{"column": "month"}]}`)
	want(t, preview(t, w, d, nil, nil),
		"1767225600 | Acme | 300.75", "1769904000 | Globex | 450.75", "1772323200 | Initech | 870.1",
		"1775001600 | Globex | 120", "1777593600 | Initech | 12.4")
	bottom := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}}
	], "top": {"n": 2, "by": "revenue", "bottom": true}, "groupings": {"total": true}, "sort": [{"column": "revenue"}]}`)
	want(t, preview(t, w, bottom, nil, nil), "Globex | 570.75", "Acme | 710.75", "TOTAL NULL | 2164")
}

func TestHaving(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "n", "measure": {"agg": "count"}}
	], "having": {"or": [{"column": "revenue", "op": "gt", "value": 800}, {"calc": "sum({total}) < 600"}]},
	"groupings": {"total": true}, "sort": [{"column": "customer"}]}`)
	want(t, preview(t, w, d, nil, nil), "Globex | 570.75 | 4", "Initech | 882.5 | 4", "TOTAL NULL | 2164 | 12")
}

func TestPivots(t *testing.T) {
	w := seedWorld(t)
	static := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "n", "measure": {"agg": "count"}}
	], "pivot": {"field": "status", "values": ["Delivered", "New"], "labels": ["Done", "Fresh"], "columns": ["revenue"], "other": true},
	"sort": [{"column": "revenue:Delivered", "desc": true}]}`)
	r := runner(t, analytics.RunnerOptions{})
	res, err := r.Preview(w.ctx, analytics.Request{Definition: static})
	if err != nil {
		t.Fatal(err)
	}
	var ids, labels []string
	for _, c := range res.Columns() {
		ids, labels = append(ids, c.ID), append(labels, c.Label)
	}
	if strings.Join(ids, ",") != "customer,revenue:Delivered,revenue:New,revenue:__other__,n" ||
		labels[1] != "Sum of Total · Done" || labels[3] != "Sum of Total · Other" {
		t.Fatalf("columns %v %v", ids, labels)
	}
	want(t, rows(t, res), "Globex | 570.75 | NULL | 0 | 4", "Acme | 110 | 100.5 | 500.25 | 4", "Initech | NULL | 712.4 | 170.1 | 4")
	dynamic := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "perShipment", "calc": "round([revenue] / count(), 2)"}
	], "pivot": {"field": "status", "top": {"n": 2, "by": "revenue"}, "columns": ["revenue", "perShipment"], "other": true},
	"sort": [{"column": "customer"}]}`)
	res, err = r.Preview(w.ctx, analytics.Request{Definition: dynamic})
	if err != nil {
		t.Fatal(err)
	}
	ids = ids[:0]
	for _, c := range res.Columns() {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "customer,revenue:New,revenue:Delivered,revenue:__other__,perShipment:New,perShipment:Delivered,perShipment:__other__" {
		t.Fatalf("columns %v", ids)
	}
	want(t, rows(t, res),
		"Acme | 100.5 | 110 | 500.25 | 100.5 | 110 | 250.13",
		"Globex | NULL | 570.75 | 0 | NULL | 190.25 | 0",
		"Initech | 712.4 | NULL | 170.1 | 356.2 | NULL | 85.05")
}

func TestPicksGroupLanes(t *testing.T) {
	w := seedWorld(t)
	d := def(t, lanes)
	want(t, preview(t, w, d, nil, nil), laneRows...)
}

// lanes groups shipments by their origin and destination cities, through picks.
const lanes = `{"entity": "shipment", "columns": [
	{"id": "origin", "dimension": {"field": "originStop.location.city"}},
	{"id": "dest", "dimension": {"field": "destinationStop.location.city"}},
	{"id": "n", "measure": {"agg": "count"}}
], "sort": [{"column": "n", "desc": true}]}`

// laneRows are the rows of lanes over the fixture.
var laneRows = []string{"Austin | Denver | 3", "Austin | Dallas | 2", "NULL | NULL | 2", "Dallas | Austin | 1",
	"Dallas | Dallas | 1", "Dallas | Denver | 1", "Denver | Austin | 1", "Denver | Denver | 1"}

func TestPagingAndPreviewTruncation(t *testing.T) {
	w := seedWorld(t)
	d := def(t, lanes)
	r := runner(t, analytics.RunnerOptions{PreviewRows: 3})
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		res, err := r.Page(w.ctx, analytics.Request{Definition: d}, cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, rows(t, res)...)
		cursor = res.Cursor()
		if cursor == "" {
			if pages != 2 {
				t.Fatalf("%d pages", pages+1)
			}
			break
		}
	}
	want(t, got, laneRows...)
	if _, err := r.Page(w.ctx, analytics.Request{Definition: d}, "not-a-cursor", 3); analytics.ErrorCode(err) != analytics.CodeCursor {
		t.Fatalf("bad cursor: %v", err)
	}
	res, err := r.Preview(w.ctx, analytics.Request{Definition: d})
	if err != nil {
		t.Fatal(err)
	}
	want(t, rows(t, res), laneRows[:3]...)
	if !res.Truncated() {
		t.Fatal("a cut preview is not truncated")
	}
	limited := def(t, strings.Replace(lanes, `"sort"`, `"limit": {"param": "top"}, "parameters": [{"name": "top", "type": "int"}], "sort"`, 1))
	res, err = r.Preview(w.ctx, analytics.Request{Definition: limited, Params: map[string]any{"top": 8}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Len() != 3 || !res.Truncated() {
		t.Fatalf("%d rows, truncated %v", res.Len(), res.Truncated())
	}
	r = runner(t, analytics.RunnerOptions{})
	res, err = r.Preview(w.ctx, analytics.Request{Definition: limited, Params: map[string]any{"top": 8}})
	if err != nil || res.Len() != 8 || res.Truncated() {
		t.Fatalf("%v rows, truncated %v, %v", res.Len(), res.Truncated(), err)
	}
	n, err := r.Count(w.ctx, analytics.Request{Definition: d})
	if err != nil || n != 8 {
		t.Fatalf("count %d %v", n, err)
	}
}

func TestDrillThrough(t *testing.T) {
	w := seedWorld(t)
	chicago := location(t, "America/Chicago")
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "hazmat", "measure": {"agg": "count", "filter": {"field": "hazmat", "op": "eq", "value": true}}}
	], "filters": {"field": "status", "op": "ne", "value": "Canceled"}}`)
	r := runner(t, analytics.RunnerOptions{})
	req := analytics.Request{Definition: d, Location: chicago}
	res, err := r.Drill(w.ctx, req, []ir.DrillValue{{Column: "month", Value: int64(1767247200)}, {Column: "customer", Value: "Acme"}},
		"", analytics.DrillOptions{Columns: proColumns()})
	if err != nil {
		t.Fatal(err)
	}
	want(t, sortedRows(t, res), "P01 | Acme", "P02 | Acme", "P03 | Acme")
	res, err = r.Drill(w.ctx, req, []ir.DrillValue{{Column: "month", Value: time.Unix(1767247200, 0)}}, "hazmat",
		analytics.DrillOptions{Columns: proColumns()})
	if err != nil {
		t.Fatal(err)
	}
	want(t, sortedRows(t, res), "P02 | Acme")
}

// proColumns is a record list of PRO numbers.
func proColumns() []ir.Column {
	return []ir.Column{{ID: "pro", Dimension: &ir.Dimension{Field: "proNumber"}}}
}

// sortedRows returns a result's rows sorted.
func sortedRows(t testing.TB, r table.Reader) []string {
	got := rows(t, r)
	for i := range got {
		for j := i + 1; j < len(got); j++ {
			if got[j] < got[i] {
				got[i], got[j] = got[j], got[i]
			}
		}
	}
	return got
}

func TestValueTypesOfComputedColumns(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "hazmat", "dimension": {"field": "hazmat"}},
		{"id": "avgWeight", "measure": {"field": "weight", "agg": "avg"}},
		{"id": "maxCreated", "measure": {"field": "createdAt", "agg": "max"}},
		{"id": "weights", "measure": {"field": "weight", "agg": "sum"}}
	], "sort": [{"column": "hazmat"}]}`)
	r := runner(t, analytics.RunnerOptions{})
	res, err := r.Preview(w.ctx, analytics.Request{Definition: d})
	if err != nil {
		t.Fatal(err)
	}
	if res.Columns()[1].Type != value.Decimal || res.Columns()[2].Type != value.Epoch || res.Columns()[3].Type != value.Int {
		t.Fatalf("columns %+v", res.Columns())
	}
	want(t, rows(t, res), "false | 2516.6666666666666667 | 1777982400 | 15100", "true | 1133.3333333333333333 | 1773027000 | 3400")
}
