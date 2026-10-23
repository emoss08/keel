package migrate

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
)

// adminDSN is the test server's DSN.
func adminDSN() string {
	if v := os.Getenv("KEEL_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
}

// requirePostgres skips or fails the test when Postgres is unreachable.
func requirePostgres(t *testing.T) {
	t.Helper()
	c, err := pgx.Connect(context.Background(), adminDSN())
	if err != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	_ = c.Close(context.Background())
}

// database creates an empty database for one test and returns its DSN.
func database(t *testing.T) string {
	t.Helper()
	requirePostgres(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := "keel_migrate_" + randomHex(5)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgx.Connect(context.Background(), adminDSN())
		if err == nil {
			_ = dropDatabase(context.Background(), a, name)
			a.Close(context.Background())
		}
	})
	dsn, err := withDatabase(adminDSN(), name)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// connect opens a connection the test closes.
func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// files builds a source file system from names and contents.
func files(kv ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 0; i < len(kv); i += 2 {
		fsys[kv[i]] = &fstest.MapFile{Data: []byte(kv[i+1])}
	}
	return fsys
}

// one returns a set of one source named core.
func one(fsys fstest.MapFS, funcs ...Func) Set {
	return Sources(Source{Name: "core", FS: fsys, Funcs: funcs})
}

// open returns a Migrator the test closes.
func open(t *testing.T, dsn string, set Set, opts Options) *Migrator {
	t.Helper()
	m, err := Open(context.Background(), dsn, set, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// recorder collects events from a run on another goroutine.
type recorder struct {
	mu     sync.Mutex
	events []Event
	notify chan Event
}

// newRecorder returns a recorder whose notify channel receives every event without blocking.
func newRecorder() *recorder { return &recorder{notify: make(chan Event, 1024)} }

// observe is the Observer.
func (r *recorder) observe(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	select {
	case r.notify <- e:
	default:
	}
}

// all returns the events so far.
func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// kinds returns the kinds of the events so far.
func (r *recorder) kinds() []EventKind {
	var out []EventKind
	for _, e := range r.all() {
		out = append(out, e.Kind)
	}
	return out
}

// ids returns the migrations' IDs.
func migrationIDs(ms []Migration) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID()
	}
	return out
}

// scalar runs a one-value query.
func scalar[T any](t *testing.T, c *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := c.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
