package keel_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/migrate"
)

// testDB is the database every integration test shares. It is copied from a template for the run
// and dropped after it with dropTest.
var (
	testDSN   string
	testDB    *keel.DB
	dropTest  func(context.Context) error
	setupErr  error
	adminDSN  = envOr("KEEL_TEST_DSN", "postgres://keel:keel@localhost:55432/keel?sslmode=disable")
	setupOnce sync.Once
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

// systemKey holds a system scope's reason on a context.
type systemKey struct{}

// tenant is the tenant columns' values.
type tenant struct{ org, bu ids.ID }

// tenantCtx returns a context scoped to a tenant.
func tenantCtx(org, bu ids.ID) context.Context {
	return context.WithValue(context.Background(), tenantKey{}, tenant{org, bu})
}

// systemCtx returns a context with a system scope.
func systemCtx(reason string) context.Context {
	return context.WithValue(context.Background(), systemKey{}, reason)
}

// testConfig returns the Config the integration tests open with.
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
		System: func(ctx context.Context) (string, bool) {
			r, ok := ctx.Value(systemKey{}).(string)
			return r, ok
		},
		NewID: func(prefix string) string {
			b := make([]byte, 10)
			_, _ = rand.Read(b)
			return prefix + hex.EncodeToString(b)
		},
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

// db returns the shared test database, creating it on first use. Tests skip when Postgres is
// unreachable and KEEL_TEST_REQUIRE is not set.
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

// setup copies the test database from a template of testdata/migrations, built on first use.
func setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var err error
	testDSN, dropTest, err = migrate.Template(ctx, migrate.TemplateOptions{
		Admin:   adminDSN,
		Sources: migrate.Sources(migrate.Source{Name: "testdata", FS: os.DirFS("testdata/migrations")}),
		Prefix:  "keel_test_",
	})
	if err != nil {
		return err
	}
	testDB, err = keel.Open(ctx, testConfig(testDSN))
	return err
}

// newTenant returns a context for a fresh tenant, so tests never see each other's rows.
func newTenant() context.Context {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	return tenantCtx(ids.ID("org_"+s), ids.ID("bu_"+s))
}
