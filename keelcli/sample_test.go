package keelcli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"iter"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/seed"
)

func testAdminDSN() string {
	if v := os.Getenv("KEEL_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func requirePostgres(t *testing.T) {
	t.Helper()
	c, err := pgx.Connect(context.Background(), testAdminDSN())
	if err != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	_ = c.Close(context.Background())
}

func adminExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, testAdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func newTestDatabase(t *testing.T) string {
	t.Helper()
	requirePostgres(t)
	name := "keelcli_" + randomHex(5)
	adminExec(t, "CREATE DATABASE "+name)
	t.Cleanup(func() {
		ctx := context.Background()
		if c, err := pgx.Connect(ctx, testAdminDSN()); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	})
	u, err := url.Parse(testAdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func dbExec(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func dbScalar[T any](t *testing.T, dsn, sql string, args ...any) T {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var v T
	if err := c.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

const sampleDir = "testdata/sample/migrations"

var sampleBackfill = migrate.Func{
	Version: "20260103000000",
	Name:    "backfill_item_states",
	Up: func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE items SET state = NULL WHERE state = ''")
		return err
	},
}

type sampleState struct {
	Code string `yaml:"code"`
	Name string `yaml:"name"`
}

func newSampleSeeds() *seed.Registry {
	seeds := seed.NewRegistry()
	data := os.DirFS("testdata/sample")
	states := seeds.Add(seed.Seed{
		Name:        "states",
		Kind:        seed.Reference,
		Description: "US states from seeds/states.yaml",
		Inputs:      seed.Files(data, "seeds/states.yaml"),
		Run: func(ctx context.Context, r *seed.Run) error {
			rows, err := seed.YAML[[]sampleState](r, "seeds/states.yaml")
			if err != nil {
				return err
			}
			for _, s := range rows {
				n, err := keel.Exec(ctx, r.DB, `INSERT INTO states (code, name) VALUES ($1, $2)
ON CONFLICT (code) DO UPDATE SET name = EXCLUDED.name`, s.Code, s.Name)
				if err != nil {
					return err
				}
				r.Count(n)
			}
			return nil
		},
	})
	seeds.Add(seed.Seed{
		Name:        "tenant_settings",
		Kind:        seed.Reference,
		Description: "default settings for every tenant",
		After:       []*seed.Seed{states},
		PerTenant:   true,
		Version:     "1",
		Run: func(ctx context.Context, r *seed.Run) error {
			n, err := keel.Exec(ctx, r.DB, `INSERT INTO tenant_settings (tenant_id, key, value) VALUES ($1, 'home_state', 'CA')
ON CONFLICT (tenant_id, key) DO NOTHING`, r.Tenant.Key)
			r.Count(n)
			return err
		},
	})
	seeds.Add(seed.Seed{
		Name:        "demo_items",
		Kind:        seed.Fixture,
		Description: "items for trying the application",
		After:       []*seed.Seed{states},
		Run: func(ctx context.Context, r *seed.Run) error {
			n, err := keel.Exec(ctx, r.DB, `INSERT INTO items (tenant_id, name, state)
SELECT id, 'widget for ' || name, 'NY' FROM tenants`)
			r.Count(n)
			return err
		},
	})
	return seeds
}

func sampleTenants(ctx context.Context, db *keel.DB) iter.Seq2[seed.Tenant, error] {
	return func(yield func(seed.Tenant, error) bool) {
		rows, err := db.Pool().Query(ctx, "SELECT id, name FROM tenants ORDER BY id")
		if err != nil {
			yield(seed.Tenant{}, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var t seed.Tenant
			if err := rows.Scan(&t.Key, &t.Name); err != nil {
				yield(seed.Tenant{}, err)
				return
			}
			t.Context = func(ctx context.Context) context.Context { return ctx }
			if !yield(t, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(seed.Tenant{}, err)
		}
	}
}

var sampleLintRule = migrate.LintRule{
	Name: "no-serial",
	Check: func(m migrate.Migration) []migrate.Finding {
		for i, line := range strings.Split(m.SQL, "\n") {
			if strings.Contains(line, " serial ") {
				return []migrate.Finding{{Line: i + 1, Message: "use identity columns, not serial", Severity: migrate.SeverityWarning}}
			}
		}
		return nil
	},
}

func sampleApp(name, dsn string) App {
	return App{
		Name:        name,
		Description: "The sample application's database.",
		Version:     "1.0.0",
		Config:      func() (keel.Config, error) { return keel.Config{DSN: dsn}, nil },
		Migrations:  migrate.Sources(migrate.Source{Name: "core", FS: os.DirFS(sampleDir), Funcs: []migrate.Func{sampleBackfill}}),
		Migrate:     migrate.Options{After: []string{"SELECT count(*) FROM tenants"}},
		Dirs:        []migrate.Dir{{Name: "core", Path: sampleDir}},
		Stamp:       "testdata/sample/schema/version_gen.go",
		Lint:        []migrate.LintRule{sampleLintRule},
		Seeds:       newSampleSeeds(),
		Tenants:     sampleTenants,
		System:      func(ctx context.Context) context.Context { return ctx },
		Extensions:  []string{"plpgsql"},
	}
}

func dropOwnTemplates(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	ts, err := migrate.Templates(ctx, testAdminDSN())
	if err != nil {
		t.Error(err)
		return
	}
	for _, tpl := range ts {
		if tpl.Key == name || strings.HasPrefix(tpl.Key, name+":") {
			if err := migrate.DropTemplate(ctx, testAdminDSN(), tpl.Name); err != nil {
				t.Error(err)
			}
		}
	}
}

type sample struct {
	name string
	dsn  string
	app  App
}

func newSample(t *testing.T) *sample {
	t.Helper()
	dsn := newTestDatabase(t)
	name := "keelcli-sample-" + randomHex(4)
	t.Cleanup(func() { dropOwnTemplates(t, name) })
	return &sample{name: name, dsn: dsn, app: sampleApp(name, dsn)}
}

func (s *sample) run(t *testing.T, args ...string) result {
	t.Helper()
	return runApp(t, s.app, false, nil, args...)
}

func runApp(t *testing.T, app App, tty bool, in io.Reader, args ...string) result {
	t.Helper()
	if in == nil {
		in = strings.NewReader("")
	}
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := app.run(ctx, args, streams{
		in: in, out: &out, err: &errb, inTTY: tty, outTTY: tty, width: 100,
		environ: []string{"TERM=xterm-256color"}, now: time.Now,
	})
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func (r result) expect(t *testing.T, code int, wants ...string) {
	t.Helper()
	all := r.stdout + r.stderr
	if r.code != code {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, code, r.stdout, r.stderr)
	}
	for _, w := range wants {
		if !strings.Contains(all, w) {
			t.Fatalf("output lacks %q\nstdout:\n%s\nstderr:\n%s", w, r.stdout, r.stderr)
		}
	}
}

func (r result) lacks(t *testing.T, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(r.stdout+r.stderr, u) {
			t.Fatalf("output holds %q\nstdout:\n%s\nstderr:\n%s", u, r.stdout, r.stderr)
		}
	}
}
