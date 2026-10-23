package keel_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

// must turns a (value, error) pair into the value, failing the test on error:
// got := must(q.One(ctx, d))(t).
func must[T any](v T, err error) func(testing.TB) T {
	return func(t testing.TB) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func ok(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eq[T comparable](t testing.TB, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func newCustomer(t testing.TB, ctx context.Context, code string) *fleet.Customer {
	t.Helper()
	c := &fleet.Customer{Name: "Customer " + code, Code: code}
	ok(t, fleetdb.Customers.Create(ctx, db(t), c))
	return c
}

func TestCreateFillsKeysTenantAndTimestamps(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := &fleet.Customer{Name: "Acme", Code: "ACME"}
	ok(t, fleetdb.Customers.Create(ctx, d, c))

	if !strings.HasPrefix(string(c.ID), "cus_") {
		t.Fatalf("id %q has no prefix", c.ID)
	}
	if c.OrganizationID == "" || c.BusinessUnitID == "" {
		t.Fatal("tenant columns not filled")
	}
	if c.CreatedAt == 0 || c.UpdatedAt != c.CreatedAt {
		t.Fatalf("timestamps not stamped: %d %d", c.CreatedAt, c.UpdatedAt)
	}

	got := must(fleetdb.Customers.Get(ctx, d, c.ID))(t)
	eq(t, got.Name, "Acme")
	eq(t, got.Email == nil, true)
}

func TestCreateReadsBackReadonlyColumns(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	w := &fleet.Worker{FirstName: "  Ada ", LastName: "Lovelace"}
	ok(t, fleetdb.Workers.Create(ctx, d, w))
	eq(t, w.FirstName, "Ada")
	eq(t, w.WholeName, "Ada Lovelace")
	if w.HiredAt.IsZero() {
		t.Fatal("time.Time autocreate not stamped")
	}

	got := must(fleetdb.Workers.Get(ctx, d, w.ID))(t)
	eq(t, got.Initials, "AL")
}

func TestSaveComparesAndIncrementsVersion(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := newCustomer(t, ctx, "V1")
	eq(t, c.Version, int64(0))

	stale := *c
	c.Name = "Renamed"
	ok(t, fleetdb.Customers.Save(ctx, d, c))
	eq(t, c.Version, int64(1))

	stale.Name = "Lost update"
	err := fleetdb.Customers.Save(ctx, d, &stale)
	if !errors.Is(err, keel.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	var ce *keel.ConflictError
	if !errors.As(err, &ce) || ce.Version != 0 {
		t.Fatalf("conflict error %#v", err)
	}

	got := must(fleetdb.Customers.Get(ctx, d, c.ID))(t)
	eq(t, got.Name, "Renamed")
	eq(t, got.Version, int64(1))
}

func TestSaveNarrowedColumns(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := newCustomer(t, ctx, "N1")
	c.Name = "New name"
	c.Code = "IGNORED"
	ok(t, fleetdb.Customers.Save(ctx, d, c, fleetdb.Customers.Name))
	got := must(fleetdb.Customers.Get(ctx, d, c.ID))(t)
	eq(t, got.Name, "New name")
	eq(t, got.Code, "N1")
}

func TestDeleteRow(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := newCustomer(t, ctx, "D1")
	ok(t, fleetdb.Customers.DeleteRow(ctx, d, c))
	_, err := fleetdb.Customers.Get(ctx, d, c.ID)
	if !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if err := fleetdb.Customers.DeleteRow(ctx, d, c); !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("second delete: got %v", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	d := db(t)
	a, b := newTenant(), newTenant()
	ca := newCustomer(t, a, "ISO")
	newCustomer(t, b, "ISO")

	n := must(fleetdb.Customers.Where(fleetdb.Customers.Code.Eq("ISO")).Count(a, d))(t)
	eq(t, n, int64(1))

	if _, err := fleetdb.Customers.Get(b, d, ca.ID); !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("other tenant read a row: %v", err)
	}

	ca.Name = "hijack"
	if err := fleetdb.Customers.Save(b, d, ca); !errors.Is(err, keel.ErrTenantMismatch) {
		t.Fatalf("save from another tenant: %v", err)
	}
	if err := fleetdb.Customers.DeleteRow(b, d, ca); !errors.Is(err, keel.ErrTenantMismatch) {
		t.Fatalf("delete from another tenant: %v", err)
	}
	n = must(fleetdb.Customers.Where(keel.True()).Update(b, d, fleetdb.Customers.Name.To("x")))(t)
	eq(t, n, int64(1))
	got := must(fleetdb.Customers.Get(a, d, ca.ID))(t)
	eq(t, got.Name, "Customer ISO")
}

func TestScopeRequired(t *testing.T) {
	d := db(t)
	_, err := fleetdb.Customers.Query().Limit(1).List(context.Background(), d)
	if !errors.Is(err, keel.ErrNoScope) {
		t.Fatalf("got %v, want ErrNoScope", err)
	}
	err = fleetdb.Customers.Create(context.Background(), d, &fleet.Customer{Name: "x", Code: "x"})
	if !errors.Is(err, keel.ErrNoScope) {
		t.Fatalf("create: got %v, want ErrNoScope", err)
	}

	sys := systemCtx("test sweep")
	_, err = fleetdb.Customers.Query().Limit(1).List(sys, d)
	if !errors.Is(err, keel.ErrAllTenantsRequired) {
		t.Fatalf("got %v, want ErrAllTenantsRequired", err)
	}
	_ = must(fleetdb.Customers.AllTenants().Limit(1).List(sys, d))(t)

	_, err = fleetdb.Customers.AllTenants().Limit(1).List(newTenant(), d)
	if !errors.Is(err, keel.ErrSystemScopeRequired) {
		t.Fatalf("got %v, want ErrSystemScopeRequired", err)
	}

	err = fleetdb.Customers.Create(sys, d, &fleet.Customer{Name: "x", Code: "x"})
	if err == nil {
		t.Fatal("system create without tenant columns succeeded")
	}
	ok(t, fleetdb.Customers.Create(sys, d, &fleet.Customer{Name: "x", Code: "x",
		OrganizationID: "org_sys", BusinessUnitID: "bu_sys"}))
}

func TestCreateRefusesOtherTenant(t *testing.T) {
	d := db(t)
	err := fleetdb.Customers.Create(newTenant(), d, &fleet.Customer{
		Name: "x", Code: "x", OrganizationID: ids.ID("org_other"), BusinessUnitID: "bu_other",
	})
	if !errors.Is(err, keel.ErrTenantMismatch) {
		t.Fatalf("got %v, want ErrTenantMismatch", err)
	}
}

func TestPlantWithoutTenant(t *testing.T) {
	d := db(t)
	ctx := context.Background()
	ok(t, fleetdb.Plants.Create(ctx, d, &fleet.Plant{Code: "P1", Name: "Plant one"}))
	got := must(fleetdb.Plants.Get(ctx, d, "P1"))(t)
	eq(t, got.Name, "Plant one")
}

func TestUniqueViolation(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	newCustomer(t, ctx, "DUP")
	err := fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "again", Code: "DUP"})
	if !keel.IsUnique(err, "uq_customers_code") {
		t.Fatalf("got %v, want a unique violation on uq_customers_code", err)
	}
}
