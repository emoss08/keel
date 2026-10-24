package rollup_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// chicagoDaily and chicagoMonthly bucket shipments on Chicago's calendar, which changed its
// clocks on 8 March 2026, for the engine tests.
var (
	chicagoDaily = rollup.MustDefine("shipments_chicago", fleetdb.ShipmentsSchema(),
		rollup.Day(s.CreatedAt),
		rollup.Dims(s.CustomerID, s.Status),
		rollup.Measures(rollup.Count(), rollup.Sum(s.Total), rollup.Avg(s.Weight), rollup.Min(s.Weight),
			rollup.Max(s.Weight), rollup.CountDistinct(s.BillToID)),
		rollup.Zone("America/Chicago"), rollup.WithoutSourceIndex())
	chicagoMonthly = rollup.MustDefine("shipments_chicago_monthly", fleetdb.ShipmentsSchema(),
		rollup.Month(s.CreatedAt),
		rollup.Dims(s.Status),
		rollup.Measures(rollup.Count(), rollup.Sum(s.Total)),
		rollup.Zone("America/Chicago"), rollup.WithoutSourceIndex())
)

// engineReport is a report run through the engine with and without rollups.
type engineReport struct {
	name   string
	def    string
	params map[string]any
	rollup string
}

// engineReports cover day, week, month and quarter buckets, dimensions of the row and of a
// related row, filters on dimensions and on the grain (dates, epoch seconds, relative days),
// every re-aggregation, a pivot, subtotals and a distinct count at the rollup's exact grain.
func engineReports() []engineReport {
	march := fmt.Sprint(time.Date(2026, 3, 1, 0, 0, 0, 0, mustLoad("America/Chicago")).Unix())
	return []engineReport{
		{name: "days by customer and status", rollup: "shipments_chicago", def: report(`
			{"id": "day", "dimension": {"field": "createdAt", "bucket": "day"}},
			{"id": "customer", "dimension": {"field": "customer.id"}},
			{"id": "status", "dimension": {"field": "status"}},
			{"id": "loads", "measure": {"agg": "count"}},
			{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
			{"id": "avgWeight", "measure": {"field": "weight", "agg": "avg"}},
			{"id": "minWeight", "measure": {"field": "weight", "agg": "min"}},
			{"id": "maxWeight", "measure": {"field": "weight", "agg": "max"}},
			{"id": "weighed", "measure": {"field": "weight", "agg": "count"}},
			{"id": "payers", "measure": {"field": "billTo.id", "agg": "count_distinct"}}`,
			`, "sort": [{"column": "day"}, {"column": "customer"}, {"column": "status"}]`)},
		{name: "weeks by customer name in a range", rollup: "shipments_chicago", def: report(`
			{"id": "customer", "dimension": {"field": "customer.name"}},
			{"id": "week", "dimension": {"field": "createdAt", "bucket": "week"}},
			{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
			{"id": "loads", "measure": {"agg": "count"}},
			{"id": "avgWeight", "measure": {"field": "weight", "agg": "avg"}, "transform": {"op": "round", "precision": 2}},
			{"id": "perLoad", "calc": "[revenue] / nullif([loads], 0)"}`,
			`, "filters": {"and": [{"field": "status", "op": "in", "value": ["New", "Delivered"]},
				{"field": "createdAt", "op": "between", "value": ["2026-03-02", "2026-03-30"]}]},
			"sort": [{"column": "customer"}, {"column": "week"}]`)},
		{name: "months by status with a total", rollup: "shipments_chicago_monthly", def: report(`
			{"id": "status", "dimension": {"field": "status"}},
			{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
			{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
			{"id": "loads", "measure": {"agg": "count"}}`,
			`, "groupings": {"total": true}, "sort": [{"column": "status"}, {"column": "month"}]`)},
		{name: "a pivot from an epoch bound", rollup: "shipments_chicago", def: report(`
			{"id": "customer", "dimension": {"field": "customer.id"}},
			{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
			{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
			{"id": "maxWeight", "measure": {"field": "weight", "agg": "max"}},
			{"id": "minWeight", "measure": {"field": "weight", "agg": "min", "filter": {"field": "status", "op": "ne", "value": "Canceled"}}}`,
			`, "filters": {"field": "createdAt", "op": "gte", "param": "from"},
			"parameters": [{"name": "from", "type": "epoch"}],
			"pivot": {"field": "status", "values": ["New", "Delivered"], "columns": ["revenue"], "other": true},
			"sort": [{"column": "customer"}, {"column": "month"}]`),
			params: map[string]any{"from": march}},
		{name: "the last ten days", rollup: "shipments_chicago", def: report(`
			{"id": "loads", "measure": {"agg": "count"}},
			{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
			{"id": "avgWeight", "measure": {"field": "weight", "agg": "avg"}}`,
			`, "filters": {"field": "createdAt", "op": "last_n_days", "value": 10}`)},
		{name: "quarters", rollup: "shipments_chicago_monthly", def: report(`
			{"id": "quarter", "dimension": {"field": "createdAt", "bucket": "quarter"}},
			{"id": "revenue", "measure": {"field": "total", "agg": "sum"}}`,
			`, "sort": [{"column": "quarter"}]`)},
		{name: "days across the clock change", rollup: "shipments_chicago", def: report(`
			{"id": "day", "dimension": {"field": "createdAt", "bucket": "day"}},
			{"id": "status", "dimension": {"field": "status"}},
			{"id": "loads", "measure": {"agg": "count"}},
			{"id": "avgWeight", "measure": {"field": "weight", "agg": "avg"}}`,
			`, "filters": {"field": "createdAt", "op": "between", "value": ["2026-03-07", "2026-03-11"]},
			"sort": [{"column": "day"}, {"column": "status"}]`)},
		{name: "late March only", rollup: "shipments_chicago", def: report(`
			{"id": "customer", "dimension": {"field": "customer.id"}},
			{"id": "loads", "measure": {"agg": "count"}}`,
			`, "filters": {"field": "createdAt", "op": "gte", "value": "2026-03-20"}, "sort": [{"column": "customer"}]`)},
	}
}

// engine runs reports with and without the Chicago rollups.
type engine struct {
	t      *testing.T
	ctx    context.Context
	base   *analytics.Runner
	rolled *analytics.Runner
	loc    *time.Location
	now    time.Time
}

// newEngine returns runners over d with and without the Chicago rollups.
func newEngine(t *testing.T, d *keel.DB, ctx context.Context) *engine {
	loc := mustLoad("America/Chicago")
	base, err := analytics.NewRunner(d, fleetCatalog(), analytics.RunnerOptions{PreviewRows: 1000})
	ok(t, err)
	rolled, err := analytics.NewRunner(d, fleetCatalog(), analytics.RunnerOptions{PreviewRows: 1000,
		Rollups: rollup.NewRouter(chicagoDaily, chicagoMonthly)})
	ok(t, err)
	return &engine{t: t, ctx: ctx, base: base, rolled: rolled, loc: loc, now: time.Date(2026, 3, 25, 12, 0, 0, 0, loc)}
}

// check runs every report both ways and fails unless the results are equal and each was answered
// as stale says: from the base tables when its rollup slices are pending, else from its rollup.
func (e *engine) check(stale map[string]bool) {
	e.t.Helper()
	for _, r := range engineReports() {
		def, err := ir.Decode([]byte(r.def))
		ok(e.t, err)
		req := analytics.Request{Definition: def, Params: r.params, Location: e.loc, Now: e.now}
		want, err := e.base.Preview(e.ctx, req)
		ok(e.t, err)
		got, err := e.rolled.Preview(e.ctx, req)
		ok(e.t, err)
		answered := r.rollup
		if stale[r.name] {
			answered = ""
		}
		if got.Rollup() != answered || want.Rollup() != "" {
			e.t.Fatalf("%s: answered by %q, want %q", r.name, got.Rollup(), answered)
		}
		g, w := resultRows(e.t, got), resultRows(e.t, want)
		if strings.Join(g, "\n") != strings.Join(w, "\n") || len(w) == 0 {
			e.t.Fatalf("%s:\nfrom %q:\n%s\nfrom the base tables:\n%s", r.name, got.Rollup(), strings.Join(g, "\n"), strings.Join(w, "\n"))
		}
	}
}

func TestEngineAnswersFromRollupsExactly(t *testing.T) {
	clk := &clock{}
	engineScenario(t, openDB(t, clk), clk, func() {})
}

func TestEngineChecksFreshnessOnTheReplica(t *testing.T) {
	clk := &clock{}
	openDB(t, clk)
	dsn := os.Getenv("KEEL_TEST_REPLICA_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_REPLICA_DSN is not set")
	}
	primary, err := pgx.ParseConfig(testDSN)
	ok(t, err)
	replica, err := pgx.ParseConfig(dsn)
	ok(t, err)
	checks := &freshnessChecks{}
	d := openDBWith(t, clk, func(cfg *keel.Config) {
		cfg.Replicas = []keel.ReplicaConfig{{DSN: fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			replica.User, replica.Password, replica.Host, replica.Port, primary.Database)}}
		cfg.MaxReplicaLag = 5 * time.Second
		cfg.Observers = []keel.Observer{checks}
	})
	engineScenario(t, d, clk, func() { waitForReplay(t, d) })
	checks.mu.Lock()
	defer checks.mu.Unlock()
	if checks.replica == 0 || checks.primary != 0 {
		t.Fatalf("freshness checked %d times on the replica and %d on the primary", checks.replica, checks.primary)
	}
}

// engineScenario checks the reports on fresh rollups, after writes that leave slices pending, and
// after a refresh; settle runs after each change, before the reports read.
func engineScenario(t *testing.T, d *keel.DB, clk *clock, settle func()) {
	f := newFixture(t, d, clk)
	loc := mustLoad("America/Chicago")
	start := time.Date(2026, 2, 27, 0, 0, 0, 0, loc)
	statuses := []fleet.Status{fleet.StatusNew, fleet.StatusDelivered, fleet.StatusInTransit, fleet.StatusCanceled}
	for i := range 70 {
		var weight *int64
		if i%5 != 0 {
			weight = ptr(int64(100 + i*7%53))
		}
		f.shipmentAt(start.Add(time.Duration(i)*13*time.Hour+time.Duration(i%7)*time.Minute), i%3, statuses[i%4], int64(1000+i*137), weight)
	}
	refreshChicago(t, d)
	settle()
	e := newEngine(t, d, f.ctx)
	e.check(nil)

	f.shipmentAt(time.Date(2026, 3, 9, 23, 30, 0, 0, loc), 1, fleet.StatusNew, 777, ptr(int64(12)))
	changed, err := fleetdb.Shipments.Where(s.CreatedAt.Lt(time.Date(2026, 3, 4, 0, 0, 0, 0, loc).Unix())).
		Update(f.ctx, d, s.Total.To(fleet.Money{Cents: 4242}))
	ok(t, err)
	if changed == 0 {
		t.Fatal("no shipment before 4 March to change")
	}
	settle()
	stale := map[string]bool{}
	for _, r := range engineReports() {
		stale[r.name] = r.name != "late March only" && r.name != "the last ten days"
	}
	e.check(stale)
	refreshChicago(t, d)
	settle()
	e.check(nil)
}

// freshnessChecks counts the rollup freshness checks run on the replica and on the primary.
type freshnessChecks struct {
	mu               sync.Mutex
	replica, primary int
}

// BeforeQuery passes the context through.
func (c *freshnessChecks) BeforeQuery(ctx context.Context, _ *keel.QueryEvent) context.Context {
	return ctx
}

// AfterQuery counts a freshness check by the server it ran on.
func (c *freshnessChecks) AfterQuery(_ context.Context, e *keel.QueryEvent) {
	if e.Op != "query" || !strings.Contains(e.SQL, "keel_rollup_state") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.Replica != "" {
		c.replica++
	} else {
		c.primary++
	}
}

// waitForReplay waits until the replica has replayed everything the primary has written.
func waitForReplay(t *testing.T, d *keel.DB) {
	t.Helper()
	ctx := context.Background()
	lsn, err := keel.QueryValue[string](ctx, d, "SELECT pg_current_wal_lsn()::text")
	ok(t, err)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var done bool
		err := keel.Read(ctx, d, func(ctx context.Context) error {
			var err error
			done, err = keel.QueryValue[bool](ctx, d, "SELECT coalesce(pg_last_wal_replay_lsn() >= $1::pg_lsn, false)", lsn)
			return err
		}, keel.ReplicaTx())
		if err == nil && done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the replica never replayed the primary's writes")
}

// refreshChicago refreshes the Chicago rollups until nothing is left.
func refreshChicago(t testing.TB, d *keel.DB) {
	t.Helper()
	_, err := rollup.Refresh(context.Background(), d, chicagoDaily, chicagoMonthly)
	ok(t, err)
}

// shipmentAt creates a shipment at the instant at.
func (f *fixture) shipmentAt(at time.Time, cust int, status fleet.Status, cents int64, weight *int64) {
	f.t.Helper()
	f.clk.set(at)
	sh := &fleet.Shipment{
		CustomerID: f.customers[cust].ID, ProNumber: "PRO" + randHex(), Status: status,
		Total: fleet.Money{Cents: cents}, Weight: weight, Labels: []string{}, Attributes: map[string]any{},
	}
	if cust == 2 {
		id := f.customers[0].ID
		sh.BillToID = &id
	}
	ok(f.t, fleetdb.Shipments.Create(f.ctx, f.db, sh))
}

// resultRows reads a result's rows as text, subtotal and total rows marked with their level.
func resultRows(t testing.TB, r table.Reader) []string {
	t.Helper()
	var out []string
	cols := r.Columns()
	for {
		b, err := r.Next(context.Background())
		if err == io.EOF {
			return out
		}
		ok(t, err)
		for i := range b.Len {
			cells := make([]string, len(cols))
			for c := range cols {
				cells[c] = cellText(&b.Columns[c], cols[c].Type, i)
			}
			out = append(out, fmt.Sprintf("%d|%s", b.Level(i), strings.Join(cells, "|")))
		}
	}
}

// cellText writes one value exactly as decoded.
func cellText(v *value.Vector, t value.Type, i int) string {
	if v.Null(i) {
		return "NULL"
	}
	switch t {
	case value.Int, value.Epoch:
		return fmt.Sprint(v.Int64[i])
	case value.Decimal:
		return string(v.Numeric[i])
	case value.Float:
		return fmt.Sprint(v.Float64[i])
	case value.Bool:
		return fmt.Sprint(v.Bool[i])
	case value.Time:
		return v.Time[i].UTC().Format(time.RFC3339Nano)
	case value.JSON:
		return string(v.JSON[i])
	}
	return v.String[i]
}
