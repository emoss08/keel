package analyticscli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/analyticscli"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/keelcli"
	"github.com/emoss08/keel/keelcli/fake"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/schema"
)

var s = fleetdb.Shipments

// byStatus is the rollup the command tests generate and refresh.
var byStatus = rollup.MustDefine("cli_shipments_by_status", fleetdb.ShipmentsSchema(),
	rollup.Month(s.CreatedAt), rollup.Dims(s.Status), rollup.Measures(rollup.Count(), rollup.Sum(s.Total)),
	rollup.WithoutSourceIndex())

func tables() []*schema.Table {
	return []*schema.Table{fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema(), fleetdb.MovesSchema(), fleetdb.TagsSchema()}
}

func TestMessages(t *testing.T) {
	m := analyticscli.Messages(tables(), rollup.Keys{})
	want := map[string]string{
		"shipment":                       "Shipment",
		"shipment@plural":                "Shipments",
		"shipment@description":           "Shipment is a load moved for a customer.",
		"shipment.proNumber":             "PRO Number",
		"shipment.proNumber@description": "ProNumber is the carrier's progressive number, unique per tenant.",
		"shipment.status.InTransit":      "In Transit",
		"shipment.customer":              "Customer",
		"shipment.billTo":                "Bill-to Customer",
		"shipment.moves":                 "Moves",
		"@category.Operations":           "Operations",
		"@category.Sales":                "Sales",
		"customer.name":                  "Name",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	for _, absent := range []string{"shipment.customerId", "shipment.organizationId", "shipment.tags", "tag", "shipment.originStop", "shipment.attributes"} {
		if _, ok := m[absent]; ok {
			t.Errorf("%s is in the catalog", absent)
		}
	}
}

func TestWriteAndCheckMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i18n", "analytics.json")
	ok(t, analyticscli.WriteMessages(path, tables(), rollup.Keys{}))
	ok(t, analyticscli.CheckMessages(path, tables(), rollup.Keys{}))
	data, err := os.ReadFile(path)
	ok(t, err)
	if !bytes.HasPrefix(data, []byte("{\n  \"@category.Operations\": \"Operations\",\n")) || !bytes.HasSuffix(data, []byte("}\n")) {
		t.Fatalf("the catalog is not sorted, indented JSON:\n%s", data)
	}
	var m map[string]string
	ok(t, json.Unmarshal(data, &m))
	m["shipment"] = "Load"
	stale, _ := json.Marshal(m)
	ok(t, os.WriteFile(path, stale, 0o644))
	err = analyticscli.CheckMessages(path, tables(), rollup.Keys{})
	if err == nil || !strings.Contains(err.Error(), "[shipment]") {
		t.Fatalf("a stale catalog passed: %v", err)
	}
}

func TestDescribe(t *testing.T) {
	text := analyticscli.Describe(tables(), rollup.Keys{}, byStatus)
	for _, want := range []string{
		"## shipment: Shipment (Operations)\nShipment is a load moved for a customer.\n",
		"- proNumber (string; dimension; count count_distinct min max): PRO Number. ProNumber is the carrier's progressive number, unique per tenant.\n",
		`- status (enum: New, Assigned, InTransit "In Transit", Delivered, Canceled; dimension; count count_distinct): Status`,
		"- total (decimal; measure; count count_distinct sum avg min max; format money): Total\n",
		"- moves -> move (many): Moves\n",
		"- cli_shipments_by_status: by month of createdAt in UTC and status; count rows, sum total\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the description lacks %q", want)
		}
	}
	if strings.Contains(text, "## tag") || strings.Contains(text, "customerId") {
		t.Fatalf("the description lists hidden things:\n%s", text)
	}
}

// app is a keelcli binary with the analytics command.
func app(cfg analyticscli.Config) keelcli.App {
	return keelcli.App{Name: "fleet-db", Migrations: fake.New(fake.Options{}).Migrations(), Commands: []keelcli.Command{analyticscli.Command(cfg)}}
}

// run runs args through the binary and returns its exit code and output.
func run(t *testing.T, a keelcli.App, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := a.Run(context.Background(), args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestGenerateCommands(t *testing.T) {
	dir := t.TempDir()
	a := app(analyticscli.Config{Tables: tables(), Rollups: []*rollup.Rollup{byStatus}})
	ts := filepath.Join(dir, "client", "analytics.ts")
	cases := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"analytics", "ts", "-check", "-out", ts}, keelcli.ExitProblems, ""},
		{[]string{"analytics", "ts", "-out", ts}, keelcli.ExitOK, "wrote " + ts},
		{[]string{"analytics", "ts", "-check", "-out", ts}, keelcli.ExitOK, "up to date: " + ts},
		{[]string{"analytics", "ts"}, keelcli.ExitUsage, ""},
		{[]string{"analytics", "i18n", "-out", filepath.Join(dir, "messages.json")}, keelcli.ExitOK, "wrote"},
		{[]string{"analytics", "i18n", "-check", "-out", filepath.Join(dir, "messages.json")}, keelcli.ExitOK, "up to date"},
		{[]string{"analytics", "rollups", "migration", "-check", "-out", filepath.Join(dir, "migrations")}, keelcli.ExitProblems, ""},
		{[]string{"analytics", "rollups", "migration", "-out", filepath.Join(dir, "migrations")}, keelcli.ExitOK, "_rollup_cli_shipments_by_status.sql"},
		{[]string{"analytics", "rollups", "migration", "-out", filepath.Join(dir, "migrations")}, keelcli.ExitOK, "up to date"},
		{[]string{"analytics", "rollups", "migration", "-check", "-out", filepath.Join(dir, "migrations")}, keelcli.ExitOK, "up to date"},
		{[]string{"analytics", "describe"}, keelcli.ExitOK, "# Reportable entities"},
		{[]string{"analytics", "describe", "-out", filepath.Join(dir, "describe.md")}, keelcli.ExitOK, "wrote"},
		{[]string{"analytics", "help"}, keelcli.ExitOK, "Usage: analytics <command>"},
		{[]string{"analytics"}, keelcli.ExitUsage, ""},
		{[]string{"analytics", "chart"}, keelcli.ExitUsage, ""},
		{[]string{"analytics", "rollups"}, keelcli.ExitUsage, ""},
		{[]string{"analytics", "rollups", "refresh", "-rollup", "nope"}, keelcli.ExitUsage, ""},
		{[]string{"analytics", "rollups", "refresh"}, keelcli.ExitFailure, ""},
		{[]string{"analytics", "describe", "extra"}, keelcli.ExitUsage, ""},
	}
	for _, tc := range cases {
		code, out, errOut := run(t, a, tc.args...)
		if code != tc.code || !strings.Contains(out, tc.out) {
			t.Fatalf("%v: exit %d, want %d\nstdout: %s\nstderr: %s", tc.args, code, tc.code, out, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "describe.md")); err != nil {
		t.Fatal(err)
	}
}

func TestRunWithoutKeelcli(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.json")
	var out bytes.Buffer
	cfg := analyticscli.Config{Tables: tables()}
	ok(t, analyticscli.Run(context.Background(), cfg, []string{"i18n", "-out", path}, &out))
	if !strings.Contains(out.String(), "wrote "+path) {
		t.Fatalf("output = %q", out.String())
	}
	err := analyticscli.Run(context.Background(), cfg, []string{"ts", "-bogus"}, &out)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("a bad flag: %v", err)
	}
}

func TestRefreshAndPendingCommands(t *testing.T) {
	d := openDB(t)
	ctx := context.WithValue(context.Background(), tenantKey{}, [2]ids.ID{ids.ID("org_" + randHex()), ids.ID("bu_" + randHex())})
	c := &fleet.Customer{Name: "C", Code: "C"}
	ok(t, fleetdb.Customers.Create(ctx, d, c))
	for _, st := range []fleet.Status{fleet.StatusNew, fleet.StatusNew, fleet.StatusDelivered} {
		ok(t, fleetdb.Shipments.Create(ctx, d, &fleet.Shipment{CustomerID: c.ID, ProNumber: "P" + randHex(), Status: st, Labels: []string{}, Attributes: map[string]any{}}))
	}
	a := app(analyticscli.Config{Tables: tables(), Rollups: []*rollup.Rollup{byStatus}, Open: func(ctx context.Context) (*keel.DB, error) {
		return keel.Open(ctx, config())
	}})
	code, out, errOut := run(t, a, "analytics", "rollups", "pending", "-json")
	var pending map[string]int64
	if code != keelcli.ExitOK || json.Unmarshal([]byte(out), &pending) != nil || pending["cli_shipments_by_status"] == 0 {
		t.Fatalf("pending: exit %d, %s %s", code, out, errOut)
	}
	code, out, errOut = run(t, a, "analytics", "rollups", "refresh", "-rebuild", "-batch", "50")
	if code != keelcli.ExitOK || !strings.Contains(out, "refreshed") {
		t.Fatalf("refresh: exit %d, %s %s", code, out, errOut)
	}
	code, out, _ = run(t, a, "analytics", "rollups", "pending")
	if code != keelcli.ExitOK || out != "cli_shipments_by_status\t0\n" {
		t.Fatalf("pending after the refresh: exit %d, %q", code, out)
	}
	var n int64
	ok(t, d.Pool().QueryRow(context.Background(), "SELECT sum(row_count) FROM cli_shipments_by_status WHERE organization_id = $1",
		string(ctx.Value(tenantKey{}).([2]ids.ID)[0])).Scan(&n))
	if n != 3 {
		t.Fatalf("the rollup counts %d shipments, want 3", n)
	}
}

type tenantKey struct{}

var (
	testDSN   string
	setupErr  error
	dropTest  func(context.Context) error
	setupOnce sync.Once
)

func TestMain(m *testing.M) {
	code := m.Run()
	if dropTest != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = dropTest(ctx)
		cancel()
	}
	os.Exit(code)
}

// config is the keel configuration of the test database.
func config() keel.Config {
	return keel.Config{
		DSN:           testDSN,
		TenantColumns: []string{"organization_id", "business_unit_id"},
		Tenant: func(ctx context.Context) ([]any, bool) {
			v, ok := ctx.Value(tenantKey{}).([2]ids.ID)
			return []any{v[0], v[1]}, ok
		},
		NewID: func(prefix string) string { return prefix + randHex() },
	}
}

// openDB opens the test database with byStatus migrated, skipping without Postgres unless
// KEEL_TEST_REQUIRE is set.
func openDB(t *testing.T) *keel.DB {
	t.Helper()
	setupOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		admin := os.Getenv("KEEL_TEST_DSN")
		if admin == "" {
			admin = "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
		}
		testDSN, dropTest, setupErr = migrate.Template(ctx, migrate.TemplateOptions{
			Admin: admin,
			Sources: migrate.Sources(
				migrate.Source{Name: "testdata", FS: os.DirFS("../../testdata/migrations")},
				migrate.Source{Name: "rollups", FS: fstest.MapFS{
					"20270101000000_rollup_cli_shipments_by_status.sql": {Data: []byte(rollup.Migration(byStatus))},
				}},
			),
			Prefix: "keel_analyticscli_test_",
		})
	})
	if setupErr != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatalf("postgres: %v", setupErr)
		}
		t.Skipf("postgres unavailable: %v", setupErr)
	}
	d, err := keel.Open(context.Background(), config())
	ok(t, err)
	t.Cleanup(d.Close)
	return d
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func ok(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
