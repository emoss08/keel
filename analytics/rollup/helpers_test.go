package rollup_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/schema"
)

var s = fleetdb.Shipments

// daily is the rollup the integration tests maintain: shipments by day, customer and status.
var daily = rollup.MustDefine("shipments_daily", fleetdb.ShipmentsSchema(),
	rollup.Day(s.CreatedAt),
	rollup.Dims(s.CustomerID, s.Status),
	rollup.Measures(rollup.Count(), rollup.Sum(s.Total), rollup.Avg(s.Weight), rollup.Max(s.Weight),
		rollup.CountDistinct(s.BillToID)),
	rollup.WithoutSourceIndex(),
)

// monthly is a coarser rollup the router prefers when it covers a report.
var monthly = rollup.MustDefine("shipments_monthly", fleetdb.ShipmentsSchema(),
	rollup.Month(s.CreatedAt),
	rollup.Dims(s.Status),
	rollup.Measures(rollup.Count(), rollup.Sum(s.Total)),
)

// stopsDaily buckets a nullable timestamptz column in Chicago.
var stopsDaily = rollup.MustDefine("stops_daily", fleetdb.StopsSchema(),
	rollup.Day(fleetdb.Stops.ArrivedAt),
	rollup.Dims(fleetdb.Stops.Kind),
	rollup.Measures(rollup.Count(), rollup.Max(fleetdb.Stops.Sequence)),
	rollup.Zone("America/Chicago"),
)

// tenantKey and systemKey hold the scope on a context.
type (
	tenantKey struct{}
	systemKey struct{}
)

// tenant is the tenant columns' values.
type tenant struct{ org, bu ids.ID }

var (
	testDSN   string
	setupErr  error
	dropTest  func(context.Context) error
	setupOnce sync.Once
)

func TestMain(m *testing.M) {
	code := m.Run()
	if dropTest != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = dropTest(ctx)
		cancel()
	}
	os.Exit(code)
}

// clock is the time stamped into autocreate columns, settable per test database.
type clock struct{ unix atomic.Int64 }

func (c *clock) now() time.Time  { return time.Unix(c.unix.Load(), 0) }
func (c *clock) set(t time.Time) { c.unix.Store(t.Unix()) }

// openDB opens a database with the rollups' migrations applied, stamping autocreate columns with
// clk's time. It skips without Postgres unless KEEL_TEST_REQUIRE is set.
func openDB(t testing.TB, clk *clock) *keel.DB {
	t.Helper()
	return openDBWith(t, clk, nil)
}

// openDBWith is openDB with edit applied to the configuration.
func openDBWith(t testing.TB, clk *clock, edit func(cfg *keel.Config)) *keel.DB {
	t.Helper()
	setupOnce.Do(func() { setupErr = setup() })
	if setupErr != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatalf("postgres: %v", setupErr)
		}
		t.Skipf("postgres unavailable: %v", setupErr)
	}
	cfg := keel.Config{
		DSN:           testDSN,
		TenantColumns: []string{"organization_id", "business_unit_id"},
		Tenant: func(ctx context.Context) ([]any, bool) {
			v, ok := ctx.Value(tenantKey{}).(tenant)
			if !ok {
				return nil, false
			}
			return []any{v.org, v.bu}, true
		},
		System: func(ctx context.Context) (string, bool) {
			r, ok := ctx.Value(systemKey{}).(string)
			return r, ok
		},
		NewID: func(prefix string) string { return prefix + randHex() },
		Now:   clk.now,
	}
	if edit != nil {
		edit(&cfg)
	}
	d, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

// setup builds the test database from keel's test migrations and the rollups' migrations.
func setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin := os.Getenv("KEEL_TEST_DSN")
	if admin == "" {
		admin = "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
	}
	rollups := fstest.MapFS{
		"20270101000000_rollup_shipments_daily.sql":           {Data: []byte(rollup.Migration(daily))},
		"20270101000001_rollup_shipments_monthly.sql":         {Data: []byte(rollup.Migration(monthly))},
		"20270101000002_rollup_stops_daily.sql":               {Data: []byte(rollup.Migration(stopsDaily))},
		"20270101000003_rollup_shipments_chicago.sql":         {Data: []byte(rollup.Migration(chicagoDaily))},
		"20270101000004_rollup_shipments_chicago_monthly.sql": {Data: []byte(rollup.Migration(chicagoMonthly))},
	}
	var err error
	testDSN, dropTest, err = migrate.Template(ctx, migrate.TemplateOptions{
		Admin: admin,
		Sources: migrate.Sources(
			migrate.Source{Name: "testdata", FS: os.DirFS("../../testdata/migrations")},
			migrate.Source{Name: "rollups", FS: rollups},
		),
		Prefix: "keel_rollup_test_",
	})
	return err
}

func randHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newTenant returns a context for a fresh tenant and the tenant.
func newTenant() (context.Context, tenant) {
	h := randHex()
	tn := tenant{ids.ID("org_" + h), ids.ID("bu_" + h)}
	return context.WithValue(context.Background(), tenantKey{}, tn), tn
}

func ok(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fixture writes shipments for one tenant.
type fixture struct {
	t         testing.TB
	db        *keel.DB
	ctx       context.Context
	tn        tenant
	clk       *clock
	customers []*fleet.Customer
	base      time.Time
}

// newFixture creates a tenant with three customers; base is the first day shipments are made on.
func newFixture(t testing.TB, d *keel.DB, clk *clock) *fixture {
	ctx, tn := newTenant()
	f := &fixture{t: t, db: d, ctx: ctx, tn: tn, clk: clk, base: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)}
	clk.set(f.base)
	for i := range 3 {
		c := &fleet.Customer{Name: fmt.Sprintf("Customer %d", i), Code: fmt.Sprintf("C%d", i)}
		ok(t, fleetdb.Customers.Create(ctx, d, c))
		f.customers = append(f.customers, c)
	}
	return f
}

// shipment creates a shipment day days after base.
func (f *fixture) shipment(day int, cust int, status fleet.Status, cents int64, weight *int64) *fleet.Shipment {
	f.t.Helper()
	f.clk.set(f.base.AddDate(0, 0, day))
	sh := &fleet.Shipment{
		CustomerID: f.customers[cust].ID, ProNumber: "PRO" + randHex(), Status: status,
		Total: fleet.Money{Cents: cents}, Weight: weight, Labels: []string{}, Attributes: map[string]any{},
	}
	if cust == 2 {
		id := f.customers[0].ID
		sh.BillToID = &id
	}
	ok(f.t, fleetdb.Shipments.Create(f.ctx, f.db, sh))
	return sh
}

func ptr[T any](v T) *T { return &v }

// baseRows is the rollup computed from the source table for the fixture's tenant, as text.
func (f *fixture) baseRows() []string {
	return f.rows(`SELECT concat_ws('|', date_trunc('day', to_timestamp(created_at), 'UTC'), customer_id, status,
		count(*), sum(total), sum(weight), count(weight), max(weight), count(DISTINCT bill_to_id))
		FROM shipments WHERE organization_id = $1 AND business_unit_id = $2
		GROUP BY date_trunc('day', to_timestamp(created_at), 'UTC'), customer_id, status`)
}

// rollupRows is the fixture tenant's rows of the daily rollup, as text.
func (f *fixture) rollupRows() []string {
	return f.rows(`SELECT concat_ws('|', bucket, customer_id, status, row_count, total_sum, weight_sum,
		weight_count, weight_max, bill_to_id_distinct)
		FROM shipments_daily WHERE organization_id = $1 AND business_unit_id = $2`)
}

// rows runs sql with the fixture's tenant and returns its single text column, sorted.
func (f *fixture) rows(sql string) []string {
	f.t.Helper()
	rs, err := f.db.Pool().Query(context.Background(), sql, string(f.tn.org), string(f.tn.bu))
	ok(f.t, err)
	defer rs.Close()
	var out []string
	for rs.Next() {
		var line string
		ok(f.t, rs.Scan(&line))
		out = append(out, line)
	}
	ok(f.t, rs.Err())
	slices.Sort(out)
	return out
}

// converged fails unless the rollup holds exactly what the source says.
func (f *fixture) converged() {
	f.t.Helper()
	got, want := f.rollupRows(), f.baseRows()
	if !slices.Equal(got, want) {
		f.t.Fatalf("rollup rows:\n%s\nsource rows:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// fleetCatalog is the analytics catalog of the fleet models, which reports resolve against.
var fleetCatalog = sync.OnceValue(func() *analytics.Catalog {
	cat, err := analytics.Build([]*schema.Table{fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema(),
		fleetdb.MovesSchema(), fleetdb.StopsSchema(), fleetdb.LocationsSchema(), fleetdb.TagsSchema(),
		fleetdb.WorkersSchema(), fleetdb.PlantsSchema()}, analytics.Options{})
	if err != nil {
		panic(err)
	}
	return cat
})

// query decodes a definition and resolves it against the fleet catalog, as a router is asked
// about it in loc at now with params.
func query(t testing.TB, js string, loc *time.Location, now time.Time, params map[string]any) analytics.RollupQuery {
	t.Helper()
	def, err := ir.Decode([]byte(js))
	ok(t, err)
	res, err := ir.Resolve(def, fleetCatalog(), ir.Limits{})
	ok(t, err)
	if loc == nil {
		loc = time.UTC
	}
	return analytics.RollupQuery{Catalog: fleetCatalog(), Resolved: res, Params: params, Location: loc, Now: now}
}
