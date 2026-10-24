package analytics_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/schema"
)

// The test database every integration test shares, copied from a template of the repository's
// test migrations and dropped after the run.
var (
	testDSN   string
	testDB    *keel.DB
	dropTest  func(context.Context) error
	setupErr  error
	setupOnce sync.Once
	adminDSN  = envOr("KEEL_TEST_DSN", "postgres://keel:keel@localhost:55432/keel?sslmode=disable")
)

// envOr returns the environment variable k, or def when it is empty.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// tenantKey holds a tenant on a context.
type tenantKey struct{}

// tenant is the tenant columns' values.
type tenant struct{ org, bu ids.ID }

// tenantCtx returns a context scoped to a tenant.
func tenantCtx(org, bu ids.ID) context.Context {
	return context.WithValue(context.Background(), tenantKey{}, tenant{org, bu})
}

// newTenant returns a context for a fresh tenant.
func newTenant() context.Context {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	return tenantCtx(ids.ID("org_"+s), ids.ID("bu_"+s))
}

// tenantOf returns the tenant of a context.
func tenantOf(ctx context.Context) tenant { return ctx.Value(tenantKey{}).(tenant) }

// testConfig returns the Config the tests open with: data versions on, flushed often.
func testConfig(dsn string) keel.Config {
	return keel.Config{
		DSN:           dsn,
		TenantColumns: []string{"organization_id", "business_unit_id"},
		Tenant: func(ctx context.Context) ([]any, bool) {
			t, ok := ctx.Value(tenantKey{}).(tenant)
			if !ok {
				return nil, false
			}
			return []any{t.org, t.bu}, true
		},
		NewID: func(prefix string) string {
			b := make([]byte, 10)
			_, _ = rand.Read(b)
			return prefix + hex.EncodeToString(b)
		},
		DataVersions: keel.DataVersionsConfig{Enabled: true, Interval: 20 * time.Millisecond},
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testDB != nil {
		testDB.Close()
	}
	if dropTest != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = dropTest(ctx)
		cancel()
	}
	os.Exit(code)
}

// db returns the shared test database, skipping when Postgres is unreachable and
// KEEL_TEST_REQUIRE is not set.
func db(t testing.TB) *keel.DB {
	t.Helper()
	setupOnce.Do(func() { setupErr = setup() })
	if setupErr != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatalf("postgres: %v", setupErr)
		}
		t.Skipf("postgres unavailable: %v", setupErr)
	}
	return testDB
}

// setup copies the test database from a template of the test migrations.
func setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var err error
	testDSN, dropTest, err = migrate.Template(ctx, migrate.TemplateOptions{
		Admin:   adminDSN,
		Sources: migrate.Sources(migrate.Source{Name: "testdata", FS: os.DirFS("../testdata/migrations")}),
		Prefix:  "keel_analytics_test_",
	})
	if err != nil {
		return err
	}
	testDB, err = keel.Open(ctx, testConfig(testDSN))
	return err
}

// replicaConfig returns the test Config with the test server's streaming replica, skipping the
// test when KEEL_TEST_REPLICA_DSN is not set.
func replicaConfig(t *testing.T) keel.Config {
	t.Helper()
	db(t)
	dsn := os.Getenv("KEEL_TEST_REPLICA_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_REPLICA_DSN is not set")
	}
	cfg, err := pgx.ParseConfig(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	rcfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig(testDSN)
	c.Replicas = []keel.ReplicaConfig{{DSN: fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		rcfg.User, rcfg.Password, rcfg.Host, rcfg.Port, cfg.Database)}}
	c.MaxReplicaLag = 5 * time.Second
	return c
}

// openHealthy opens a DB and waits until its replicas are healthy.
func openHealthy(t *testing.T, c keel.Config) *keel.DB {
	t.Helper()
	d, err := keel.Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		healthy := true
		for _, st := range d.Replicas() {
			healthy = healthy && st.Healthy
		}
		if healthy {
			return d
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the replica never became healthy")
	return nil
}

// fleetTables are the generated tables of the test models.
func fleetTables() []*schema.Table {
	return []*schema.Table{fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema(), fleetdb.MovesSchema(),
		fleetdb.StopsSchema(), fleetdb.LocationsSchema(), fleetdb.TagsSchema(), fleetdb.WorkersSchema(),
		fleetdb.PlantsSchema()}
}

// catalog builds the catalog of the test models.
func catalog(t testing.TB) *analytics.Catalog {
	t.Helper()
	c, err := analytics.Build(fleetTables(), analytics.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// must fails the test on an error and returns the value.
func must[T any](t testing.TB) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// ok fails the test on an error.
func ok(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
