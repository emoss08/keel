package keel_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestUpdateReturning(t *testing.T) {
	d := db(t)
	w := seed(t, 8)
	other := seed(t, 8)
	s := fleetdb.Shipments
	before := must(s.Get(w.ctx, d, w.shipments[0].ID))(t)

	got := must(s.Where(s.Status.Eq(fleet.StatusNew)).
		Load(s.Customer(), s.Moves()).
		Order(s.ProNumber.Desc()).
		UpdateReturning(w.ctx, d, s.Status.To(fleet.StatusCanceled), s.CanceledAt.To(42)))(t)
	eq(t, strings.Join(pros(got), ","), "PRO-0004,PRO-0000")
	for _, sh := range got {
		eq(t, sh.Status, fleet.StatusCanceled)
		eq(t, *sh.CanceledAt, int64(42))
		stored := must(s.Get(w.ctx, d, sh.ID))(t)
		eq(t, sh.Version, stored.Version)
		eq(t, sh.UpdatedAt, stored.UpdatedAt)
		eq(t, sh.StageRank, stored.StageRank)
		eq(t, sh.Total, stored.Total)
		eq(t, sh.OrganizationID, stored.OrganizationID)
		if sh.Customer == nil || sh.Customer.ID != sh.CustomerID {
			t.Fatalf("%s: customer not loaded", sh.ProNumber)
		}
	}
	eq(t, got[1].Version, before.Version+1)
	eq(t, got[1].UpdatedAt >= before.UpdatedAt, true)
	eq(t, len(got[1].Moves), 2)
	eq(t, len(got[0].Moves), 2)
	eq(t, must(s.Where(s.Status.Eq(fleet.StatusNew)).Count(other.ctx, d))(t), int64(2))

	none := must(s.Where(s.ProNumber.Eq("nope")).UpdateReturning(w.ctx, d, s.Hazmat.To(true)))(t)
	eq(t, len(none), 0)

	narrow := must(s.Pick(s.Status).Where(s.ProNumber.Eq("PRO-0001")).
		UpdateReturning(w.ctx, d, s.Priority.To(7)))(t)
	eq(t, len(narrow), 1)
	eq(t, narrow[0].ProNumber, "")
	eq(t, narrow[0].Priority, fleet.Priority(0))
	eq(t, narrow[0].Status, fleet.StatusAssigned)
	eq(t, narrow[0].ID, w.shipments[1].ID)
	eq(t, must(s.Get(w.ctx, d, w.shipments[1].ID))(t).Priority, fleet.Priority(7))

	wk := fleetdb.Workers
	emp := &fleet.Worker{FirstName: "Alan", LastName: "Turing"}
	ok(t, wk.Create(w.ctx, d, emp))
	renamed := must(wk.Where(wk.ID.Eq(emp.ID)).UpdateReturning(w.ctx, d, wk.FirstName.To("Grace")))(t)
	eq(t, renamed[0].WholeName, "Grace Turing")
	eq(t, renamed[0].Initials, "GT")

	errs := []func() error{
		func() error { _, err := s.Query().UpdateReturning(w.ctx, d, s.Hazmat.To(true)); return err },
		func() error { _, err := s.Where(keel.True()).UpdateReturning(w.ctx, d); return err },
		func() error {
			_, err := s.Where(keel.True()).Limit(1).UpdateReturning(w.ctx, d, s.Hazmat.To(true))
			return err
		},
		func() error {
			_, err := s.Pick(s.Customer().Name).Where(keel.True()).UpdateReturning(w.ctx, d, s.Hazmat.To(true))
			return err
		},
		func() error {
			_, err := s.Where(keel.True()).UpdateReturning(w.ctx, d, s.Customer().Name.To("x"))
			return err
		},
	}
	for i, f := range errs {
		if f() == nil {
			t.Fatalf("case %d succeeded", i)
		}
	}
	eq(t, must(s.Where(s.Hazmat.IsTrue()).Count(w.ctx, d))(t), int64(2))
}

func TestUpdateReturningInterceptorsAndHooks(t *testing.T) {
	d := openDB(t, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	d.Intercept(c, func(_ context.Context, q *keel.Intercepted) error {
		if q.Read {
			q.Where(c.Code.Ne("HIDDEN"))
		} else {
			q.Where(c.Code.Ne("LOCKED"))
		}
		return nil
	})
	var mu sync.Mutex
	var log []string
	keel.OnUpdate(d, c, keel.AfterWrite, func(ctx context.Context, m *keel.Mutation[fleet.Customer]) error {
		mu.Lock()
		defer mu.Unlock()
		if !keel.InTx(ctx, d) {
			t.Error("AfterWrite hook outside the update's transaction")
		}
		log = append(log, m.Old.Name+">"+m.Row.Name+":"+m.Row.Code)
		return nil
	}, keel.Needs(c.Name))
	rows := []*fleet.Customer{{Name: "A", Code: "OPEN"}, {Name: "B", Code: "LOCKED"}, {Name: "C", Code: "HIDDEN"}}
	ok(t, c.CreateMany(ctx, d, rows))

	got := must(c.Where(keel.True()).Order(c.Code.Asc()).
		UpdateReturning(ctx, d, c.Name.ToExpr(keel.Raw("? || '2'", c.Name))))(t)
	names := make([]string, len(got))
	for i, r := range got {
		names[i] = r.Name + ":" + r.Code
	}
	eq(t, strings.Join(names, ","), "C2:HIDDEN,A2:OPEN")
	mu.Lock()
	slices.Sort(log)
	eq(t, strings.Join(log, ","), "A>A2:OPEN,C>C2:HIDDEN")
	mu.Unlock()
	if got[0].Version != rows[2].Version+1 || got[0].Email != nil {
		t.Fatalf("returned row %+v", *got[0])
	}
}

func TestUpdateReturningDataVersions(t *testing.T) {
	d := versionsDB(t, time.Hour, nil)
	c := fleetdb.Customers
	a, b := newTenant(), newTenant()
	org, _ := tenantOf(a)
	codeA, codeB := "UR-A-"+string(org), "UR-B-"+string(org)
	ok(t, c.Create(a, d, &fleet.Customer{Name: "A", Code: codeA}))
	ok(t, c.Create(b, d, &fleet.Customer{Name: "B", Code: codeB}))
	eq(t, versionOf(t, d, a, "customers"), int64(1))
	got := must(c.Omit(c.OrganizationID, c.BusinessUnitID).Where(c.Code.In(codeA, codeB)).AllTenants().
		UpdateReturning(systemCtx("returning"), d, c.Name.To("x")))(t)
	eq(t, len(got), 2)
	eq(t, versionOf(t, d, a, "customers"), int64(2))
	eq(t, versionOf(t, d, b, "customers"), int64(2))
	mine := must(c.Where(c.Code.Eq(codeA)).UpdateReturning(a, d, c.Name.To("y")))(t)
	eq(t, len(mine), 1)
	eq(t, versionOf(t, d, a, "customers"), int64(3))
	eq(t, versionOf(t, d, b, "customers"), int64(2))
}
