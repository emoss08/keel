package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestOnConflictExpressionTarget(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	ok(t, c.Create(ctx, d, &fleet.Contact{Name: "md5", Score: 1}))
	target := keel.OnConflict(c.OrganizationID, c.BusinessUnitID, keel.Raw("md5(?)", c.Name))
	ins, err := c.Upsert(ctx, d, []*fleet.Contact{{Name: "md5", Score: 2}, {Name: "other", Score: 3}},
		target.Update(c.Score))
	ok(t, err)
	eq(t, ins[0], false)
	eq(t, ins[1], true)
	eq(t, must(c.Where(c.Name.Eq("md5")).One(ctx, d))(t).Score, int64(2))

	n := must(c.CreateOrSkip(ctx, d, []*fleet.Contact{{Name: "md5"}, {Name: "third"}},
		c.OrganizationID, c.BusinessUnitID, keel.Raw("md5(?)", c.Name)))(t)
	eq(t, n, 1)

	_, err = c.Upsert(ctx, d, []*fleet.Contact{{Name: "x"}},
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, keel.Raw("md5(? || ?)", c.Name, "salt")))
	if err == nil || !strings.Contains(err.Error(), "must not bind values") {
		t.Fatalf("got %v", err)
	}
}

func TestCreateOrSkipWithoutTarget(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	ok(t, c.Create(ctx, d, &fleet.Contact{Name: "taken"}))
	n := must(c.CreateOrSkip(ctx, d, []*fleet.Contact{{Name: "taken"}, {Name: "free"}, {Name: "free2"}}))(t)
	eq(t, n, 2)
	eq(t, must(c.Query().Count(ctx, d))(t), 3)
	one := must(c.CreateOrSkip(ctx, d, []*fleet.Contact{{Name: "taken"}}))(t)
	eq(t, one, 0)
}

func TestUpsertFromUpdatesConflictingRowsAndInsertsTheRest(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	ct := fleetdb.Contacts
	old := newCustomer(t, ctx, "A")
	for _, n := range []string{"A", "B"} {
		ok(t, ct.Create(ctx, d, &fleet.Contact{Name: n}))
	}
	src := ct.Pick(
		keel.DynConst("cus_").Concat(ct.ID),
		ct.Name,
		keel.DynOf(ct.Name).Concat("-new"),
		keel.DynConst(1),
		keel.DynConst(1),
	)
	target := keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code)
	n := must(c.UpsertFrom(ctx, d, src, target, c.ID, c.Code, c.Name, c.CreatedAt, c.UpdatedAt))(t)
	eq(t, n, int64(2))
	a := must(c.Where(c.Code.Eq("A")).One(ctx, d))(t)
	eq(t, a.ID, old.ID)
	eq(t, a.Name, "A-new")
	eq(t, a.Version, old.Version+1)
	eq(t, a.CreatedAt, old.CreatedAt)
	if a.UpdatedAt <= 1 {
		t.Fatalf("the updated row was not stamped: %d", a.UpdatedAt)
	}
	b := must(c.Where(c.Code.Eq("B")).One(ctx, d))(t)
	eq(t, b.Name, "B-new")

	skipped := must(c.UpsertFrom(ctx, d, src, target.DoNothing(), c.ID, c.Code, c.Name, c.CreatedAt,
		c.UpdatedAt))(t)
	eq(t, skipped, int64(0))

	set := must(c.UpsertFrom(ctx, d, src.Where(ct.Name.Eq("A")), target.Set(c.Name.To("set")), c.ID,
		c.Code, c.Name, c.CreatedAt, c.UpdatedAt))(t)
	eq(t, set, int64(1))
	eq(t, must(c.Where(c.Code.Eq("A")).One(ctx, d))(t).Name, "set")

	_, err := c.UpsertFrom(ctx, d, ct.Pick(keel.DynConst("cus_x")), target, c.ID)
	if err == nil || !strings.Contains(err.Error(), "no updatable column") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateWhereCapsAConflictingRow(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	reserve := func() int64 {
		t.Helper()
		src := keel.Rows("src", []string{"id", "name", "score"},
			keel.DynLit("con_capped"), keel.DynLit("capped"), keel.DynLit(1))
		return must(c.UpsertFrom(ctx, d, src,
			keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Name).
				Set(c.Score.Add(1)).
				UpdateWhere(c.Score.Lt(3)),
			c.ID, c.Name, c.Score))(t)
	}
	eq(t, reserve(), int64(1))
	eq(t, reserve(), int64(1))
	eq(t, reserve(), int64(1))
	eq(t, reserve(), int64(0))
	eq(t, must(c.Where(c.Name.Eq("capped")).One(ctx, d))(t).Score, int64(3))

	_, err := c.Upsert(ctx, d, []*fleet.Contact{{Name: "capped"}},
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Name).UpdateWhere(c.Score.Lt(3)))
	if err == nil || !strings.Contains(err.Error(), "use UpsertFrom") {
		t.Fatalf("Upsert took UpdateWhere: %v", err)
	}
	_, err = c.UpsertFrom(ctx, d, c.Pick(c.ID, c.Name),
		keel.OnConflict(c.Name).DoNothing().UpdateWhere(c.Score.Lt(3)), c.ID, c.Name)
	if err == nil || !strings.Contains(err.Error(), "DoNothing and UpdateWhere") {
		t.Fatalf("DoNothing took UpdateWhere: %v", err)
	}
}
