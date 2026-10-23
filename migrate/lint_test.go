package migrate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// lintOne lints one migration file and returns "rule:line:severity" for each finding.
func lintOne(t *testing.T, body string, rules ...LintRule) []string {
	t.Helper()
	ms, err := one(files("20260101000000_m.sql", body)).Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range Lint(ms, rules...) {
		if f.Migration.ID() != "20260101000000_m" || f.Message == "" {
			t.Fatalf("finding %+v", f)
		}
		out = append(out, fmt.Sprintf("%s:%d:%s", f.Rule, f.Line, f.Severity))
	}
	return out
}

func TestLintRules(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"index on existing table", "SELECT 1;\nCREATE INDEX shipments_status ON shipments (status);", "[index-concurrently:2:warning]"},
		{"unique index on existing table", "CREATE UNIQUE INDEX IF NOT EXISTS x ON ONLY public.\"Shipments\" (a);", "[index-concurrently:1:warning]"},
		{"index concurrently", "--keel:notx\nCREATE INDEX CONCURRENTLY x ON shipments (status);", "[]"},
		{"index on new table", "CREATE TABLE IF NOT EXISTS public.items (id int);\nCREATE INDEX items_id ON items (id);", "[]"},
		{"index on new quoted table", "CREATE UNLOGGED TABLE \"Items\" (id int);\nCREATE INDEX i ON public.\"Items\" (id);", "[]"},
		{"index text in a literal", "SELECT 'CREATE INDEX x ON t (a)'; -- CREATE INDEX y ON t (a)\nSELECT $$CREATE INDEX z ON t (a)$$;", "[]"},

		{"not null without default", "ALTER TABLE shipments ADD COLUMN hold boolean NOT NULL;", "[not-null-default:1:error]"},
		{"not null in a list", "ALTER TABLE shipments ADD COLUMN a int, ADD b numeric(10, 2) NOT NULL CHECK (b > 0);", "[not-null-default:1:error]"},
		{"not null with default", "ALTER TABLE shipments ADD COLUMN IF NOT EXISTS hold boolean NOT NULL DEFAULT false;", "[]"},
		{"not null generated", "ALTER TABLE shipments ADD COLUMN n int NOT NULL GENERATED ALWAYS AS IDENTITY;", "[]"},
		{"nullable column", "ALTER TABLE shipments ADD COLUMN hold boolean;", "[]"},
		{"not null on new table", "CREATE TABLE t (id int);\nALTER TABLE t ADD COLUMN hold boolean NOT NULL;", "[]"},

		{"concurrently in a transaction", "CREATE INDEX CONCURRENTLY x ON shipments (status);", "[notx-mixed:1:error]"},
		{"drop concurrently in a transaction", "DROP INDEX CONCURRENTLY IF EXISTS x;", "[notx-mixed:1:error]"},
		{"vacuum in a transaction", "VACUUM shipments;", "[notx-mixed:1:error]"},
		{"refresh concurrently", "REFRESH MATERIALIZED VIEW CONCURRENTLY v;", "[]"},
		{"concurrently sharing a part", "--keel:notx\nSELECT 1;\nCREATE INDEX CONCURRENTLY x ON t (a);\n--keel:split\nCREATE INDEX CONCURRENTLY y ON t (b);", "[notx-mixed:3:error]"},
		{"enum value beside others", "ALTER TYPE status ADD VALUE 'held';\nUPDATE shipments SET status = 'held';", "[notx-mixed:1:warning]"},
		{"enum value alone", "ALTER TYPE status ADD VALUE IF NOT EXISTS 'held';", "[]"},

		{"column type change", "ALTER TABLE shipments ALTER COLUMN weight TYPE bigint;", "[lock-heavy:1:warning]"},
		{"column set data type", "ALTER TABLE shipments ALTER weight SET DATA TYPE bigint USING weight::bigint;", "[lock-heavy:1:warning]"},
		{"set not null", "ALTER TABLE shipments ALTER COLUMN weight SET NOT NULL;", "[lock-heavy:1:warning]"},
		{"validating constraint", "ALTER TABLE stops ADD CONSTRAINT fk_move FOREIGN KEY (move_id) REFERENCES moves (id);", "[lock-heavy:1:warning]"},
		{"unique constraint", "ALTER TABLE stops ADD CONSTRAINT uq UNIQUE (a, b);", "[lock-heavy:1:warning]"},
		{"unique using index", "ALTER TABLE stops ADD CONSTRAINT uq UNIQUE USING INDEX stops_uq;", "[]"},
		{"not valid constraint", "ALTER TABLE stops ADD CONSTRAINT ck CHECK (a > 0) NOT VALID;", "[]"},
		{"validate in the same migration", "ALTER TABLE stops ADD CONSTRAINT ck CHECK (a > 0) NOT VALID;\nALTER TABLE stops VALIDATE CONSTRAINT ck;", "[lock-heavy:2:warning]"},
		{"validate later", "ALTER TABLE stops VALIDATE CONSTRAINT ck;", "[]"},
		{"drop default", "ALTER TABLE shipments ALTER COLUMN weight DROP DEFAULT, ALTER COLUMN weight SET DEFAULT 0;", "[]"},
		{"type change on new table", "CREATE TABLE t (a int);\nALTER TABLE t ALTER COLUMN a TYPE bigint;", "[]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fmt.Sprint(lintOne(t, c.sql)); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestLintIgnoreAndCustomRules(t *testing.T) {
	if got := lintOne(t, "--keel:lint_ignore index-concurrently\nCREATE INDEX x ON shipments (status);"); len(got) != 0 {
		t.Fatalf("ignored rule reported: %v", got)
	}
	rls := LintRule{Name: "tenant-rls", Check: func(m Migration) []Finding {
		if strings.Contains(m.SQL, "CREATE TABLE") && !strings.Contains(m.SQL, "ENABLE ROW LEVEL SECURITY") {
			return []Finding{{Line: 1, Message: "tenant table without RLS", Severity: SeverityError}}
		}
		return nil
	}}
	got := lintOne(t, "CREATE TABLE t (id int);\nCREATE INDEX x ON shipments (a);", rls)
	if fmt.Sprint(got) != "[tenant-rls:1:error index-concurrently:2:warning]" {
		t.Fatalf("findings %v", got)
	}
	ms, err := one(nil, Func{Version: "20260101000000", Name: "g", Up: func(context.Context, pgx.Tx) error { return nil }}).Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if got := Lint(ms); len(got) != 0 {
		t.Fatalf("Go migration findings %v", got)
	}
}
