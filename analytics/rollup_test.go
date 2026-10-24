package analytics_test

import (
	"context"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// shipmentsDaily is a rollup of shipments by tenant, UTC day, customer and status.
var shipmentsDaily = func() *schema.Table {
	t := &schema.Table{Name: "test_shipments_daily", Model: "test.ShipmentsDaily", Columns: []schema.Column{
		{Name: "organization_id", Kind: schema.KindString, Flags: schema.FlagPK | schema.FlagTenant},
		{Name: "business_unit_id", Kind: schema.KindString, Flags: schema.FlagPK | schema.FlagTenant},
		{Name: "day", Kind: schema.KindInt64, Flags: schema.FlagPK},
		{Name: "customer_id", Kind: schema.KindString, Flags: schema.FlagPK},
		{Name: "status", Kind: schema.KindString, Flags: schema.FlagPK},
		{Name: "total_sum", Kind: schema.KindOther},
		{Name: "n", Kind: schema.KindInt64},
	}}
	schema.Derive(t)
	return t
}()

// fakeRouter answers reports grouped by created day buckets and status from shipmentsDaily.
type fakeRouter struct{ asked int }

func (f *fakeRouter) Route(_ context.Context, res *ir.Resolved) (analytics.RollupPlan, bool) {
	f.asked++
	if res.Definition.Entity != "shipment" {
		return analytics.RollupPlan{}, false
	}
	plan := analytics.RollupPlan{Name: "shipments_daily", Table: shipmentsDaily,
		Fields: map[string]string{"createdAt": "day", "status": "status"}, Measures: map[string]analytics.RollupMeasure{}}
	for _, c := range res.Definition.Columns {
		if m := c.Measure; m != nil {
			switch {
			case m.Agg == ir.AggCount && m.Field == "":
				plan.Measures[c.ID] = analytics.RollupMeasure{Agg: analytics.ReSum, Column: "n"}
			case m.Agg == ir.AggSum && m.Field == "total":
				plan.Measures[c.ID] = analytics.RollupMeasure{Agg: analytics.ReSum, Column: "total_sum"}
			case m.Agg == ir.AggAvg && m.Field == "total":
				plan.Measures[c.ID] = analytics.RollupMeasure{Agg: analytics.ReAvg, Column: "total_sum", Count: "n"}
			}
		}
	}
	return plan, true
}

func TestRollupAnswersWhatItCovers(t *testing.T) {
	w := seedWorld(t)
	d := db(t)
	_, err := d.Pool().Exec(context.Background(), `CREATE TABLE IF NOT EXISTS test_shipments_daily (
		organization_id text, business_unit_id text, day bigint, customer_id text, status text,
		total_sum numeric, n bigint, PRIMARY KEY (organization_id, business_unit_id, day, customer_id, status))`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Pool().Exec(context.Background(), `INSERT INTO test_shipments_daily SELECT organization_id, business_unit_id, created_at / 86400 * 86400,
		customer_id, status::text, sum(total), count(*) FROM shipments WHERE organization_id = $1
		GROUP BY 1, 2, 3, 4, 5`, w.org)
	if err != nil {
		t.Fatal(err)
	}
	router := &fakeRouter{}
	cat := catalog(t)
	report := def(t, `{"entity": "shipment", "columns": [
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "n", "measure": {"agg": "count"}},
		{"id": "avg", "measure": {"field": "total", "agg": "avg"}, "transform": {"op": "round", "precision": 2}},
		{"id": "perShipment", "calc": "round([revenue] / [n], 2)"}
	], "filters": {"field": "status", "op": "ne", "value": "Canceled"},
	"pivot": {"field": "status", "values": ["Delivered"], "columns": ["revenue", "perShipment"], "other": true},
	"groupings": {"total": true}, "sort": [{"column": "month"}]}`)
	routed := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, cat, nil, report, nil, analytics.CompileOptions{Rollups: router}))
	sql, _, err := routed.ToSQL(w.ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if routed.Rollup != "shipments_daily" || !strings.Contains(sql, `FROM "test_shipments_daily"`) ||
		strings.Contains(sql, `"shipments"`) || strings.Join(routed.Tables, ",") != "test_shipments_daily" {
		t.Fatalf("not answered from the rollup (%q):\n%s", routed.Rollup, sql)
	}
	r := runner(t, analytics.RunnerOptions{Rollups: router})
	res, err := r.Preview(w.ctx, analytics.Request{Definition: report})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rollup() != "shipments_daily" {
		t.Fatalf("result rollup %q", res.Rollup())
	}
	want(t, rows(t, res), preview(t, w, report, nil, nil)...)
	byCustomer := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}}], "sort": [{"column": "customer"}]}`)
	fallback := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, cat, nil, byCustomer, nil, analytics.CompileOptions{Rollups: router}))
	if fallback.Rollup != "" {
		t.Fatal("a report the rollup does not cover was answered from it")
	}
	asked := router.asked
	ruled := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, cat, &fleetPolicy{asked: map[string]int{}, rows: true},
		report, nil, analytics.CompileOptions{Rollups: router}))
	if ruled.Rollup != "" || router.asked != asked {
		t.Fatal("a report under row rules was answered from a rollup")
	}
}
