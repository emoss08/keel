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
