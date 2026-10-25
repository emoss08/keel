package keellog_test

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/keellog"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestFormat(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "select",
			sql: `SELECT s."id", s."status" FROM "shipments" s LEFT JOIN "customers" c ON c."id" = s."customer_id" ` +
				`WHERE s."organization_id" = $1 AND s."status" IS DISTINCT FROM $2 ORDER BY s."created_at" DESC LIMIT 25`,
			want: "SELECT s.id, s.status\n" +
				"FROM shipments s\n" +
				"LEFT JOIN customers c ON c.id = s.customer_id\n" +
				"WHERE s.organization_id = $1 AND s.status IS DISTINCT FROM $2\n" +
				"ORDER BY s.created_at DESC LIMIT 25",
		},
		{
			name: "reserved and mixed-case identifiers keep their quotes",
			sql:  `SELECT "order", "Name", 'a "b" c' FROM "t"`,
			want: "SELECT \"order\", \"Name\", 'a \"b\" c'\nFROM t",
		},
		{
			name: "upsert",
			sql: `INSERT INTO "tags" ("id", "name") VALUES ($1, $2) ON CONFLICT ("name") DO UPDATE SET "name" = ` +
				`EXCLUDED."name" RETURNING "id"`,
			want: "INSERT INTO tags (id, name)\n" +
				"VALUES ($1, $2)\n" +
				"ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name\n" +
				"RETURNING id",
		},
		{
			name: "delete and locking",
			sql:  `DELETE FROM "t" WHERE "id" IN (SELECT "id" FROM "t" FOR UPDATE)`,
			want: "DELETE FROM t\nWHERE id IN (SELECT id FROM t FOR UPDATE)",
		},
		{
			name: "long subquery is indented",
			sql: `WITH open AS (SELECT i."id", i."total" FROM "invoices" i WHERE i."status" = $1 AND i."due_at" < now()) ` +
				`SELECT o."id" FROM open o`,
			want: "WITH open AS (\n" +
				"  SELECT i.id, i.total\n" +
				"  FROM invoices i\n" +
				"  WHERE i.status = $1 AND i.due_at < now()\n" +
				")\n" +
				"SELECT o.id\n" +
				"FROM open o",
		},
		{
			name: "keywords inside literals and functions",
			sql:  `SELECT extract(epoch FROM x), 'FROM WHERE' FROM t`,
			want: "SELECT extract(epoch FROM x), 'FROM WHERE'\nFROM t",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keellog.Format(tt.sql); got != tt.want {
				t.Fatalf("Format:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

type status string

func (s status) String() string { return "status:" + string(s) }

type valuer struct{ v string }

func (v valuer) Value() (driver.Value, error) { return v.v, nil }

func TestInline(t *testing.T) {
	when := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	name := "it's"
	var nilPtr *string
	got := keellog.Inline(
		`SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, '$1', $13`,
		[]any{
			"plain", &name, nilPtr, 42, true, 1.5, when, []string{"New", "In Transit", ""},
			[]byte{0, 1, 2}, valuer{"v"}, strings.Repeat("x", 10), []int{1, 2, 3, 4}, status("a"),
		},
		6, 3,
	)
	want := `SELECT 'plain', 'it''s', NULL, 42, true, 1.5, '2026-10-25 09:30:00Z', '{New,"In Tra…",""}', ` +
		`<3 bytes>, 'v', 'xxxxxx…', '{1,2,3,… +1}', '$1', 'status…'`
	if got != want {
		t.Fatalf("Inline:\n%s\nwant:\n%s", got, want)
	}
}

func TestLoggerModes(t *testing.T) {
	off := false
	ctx := context.Background()
	query := &keel.QueryEvent{
		Op:       "select",
		Table:    "shipments",
		SQL:      `SELECT "id" FROM "shipments" WHERE "id" = $1`,
		Args:     1,
		Values:   []any{"shp_1"},
		Duration: 2300 * time.Microsecond,
		Rows:     1,
	}
	notFound := *query
	notFound.Err = keel.ErrNotFound
	notFound.Rows = 0
	unique := *query
	unique.Op, unique.Tx = "insert", true
	unique.Err = &keel.ConstraintError{
		Kind:       keel.ConstraintUnique,
		Constraint: "uq_customers_code",
		Columns:    []string{"code"},
		Err:        &pgconn.PgError{Code: "23505"},
	}
	slow := *query
	slow.Duration = 412 * time.Millisecond
	slow.Rows = 1204

	var buf bytes.Buffer
	all := keellog.New(keellog.Options{Out: &buf, Mode: keellog.All, Color: &off})
	for _, e := range []*keel.QueryEvent{query, &notFound, &unique, &slow} {
		all.AfterQuery(ctx, e)
	}
	out := buf.String()
	for _, want := range []string{
		"keel SELECT shipments                    2.3ms  1 row       keellog/keellog_test.go:",
		"     SELECT id\n     FROM shipments\n     WHERE id = 'shp_1'\n",
		"not found",
		"keel INSERT shipments             tx     2.3ms  ERROR",
		"     23505 unique violation uq_customers_code → code\n",
		"412.0ms  1,204 rows",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}

	buf.Reset()
	errs := keellog.New(keellog.Options{Out: &buf, Color: &off})
	for _, e := range []*keel.QueryEvent{query, &notFound, &unique, &slow} {
		errs.AfterQuery(ctx, e)
	}
	out = buf.String()
	if strings.Contains(out, "not found") || strings.Count(out, "keel ") != 2 {
		t.Fatalf("errors mode printed the wrong statements:\n%s", out)
	}
	if !strings.Contains(out, "ERROR") || !strings.Contains(out, "1,204 rows") {
		t.Fatalf("errors mode dropped a failure or a slow statement:\n%s", out)
	}
}

func TestLoggerSkip(t *testing.T) {
	off := false
	var buf bytes.Buffer
	l := keellog.New(keellog.Options{Out: &buf, Mode: keellog.All, Color: &off, Skip: []string{"github.com/emoss08/keel/keellog_test.helper"}})
	_, _, line, _ := runtime.Caller(0)
	helper(l)
	if want := fmt.Sprintf("keellog/keellog_test.go:%d", line+1); !strings.Contains(buf.String(), want) {
		t.Fatalf("caller not resolved past the skipped helper to %s:\n%s", want, buf.String())
	}
}

func helper(l *keellog.Logger) {
	l.AfterQuery(context.Background(), &keel.QueryEvent{Op: "exec", SQL: "SELECT 1", Err: errors.New("boom")})
}

func TestParseMode(t *testing.T) {
	if m, ok := keellog.ParseMode(" ALL "); !ok || m != keellog.All {
		t.Fatal("all")
	}
	if m, ok := keellog.ParseMode("errors"); !ok || m != keellog.Errors {
		t.Fatal("errors")
	}
	if _, ok := keellog.ParseMode("verbose"); ok {
		t.Fatal("verbose")
	}
}
