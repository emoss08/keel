package rollup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/schema"
)

// report wraps columns and filters in a version 2 shipment definition.
func report(columns, extra string) string {
	return `{"version": 2, "entity": "shipment", "columns": [` + columns + `]` + extra + `}`
}

// path is a field's path in the rollup table, joined with dots.
func path(p *analytics.RollupPlan, field string) string {
	return strings.Join(p.Fields[field].Path, ".")
}

func TestMatchCovers(t *testing.T) {
	now := time.Date(2026, 3, 18, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		def    string
		params map[string]any
		rollup string
		check  func(t *testing.T, p *analytics.RollupPlan)
		from   string
		to     string
	}{
		{
			name: "dimensions, day buckets and composable measures",
			def: report(`{"id": "customer", "dimension": {"field": "customer.id"}},
				{"id": "status", "dimension": {"field": "status"}},
				{"id": "day", "dimension": {"field": "createdAt", "bucket": "day"}},
				{"id": "loads", "measure": {"agg": "count"}},
				{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
				{"id": "avgWeight", "measure": {"field": "weight", "agg": "avg"}},
				{"id": "maxWeight", "measure": {"field": "weight", "agg": "max"}},
				{"id": "rpl", "calc": "[revenue] / nullif([loads], 0)"}`,
				`, "filters": {"and": [{"field": "status", "op": "in", "value": ["New", "Delivered"]}, {"field": "createdAt", "op": "this_month"}]}`),
			rollup: "shipments_daily",
			from:   "2026-03-01T00:00:00Z",
			to:     "2026-04-01T00:00:00Z",
			check: func(t *testing.T, p *analytics.RollupPlan) {
				if path(p, "customer.id") != "customer_id" || path(p, "createdAt") != "bucket" || !p.Fields["createdAt"].Grain ||
					path(p, "status") != "status" || p.Fields["status"].Grain {
					t.Fatalf("fields = %+v", p.Fields)
				}
				want := map[string]analytics.RollupMeasure{
					"loads":     {Agg: analytics.ReSum, Value: "row_count"},
					"revenue":   {Agg: analytics.ReSum, Value: "total_sum"},
					"avgWeight": {Agg: analytics.ReAvg, Value: "weight_sum", Divisor: "weight_count"},
					"maxWeight": {Agg: analytics.ReMax, Value: "weight_max"},
				}
				for id, m := range want {
					if p.Measures[id] != m {
						t.Fatalf("measure %s = %+v, want %+v", id, p.Measures[id], m)
					}
				}
				if p.Unit != ir.BucketDay || p.Location != time.UTC || p.Fresh == nil {
					t.Fatalf("plan = %+v", p)
				}
			},
		},
		{
			name:   "a coarser rollup wins when it covers",
			def:    report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "q", "dimension": {"field": "createdAt", "bucket": "quarter"}}, {"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`, ""),
			rollup: "shipments_monthly",
			check: func(t *testing.T, p *analytics.RollupPlan) {
				if p.Unit != ir.BucketMonth || p.Table != monthly.Table() {
					t.Fatalf("plan = %+v", p)
				}
			},
		},
		{
			name:   "fields of a related row join through the rollup's key",
			def:    report(`{"id": "customer", "dimension": {"field": "customer.name"}}, {"id": "week", "dimension": {"field": "createdAt", "bucket": "week"}}, {"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`, `, "filters": {"field": "customer.code", "op": "starts_with", "value": "C"}`),
			rollup: "shipments_daily",
			check: func(t *testing.T, p *analytics.RollupPlan) {
				if path(p, "customer.name") != "Customer.name" || path(p, "customer.code") != "Customer.code" {
					t.Fatalf("fields = %+v", p.Fields)
				}
			},
		},
		{
			name: "a distinct count at the exact grain",
			def: report(`{"id": "customer", "dimension": {"field": "customer.id"}}, {"id": "status", "dimension": {"field": "status"}},
				{"id": "day", "dimension": {"field": "createdAt", "bucket": "day"}}, {"id": "payers", "measure": {"field": "billTo.id", "agg": "count_distinct"}}`, ""),
			rollup: "shipments_daily",
			check: func(t *testing.T, p *analytics.RollupPlan) {
				if p.Measures["payers"] != (analytics.RollupMeasure{Agg: analytics.ReSum, Value: "bill_to_id_distinct"}) {
					t.Fatalf("measure = %+v", p.Measures["payers"])
				}
			},
		},
		{
			name:   "an aligned range",
			def:    report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "weight", "measure": {"field": "weight", "agg": "sum"}}`, `, "filters": {"field": "createdAt", "op": "between", "value": ["2026-03-02", "2026-03-09"]}`),
			rollup: "shipments_daily",
			from:   "2026-03-02T00:00:00Z",
			to:     "2026-03-09T00:00:00Z",
		},
		{
			name:   "a window of whole days",
			def:    report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "loads", "measure": {"agg": "count"}}`, `, "filters": {"field": "createdAt", "op": "last_n_days", "param": "days"}, "parameters": [{"name": "days", "type": "int"}]`),
			params: map[string]any{"days": int64(7)},
			rollup: "shipments_daily",
			from:   "2026-03-11T00:00:00Z",
			to:     "2026-03-19T00:00:00Z",
		},
		{
			name:   "a bound in epoch seconds on a bucket start",
			def:    report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "loads", "measure": {"agg": "count"}}`, `, "filters": {"field": "createdAt", "op": "gte", "value": 1772409600}`),
			rollup: "shipments_daily",
			from:   "2026-03-02T00:00:00Z",
		},
		{
			name:   "a bucket chosen by a parameter",
			def:    report(`{"id": "b", "dimension": {"field": "createdAt", "bucket": {"param": "grain"}}}, {"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`, `, "parameters": [{"name": "grain", "type": "string", "default": "month"}]`),
			params: map[string]any{"grain": "year"},
			rollup: "shipments_monthly",
		},
		{
			name:   "row-level calculations, subtotals and totals over dimensions",
			def:    report(`{"id": "s", "calc": "lower({status})"}, {"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`, `, "groupings": {"total": true}`),
			rollup: "shipments_monthly",
		},
	}
	rt := rollup.NewRouter(daily, monthly)
	ok(t, rt.Check(fleetCatalog()))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := rt.Match(query(t, tc.def, nil, now, tc.params))
			ok(t, err)
			if p.Name != tc.rollup {
				t.Fatalf("answered by %s, want %s", p.Name, tc.rollup)
			}
			if tc.from != "" && p.From.UTC().Format(time.RFC3339) != tc.from {
				t.Fatalf("range starts %v", p.From)
			}
			if tc.to != "" && p.To.UTC().Format(time.RFC3339) != tc.to {
				t.Fatalf("range ends %v", p.To)
			}
			if tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

func TestMatchRefuses(t *testing.T) {
	now := time.Date(2026, 3, 18, 15, 0, 0, 0, time.UTC)
	revenue := `{"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`
	status := `{"id": "status", "dimension": {"field": "status"}}, `
	distinct := `{"id": "payers", "measure": {"field": "billTo.id", "agg": "count_distinct"}}`
	cases := []struct {
		name   string
		def    string
		params map[string]any
		loc    *time.Location
		want   string
	}{
		{"finer buckets than the grain", report(`{"id": "h", "dimension": {"field": "createdAt", "bucket": "hour"}}, `+revenue, ""), nil, nil, "hour buckets are not whole day buckets"},
		{"a finer bucket chosen by a parameter", report(`{"id": "b", "dimension": {"field": "createdAt", "bucket": {"param": "grain"}}}, `+revenue, `, "parameters": [{"name": "grain", "type": "string"}]`), map[string]any{"grain": "hour"}, nil, "hour buckets are not whole day buckets"},
		{"a dimension the rollup lacks", report(`{"id": "pro", "dimension": {"field": "proNumber"}}, `+revenue, ""), nil, nil, "not a dimension of the rollup"},
		{"a bound inside a bucket", report(status+revenue, `, "filters": {"field": "createdAt", "op": "gte", "value": "2026-03-01T12:00:00Z"}`), nil, nil, "does not fall on a day bucket start"},
		{"a bound inside a bucket from a parameter", report(status+revenue, `, "filters": {"field": "createdAt", "op": "lt", "param": "until"}, "parameters": [{"name": "until", "type": "timestamp"}]`), map[string]any{"until": time.Date(2026, 3, 2, 6, 0, 0, 0, time.UTC)}, nil, "does not fall on a day bucket start"},
		{"an exclusive bound on an instant", report(status+revenue, `, "filters": {"field": "createdAt", "op": "gt", "value": "2026-03-01"}`), nil, nil, "does not select whole buckets"},
		{"a distinct count over coarser buckets", report(`{"id": "customer", "dimension": {"field": "customer.id"}}, `+status+`{"id": "m", "dimension": {"field": "createdAt", "bucket": "month"}}, `+distinct, ""), nil, nil, "must group by the day"},
		{"a distinct count over fewer dimensions", report(status+`{"id": "d", "dimension": {"field": "createdAt", "bucket": "day"}}, `+distinct, ""), nil, nil, "every dimension"},
		{"a distinct count across a pivot's other cell", report(`{"id": "customer", "dimension": {"field": "customer.id"}}, {"id": "d", "dimension": {"field": "createdAt", "bucket": "day"}}, `+distinct, `, "pivot": {"field": "status", "values": ["New"], "columns": ["payers"], "other": true}`), nil, nil, "Other cell"},
		{"a list of rows", report(`{"id": "status", "dimension": {"field": "status"}}`, ""), nil, nil, "lists rows"},
		{"another time zone", report(status+revenue, ""), nil, mustLoad("America/Chicago"), "runs in America/Chicago"},
		{"a to-many path", report(status+`{"id": "seq", "measure": {"field": "moves.sequence", "agg": "max"}}`, ""), nil, nil, "to-many"},
		{"two relations deep", report(`{"id": "city", "dimension": {"field": "originStop.location.city"}}, `+revenue, ""), nil, nil, "relations"},
		{"an aggregate the rollup does not store", report(status+`{"id": "w", "measure": {"field": "weight", "agg": "min"}}`, ""), nil, nil, "stores no min of weight"},
		{"a measure filtered on another column", report(status+`{"id": "r", "measure": {"field": "total", "agg": "sum", "filter": {"field": "proNumber", "op": "eq", "value": "X"}}}`, ""), nil, nil, "proNumber is not a dimension"},
		{"an aggregate calculation over fields", report(status+`{"id": "x", "calc": "sum({total}) * 2"}`, ""), nil, nil, "aggregate calculation"},
	}
	rt := rollup.NewRouter(daily, monthly)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.Match(query(t, tc.def, tc.loc, now, tc.params))
			if !errors.Is(err, rollup.ErrNotCovered) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	customers := query(t, `{"version": 2, "entity": "customer", "columns": [{"id": "n", "measure": {"agg": "count"}}]}`, nil, now, nil)
	if _, err := rt.Match(customers); !errors.Is(err, rollup.ErrNotCovered) {
		t.Fatalf("an entity without rollups: %v", err)
	}
	if _, routed := rt.Route(context.Background(), customers); routed {
		t.Fatal("routed an entity without rollups")
	}
}

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func TestCheckRejectsDuplicatesAndUnreportedSources(t *testing.T) {
	rt := rollup.NewRouter(daily, daily)
	if err := rt.Check(fleetCatalog()); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("a rollup listed twice: %v", err)
	}
	def := report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "n", "measure": {"agg": "count"}}`, "")
	if _, routed := rt.Route(context.Background(), query(t, def, nil, time.Now(), nil)); routed {
		t.Fatal("a router with a duplicate rollup routed a report")
	}
	if err := rollup.NewRouter(daily, stopsDaily).Check(fleetCatalog()); err != nil {
		t.Fatalf("reportable sources refused: %v", err)
	}
	plants, err := analytics.Build([]*schema.Table{fleetdb.PlantsSchema()}, analytics.Options{})
	ok(t, err)
	if err := rollup.NewRouter(daily).Check(plants); err == nil || !strings.Contains(err.Error(), "does not report on") {
		t.Fatalf("a source the catalog does not report on: %v", err)
	}
}

func TestFreshnessFollowsDirtySlices(t *testing.T) {
	clk := &clock{}
	d := openDB(t, clk)
	f := newFixture(t, d, clk)
	for day := range 10 {
		f.shipment(day, day%3, []fleet.Status{fleet.StatusNew, fleet.StatusDelivered}[day%2], int64(1000+day*37), ptr(int64(day*3)))
	}
	refreshAll(t, d)
	rt := rollup.NewRouter(daily, monthly)
	now := time.Date(2026, 3, 18, 15, 0, 0, 0, time.UTC)
	byCustomer := query(t, report(`{"id": "customer", "dimension": {"field": "customer.id"}},
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "loads", "measure": {"agg": "count"}}`,
		`, "filters": {"field": "createdAt", "op": "gte", "value": "2026-03-01"}`), nil, now, nil)
	plan, routed := rt.Route(f.ctx, byCustomer)
	if !routed || plan.Name != "shipments_daily" {
		t.Fatalf("Route = %v %+v", routed, plan)
	}
	fresh := func(ctx context.Context, p *analytics.RollupPlan) bool {
		t.Helper()
		got, err := p.Fresh(ctx, d)
		ok(t, err)
		return got
	}
	if !fresh(f.ctx, plan) {
		t.Fatal("a refreshed rollup is stale")
	}
	f.shipment(2, 0, fleet.StatusNew, 1, nil)
	if fresh(f.ctx, plan) {
		t.Fatal("fresh with a pending slice in range")
	}
	other, _ := newTenant()
	if !fresh(other, plan) {
		t.Fatal("another tenant's pending slice made this one stale")
	}
	late := query(t, report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "loads", "measure": {"agg": "count"}}`,
		`, "filters": {"field": "createdAt", "op": "gte", "value": "2026-03-20"}`), nil, now, nil)
	inRange, _ := rt.Route(f.ctx, late)
	if !fresh(f.ctx, inRange) {
		t.Fatal("a pending slice outside the report's range made it stale")
	}
	system := context.WithValue(context.Background(), systemKey{}, "report")
	if fresh(system, plan) {
		t.Fatal("a system report was fresh with a tenant's slice pending")
	}
	refreshAll(t, d)
	if !fresh(f.ctx, plan) || !fresh(system, plan) {
		t.Fatal("stale after the refresh")
	}
	exact := query(t, report(`{"id": "customer", "dimension": {"field": "customer.id"}}, {"id": "status", "dimension": {"field": "status"}},
		{"id": "day", "dimension": {"field": "createdAt", "bucket": "day"}}, {"id": "payers", "measure": {"field": "billTo.id", "agg": "count_distinct"}}`, ""), nil, now, nil)
	exactPlan, routed := rt.Route(f.ctx, exact)
	if !routed || !fresh(f.ctx, exactPlan) || fresh(system, exactPlan) {
		t.Fatal("a distinct count per tenant was answered across tenants, or not for its tenant")
	}
	never := rollup.NewRouter(rollup.MustDefine("never_migrated", daily.Source(), rollup.Day(s.CreatedAt), nil, rollup.Measures(rollup.Count())))
	total := query(t, report(`{"id": "loads", "measure": {"agg": "count"}}`, ""), nil, now, nil)
	p, routed := never.Route(f.ctx, total)
	if !routed || fresh(f.ctx, p) {
		t.Fatal("a rollup that was never built is fresh")
	}
}
