package adapt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emoss08/keel/keelcli/migrateport"
)

func TestWithDatabase(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@h:5432/app?sslmode=disable": "postgres://u:p@h:5432/postgres?sslmode=disable",
		"postgresql://h/app":                        "postgresql://h/postgres",
		"host=h dbname=app user=u":                  "host=h dbname=app user=u dbname=postgres",
	}
	for in, want := range cases {
		if got := WithDatabase(in, "postgres"); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestCopyPrefix(t *testing.T) {
	if got := copyPrefix("Trenova-DevDB"); got != "keel_trenova_devdb_" {
		t.Fatalf("got %q", got)
	}
	if got := copyPrefix(strings.Repeat("x", 80)); len(got)+12 > 63 {
		t.Fatalf("%q leaves no room for the suffix", got)
	}
}

func TestCreated(t *testing.T) {
	m := created("schema/migrations/20261024120000_add_holds.sql", "core", migrateport.NoTx)
	if m.Version != "20261024120000" || m.Name != "add_holds" || m.Source != "core" || !m.Directives.NoTx {
		t.Fatalf("%+v", m)
	}
	r := created("schema/migrations/R_views.sql", "core", migrateport.RepeatableFile)
	if r.ID() != "R_views" || r.Kind != migrateport.Repeatable {
		t.Fatalf("%+v", r)
	}
}

func TestStampPackage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "schema")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(dir, "version_gen.go")
	if got := stampPackage(stamp); got != "schema" {
		t.Fatalf("from the directory: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("package dbschema\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := stampPackage(stamp); got != "dbschema" {
		t.Fatalf("from the files beside it: %q", got)
	}
}
