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

// pickWorld is a tenant's shipments for pick tests: one with moves and stops written out of order,
// one without moves.
type pickWorld struct {
	ctx      context.Context
	full     *fleet.Shipment
	empty    *fleet.Shipment
	moves    []*fleet.Move
	origin   *fleet.Stop
	dest     *fleet.Stop
	austin   *fleet.Location
	dallas   *fleet.Location
	firstOf  map[ids.ID]ids.ID
	shipment map[ids.ID]*fleet.Shipment
}

// seedPicks writes a shipment whose moves (sequences 2 and 1) and stops (sequences 3, 1 and 2)
// are inserted out of order, so only the pick's order finds the origin and the destination.
func seedPicks(t *testing.T, ctx context.Context) *pickWorld {
	t.Helper()
	d := db(t)
	w := &pickWorld{ctx: ctx, firstOf: map[ids.ID]ids.ID{}}
	w.austin = &fleet.Location{Name: "North", City: "Austin"}
	w.dallas = &fleet.Location{Name: "South", City: "Dallas"}
	ok(t, fleetdb.Locations.CreateMany(ctx, d, []*fleet.Location{w.austin, w.dallas}))
	c := newCustomer(t, ctx, "PK")
	w.full = &fleet.Shipment{CustomerID: c.ID, ProNumber: "PICK-1", Status: fleet.StatusNew}
	w.empty = &fleet.Shipment{CustomerID: c.ID, ProNumber: "PICK-2", Status: fleet.StatusNew}
	ok(t, fleetdb.Shipments.CreateMany(ctx, d, []*fleet.Shipment{w.full, w.empty}))
	w.shipment = map[ids.ID]*fleet.Shipment{w.full.ID: w.full, w.empty.ID: w.empty}
	for _, seq := range []int32{2, 1} {
		m := &fleet.Move{ShipmentID: w.full.ID, Sequence: seq, Status: fleet.StatusAssigned}
		ok(t, fleetdb.Moves.Create(ctx, d, m))
		w.moves = append(w.moves, m)
		for _, ss := range []int32{3, 1, 2} {
			loc := w.dallas
			if ss == 1 {
				loc = w.austin
			}
			st := &fleet.Stop{MoveID: m.ID, LocationID: loc.ID, Sequence: ss, Kind: "stop"}
			ok(t, fleetdb.Stops.Create(ctx, d, st))
			switch {
			case seq == 1 && ss == 1:
				w.origin = st
			case seq == 2 && ss == 3:
				w.dest = st
			}
			if ss == 1 {
				w.firstOf[m.ID] = st.ID
			}
		}
	}
	return w
}

// stopID returns the ID of a loaded stop, or "" for nil.
func stopID(s *fleet.Stop) ids.ID {
	if s == nil {
		return ""
	}
	return s.ID
}

func TestPickRendersLateral(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	cases := []struct {
		name string
		q    interface {
			ToSQL(context.Context, *keel.DB) (string, []any, error)
		}
		want string
	}{
		{
			"one row joins the pick",
			s.Pick(s.ID).Where(s.ID.Eq("shp_1")).Load(s.OriginStop()).Limit(1),
			`SELECT s."id", s."organization_id", s."business_unit_id", os."id", os."organization_id", ` +
				`os."business_unit_id", os."move_id", os."location_id", os."sequence", os."kind", os."arrived_at" ` +
				`FROM "shipments" s LEFT JOIN LATERAL (SELECT s2.* FROM "moves" m JOIN "stops" s2 ON ` +
				`s2."move_id" = m."id" AND s2."organization_id" = $1 AND s2."business_unit_id" = $2 ` +
				`WHERE m."shipment_id" = s."id" AND m."organization_id" = $1 AND m."business_unit_id" = $2 ` +
				`ORDER BY m."sequence", s2."sequence", s2."id" LIMIT 1) os ON TRUE ` +
				`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND s."id" = $3 LIMIT 1`,
		},
		{
			"last orders descending",
			s.Pick(s.ID).Where(s.DestinationStop().Kind.Eq("x")),
			`SELECT s."id", s."organization_id", s."business_unit_id" FROM "shipments" s ` +
				`LEFT JOIN LATERAL (SELECT s2.* FROM "moves" m JOIN "stops" s2 ON s2."move_id" = m."id" ` +
				`AND s2."organization_id" = $1 AND s2."business_unit_id" = $2 WHERE m."shipment_id" = s."id" ` +
				`AND m."organization_id" = $1 AND m."business_unit_id" = $2 ` +
				`ORDER BY m."sequence" DESC, s2."sequence" DESC, s2."id" DESC LIMIT 1) ds ON TRUE ` +
				`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND ds."kind" = $3`,
		},
		{
			"filter through a pick and its belongs-to",
			s.Pick(s.ID).Where(s.OriginStop().Location().City.Eq("Austin")),
			`SELECT s."id", s."organization_id", s."business_unit_id" FROM "shipments" s ` +
				`LEFT JOIN LATERAL (SELECT s2.* FROM "moves" m JOIN "stops" s2 ON s2."move_id" = m."id" ` +
				`AND s2."organization_id" = $1 AND s2."business_unit_id" = $2 WHERE m."shipment_id" = s."id" ` +
				`AND m."organization_id" = $1 AND m."business_unit_id" = $2 ` +
				`ORDER BY m."sequence", s2."sequence", s2."id" LIMIT 1) os ON TRUE ` +
				`LEFT JOIN "locations" l ON l."id" = os."location_id" AND l."organization_id" = $1 ` +
				`AND l."business_unit_id" = $2 ` +
				`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND l."city" = $3`,
		},
		{
			"any over a pick",
			s.Pick(s.ID).Where(s.OriginStop().Any(s.OriginStop().Kind.Eq("pickup"))),
			`SELECT s."id", s."organization_id", s."business_unit_id" FROM "shipments" s ` +
				`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND EXISTS (SELECT 1 FROM ` +
				`(SELECT s2.* FROM "moves" m JOIN "stops" s2 ON s2."move_id" = m."id" ` +
				`AND s2."organization_id" = $1 AND s2."business_unit_id" = $2 WHERE m."shipment_id" = s."id" ` +
				`AND m."organization_id" = $1 AND m."business_unit_id" = $2 ` +
				`ORDER BY m."sequence", s2."sequence", s2."id" LIMIT 1) os WHERE os."kind" = $3)`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := c.q.ToSQL(ctx, d)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

func TestPickSystemScopeRendersTenantEqualities(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	got, _, err := s.Pick(s.ID).Where(s.OriginStop().Kind.Eq("x")).AllTenants().ToSQL(systemCtx("test"), d)
	ok(t, err)
	want := `SELECT s."id", s."organization_id", s."business_unit_id" FROM "shipments" s ` +
		`LEFT JOIN LATERAL (SELECT s2.* FROM "moves" m JOIN "stops" s2 ON s2."move_id" = m."id" ` +
		`AND s2."organization_id" = m."organization_id" AND s2."business_unit_id" = m."business_unit_id" ` +
		`WHERE m."shipment_id" = s."id" AND m."organization_id" = s."organization_id" ` +
		`AND m."business_unit_id" = s."business_unit_id" ` +
		`ORDER BY m."sequence", s2."sequence", s2."id" LIMIT 1) os ON TRUE WHERE os."kind" = $1`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	_, _, err = s.Pick(s.ID).Where(s.OriginStop().Kind.Eq("x")).ToSQL(systemCtx("test"), d)
	if !errors.Is(err, keel.ErrAllTenantsRequired) {
		t.Fatalf("got %v, want ErrAllTenantsRequired", err)
	}
}

func TestPickLoadsOneParentInItsStatement(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	w := seedPicks(t, newTenant())
	s := fleetdb.Shipments
	rec.reset()
	got := must(s.Load(s.OriginStop().Location(), s.DestinationStop()).Where(s.ID.Eq(w.full.ID)).One(w.ctx, d))(t)
	eq(t, stopID(got.OriginStop), w.origin.ID)
	eq(t, got.OriginStop.Location.City, "Austin")
	eq(t, stopID(got.DestinationStop), w.dest.ID)
	eq(t, rec.statements(), 1)

	empty := must(s.Load(s.OriginStop(), s.DestinationStop()).Where(s.ID.Eq(w.empty.ID)).One(w.ctx, d))(t)
	eq(t, empty.OriginStop == nil && empty.DestinationStop == nil, true)
}

func TestPickLoadsManyParentsPerKey(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	w := seedPicks(t, newTenant())
	s := fleetdb.Shipments
	rec.reset()
	rows := must(s.Load(s.OriginStop().Location(), s.DestinationStop()).Order(s.ProNumber.Asc()).All(w.ctx, d))(t)
	eq(t, len(rows), 2)
	eq(t, stopID(rows[0].OriginStop), w.origin.ID)
	eq(t, rows[0].OriginStop.Location.City, "Austin")
	eq(t, stopID(rows[0].DestinationStop), w.dest.ID)
	eq(t, rows[1].OriginStop == nil && rows[1].DestinationStop == nil, true)
	eq(t, rec.statements(), 3)
	eq(t, rec.trips(), 2)
	rec.mu.Lock()
	pick := rec.events[1].SQL
	rec.mu.Unlock()
	want := `SELECT k.k, os."id", os."organization_id", os."business_unit_id", os."move_id", ` +
		`os."location_id", os."sequence", os."kind", os."arrived_at", l."id", l."organization_id", ` +
		`l."business_unit_id", l."name", l."city" FROM unnest($1::text[]) AS k(k) CROSS JOIN LATERAL ` +
		`(SELECT s.* FROM "moves" m JOIN "stops" s ON s."move_id" = m."id" AND s."organization_id" = $2 ` +
		`AND s."business_unit_id" = $3 WHERE m."shipment_id" = k.k AND m."organization_id" = $2 ` +
		`AND m."business_unit_id" = $3 ORDER BY m."sequence", s."sequence", s."id" LIMIT 1) os ` +
		`LEFT JOIN "locations" l ON l."id" = os."location_id" AND l."organization_id" = $2 ` +
		`AND l."business_unit_id" = $3`
	if pick != want {
		t.Fatalf("got\n%s\nwant\n%s", pick, want)
	}
}

func TestPickUnderToManyLoad(t *testing.T) {
	d := db(t)
	w := seedPicks(t, newTenant())
	s := fleetdb.Shipments
	m := s.Moves()
	got := must(s.Load(m.Order(m.Sequence.Asc()), m.FirstStop()).Where(s.ID.Eq(w.full.ID)).One(w.ctx, d))(t)
	eq(t, len(got.Moves), 2)
	for _, mv := range got.Moves {
		eq(t, stopID(mv.FirstStop), w.firstOf[mv.ID])
	}
}

func TestPickLoadWhereLeavesNil(t *testing.T) {
	d := db(t)
	w := seedPicks(t, newTenant())
	s := fleetdb.Shipments
	os := s.OriginStop()
	one := must(s.Load(os.Where(os.Kind.Eq("none"))).Where(s.ID.Eq(w.full.ID)).One(w.ctx, d))(t)
	eq(t, one.OriginStop == nil, true)
	rows := must(s.Load(os.Where(os.Sequence.Eq(1))).Order(s.ProNumber.Asc()).All(w.ctx, d))(t)
	eq(t, stopID(rows[0].OriginStop), w.origin.ID)
	_, err := s.Load(os.Limit(1)).Order(s.ProNumber.Asc()).All(w.ctx, d)
	if err == nil {
		t.Fatal("a pick load with a Limit was accepted")
	}
}

func TestPickFilters(t *testing.T) {
	d := db(t)
	w := seedPicks(t, newTenant())
	s := fleetdb.Shipments
	austin := must(s.Where(s.OriginStop().Location().City.Eq("Austin")).All(w.ctx, d))(t)
	eq(t, len(austin), 1)
	eq(t, austin[0].ID, w.full.ID)
	eq(t, must(s.Where(s.DestinationStop().Location().City.Eq("Austin")).Count(w.ctx, d))(t), int64(0))
	eq(t, must(s.Where(s.OriginStop().None()).Count(w.ctx, d))(t), int64(1))
	eq(t, must(s.Where(s.OriginStop().Any(s.OriginStop().Sequence.Eq(1))).Count(w.ctx, d))(t), int64(1))
	_, err := s.Where(s.OriginStop().Kind.Eq("stop")).Update(w.ctx, d, s.Hazmat.To(true))
	if err == nil {
		t.Fatal("a bulk update joined a pick")
	}
}

func TestPickIsTenantScopedOnEveryHop(t *testing.T) {
	d := db(t)
	w := seedPicks(t, newTenant())
	other := newTenant()
	decoyMove := &fleet.Move{ShipmentID: w.full.ID, Sequence: -1, Status: fleet.StatusNew}
	ok(t, fleetdb.Moves.Create(other, d, decoyMove))
	loc := &fleet.Location{Name: "Decoy", City: "Elsewhere"}
	ok(t, fleetdb.Locations.Create(other, d, loc))
	for _, moveID := range []ids.ID{decoyMove.ID, w.moves[1].ID} {
		ok(t, fleetdb.Stops.Create(other, d, &fleet.Stop{MoveID: moveID, LocationID: loc.ID, Sequence: -1, Kind: "decoy"}))
	}
	s := fleetdb.Shipments
	one := must(s.Load(s.OriginStop()).Where(s.ID.Eq(w.full.ID)).One(w.ctx, d))(t)
	eq(t, stopID(one.OriginStop), w.origin.ID)
	rows := must(s.Load(s.OriginStop()).Order(s.ProNumber.Asc()).All(w.ctx, d))(t)
	eq(t, stopID(rows[0].OriginStop), w.origin.ID)
	eq(t, must(s.Where(s.OriginStop().Kind.Eq("decoy")).Count(w.ctx, d))(t), int64(0))
	eq(t, len(must(s.Load(s.OriginStop()).All(other, d))(t)), 0)

	sys := systemCtx("pick test")
	_, err := s.Where(s.ID.Eq(w.full.ID)).Load(s.OriginStop()).All(sys, d)
	if !errors.Is(err, keel.ErrAllTenantsRequired) {
		t.Fatalf("got %v, want ErrAllTenantsRequired", err)
	}
	across := must(s.Where(s.ID.Eq(w.full.ID)).Load(s.OriginStop()).AllTenants().One(sys, d))(t)
	eq(t, stopID(across.OriginStop), w.origin.ID)
}

func TestPickAppliesInterceptorsOnEveryHop(t *testing.T) {
	d := openDB(t, nil)
	w := seedPicks(t, newTenant())
	mv := fleetdb.Moves
	d.Intercept(mv, func(_ context.Context, q *keel.Intercepted) error {
		q.Where(mv.Sequence.Ne(1))
		return nil
	})
	s := fleetdb.Shipments
	got := must(s.Load(s.OriginStop()).Where(s.ID.Eq(w.full.ID)).One(w.ctx, d))(t)
	eq(t, got.OriginStop.MoveID, w.moves[0].ID)
	eq(t, got.OriginStop.Sequence, int32(1))
}
