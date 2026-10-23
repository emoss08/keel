package seed_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/seed"
	"github.com/jackc/pgx/v5"
)

// The tests share a template database holding the test models' tables. Each test gets its own
// database cloned from it, so bookkeeping, the environment mark and the seed lock never leak
// between tests.
var (
	adminDSN  = envOr("KEEL_TEST_DSN", "postgres://keel:keel@localhost:55432/keel?sslmode=disable")
	tplName   string
	setupErr  error
	setupOnce sync.Once
)

var tables = []string{
	`CREATE TABLE customers (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    code text NOT NULL,
    email text,
    version bigint NOT NULL DEFAULT 0,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT pk_customers PRIMARY KEY (id, organization_id, business_unit_id),
    CONSTRAINT uq_customers_code UNIQUE (organization_id, business_unit_id, code)
)`,
	`CREATE TABLE plants (
    code text PRIMARY KEY,
    name text NOT NULL
)`,
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	code := m.Run()
	if tplName != "" {
		dropDatabase(tplName)
	}
	os.Exit(code)
}

func randomName(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	name := randomName("keel_seed_tpl_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return err
	}
	tplName = name
	conn, err := pgx.Connect(ctx, dsnFor(name))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for _, q := range tables {
		if _, err := conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func dsnFor(name string) string {
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host,
		cfg.Port, name)
}

func dropDatabase(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return
	}
	defer admin.Close(ctx)
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
}

// newDatabase creates a database from the template and returns its DSN. Tests skip when Postgres
// is unreachable and KEEL_TEST_REQUIRE is not set.
func newDatabase(t testing.TB) string {
	t.Helper()
	setupOnce.Do(func() { setupErr = setup() })
	if setupErr != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatalf("postgres: %v", setupErr)
		}
		t.Skipf("postgres unavailable: %v", setupErr)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := randomName("keel_seed_test_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE "+tplName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dropDatabase(name) })
	return dsnFor(name)
}

// newDB returns a keel.DB on a fresh database.
func newDB(t testing.TB) *keel.DB {
	t.Helper()
	return openDB(t, newDatabase(t))
}

func openDB(t testing.TB, dsn string) *keel.DB {
	t.Helper()
	d, err := keel.Open(context.Background(), testConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

type tenantCtxKey struct{}
type systemCtxKey struct{}
type tenantScope struct{ org, bu ids.ID }

func testConfig(dsn string) keel.Config {
	return keel.Config{
		DSN:           dsn,
		TenantColumns: []string{"organization_id", "business_unit_id"},
		Tenant: func(ctx context.Context) ([]any, bool) {
			t, ok := ctx.Value(tenantCtxKey{}).(tenantScope)
			if !ok {
				return nil, false
			}
			return []any{t.org, t.bu}, true
		},
		System: func(ctx context.Context) (string, bool) {
			r, ok := ctx.Value(systemCtxKey{}).(string)
			return r, ok
		},
		NewID: func(prefix string) string { return randomName(prefix) },
	}
}

func system(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemCtxKey{}, "seed")
}

func tenantContext(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantScope{ids.ID("org_" + key), ids.ID("bu_" + key)})
}

// tenantList returns an Options.Tenants function listing the tenants named by keys.
func tenantList(keys ...string) func(context.Context, *keel.DB) iter.Seq2[seed.Tenant, error] {
	return func(context.Context, *keel.DB) iter.Seq2[seed.Tenant, error] {
		return func(yield func(seed.Tenant, error) bool) {
			for _, k := range keys {
				t := seed.Tenant{Key: k, Name: strings.ToUpper(k), Context: func(ctx context.Context) context.Context {
					return tenantContext(ctx, k)
				}}
				if !yield(t, nil) {
					return
				}
			}
		}
	}
}

// recorder collects the events of a run.
type recorder struct {
	mu     sync.Mutex
	events []seed.Event
}

func (r *recorder) observe(e seed.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) list() []seed.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seed.Event(nil), r.events...)
}

func (r *recorder) of(kind seed.EventKind) []seed.Event {
	var out []seed.Event
	for _, e := range r.list() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// counter counts the runs of seeds by name.
type counter struct {
	mu   sync.Mutex
	runs map[string]int
	log  []string
}

func newCounter() *counter { return &counter{runs: map[string]int{}} }

func (c *counter) run(name string) func(context.Context, *seed.Run) error {
	return func(context.Context, *seed.Run) error {
		c.hit(name)
		return nil
	}
}

func (c *counter) hit(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs[name]++
	c.log = append(c.log, name)
}

func (c *counter) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[name]
}

func (c *counter) order() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log...)
}

func ok(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eq[T comparable](t testing.TB, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func must[T any](v T, err error) func(testing.TB) T {
	return func(t testing.TB) T {
		t.Helper()
		ok(t, err)
		return v
	}
}

// opts returns the options most tests use.
func opts(rec *recorder) seed.Options {
	o := seed.Options{System: system, Environment: seed.Test}
	if rec != nil {
		o.Observer = rec.observe
	}
	return o
}

// item returns the plan item of the named seed.
func item(t testing.TB, p seed.Plan, name string) seed.PlanItem {
	t.Helper()
	for _, it := range p.Seeds {
		if it.Seed.Name == name {
			return it
		}
	}
	t.Fatalf("no plan item %q", name)
	return seed.PlanItem{}
}

// result returns the result of the named seed.
func result(t testing.TB, r seed.Report, name string) seed.Result {
	t.Helper()
	for _, res := range r.Results {
		if res.Name == name {
			return res
		}
	}
	t.Fatalf("no result %q", name)
	return seed.Result{}
}

// scalar reads one value with plain SQL.
func scalar[T any](t testing.TB, d *keel.DB, sql string, args ...any) T {
	t.Helper()
	var v T
	ok(t, d.Pool().QueryRow(context.Background(), sql, args...).Scan(&v))
	return v
}
