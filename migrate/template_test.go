package migrate

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// dropTemplate removes a template the test built.
func dropTemplate(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminDSN())
		if err == nil {
			_ = dropDatabase(context.Background(), c, name)
			c.Close(context.Background())
		}
	})
}

func TestTemplate(t *testing.T) {
	requirePostgres(t)
	ctx := context.Background()
	key := randomHex(4)
	set := one(files("20260101000000_items.sql", "CREATE TABLE items (id int PRIMARY KEY, name text);"))
	name, err := TemplateName(set, key)
	if err != nil {
		t.Fatal(err)
	}
	dropTemplate(t, name)
	if !strings.HasPrefix(name, TemplatePrefix) || len(name) != len(TemplatePrefix)+16 {
		t.Fatalf("name %s", name)
	}
	var builds atomic.Int32
	opts := TemplateOptions{
		Admin: adminDSN(), Sources: set, Key: key, Prefix: "keel_tpltest_",
		Prepare: func(ctx context.Context, dsn string) error {
			builds.Add(1)
			time.Sleep(100 * time.Millisecond)
			c, err := pgx.Connect(ctx, dsn)
			if err != nil {
				return err
			}
			defer c.Close(ctx)
			_, err = c.Exec(ctx, "INSERT INTO items VALUES (1, 'seeded')")
			return err
		},
	}
	dsns := make([]string, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			var drop func(context.Context) error
			dsns[i], drop, errs[i] = Template(ctx, opts)
			if drop != nil {
				t.Cleanup(func() { _ = drop(context.Background()) })
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if builds.Load() != 1 {
		t.Fatalf("built %d times", builds.Load())
	}
	if dsns[0] == dsns[1] || !strings.Contains(dsns[0], "keel_tpltest_") {
		t.Fatalf("dsns %v", dsns)
	}
	a, b := connect(t, dsns[0]), connect(t, dsns[1])
	if got := scalar[string](t, a, "SELECT name FROM items"); got != "seeded" {
		t.Fatalf("clone holds %q", got)
	}
	if _, err := a.Exec(ctx, "INSERT INTO items VALUES (2, 'mine')"); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, b, "SELECT count(*) FROM items"); n != 1 {
		t.Fatalf("clones share rows: %d", n)
	}
	if err := Require(ctx, b, "20260101000000"); err != nil {
		t.Fatalf("clone is not migrated: %v", err)
	}

	list, err := Templates(ctx, adminDSN())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list, func(ti TemplateInfo) bool { return ti.Name == name })
	if i < 0 || list[i].Size <= 0 || list[i].Created.IsZero() || list[i].Fingerprint != set.Fingerprint() || list[i].Key != key {
		t.Fatalf("templates %+v", list)
	}

	db := strings.TrimPrefix(dsns[0][strings.LastIndex(dsns[0], "/"):], "/")
	db, _, _ = strings.Cut(db, "?")
	a.Close(ctx)
	if err := ResetFromTemplate(ctx, adminDSN(), db, name); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, connect(t, dsns[0]), "SELECT count(*) FROM items"); n != 1 {
		t.Fatalf("reset kept %d rows", n)
	}
	if err := ResetFromTemplate(ctx, adminDSN(), db, "keel_tpl_missing"); err == nil {
		t.Fatal("reset from a missing template")
	}

	if list, err = Templates(ctx, adminDSN()); err != nil {
		t.Fatal(err)
	}
	var keep []string
	for _, ti := range list {
		if ti.Name != name {
			keep = append(keep, ti.Name)
		}
	}
	dropped, err := PruneTemplates(ctx, adminDSN(), keep)
	if err != nil || !slices.Contains(dropped, name) {
		t.Fatalf("pruned %v %v", dropped, err)
	}
	list, _ = Templates(ctx, adminDSN())
	if slices.ContainsFunc(list, func(ti TemplateInfo) bool { return ti.Name == name }) {
		t.Fatal("template survived prune")
	}
}

func TestTemplateFailedBuild(t *testing.T) {
	requirePostgres(t)
	ctx := context.Background()
	set := one(files("20260101000000_bad.sql", "SELECT 1/0;"))
	key := randomHex(4)
	name, _ := TemplateName(set, key)
	if _, _, err := Template(ctx, TemplateOptions{Admin: adminDSN(), Sources: set, Key: key}); err == nil {
		t.Fatal("no error")
	}
	c := connect(t, adminDSN())
	if n := scalar[int](t, c, "SELECT count(*) FROM pg_database WHERE datname = $1 OR datname = $2",
		name, buildPrefix+strings.TrimPrefix(name, TemplatePrefix)); n != 0 {
		t.Fatalf("failed build left %d databases", n)
	}
}

func TestWithDatabase(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@h:5432/a?sslmode=disable": "postgres://u:p@h:5432/b?sslmode=disable",
		"postgresql://h/a":                        "postgresql://h/b",
		"host=h dbname=a user=u":                  "host=h dbname=b user=u",
		"host=h dbname='a b' user=u":              "host=h dbname=b user=u",
		"host=h user=u":                           "host=h user=u dbname=b",
	}
	for in, want := range cases {
		if got, err := withDatabase(in, "b"); err != nil || got != want {
			t.Errorf("%s: got %s %v, want %s", in, got, err, want)
		}
	}
}
