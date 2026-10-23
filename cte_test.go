package keel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

func TestCTE(t *testing.T) {
	d := db(t)
	w := seed(t, 9)
	s, c := fleetdb.Shipments, fleetdb.Customers
	heavy := keel.CTE("heavy", []string{"customer_id", "loads"},
		s.Group(s.CustomerID).Pick(s.CustomerID, keel.Count()).Where(s.Weight.Gt(1010)))
	rows := must(c.With(heavy).
		Join(heavy, heavy.Col("customer_id").EqCol(c.ID)).
		Where(heavy.Col("loads").Gte(2)).
		Order(c.Code.Asc()).
		All(w.ctx, d))(t)
	codes := []string{}
	for _, r := range rows {
		codes = append(codes, r.Code)
	}
	if len(codes) != 2 || codes[0] != "C0" || codes[1] != "C2" {
		t.Fatalf("got %v", codes)
	}

	type load struct {
		Customer ids.ID
		Loads    int64
	}
	top := must(keel.Into[load](w.ctx, d, heavy.Query().With(heavy).
		Pick(heavy.Col("customer_id"), heavy.Col("loads")).
		Order(heavy.Col("loads").Desc()).Limit(1)))(t)
	eq(t, len(top), 1)
}

func TestRecursiveCTE(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	wk := fleetdb.Workers
	mk := func(first string, mgr *fleet.Worker) *fleet.Worker {
		x := &fleet.Worker{FirstName: first, LastName: "X"}
		if mgr != nil {
			x.ManagerID = &mgr.ID
		}
		ok(t, wk.Create(ctx, d, x))
		return x
	}
	ceo := mk("Ceo", nil)
	vp := mk("Vp", ceo)
	mgr := mk("Mgr", vp)
	mk("Ic", mgr)
	mk("Other", nil)

	chain := keel.RecursiveCTE("chain", []string{"id", "depth"},
		wk.Pick(wk.ID, keel.Raw("0")).Where(wk.ID.Eq(ceo.ID)))
	sub := wk.As("sub")
	chain.Recurse(sub.Pick(sub.ID, keel.Raw("? + 1", chain.Col("depth"))).
		Join(chain, chain.Col("id").EqCol(sub.ManagerID)))

	type node struct {
		ID    ids.ID
		Depth int32
	}
	nodes := must(keel.Into[node](ctx, d, chain.Query().With(chain).
		Pick(chain.Col("id"), chain.Col("depth")).Order(chain.Col("depth").Asc())))(t)
	eq(t, len(nodes), 4)
	eq(t, nodes[3].Depth, int32(3))

	under := must(wk.With(chain).Where(wk.ID.InQuery(chain.Query().Pick(chain.Col("id")))).Count(ctx, d))(t)
	eq(t, under, int64(4))
}

func TestCompound(t *testing.T) {
	d := db(t)
	w := seed(t, 6)
	s, c := fleetdb.Shipments, fleetdb.Customers
	names := must(keel.Into[string](w.ctx, d, keel.UnionAll(
		s.Pick(s.ProNumber).Where(s.Hazmat.IsTrue()),
		c.Pick(c.Code).Where(c.Code.Eq("C2")),
	)))(t)
	eq(t, len(names), 3)

	both := keel.CTE("ids", []string{"id"}, keel.Union(
		s.Pick(s.CustomerID).Where(s.Hazmat.IsTrue()),
		s.Pick(s.CustomerID).Where(s.Status.Eq(fleet.StatusDelivered)),
	))
	n := must(c.With(both).Where(c.ID.InQuery(both.Query().Pick(both.Col("id")))).Count(w.ctx, d))(t)
	eq(t, n, int64(2))
}

func TestSavepoint(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		if err := c.Create(ctx, d, &fleet.Customer{Name: "kept", Code: "K"}); err != nil {
			return err
		}
		err := keel.Savepoint(ctx, d, func(ctx context.Context) error {
			if err := c.Create(ctx, d, &fleet.Customer{Name: "dup", Code: "K"}); err != nil {
				return err
			}
			return nil
		})
		if !keel.IsUnique(err) {
			t.Fatalf("savepoint: got %v", err)
		}
		return c.Create(ctx, d, &fleet.Customer{Name: "after", Code: "A"})
	}))
	eq(t, must(c.Query().Count(ctx, d))(t), int64(2))
}

func TestLockKey(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	if err := keel.LockKey(ctx, d, "x"); err == nil {
		t.Fatal("LockKey outside a transaction succeeded")
	}
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		if err := keel.LockKey(ctx, d, "shipment-split:1"); err != nil {
			return err
		}
		other := newTenant()
		return keel.Write(other, d, func(other context.Context) error {
			got, err := keel.TryLockKey(other, d, "shipment-split:1")
			if err != nil {
				return err
			}
			if got {
				return errors.New("a second transaction took the same lock")
			}
			return nil
		})
	}))
}
