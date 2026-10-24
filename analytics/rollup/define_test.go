package rollup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/schema"
)

func TestDefineRejects(t *testing.T) {
	ship := fleetdb.ShipmentsSchema()
	count := rollup.Measures(rollup.Count())
	cases := []struct {
		name string
		def  func() (*rollup.Rollup, error)
		want string
	}{
		{"bad name", func() (*rollup.Rollup, error) {
			return rollup.Define("Daily", ship, rollup.Day(s.CreatedAt), nil, count)
		}, "the name must match"},
		{"column of another table", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(fleetdb.Customers.CreatedAt), nil, count)
		}, "customers.created_at is not a column of shipments"},
		{"column through a relation", func() (*rollup.Rollup, error) {
			return rollup.Define("r", fleetdb.CustomersSchema(), rollup.Day(fleetdb.Customers.CreatedAt),
				rollup.Dims(s.Customer().Name), count)
		}, "taken through a relation"},
		{"grain not a time", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.Weight), nil, count)
		}, "holds neither dates, times nor epochs"},
		{"tenant dimension", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), rollup.Dims(s.OrganizationID), count)
		}, "is a tenant column"},
		{"grain as dimension", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), rollup.Dims(s.CreatedAt), count)
		}, "is the grain column"},
		{"dimension twice", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), rollup.Dims(s.Status, s.Status), count)
		}, "listed twice"},
		{"tsvector dimension", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), rollup.Dims(s.SearchVector), count)
		}, "cannot be grouped by"},
		{"sum of text", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), nil, rollup.Measures(rollup.Sum(s.ProNumber)))
		}, "does not hold numbers"},
		{"max of booleans", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), nil, rollup.Measures(rollup.Max(s.Hazmat)))
		}, "not ordered"},
		{"no measures", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), nil, nil)
		}, "at least one measure"},
		{"bad zone", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), nil, count, rollup.Zone("Mars/Olympus"))
		}, "time zone"},
		{"not a column", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(keel.Val(1)), nil, count)
		}, "is not a generated column"},
		{"valid declaration", func() (*rollup.Rollup, error) {
			return rollup.Define("r", ship, rollup.Day(s.CreatedAt), rollup.Dims(s.StageRank), rollup.Measures(rollup.Count(), rollup.Sum(s.Weight), rollup.Min(s.Weight)))
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.def()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRollupTableSchema(t *testing.T) {
	tbl := daily.Table()
	var names []string
	for _, c := range tbl.Columns {
		names = append(names, c.Name)
	}
	want := "organization_id business_unit_id bucket customer_id status row_count total_sum weight_sum weight_count weight_max bill_to_id_distinct"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("columns = %s\nwant      %s", got, want)
	}
	if len(tbl.Tenant) != 2 || tbl.Columns[tbl.Tenant[0]].Name != "organization_id" {
		t.Fatalf("tenant columns = %v", tbl.Tenant)
	}
	if len(tbl.Relations) != 1 || tbl.Relations[0].Name != "Customer" || tbl.Relations[0].Local[0] != 3 {
		t.Fatalf("relations = %+v", tbl.Relations)
	}
	if k := tbl.Columns[6].Kind; k != schema.KindOther || tbl.Columns[6].Report.Type != schema.ValueDecimal {
		t.Fatalf("total_sum is %v %v, want a decimal", k, tbl.Columns[6].Report.Type)
	}
	if daily.Unit() != ir.BucketDay || daily.Location().String() != "UTC" || daily.GrainColumn().Name != "created_at" {
		t.Fatal("accessors disagree with the declaration")
	}
	if len(daily.Dimensions()) != 2 || daily.Source() != fleetdb.ShipmentsSchema() || daily.Name() != "shipments_daily" {
		t.Fatal("accessors disagree with the declaration")
	}
}

func TestMigration(t *testing.T) {
	sql := rollup.Migration(monthly)
	for _, want := range []string{
		"--keel:notx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS "keel_rollup_shipments_created_at" ON "shipments" ("organization_id", "business_unit_id", "created_at");`,
		`CREATE TABLE "shipments_monthly" AS SELECT s."organization_id" AS "organization_id", s."business_unit_id" AS "business_unit_id", date_trunc('month', to_timestamp(s."created_at"), 'UTC') AS "bucket", s."status" AS "status", count(*) AS "row_count", sum(s."total") AS "total_sum" FROM "shipments" s GROUP BY 1, 2, 3, 4 WITH NO DATA;`,
		`CREATE UNIQUE INDEX "shipments_monthly_key" ON "shipments_monthly" ("organization_id", "business_unit_id", "bucket", "status") NULLS NOT DISTINCT;`,
		`CREATE OR REPLACE TRIGGER "keel_rollup_shipments_monthly_update" AFTER UPDATE ON "shipments" REFERENCING OLD TABLE AS keel_old NEW TABLE AS keel_new FOR EACH STATEMENT EXECUTE FUNCTION keel_rollup_shipments_monthly();`,
		`EXCEPT ALL SELECT "organization_id", "business_unit_id", "status", "total", "created_at" FROM keel_new`,
		"INSERT INTO keel_rollup_state (rollup, ready_at) VALUES ('shipments_monthly', now())",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("the migration lacks %s\n%s", want, sql)
		}
	}
	m := migrate.Migration{SQL: sql, Directives: migrate.Directives{NoTx: true}, Version: "20270101000000", Name: "rollup"}
	if parts := m.Statements(); len(parts) != 3 {
		t.Fatalf("the migration has %d parts, want 3", len(parts))
	}
	if strings.Contains(rollup.Migration(daily), "CONCURRENTLY") {
		t.Fatal("WithoutSourceIndex still creates the source index")
	}
	for _, f := range migrate.Lint([]migrate.Migration{m}) {
		t.Errorf("lint: %s: %s", f.Rule, f.Message)
	}
	drop, err := rollup.DropMigration("shipments_monthly", fleetdb.ShipmentsSchema())
	ok(t, err)
	for _, want := range []string{`DROP TRIGGER IF EXISTS "keel_rollup_shipments_monthly_delete" ON "shipments";`, `DROP TABLE IF EXISTS "shipments_monthly";`} {
		if !strings.Contains(drop, want) {
			t.Fatalf("the drop migration lacks %s", want)
		}
	}
	if _, err := rollup.DropMigration("x; DROP", fleetdb.ShipmentsSchema()); err == nil {
		t.Fatal("a bad name was accepted")
	}
	fn := rollup.Func("20270101000000", daily)
	if fn.Name != "rollup_shipments_daily" || fn.UpNoTx == nil {
		t.Fatalf("Func = %+v", fn)
	}
}

func TestWriteMigrations(t *testing.T) {
	dir := t.TempDir()
	if err := rollup.CheckMigrations(dir, daily); err == nil {
		t.Fatal("a missing migration passed the check")
	}
	written, err := rollup.WriteMigrations(dir, daily, monthly)
	ok(t, err)
	if len(written) != 2 {
		t.Fatalf("wrote %v", written)
	}
	again, err := rollup.WriteMigrations(dir, daily, monthly)
	ok(t, err)
	if len(again) != 0 {
		t.Fatalf("rewrote unchanged rollups: %v", again)
	}
	ok(t, rollup.CheckMigrations(dir, daily, monthly))
	changed := rollup.MustDefine("shipments_daily", fleetdb.ShipmentsSchema(), rollup.Day(s.CreatedAt),
		rollup.Dims(s.Status), rollup.Measures(rollup.Count()))
	if err := rollup.CheckMigrations(dir, changed); err == nil || !strings.Contains(err.Error(), "shipments_daily") {
		t.Fatalf("a changed declaration passed the check: %v", err)
	}
	more, err := rollup.WriteMigrations(dir, changed)
	ok(t, err)
	if len(more) != 1 {
		t.Fatalf("wrote %v for the changed rollup", more)
	}
	body, err := os.ReadFile(more[0])
	ok(t, err)
	if string(body) != rollup.Migration(changed) || !strings.HasSuffix(filepath.Base(more[0]), "_rollup_shipments_daily.sql") {
		t.Fatalf("wrote %s", more[0])
	}
}
