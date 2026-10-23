package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
)

func adminDSN() string {
	if v := os.Getenv("KEEL_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
}

// database creates an empty database for one test and returns its DSN.
func database(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN())
	if err != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatal(err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "keel_migrate_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgx.Connect(context.Background(), adminDSN())
		if err == nil {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			a.Close(context.Background())
		}
	})
	cfg, _ := pgx.ParseConfig(adminDSN())
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host,
		cfg.Port, name)
}

func source() fstest.MapFS {
	return fstest.MapFS{
		"1_items.up.sql":        {Data: []byte("CREATE TABLE items (id int PRIMARY KEY, name text);")},
		"1_items.down.sql":      {Data: []byte("DROP TABLE items;")},
		"2_index.notx.up.sql":   {Data: []byte("CREATE INDEX CONCURRENTLY idx_items_name ON items (name);\n--keel:split\nCREATE INDEX CONCURRENTLY idx_items_id_name ON items (id, name);\n")},
		"2_index.notx.down.sql": {Data: []byte("DROP INDEX CONCURRENTLY idx_items_name;\n--keel:split\nDROP INDEX CONCURRENTLY idx_items_id_name;\n")},
		"10_column.up.sql":      {Data: []byte("ALTER TABLE items ADD COLUMN price numeric;")},
		"10_column.down.sql":    {Data: []byte("ALTER TABLE items DROP COLUMN price;")},
		"README.md":             {Data: []byte("ignored")},
	}
}

func open(t *testing.T, dsn string, fsys fstest.MapFS) *Migrator {
	t.Helper()
	m, err := Open(context.Background(), dsn, fsys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestUpStatusDown(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	m := open(t, dsn, source())

	ids := []string{}
	for _, mg := range m.Migrations() {
		ids = append(ids, mg.ID())
	}
	if fmt.Sprint(ids) != "[1_items 2_index 10_column]" {
		t.Fatalf("order %v", ids)
	}

	applied, err := m.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 3 {
		t.Fatalf("applied %v", applied)
	}
	again, err := m.Up(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("second up: %v %v", again, err)
	}

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if !s.Applied || s.Changed {
			t.Fatalf("status %+v", s)
		}
	}

	var n int
	if err := m.conn.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE tablename = 'items'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("%d indexes", n)
	}

	reverted, err := m.Down(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(reverted) != "[10_column 2_index]" {
		t.Fatalf("reverted %v", reverted)
	}
	st, _ = m.Status(ctx)
	if !st[0].Applied || st[1].Applied || st[2].Applied {
		t.Fatalf("status after down %+v", st)
	}
}

func TestChangedMigrationIsRefused(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	src := source()
	m := open(t, dsn, src)
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	src["1_items.up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE items (id bigint PRIMARY KEY);")}
	m2 := open(t, dsn, src)
	if _, err := m2.Up(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("got %v, want ErrChanged", err)
	}
	st, err := m2.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st[0].Changed {
		t.Fatal("status does not report the change")
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	src := fstest.MapFS{
		"1_ok.up.sql":  {Data: []byte("CREATE TABLE a (id int);")},
		"2_bad.up.sql": {Data: []byte("CREATE TABLE b (id int); SELECT 1/0;")},
	}
	m := open(t, dsn, src)
	applied, err := m.Up(ctx)
	if err == nil {
		t.Fatal("bad migration applied")
	}
	if len(applied) != 1 {
		t.Fatalf("applied %v", applied)
	}
	var exists bool
	if err := m.conn.QueryRow(ctx, "SELECT to_regclass('b') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("a failed migration left its table behind")
	}
}

func TestConcurrentUpAppliesOnce(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	var wg sync.WaitGroup
	results := make([][]string, 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := Open(ctx, dsn, source())
			if err != nil {
				errs[i] = err
				return
			}
			defer m.Close()
			results[i], errs[i] = m.Up(ctx)
		}()
	}
	wg.Wait()
	total := 0
	for i := range 4 {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		total += len(results[i])
	}
	if total != 3 {
		t.Fatalf("applied %d migrations across runners, want 3", total)
	}
}

func TestReadErrors(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"bad name":          {"items.up.sql": {Data: []byte("x")}},
		"bad suffix":        {"1_items.sql": {Data: []byte("x")}},
		"down without up":   {"1_items.down.sql": {Data: []byte("x")}},
		"duplicate version": {"1_a.up.sql": {Data: []byte("x")}, "1_b.up.sql": {Data: []byte("x")}},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Read(fsys); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestSplit(t *testing.T) {
	got := Split("a;\n--keel:split\n\n  --keel:split  \nb;\nc;\n")
	if len(got) != 2 || got[0] != "a;" || got[1] != "b;\nc;" {
		t.Fatalf("%q", got)
	}
}

func TestCreate(t *testing.T) {
	dir := t.TempDir()
	paths, err := Create(dir, "Add Items", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatal(paths)
	}
	ms, err := Read(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Name != "add_items" || !ms[0].NoTx {
		t.Fatalf("%+v (%s)", ms, filepath.Base(paths[0]))
	}
}
