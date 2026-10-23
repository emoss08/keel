package keel_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/migrate"
	"github.com/jackc/pgx/v5"
)

// testDB is the database every integration test shares. It is created for the run and dropped
// after it.
var (
	testDSN   string
	testDB    *keel.DB
	setupErr  error
	adminDSN  = envOr("KEEL_TEST_DSN", "postgres://keel:keel@localhost:55432/keel?sslmode=disable")
	setupOnce sync.Once
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type tenantKey struct{}
type systemKey struct{}
type tenant struct{ org, bu ids.ID }

func tenantCtx(org, bu ids.ID) context.Context {
	return context.WithValue(context.Background(), tenantKey{}, tenant{org, bu})
}

func systemCtx(reason string) context.Context {
	return context.WithValue(context.Background(), systemKey{}, reason)
}

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
		dropDatabase()
	}
	os.Exit(code)
}

var dbName string

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

func setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	dbName = "keel_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		return err
	}
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		return err
	}
	cfg.Database = dbName
	testDSN = cfg.ConnString()
	testDSN = replaceDB(adminDSN, dbName)
	mig, err := migrate.Open(ctx, testDSN, os.DirFS("testdata/migrations"))
	if err != nil {
		return err
	}
	defer mig.Close()
	if _, err := mig.Up(ctx); err != nil {
		return err
	}
	testDB, err = keel.Open(ctx, testConfig(testDSN))
	return err
}

func replaceDB(dsn, name string) string {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host,
		cfg.Port, name)
}

func dropDatabase() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return
	}
	defer admin.Close(ctx)
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
}

// newTenant returns a context for a fresh tenant, so tests never see each other's rows.
func newTenant() context.Context {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	return tenantCtx(ids.ID("org_"+s), ids.ID("bu_"+s))
}
