package keel_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

func TestSetLinksReplacesTheJoinRows(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := newCustomer(t, ctx, "LNK")
	sh := &fleet.Shipment{CustomerID: c.ID, ProNumber: "PRO-LNK", Status: fleet.StatusNew}
	ok(t, fleetdb.Shipments.Create(ctx, d, sh))
	tags := []*fleet.Tag{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	ok(t, fleetdb.Tags.CreateMany(ctx, d, tags))
	s := fleetdb.Shipments

	linked := func() []string {
		t.Helper()
		got := must(s.Where(s.ID.Eq(sh.ID)).Load(s.Tags()).One(ctx, d))(t)
		names := make([]string, 0, len(got.Tags))
		for _, tg := range got.Tags {
			names = append(names, tg.Name)
		}
		slices.Sort(names)
		return names
	}

	ok(t, keel.SetLinks(ctx, d, s.Tags(), sh.ID, []ids.ID{tags[0].ID, tags[1].ID, tags[1].ID}))
	eq(t, slices.Equal(linked(), []string{"a", "b"}), true)

	ok(t, keel.SetLinks(ctx, d, s.Tags(), sh.ID, []ids.ID{tags[1].ID, tags[2].ID}))
	eq(t, slices.Equal(linked(), []string{"b", "c"}), true)

	other := newTenant()
	oc := newCustomer(t, other, "LNK")
	osh := &fleet.Shipment{CustomerID: oc.ID, ProNumber: "PRO-LNK", Status: fleet.StatusNew}
	ok(t, fleetdb.Shipments.Create(other, d, osh))
	ok(t, keel.SetLinks(other, d, s.Tags(), sh.ID, []ids.ID{}))
	eq(t, slices.Equal(linked(), []string{"b", "c"}), true)

	ok(t, keel.SetLinks(ctx, d, s.Tags(), sh.ID, nil))
	eq(t, len(linked()), 0)
}

func TestSetLinksRefusesWhatItCannotWrite(t *testing.T) {
	d := db(t)
	s := fleetdb.Shipments
	if err := keel.SetLinks(newTenant(), d, s.Customer(), ids.ID("shp_x"), nil); err == nil {
		t.Fatal("a belongs-to relation was accepted")
	}
	if err := keel.SetLinks(systemCtx("test"), d, s.Tags(), ids.ID("shp_x"), nil); err == nil {
		t.Fatal("a system scope was accepted")
	}
	err := keel.SetLinks(t.Context(), d, s.Tags(), ids.ID("shp_x"), nil)
	if !errors.Is(err, keel.ErrNoScope) {
		t.Fatalf("got %v, want ErrNoScope", err)
	}
}
