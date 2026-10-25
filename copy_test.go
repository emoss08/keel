package keel_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

func TestCopyEveryKind(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := newCustomer(t, ctx, "K")
	w := int64(42)
	canceled := int64(1_700_000_000)
	tricky := []string{`plain`, `with "quotes"`, `back\slash`, `comma,brace}`, ``}
	rows := []*fleet.Shipment{
		{CustomerID: c.ID, ProNumber: "A", Status: fleet.StatusNew, Weight: &w, Total: fleet.Money{Cents: 1234},
			Labels: tricky, Attributes: map[string]any{"k": "v"}, BillToID: &c.ID, Priority: 2, Hazmat: true,
			CanceledAt: &canceled, Version: 3},
		{CustomerID: c.ID, ProNumber: "B", Status: fleet.StatusDelivered},
	}
	n := must(fleetdb.Shipments.Copy(ctx, d, rows))(t)
	eq(t, n, int64(2))
	for _, r := range rows {
		if !strings.HasPrefix(string(r.ID), "shp_") {
			t.Fatalf("id %q has no prefix", r.ID)
		}
		if r.OrganizationID == "" || r.BusinessUnitID == "" {
			t.Fatal("tenant columns not filled")
		}
		if r.CreatedAt == 0 || r.UpdatedAt != r.CreatedAt {
			t.Fatalf("timestamps not stamped: %d %d", r.CreatedAt, r.UpdatedAt)
		}
		eq(t, r.StageRank, int16(0))
	}

	s := fleetdb.Shipments
	got := must(s.Order(s.ProNumber.Asc()).All(ctx, d))(t)
	eq(t, len(got), 2)
	a, b := got[0], got[1]
	eq(t, a.ID, rows[0].ID)
	eq(t, a.CreatedAt, rows[0].CreatedAt)
	eq(t, a.UpdatedAt, rows[0].UpdatedAt)
	eq(t, a.Status, fleet.StatusNew)
	eq(t, *a.Weight, int64(42))
	eq(t, a.Total.Cents, int64(1234))
	eq(t, *a.BillToID, c.ID)
	eq(t, a.Priority, fleet.Priority(2))
	eq(t, a.Hazmat, true)
	eq(t, *a.CanceledAt, canceled)
	eq(t, a.Version, int64(3))
	eq(t, a.Attributes["k"], any("v"))
	eq(t, a.StageRank, int16(1))
	if !slices.Equal(a.Labels, tricky) {
		t.Fatalf("labels %q", a.Labels)
	}
	eq(t, b.Weight == nil, true)
	eq(t, b.BillToID == nil, true)
	eq(t, b.CanceledAt == nil, true)
	eq(t, len(b.Labels), 0)
	eq(t, len(b.Attributes), 0)
	eq(t, b.Status, fleet.StatusDelivered)
	eq(t, b.StageRank, int16(4))
	empty := keel.Raw("cardinality(labels) = 0 AND attributes = '{}'::jsonb")
	eq(t, must(s.Where(empty, s.ProNumber.Eq("B")).Count(ctx, d))(t), int64(1))
}

func TestCopyEmpty(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	eq(t, must(fleetdb.Customers.Copy(newTenant(), d, nil))(t), int64(0))
	eq(t, rec.statements(), 0)
}

func TestCopyRunsModelHooks(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	ws := []*fleet.Worker{{FirstName: "  Ada ", LastName: " Lovelace "}, {FirstName: "Alan", LastName: "Turing"}}
	eq(t, must(fleetdb.Workers.Copy(ctx, d, ws))(t), int64(2))
	eq(t, ws[0].FirstName, "Ada")
	eq(t, ws[0].WholeName, "")
	if ws[0].HiredAt.IsZero() {
		t.Fatal("time.Time autocreate not stamped")
	}
	got := must(fleetdb.Workers.Get(ctx, d, ws[0].ID))(t)
	eq(t, got.WholeName, "Ada Lovelace")
	eq(t, got.HiredAt.Sub(ws[0].HiredAt).Abs() < time.Microsecond, true)
}

func TestCopyScope(t *testing.T) {
	d := db(t)
	c := fleetdb.Customers

	_, err := c.Copy(context.Background(), d, []*fleet.Customer{{Name: "x", Code: "x"}})
	if !errors.Is(err, keel.ErrNoScope) {
		t.Fatalf("no scope: got %v, want ErrNoScope", err)
	}

	ctx := newTenant()
	_, err = c.Copy(ctx, d, []*fleet.Customer{
		{Name: "mine", Code: "M"},
		{Name: "theirs", Code: "T", OrganizationID: ids.ID("org_other"), BusinessUnitID: "bu_other"},
	})
	if !errors.Is(err, keel.ErrTenantMismatch) {
		t.Fatalf("other tenant: got %v, want ErrTenantMismatch", err)
	}
	eq(t, must(c.Query().Count(ctx, d))(t), int64(0))

	sys := systemCtx("test import")
	if _, err := c.Copy(sys, d, []*fleet.Customer{{Name: "x", Code: "x"}}); err == nil ||
		!strings.Contains(err.Error(), "system scope must set tenant columns") {
		t.Fatalf("system copy without tenant columns: %v", err)
	}
	other := newTenant()
	org, bu := tenantOf(other)
	n := must(c.Copy(sys, d, []*fleet.Customer{{Name: "x", Code: "x", OrganizationID: org, BusinessUnitID: bu}}))(t)
	eq(t, n, int64(1))
	eq(t, must(c.Query().Count(other, d))(t), int64(1))

	eq(t, must(fleetdb.Plants.Copy(context.Background(), d, []*fleet.Plant{{Code: "CP1", Name: "Copied"}}))(t),
		int64(1))
}

func tenantOf(ctx context.Context) (ids.ID, ids.ID) {
	v := ctx.Value(tenantKey{}).(tenant)
	return v.org, v.bu
}

func TestCopyOnBegin(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	ctx := newTenant()
	org, _ := tenantOf(ctx)
	c := fleetdb.Customers
	rows := []*fleet.Customer{{Name: "one", Code: "O1"}, {Name: "two", Code: "O2"}, {Name: "three", Code: "O3"}}
	eq(t, must(c.Copy(ctx, d, rows))(t), int64(3))
	eq(t, must(c.Query().Count(ctx, d))(t), int64(3))

	rec.mu.Lock()
	first := rec.events[0]
	rec.mu.Unlock()
	eq(t, first.Op, "copy")
	eq(t, first.Table, "customers")
	eq(t, first.Rows, int64(3))
	eq(t, first.Args, 0)
	eq(t, first.Err, error(nil))
	if !strings.HasPrefix(first.SQL, `COPY "customers" ("id", "organization_id"`) {
		t.Fatalf("copy event sql %q", first.SQL)
	}

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		if _, err := c.Copy(ctx, d, []*fleet.Customer{{Name: "four", Code: "O4"}}); err != nil {
			return err
		}
		n, err := c.Where(keel.Raw("current_setting('keel.org') = ?", string(org))).Count(ctx, d)
		if err != nil {
			return err
		}
		if n != 4 {
			t.Fatalf("OnBegin setting not set by the transaction COPY began: %d rows", n)
		}
		return nil
	}))
}

func TestCopyRunsAfterOnBeginStatements(t *testing.T) {
	db(t)
	cfg := testConfig(testDSN)
	var begins int
	cfg.OnBegin = func(context.Context, keel.TxInfo) ([]keel.Statement, error) {
		begins++
		return []keel.Statement{{SQL: "SELECT set_config('transaction_read_only', 'on', true)"}}, nil
	}
	d := must(keel.Open(context.Background(), cfg))(t)
	defer d.Close()
	ctx := newTenant()
	_, err := fleetdb.Customers.Copy(ctx, d, []*fleet.Customer{{Name: "ro", Code: "RO"}})
	eq(t, keel.Code(err), "25006")
	eq(t, begins, 1)

	boom := errors.New("no session")
	cfg.OnBegin = func(context.Context, keel.TxInfo) ([]keel.Statement, error) { return nil, boom }
	d2 := must(keel.Open(context.Background(), cfg))(t)
	defer d2.Close()
	if _, err := fleetdb.Customers.Copy(ctx, d2, []*fleet.Customer{{Name: "x", Code: "X"}}); !errors.Is(err, boom) {
		t.Fatalf("OnBegin error: got %v", err)
	}
	eq(t, must(fleetdb.Customers.Query().Count(ctx, db(t)))(t), int64(0))
}

func TestCopyTableHooks(t *testing.T) {
	db(t)
	d := must(keel.Open(context.Background(), testConfig(testDSN)))(t)
	defer d.Close()
	c := fleetdb.Customers
	var mu sync.Mutex
	var log []string
	add := func(s string) {
		mu.Lock()
		log = append(log, s)
		mu.Unlock()
	}
	keel.OnCreate(d, c, keel.BeforeWrite, func(_ context.Context, m *keel.Mutation[*fleet.Customer]) error {
		m.Row.Code = "HOOK-" + m.Row.Code
		add("before " + m.Row.Code)
		return nil
	})
	keel.OnCreate(d, c, keel.AfterWrite, func(_ context.Context, m *keel.Mutation[*fleet.Customer]) error {
		add("after " + m.Row.Code)
		if m.Row.Name == "reject" {
			return errors.New("rejected")
		}
		return nil
	})
	keel.OnCreate(d, c, keel.Committed, func(_ context.Context, m *keel.Mutation[*fleet.Customer]) error {
		add("committed " + m.Row.Code)
		return nil
	})

	ctx := newTenant()
	rows := []*fleet.Customer{{Name: "a", Code: "A"}, {Name: "b", Code: "B"}}
	eq(t, must(c.Copy(ctx, d, rows))(t), int64(2))
	eq(t, rows[1].Code, "HOOK-B")

	_, err := c.Copy(ctx, d, []*fleet.Customer{{Name: "ok", Code: "C"}, {Name: "reject", Code: "R"}})
	if err == nil || err.Error() != "rejected" {
		t.Fatalf("AfterWrite error: got %v", err)
	}
	eq(t, must(c.Query().Count(ctx, d))(t), int64(2))

	want := []string{
		"before HOOK-A", "before HOOK-B",
		"after HOOK-A", "after HOOK-B",
		"committed HOOK-A", "committed HOOK-B",
		"before HOOK-C", "before HOOK-R",
		"after HOOK-C", "after HOOK-R",
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(log, want) {
		t.Fatalf("hook log:\n%q\nwant:\n%q", log, want)
	}
}

func TestCopyRollsBackWithWrite(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	boom := errors.New("boom")
	err := keel.Write(ctx, d, func(ctx context.Context) error {
		n, err := c.Copy(ctx, d, []*fleet.Customer{{Name: "a", Code: "A"}, {Name: "b", Code: "B"}})
		if err != nil {
			return err
		}
		eq(t, n, int64(2))
		eq(t, must(c.Query().Count(ctx, d))(t), int64(2))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	eq(t, must(c.Query().Count(ctx, d))(t), int64(0))

	err = keel.Read(ctx, d, func(ctx context.Context) error {
		_, err := c.Copy(ctx, d, []*fleet.Customer{{Name: "ro", Code: "RO"}})
		return err
	})
	if !errors.Is(err, keel.ErrReadOnly) {
		t.Fatalf("copy in a Read: got %v, want ErrReadOnly", err)
	}
}

func TestCopyUniqueViolation(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	newCustomer(t, ctx, "DUP")
	_, err := fleetdb.Customers.Copy(ctx, d, []*fleet.Customer{{Name: "fresh", Code: "NEW"}, {Name: "again", Code: "DUP"}})
	if !keel.IsUnique(err, "uq_customers_code") {
		t.Fatalf("got %v, want a unique violation on uq_customers_code", err)
	}
	eq(t, must(fleetdb.Customers.Query().Count(ctx, d))(t), int64(1))
}

func TestCopyJSONCodec(t *testing.T) {
	db(t)
	var marshals int
	var mu sync.Mutex
	cfg := testConfig(testDSN)
	cfg.JSON = &keel.JSONCodec{
		Marshal: func(v any) ([]byte, error) {
			mu.Lock()
			marshals++
			mu.Unlock()
			return json.Marshal(v)
		},
		Unmarshal: json.Unmarshal,
	}
	d := must(keel.Open(context.Background(), cfg))(t)
	defer d.Close()
	ctx := newTenant()
	cus := newCustomer(t, ctx, "J")
	rows := []*fleet.Shipment{
		{CustomerID: cus.ID, ProNumber: "J1", Status: fleet.StatusNew, Attributes: map[string]any{"via": "codec"}},
		{CustomerID: cus.ID, ProNumber: "J2", Status: fleet.StatusNew},
	}
	eq(t, must(fleetdb.Shipments.Copy(ctx, d, rows))(t), int64(2))
	mu.Lock()
	eq(t, marshals, 1)
	mu.Unlock()
	got := must(fleetdb.Shipments.Get(ctx, d, rows[0].ID))(t)
	eq(t, got.Attributes["via"], any("codec"))
}

func TestCopyPlanCacheOff(t *testing.T) {
	db(t)
	cfg := testConfig(testDSN)
	cfg.PlanCache = keel.PlanCacheOff
	d := must(keel.Open(context.Background(), cfg))(t)
	defer d.Close()
	ctx := newTenant()
	for i := range 3 {
		rows := []*fleet.Customer{{Name: "a", Code: "A" + strconv.Itoa(i)}, {Name: "b", Code: "B" + strconv.Itoa(i)}}
		eq(t, must(fleetdb.Customers.Copy(ctx, d, rows))(t), int64(2))
	}
	eq(t, must(fleetdb.Customers.Query().Count(ctx, d))(t), int64(6))
}
