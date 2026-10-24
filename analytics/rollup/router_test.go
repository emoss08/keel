package rollup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/internal/testmodels/fleet"
)

// router returns a Router over both rollups with a fixed clock; db may be nil for Match.
func newRouter(t testing.TB, now time.Time) *rollup.Router {
	t.Helper()
	rt, err := rollup.NewRouter(nil, rollup.RouterOptions{Now: func() time.Time { return now }}, daily, monthly)
	ok(t, err)
	return rt
}

// report wraps columns and filters in a version 2 shipment definition.
func report(columns, extra string) string {
	return `{"version": 2, "entity": "shipment", "columns": [` + columns + `]` + extra + `}`
}

func TestMatchCovers(t *testing.T) {
	now := time.Date(2026, 3, 18, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		def     string
		rollup  string
		check   func(t *testing.T, p *rollup.Plan)
		from    string
		to      string
		measure string
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
			rollup:  "shipments_daily",
			from:    "2026-03-01T00:00:00Z",
			to:      "2026-04-01T00:00:00Z",
			measure: `sum(r."weight_sum")::numeric / nullif(sum(r."weight_count"), 0)`,
			check: func(t *testing.T, p *rollup.Plan) {
				if p.Fields["customer.id"].Column != 3 || p.Fields["createdAt"].Column != 2 || !p.Fields["createdAt"].Grain {
					t.Fatalf("fields = %+v", p.Fields)
				}
				if p.Dimensions["day"].Bucket != "" {
					t.Fatal("a day bucket over a daily rollup was re-truncated")
				}
				if got := p.Measures["loads"].SQL("r", ""); got != `coalesce(sum(r."row_count"), 0)::int8` {
					t.Fatalf("count = %s", got)
				}
				if got := p.Measures["revenue"].SQL("r", `r."status" = 'New'`); got != `sum(r."total_sum") FILTER (WHERE r."status" = 'New')` {
					t.Fatalf("filtered sum = %s", got)
				}
			},
		},
		{
			name:   "a coarser rollup wins when it covers",
			def:    report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "month", "dimension": {"field": "createdAt", "bucket": "quarter"}}, {"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`, ""),
			rollup: "shipments_monthly",
			check: func(t *testing.T, p *rollup.Plan) {
				if p.Dimensions["month"].Bucket != ir.BucketQuarter {
					t.Fatalf("dimension = %+v", p.Dimensions["month"])
				}
				if got := p.BucketSQL("r", ir.BucketQuarter); got != `date_trunc('quarter', r."bucket", 'UTC')` {
					t.Fatalf("bucket = %s", got)
				}
			},
		},
		{
			name:   "fields of a related row join through the rollup's key",
			def:    report(`{"id": "customer", "dimension": {"field": "customer.name"}}, {"id": "week", "dimension": {"field": "createdAt", "bucket": "week"}}, {"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`, `, "filters": {"field": "customer.code", "op": "starts_with", "value": "C"}`),
			rollup: "shipments_daily",
			check: func(t *testing.T, p *rollup.Plan) {
				f := p.Fields["customer.name"]
				if f.Column != -1 || f.Relation != 0 || p.Table.Relations[f.Relation].Target.Columns[f.Target].Name != "name" {
					t.Fatalf("field = %+v", f)
				}
			},
		},
		{
			name: "a distinct count at the exact grain",
			def: report(`{"id": "customer", "dimension": {"field": "customer.id"}}, {"id": "status", "dimension": {"field": "status"}},
				{"id": "day", "dimension": {"field": "createdAt", "bucket": "day"}}, {"id": "payers", "measure": {"field": "billTo.id", "agg": "count_distinct"}}`, ""),
			rollup: "shipments_daily",
		},
		{
			name:   "an aligned range",
			def:    report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "weight", "measure": {"field": "weight", "agg": "sum"}}`, `, "filters": {"field": "createdAt", "op": "between", "value": ["2026-03-02", "2026-03-09"]}`),
			rollup: "shipments_daily",
			from:   "2026-03-02T00:00:00Z",
			to:     "2026-03-09T00:00:00Z",
		},
		{
			name:   "row-level calculations, subtotals and totals over dimensions",
			def:    report(`{"id": "s", "calc": "lower({status})"}, {"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`, `, "groupings": {"total": true}`),
			rollup: "shipments_monthly",
		},
	}
	rt := newRouter(t, now)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := rt.Match(resolve(t, tc.def), time.UTC)
			ok(t, err)
			if p.Rollup.Name() != tc.rollup {
				t.Fatalf("answered by %s, want %s", p.Rollup.Name(), tc.rollup)
			}
			if tc.from != "" && (p.From.UTC().Format(time.RFC3339) != tc.from || p.To.UTC().Format(time.RFC3339) != tc.to) {
				t.Fatalf("range = [%v, %v)", p.From, p.To)
			}
			if tc.measure != "" && p.Measures["avgWeight"].SQL("r", "") != tc.measure {
				t.Fatalf("avg = %s", p.Measures["avgWeight"].SQL("r", ""))
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
	cases := []struct {
		name string
		def  string
		loc  *time.Location
		want string
	}{
		{"finer buckets than the grain", report(`{"id": "h", "dimension": {"field": "createdAt", "bucket": "hour"}}, `+revenue, ""), nil, "hour buckets are not whole day buckets"},
		{"a dimension the rollup lacks", report(`{"id": "pro", "dimension": {"field": "proNumber"}}, `+revenue, ""), nil, "not a dimension of the rollup"},
		{"a moving window of days", report(`{"id": "status", "dimension": {"field": "status"}}, `+revenue, `, "filters": {"field": "createdAt", "op": "last_n_days", "value": 7}`), nil, "does not select whole buckets"},
		{"a bound inside a bucket", report(`{"id": "status", "dimension": {"field": "status"}}, `+revenue, `, "filters": {"field": "createdAt", "op": "gte", "value": "2026-03-01T12:00:00Z"}`), nil, "does not fall on a day bucket start"},
		{"a distinct count over coarser buckets", report(`{"id": "customer", "dimension": {"field": "customer.id"}}, {"id": "status", "dimension": {"field": "status"}}, {"id": "m", "dimension": {"field": "createdAt", "bucket": "month"}}, {"id": "payers", "measure": {"field": "billTo.id", "agg": "count_distinct"}}`, ""), nil, "must group by the day"},
		{"a distinct count over fewer dimensions", report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "d", "dimension": {"field": "createdAt", "bucket": "day"}}, {"id": "payers", "measure": {"field": "billTo.id", "agg": "count_distinct"}}`, ""), nil, "every dimension"},
		{"a list of rows", report(`{"id": "status", "dimension": {"field": "status"}}`, ""), nil, "lists rows"},
		{"another time zone", report(`{"id": "status", "dimension": {"field": "status"}}, `+revenue, ""), mustLoad("America/Chicago"), "runs in America/Chicago"},
		{"a to-many path", report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "seq", "measure": {"field": "moves.sequence", "agg": "max"}}`, ""), nil, "to-many"},
		{"an aggregate the rollup does not store", report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "w", "measure": {"field": "weight", "agg": "min"}}`, ""), nil, "stores no min of weight"},
		{"a bucket chosen by a parameter", report(`{"id": "b", "dimension": {"field": "createdAt", "bucket": {"param": "grain"}}}, `+revenue, `, "parameters": [{"name": "grain", "type": "string", "default": "month"}]`), nil, "parameter"},
		{"a measure filtered on another column", report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "r", "measure": {"field": "total", "agg": "sum", "filter": {"field": "proNumber", "op": "eq", "value": "X"}}}`, ""), nil, "proNumber is not a dimension"},
		{"an aggregate calculation over fields", report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "x", "calc": "sum({total}) * 2"}`, ""), nil, "aggregate calculation"},
	}
	rt := newRouter(t, now)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc := tc.loc
			if loc == nil {
				loc = time.UTC
			}
			_, err := rt.Match(resolve(t, tc.def), loc)
			if !errors.Is(err, rollup.ErrNotCovered) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := rt.Match(&ir.Resolved{Definition: &ir.Definition{Entity: "customer"}}, time.UTC); !errors.Is(err, rollup.ErrNotCovered) {
		t.Fatalf("an entity without rollups: %v", err)
	}
}

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func TestNewRouterRejectsDuplicates(t *testing.T) {
	if _, err := rollup.NewRouter(nil, rollup.RouterOptions{}, daily, daily); err == nil {
		t.Fatal("a rollup listed twice was accepted")
	}
}

func TestRouteChecksFreshnessAndMatchesSource(t *testing.T) {
	clk := &clock{}
	d := openDB(t, clk)
	f := newFixture(t, d, clk)
	for day := range 10 {
		f.shipment(day, day%3, []fleet.Status{fleet.StatusNew, fleet.StatusDelivered}[day%2], int64(1000+day*37), ptr(int64(day*3)))
	}
	f.shipment(40, 1, fleet.StatusNew, 5, nil)
	refreshAll(t, d)
	rt, err := rollup.NewRouter(d, rollup.RouterOptions{TenantColumns: []string{"organization_id", "business_unit_id"}}, daily, monthly)
	ok(t, err)
	res := resolve(t, report(`{"id": "customer", "dimension": {"field": "customer.id"}},
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "loads", "measure": {"agg": "count"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "avgWeight", "measure": {"field": "weight", "agg": "avg"}}`,
		`, "filters": {"field": "createdAt", "op": "gte", "value": "2026-03-01"}`))
	plan, routed := rt.Route(f.ctx, res)
	if !routed || plan.Rollup.Name() != "shipments_daily" || plan.Tenant == "" {
		t.Fatalf("Route = %v %+v", routed, plan)
	}

	fromRollup := f.rows(`SELECT concat_ws('|', r."customer_id", ` + plan.BucketSQL("r", ir.BucketMonth) + `, ` +
		plan.Measures["loads"].SQL("r", "") + `, ` + plan.Measures["revenue"].SQL("r", "") + `, round(` +
		plan.Measures["avgWeight"].SQL("r", "") + `, 6)) FROM shipments_daily r
		WHERE r.organization_id = $1 AND r.business_unit_id = $2 AND r.bucket >= '2026-03-01'
		GROUP BY r."customer_id", ` + plan.BucketSQL("r", ir.BucketMonth))
	fromSource := f.rows(`SELECT concat_ws('|', customer_id, date_trunc('month', to_timestamp(created_at), 'UTC'),
		count(*), sum(total), round(avg(weight), 6)) FROM shipments
		WHERE organization_id = $1 AND business_unit_id = $2 AND created_at >= extract(epoch FROM timestamptz '2026-03-01')
		GROUP BY customer_id, date_trunc('month', to_timestamp(created_at), 'UTC')`)
	if strings.Join(fromRollup, "\n") != strings.Join(fromSource, "\n") || len(fromSource) == 0 {
		t.Fatalf("from the rollup:\n%s\nfrom the source:\n%s", strings.Join(fromRollup, "\n"), strings.Join(fromSource, "\n"))
	}

	f.shipment(2, 0, fleet.StatusNew, 1, nil)
	if _, routed := rt.Route(f.ctx, res); routed {
		t.Fatal("routed to a rollup with a pending slice in range")
	}
	other, _ := newTenant()
	if _, routed := rt.Route(other, res); !routed {
		t.Fatal("another tenant's pending slice blocked this one")
	}
	inRange := resolve(t, report(`{"id": "status", "dimension": {"field": "status"}}, {"id": "loads", "measure": {"agg": "count"}}`,
		`, "filters": {"field": "createdAt", "op": "gte", "value": "2026-03-20"}`))
	if _, routed := rt.Route(f.ctx, inRange); !routed {
		t.Fatal("a pending slice outside the report's range blocked it")
	}
	refreshAll(t, d)
	if _, routed := rt.Route(f.ctx, res); !routed {
		t.Fatal("not routed after the refresh")
	}
	if _, routed := rt.Route(context.WithValue(context.Background(), systemKey{}, "report"), res); !routed {
		t.Fatal("a system report was not routed with every tenant fresh")
	}
	var errs int
	broken, err := rollup.NewRouter(d, rollup.RouterOptions{OnError: func(error) { errs++ }}, rollup.MustDefine("never_migrated",
		daily.Source(), rollup.Day(s.CreatedAt), nil, rollup.Measures(rollup.Count())))
	ok(t, err)
	if _, routed := broken.Route(f.ctx, inRange); routed {
		t.Fatal("routed to a rollup that was never built")
	}
	if errs != 0 {
		t.Fatalf("a missing state row reported %d errors", errs)
	}
}
