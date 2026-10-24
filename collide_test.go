package keel_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

// collision is the same graph of rows in two tenants under the same IDs: a shipment with its
// customer, moves, stops, locations and tags, and a leave with its entries. Tenant a has more of
// everything than tenant b, and one tag of its own, so rows matched to the wrong tenant show.
type collision struct {
	a, b context.Context
	id   func(s string) ids.ID
}

// collidingTenants creates the graph of collision in two new tenants.
func collidingTenants(t *testing.T) *collision {
	t.Helper()
	d := db(t)
	raw := make([]byte, 5)
	_, _ = rand.Read(raw)
	run := hex.EncodeToString(raw)
	c := &collision{a: newTenant(), b: newTenant(), id: func(s string) ids.ID { return ids.ID(s + "_" + run) }}
	id := c.id
	for _, side := range []struct {
		ctx   context.Context
		name  string
		moves int
	}{{c.a, "A", 2}, {c.b, "B", 1}} {
		ctx, n := side.ctx, side.name
		ok(t, fleetdb.Customers.Create(ctx, d, &fleet.Customer{ID: id("cus"), Name: "Customer " + n, Code: "C" + n}))
		ok(t, fleetdb.Locations.Create(ctx, d, &fleet.Location{ID: id("loc"), Name: "Dock " + n, City: n}))
		bill := id("cus")
		ok(t, fleetdb.Shipments.Create(ctx, d, &fleet.Shipment{ID: id("shp"), CustomerID: id("cus"),
			BillToID: &bill, ProNumber: "PRO-" + n, Status: fleet.StatusNew}))
		stop := 0
		for m := range side.moves {
			mid := id("mov" + string(rune('1'+m)))
			ok(t, fleetdb.Moves.Create(ctx, d, &fleet.Move{ID: mid, ShipmentID: id("shp"), Sequence: int32(m),
				Status: fleet.StatusNew}))
			for k := range side.moves - m {
				stop++
				ok(t, fleetdb.Stops.Create(ctx, d, &fleet.Stop{ID: id("stp" + string(rune('0'+stop))), MoveID: mid,
					LocationID: id("loc"), Sequence: int32(k), Kind: n + string(rune('0'+stop))}))
			}
		}
		tags := []*fleet.Tag{{ID: id("tag"), Name: "Tag " + n}}
		if n == "A" {
			tags = append(tags, &fleet.Tag{ID: id("taga"), Name: "Only A"})
		}
		ok(t, fleetdb.Tags.CreateMany(ctx, d, tags))
		for _, tag := range tags {
			_, err := keel.Exec(ctx, d, `INSERT INTO shipment_tags (shipment_id, tag_id, organization_id,
				business_unit_id) VALUES ($1, $2, $3, $4)`, id("shp"), tag.ID, tag.OrganizationID, tag.BusinessUnitID)
			ok(t, err)
		}
		ok(t, fleetdb.Leaves.Create(ctx, d, &fleet.Leave{ID: id("lv"), WorkerID: "w1", Days: 10}))
		for e := range side.moves {
			ok(t, fleetdb.LeaveEntries.Create(ctx, d, &fleet.LeaveEntry{ID: id("lve" + string(rune('1'+e))),
				WorkerID: "w1", SourceLeaveID: id("lv"), Hours: 8, PostedAt: int64(e)}))
		}
	}
	return c
}

// isA reports whether a row's organization is tenant a's.
func (c *collision) isA(org ids.ID) bool {
	a, _ := tenantOf(c.a)
	return org == a
}

// stopKinds lists the kinds of a shipment's stops, move by move.
func stopKinds(s *fleet.Shipment) string {
	var out []string
	for _, m := range s.Moves {
		for _, st := range m.Stops {
			out = append(out, st.Kind+"@"+st.Location.City)
		}
	}
	return strings.Join(out, ",")
}

// tagNames lists the names of a shipment's tags, sorted.
func tagNames(s *fleet.Shipment) string {
	var out []string
	for _, t := range s.Tags {
		out = append(out, t.Name)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

func TestLoadsMatchTenantsWhenIDsCollide(t *testing.T) {
	d := db(t)
	c := collidingTenants(t)
	s := fleetdb.Shipments
	m := s.Moves()
	st := m.Stops()
	load := func(q *keel.Query[fleet.Shipment]) *keel.Query[fleet.Shipment] {
		return q.Where(s.ID.Eq(c.id("shp"))).
			Load(s.Customer(), s.BillTo(), m.Order(m.Sequence.Asc()), st.Order(st.Sequence.Asc()), st.Location(),
				m.FirstStop(), s.Tags(), s.OriginStop(), s.DestinationStop())
	}
	check := func(scope string, rows []*fleet.Shipment, want int) {
		t.Helper()
		eq(t, len(rows), want)
		for _, r := range rows {
			n := "B"
			if c.isA(r.OrganizationID) {
				n = "A"
			}
			at := scope + " " + n + ": "
			if r.Customer == nil || r.Customer.Name != "Customer "+n || r.BillTo == nil || r.BillTo.Name != "Customer "+n {
				t.Fatalf("%sbelongs-to loaded %+v and %+v", at, r.Customer, r.BillTo)
			}
			wantStops, wantTags, origin, dest := "B1@B", "Tag B", "B1", "B1"
			if n == "A" {
				wantStops, wantTags, origin, dest = "A1@A,A2@A,A3@A", "Only A,Tag A", "A1", "A3"
			}
			if got := stopKinds(r); got != wantStops {
				t.Fatalf("%smoves and stops %q, want %q", at, got, wantStops)
			}
			if got := tagNames(r); got != wantTags {
				t.Fatalf("%stags %q, want %q", at, got, wantTags)
			}
			if r.OriginStop == nil || r.OriginStop.Kind != origin || r.DestinationStop == nil || r.DestinationStop.Kind != dest {
				t.Fatalf("%spicks %+v and %+v, want %s and %s", at, r.OriginStop, r.DestinationStop, origin, dest)
			}
			for _, mv := range r.Moves {
				if mv.FirstStop == nil || mv.FirstStop.Kind != mv.Stops[0].Kind {
					t.Fatalf("%sfirst stop of move %s: %+v, want %s", at, mv.ID, mv.FirstStop, mv.Stops[0].Kind)
				}
			}
		}
	}

	sys := systemCtx("colliding ids")
	check("system", must(load(s.AllTenants()).Order(s.OrganizationID.Asc()).All(sys, d))(t), 2)
	check("tenant", must(load(s.Query()).All(c.a, d))(t), 1)
	check("tenant", must(load(s.Query()).All(c.b, d))(t), 1)

	limited := must(s.AllTenants().Where(s.ID.Eq(c.id("shp"))).
		Load(m.Order(m.Sequence.Desc()).Limit(1)).All(sys, d))(t)
	eq(t, len(limited), 2)
	for _, r := range limited {
		eq(t, len(r.Moves), 1)
		eq(t, r.Moves[0].OrganizationID, r.OrganizationID)
		want := c.id("mov1")
		if c.isA(r.OrganizationID) {
			want = c.id("mov2")
		}
		eq(t, r.Moves[0].ID, want)
	}

	tg := s.Tags()
	eq(t, must(s.AllTenants().Where(s.ID.Eq(c.id("shp")), tg.Any(tg.Name.Eq("Only A"))).Count(sys, d))(t), int64(1))
	eq(t, must(s.AllTenants().Where(s.ID.Eq(c.id("shp")), tg.None(tg.Name.Eq("Only A"))).Count(sys, d))(t), int64(1))
	eq(t, must(s.Where(tg.Any(tg.Name.Eq("Only A"))).Count(c.b, d))(t), int64(0))
}

func TestMultiColumnLoadsMatchTenantsWhenIDsCollide(t *testing.T) {
	d := db(t)
	c := collidingTenants(t)
	l := fleetdb.Leaves
	en := l.Entries()
	sys := systemCtx("colliding ids")
	for _, q := range []*keel.Query[fleet.Leave]{
		l.AllTenants().Load(en.Order(en.PostedAt.Asc()), l.LastEntry()),
		l.AllTenants().Load(en.Order(en.PostedAt.Desc()).Limit(5), l.LastEntry()),
	} {
		rows := must(q.Where(l.ID.Eq(c.id("lv"))).All(sys, d))(t)
		eq(t, len(rows), 2)
		for _, r := range rows {
			want := 1
			if c.isA(r.OrganizationID) {
				want = 2
			}
			eq(t, len(r.Entries), want)
			for _, e := range r.Entries {
				eq(t, e.OrganizationID, r.OrganizationID)
			}
			eq(t, r.LastEntry.OrganizationID, r.OrganizationID)
			eq(t, r.LastEntry.ID, c.id("lve"+string(rune('0'+want))))
		}
	}
}

func TestDynamicManyToManyMatchesTenantsWhenIDsCollide(t *testing.T) {
	d := db(t)
	c := collidingTenants(t)
	reg := must(keel.NewRegistry(fleetdb.ShipmentsSchema(), fleetdb.TagsSchema(), fleetdb.CustomersSchema(),
		fleetdb.MovesSchema(), fleetdb.StopsSchema(), fleetdb.LocationsSchema()))(t)
	sys := systemCtx("colliding ids")
	for _, strategy := range []keel.PreAggStrategy{keel.PreAggCTE, keel.PreAggLateral} {
		q := reg.Query("shipments")
		n := q.Rel("tags").PreAggregate(strategy).Measure(keel.DynCount().As("tags"))
		q.Select(q.Path("organization_id"), n).Where(q.Path("id").Eq(string(c.id("shp"))))
		res := must(q.AllTenants().Run(sys, d))(t)
		eq(t, res.Rows, 2)
		for i := range res.Rows {
			want := int64(1)
			if c.isA(ids.ID(res.Columns[0].String[i])) {
				want = 2
			}
			eq(t, res.Columns[1].Int64[i], want)
		}
	}
	q := reg.Query("shipments")
	tags := q.Rel("tags")
	q.Select(q.Path("id")).Where(q.Path("id").Eq(string(c.id("shp"))), tags.Any(tags.Col("name").Eq("Only A")))
	eq(t, must(q.AllTenants().Count(sys, d))(t), int64(1))
}
