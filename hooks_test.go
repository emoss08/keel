package keel_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// openDB opens a DB of its own on the test database, without OnBegin, so hooks, interceptors and
// observers registered on it reach no other test. edit, when set, changes the configuration.
func openDB(t *testing.T, edit func(cfg *keel.Config)) *keel.DB {
	t.Helper()
	db(t)
	cfg := testConfig(testDSN)
	if edit != nil {
		edit(&cfg)
	}
	d, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func TestInterceptorGuardsSaveAndDeleteRow(t *testing.T) {
	d := openDB(t, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	var ops []keel.Op
	d.Intercept(c, func(_ context.Context, q *keel.Intercepted) error {
		if !q.Read {
			ops = append(ops, q.Op)
			q.Where(c.Code.Ne("LOCKED"))
		}
		return nil
	})
	locked := &fleet.Customer{Name: "Locked", Code: "LOCKED"}
	ok(t, c.Create(ctx, d, locked))
	open := &fleet.Customer{Name: "Open", Code: "OPEN"}
	ok(t, c.Create(ctx, d, open))

	locked.Name = "Changed"
	if err := c.Save(ctx, d, locked); !errors.Is(err, keel.ErrConflict) {
		t.Fatalf("Save of an intercepted row: got %v, want ErrConflict", err)
	}
	if err := c.DeleteRow(ctx, d, locked); !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("DeleteRow of an intercepted row: got %v, want ErrNotFound", err)
	}
	eq(t, must(c.Get(ctx, d, locked.ID))(t).Name, "Locked")

	open.Name = "Renamed"
	ok(t, c.Save(ctx, d, open))
	eq(t, must(c.Get(ctx, d, open.ID))(t).Name, "Renamed")
	ok(t, c.DeleteRow(ctx, d, open))
	eq(t, must(c.Where(c.ID.Eq(open.ID)).Exists(ctx, d))(t), false)

	want := []keel.Op{keel.OpUpdate, keel.OpDelete, keel.OpUpdate, keel.OpDelete}
	eq(t, len(ops), len(want))
	for i := range want {
		eq(t, ops[i], want[i])
	}
}

func TestInterceptorOnRelationPathGuardsSave(t *testing.T) {
	d := openDB(t, nil)
	ctx := newTenant()
	s := fleetdb.Shipments
	d.Intercept(s, func(_ context.Context, q *keel.Intercepted) error {
		if !q.Read {
			q.Where(s.Customer().Code.Ne("LOCKED"))
		}
		return nil
	})
	lockedCust := newCustomer(t, ctx, "LOCKED")
	openCust := newCustomer(t, ctx, "OPEN")
	locked := &fleet.Shipment{CustomerID: lockedCust.ID, ProNumber: "P-LOCKED", Status: fleet.StatusNew}
	open := &fleet.Shipment{CustomerID: openCust.ID, ProNumber: "P-OPEN", Status: fleet.StatusNew}
	ok(t, s.CreateMany(ctx, d, []*fleet.Shipment{locked, open}))

	locked.Status = fleet.StatusCanceled
	if err := s.Save(ctx, d, locked, s.Status); !errors.Is(err, keel.ErrConflict) {
		t.Fatalf("Save: got %v, want ErrConflict", err)
	}
	if err := s.DeleteRow(ctx, d, locked); !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("DeleteRow: got %v, want ErrNotFound", err)
	}

	open.Status = fleet.StatusAssigned
	ok(t, s.Save(ctx, d, open, s.Status))
	eq(t, open.Version, int64(1))
	open.Status = fleet.StatusInTransit
	ok(t, s.Save(systemCtx("test"), d, open, s.Status))
	eq(t, must(s.Get(ctx, d, open.ID))(t).Status, fleet.StatusInTransit)
	ok(t, s.DeleteRow(ctx, d, open))
}

func TestUpsertUpdateHookRollsBack(t *testing.T) {
	d := openDB(t, nil)
	c := fleetdb.Customers
	keel.OnUpdate(d, c, keel.AfterWrite, func(context.Context, *keel.Mutation[fleet.Customer]) error {
		return errors.New("refused")
	})
	ctx := newTenant()
	existing := &fleet.Customer{Name: "Before", Code: "UP"}
	ok(t, c.Create(ctx, d, existing))

	_, err := c.Upsert(ctx, d, []*fleet.Customer{{Name: "After", Code: "UP"}},
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Update(c.Name))
	if err == nil {
		t.Fatal("a failing update AfterWrite hook did not fail the upsert")
	}
	eq(t, must(c.Get(ctx, d, existing.ID))(t).Name, "Before")
}

func TestMutationOldOnEveryUpdateAndDelete(t *testing.T) {
	d := openDB(t, nil)
	c := fleetdb.Customers
	var mu sync.Mutex
	var log []string
	record := func(_ context.Context, m *keel.Mutation[fleet.Customer]) error {
		mu.Lock()
		defer mu.Unlock()
		if m.Old == nil {
			log = append(log, m.Op.String()+" without Old")
			return nil
		}
		log = append(log, m.Op.String()+" from "+m.Old.Name)
		return nil
	}
	keel.OnUpdate(d, c, keel.AfterWrite, record, keel.Needs(c.Name))
	keel.OnDelete(d, c, keel.Committed, record, keel.Needs(c.Name))
	ctx := newTenant()
	a := &fleet.Customer{Name: "Alpha", Code: "A"}
	b := &fleet.Customer{Name: "Beta", Code: "B"}
	ok(t, c.CreateMany(ctx, d, []*fleet.Customer{a, b}))

	_ = must(c.Upsert(ctx, d, []*fleet.Customer{{Name: "Alpha 2", Code: "A"}},
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Update(c.Name)))(t)
	ok(t, c.DeleteRow(ctx, d, &fleet.Customer{ID: a.ID, OrganizationID: a.OrganizationID,
		BusinessUnitID: a.BusinessUnitID}))
	eq(t, must(c.Where(c.ID.Eq(b.ID)).Delete(ctx, d))(t), int64(1))

	want := []string{"update from Alpha", "delete from Alpha 2", "delete from Beta"}
	mu.Lock()
	defer mu.Unlock()
	if len(log) != len(want) {
		t.Fatalf("hook log:\n%q\nwant:\n%q", log, want)
	}
	for i := range want {
		eq(t, log[i], want[i])
	}
}
