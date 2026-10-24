package keel_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

// world is a tenant's seeded data: customers, shipments with moves and stops, tags.
type world struct {
	ctx       context.Context
	customers []*fleet.Customer
	shipments []*fleet.Shipment
	locations []*fleet.Location
	tags      []*fleet.Tag
}

func seed(t testing.TB, shipments int) *world {
	t.Helper()
	d := db(t)
	w := &world{ctx: newTenant()}
	ctx := w.ctx
	for i := range 3 {
		w.customers = append(w.customers, newCustomer(t, ctx, fmt.Sprintf("C%d", i)))
	}
	for i := range 2 {
		l := &fleet.Location{Name: fmt.Sprintf("Dock %d", i), City: []string{"Austin", "Dallas"}[i]}
		ok(t, fleetdb.Locations.Create(ctx, d, l))
		w.locations = append(w.locations, l)
	}
	for _, n := range []string{"rush", "fragile"} {
		tag := &fleet.Tag{Name: n}
		ok(t, fleetdb.Tags.Create(ctx, d, tag))
		w.tags = append(w.tags, tag)
	}
	statuses := []fleet.Status{fleet.StatusNew, fleet.StatusAssigned, fleet.StatusInTransit, fleet.StatusDelivered}
	rows := make([]*fleet.Shipment, shipments)
	for i := range rows {
		weight := int64(1000 + i*10)
		s := &fleet.Shipment{
			CustomerID: w.customers[i%3].ID,
			ProNumber:  fmt.Sprintf("PRO-%04d", i),
			Status:     statuses[i%4],
			Priority:   fleet.Priority(i % 3),
			Total:      fleet.Money{Cents: int64(10000 + i*100)},
			Hazmat:     i%5 == 0,
			Labels:     []string{fmt.Sprintf("lane-%d", i%2)},
			Attributes: map[string]any{"index": i, "temp": i%2 == 0},
		}
		if i%4 != 0 {
			s.Weight = &weight
		}
		if i%3 == 1 {
			s.BillToID = &w.customers[0].ID
		}
		rows[i] = s
	}
	ok(t, fleetdb.Shipments.CreateMany(ctx, d, rows))
	w.shipments = rows
	for i, s := range rows {
		if i >= 6 {
			break
		}
		for seq := range 2 {
			m := &fleet.Move{ShipmentID: s.ID, Sequence: int32(seq), Status: fleet.StatusAssigned,
				Loaded: seq == 0}
			if i == 0 && seq == 1 {
				m.Status = fleet.StatusNew
			}
			ok(t, fleetdb.Moves.Create(ctx, d, m))
			for k := range 2 {
				st := &fleet.Stop{MoveID: m.ID, LocationID: w.locations[k].ID, Sequence: int32(k),
					Kind: []string{"pickup", "delivery"}[k]}
				ok(t, fleetdb.Stops.Create(ctx, d, st))
			}
		}
	}
	_, err := d.Pool().Exec(context.Background(),
		`INSERT INTO shipment_tags (shipment_id, tag_id, organization_id, business_unit_id)
		 SELECT s.id, t.id, s.organization_id, s.business_unit_id FROM shipments s CROSS JOIN tags t
		 WHERE s.organization_id = $1 AND t.organization_id = $1 AND s.pro_number IN ('PRO-0000', 'PRO-0001')
		   AND (s.pro_number = 'PRO-0000' OR t.name = 'rush')`, string(rows[0].OrganizationID))
	ok(t, err)
	return w
}

func pros(rows []*fleet.Shipment) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ProNumber
	}
	return out
}

func TestOperators(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	s := fleetdb.Shipments
	cases := []struct {
		name string
		pred keel.Pred
		want int
	}{
		{"enum eq", s.Status.Eq(fleet.StatusNew), 5},
		{"enum in", s.Status.In(fleet.StatusNew, fleet.StatusAssigned), 10},
		{"enum not in", s.Status.NotIn(fleet.StatusNew), 15},
		{"named int", s.Priority.Eq(2), 6},
		{"nullable is null", s.Weight.IsNull(), 5},
		{"between", s.Weight.Between(1050, 1100), 5},
		{"pointer named eq", s.BillToID.Eq(w.customers[0].ID), 7},
		{"contains", s.ProNumber.Contains("-001"), 10},
		{"contains escapes", s.ProNumber.Contains("%"), 0},
		{"prefix fold", s.ProNumber.HasPrefixFold("pro-000"), 10},
		{"bool", s.Hazmat.IsTrue(), 4},
		{"or", keel.Or(s.Hazmat.IsTrue(), s.Status.Eq(fleet.StatusDelivered)), 8},
		{"not", keel.Not(s.Hazmat.IsTrue()), 16},
		{"when false", keel.When(false, s.Hazmat.IsTrue()), 20},
		{"jsonb has key", s.Attributes.HasKey("temp"), 20},
		{"jsonb contains", s.Attributes.Contains(map[string]any{"temp": true}), 10},
		{"jsonb text", s.Attributes.Text("index").Eq("7"), 1},
		{"array has", s.Labels.Has("lane-1"), 10},
		{"array overlaps", s.Labels.Overlaps("lane-0", "lane-9"), 10},
		{"tsvector", s.SearchVector.Match("PRO-0003"), 1},
		{"custom scanner", s.Total.Gte(fleet.Money{Cents: 11000}), 10},
		{"readonly column", s.StageRank.Eq(4), 5},
		{"raw", keel.Raw("length(?) = ?", s.ProNumber, 8), 20},
		{"relation path", s.Customer().Code.Eq("C1"), 7},
		{"nullable relation path", s.BillTo().Code.Eq("C0"), 7},
		{"any", s.Moves().Any(fleetdb.Shipments.Moves().Status.Eq(fleet.StatusNew)), 1},
		{"none", s.Moves().None(), 14},
		{"m2m any", s.Tags().Any(s.Tags().Name.Eq("rush")), 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := must(s.Where(c.pred).Count(w.ctx, d))(t)
			eq(t, n, int64(c.want))
		})
	}
}

func TestListNeedsLimit(t *testing.T) {
	d := db(t)
	w := seed(t, 3)
	_, err := fleetdb.Shipments.Query().List(w.ctx, d)
	if !errors.Is(err, keel.ErrUnbounded) {
		t.Fatalf("got %v, want ErrUnbounded", err)
	}
	rows := must(fleetdb.Shipments.Query().All(w.ctx, d))(t)
	eq(t, len(rows), 3)
}

func TestOneAndFirst(t *testing.T) {
	d := db(t)
	w := seed(t, 4)
	s := fleetdb.Shipments
	if _, err := s.Query().One(w.ctx, d); !errors.Is(err, keel.ErrNotSingular) {
		t.Fatalf("got %v, want ErrNotSingular", err)
	}
	if _, err := s.Where(s.ProNumber.Eq("nope")).One(w.ctx, d); !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	first := must(s.Order(s.ProNumber.Desc()).First(w.ctx, d))(t)
	eq(t, first.ProNumber, "PRO-0003")
}

func TestDecodesEveryKind(t *testing.T) {
	d := db(t)
	w := seed(t, 2)
	s := fleetdb.Shipments
	got := must(s.Get(w.ctx, d, w.shipments[1].ID))(t)
	want := w.shipments[1]
	eq(t, got.Status, want.Status)
	eq(t, got.Priority, want.Priority)
	eq(t, *got.Weight, *want.Weight)
	eq(t, got.Total.Cents, want.Total.Cents)
	eq(t, *got.BillToID, *want.BillToID)
	eq(t, slices.Equal(got.Labels, want.Labels), true)
	eq(t, got.Attributes["index"], any(float64(1)))
	eq(t, got.StageRank, int16(2))
	eq(t, got.SearchVector, "")
	eq(t, got.CanceledAt == nil, true)
}

func TestPickNarrowsColumns(t *testing.T) {
	d := db(t)
	w := seed(t, 2)
	s := fleetdb.Shipments
	got := must(s.Pick(s.ProNumber).Where(s.ID.Eq(w.shipments[0].ID)).One(w.ctx, d))(t)
	eq(t, got.ProNumber, "PRO-0000")
	eq(t, got.ID, w.shipments[0].ID)
	eq(t, got.Status, fleet.Status(""))
}

func TestLoadRelations(t *testing.T) {
	d := db(t)
	w := seed(t, 8)
	s := fleetdb.Shipments
	m := s.Moves()
	rows := must(s.
		Where(s.ProNumber.In("PRO-0000", "PRO-0001", "PRO-0007")).
		Load(
			s.Customer(),
			s.BillTo(),
			m.Order(m.Sequence.Desc()),
			m.Stops().Location(),
			s.Tags(),
		).
		Order(s.ProNumber.Asc()).
		All(w.ctx, d))(t)
	eq(t, len(rows), 3)

	r0 := rows[0]
	eq(t, r0.Customer.Code, "C0")
	eq(t, r0.BillTo == nil, true)
	eq(t, len(r0.Moves), 2)
	eq(t, r0.Moves[0].Sequence, int32(1))
	eq(t, len(r0.Moves[0].Stops), 2)
	eq(t, r0.Moves[0].Stops[0].Location != nil, true)
	eq(t, len(r0.Tags), 2)

	r1 := rows[1]
	eq(t, r1.BillTo.Code, "C0")
	eq(t, len(r1.Tags), 1)
	eq(t, r1.Tags[0].Name, "rush")

	r7 := rows[2]
	eq(t, len(r7.Moves), 0)
	eq(t, len(r7.Tags), 0)
}

func TestLoadPerParentLimit(t *testing.T) {
	d := db(t)
	w := seed(t, 4)
	s := fleetdb.Shipments
	m := s.Moves()
	rows := must(s.Load(m.Order(m.Sequence.Desc()).Limit(1)).Order(s.ProNumber.Asc()).All(w.ctx, d))(t)
	for i, r := range rows {
		eq(t, len(r.Moves), 1)
		eq(t, r.Moves[0].Sequence, int32(1))
		eq(t, r.Moves[0].ShipmentID, w.shipments[i].ID)
	}
}

func TestLoadFiltered(t *testing.T) {
	d := db(t)
	w := seed(t, 2)
	s := fleetdb.Shipments
	m := s.Moves()
	got := must(s.Where(s.ID.Eq(w.shipments[0].ID)).Load(m.Where(m.Loaded.IsTrue())).One(w.ctx, d))(t)
	eq(t, len(got.Moves), 1)
	eq(t, got.Moves[0].Loaded, true)
}

func TestLoadUpward(t *testing.T) {
	d := db(t)
	w := seed(t, 2)
	mv := fleetdb.Moves
	rows := must(mv.Load(mv.Shipment().Customer()).Order(mv.CreatedAt.Asc()).All(w.ctx, d))(t)
	eq(t, len(rows) > 0, true)
	for _, r := range rows {
		eq(t, r.Shipment.Customer != nil, true)
	}
}

func TestSelfRelation(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	boss := &fleet.Worker{FirstName: "Grace", LastName: "Hopper"}
	ok(t, fleetdb.Workers.Create(ctx, d, boss))
	emp := &fleet.Worker{FirstName: "Alan", LastName: "Turing", ManagerID: &boss.ID}
	ok(t, fleetdb.Workers.Create(ctx, d, emp))

	wk := fleetdb.Workers
	got := must(wk.Where(wk.ID.Eq(emp.ID)).Load(wk.Manager()).One(ctx, d))(t)
	eq(t, got.Manager.WholeName, "Grace Hopper")
	eq(t, got.Manager.Initials, "GH")

	mgr := wk.As("mgr")
	reports := must(wk.
		Join(mgr, mgr.ID.EqCol(wk.ManagerID)).
		Where(mgr.LastName.Eq("Hopper")).
		All(ctx, d))(t)
	eq(t, len(reports), 1)
	eq(t, reports[0].FirstName, "Alan")
}

func TestPage(t *testing.T) {
	d := db(t)
	w := seed(t, 25)
	s := fleetdb.Shipments
	var seen []string
	cursor := ""
	pages := 0
	for {
		p := must(s.Order(s.ProNumber.Desc()).Page(w.ctx, d, keel.After(cursor, 10).Total(100)))(t)
		eq(t, p.Total, int64(25))
		seen = append(seen, pros(p.Items)...)
		pages++
		if p.Next == "" {
			break
		}
		cursor = p.Next
	}
	eq(t, pages, 3)
	eq(t, len(seen), 25)
	eq(t, seen[0], "PRO-0024")
	eq(t, seen[24], "PRO-0000")

	capped := must(s.Order(s.ProNumber.Asc()).Page(w.ctx, d, keel.After("", 5).Total(10)))(t)
	eq(t, capped.Total, int64(10))
	eq(t, capped.TotalCapped, true)
}

func TestPageNullableOrder(t *testing.T) {
	d := db(t)
	w := seed(t, 12)
	s := fleetdb.Shipments
	var all []string
	cursor := ""
	for {
		p := must(s.Order(s.Weight.Desc().NullsLast()).Page(w.ctx, d, keel.After(cursor, 4)))(t)
		all = append(all, pros(p.Items)...)
		if p.Next == "" {
			break
		}
		cursor = p.Next
	}
	eq(t, len(all), 12)
	seen := map[string]bool{}
	for _, p := range all {
		if seen[p] {
			t.Fatalf("%s appeared twice", p)
		}
		seen[p] = true
	}
	eq(t, all[0], "PRO-0011")
	last := all[len(all)-3:]
	for _, p := range last {
		var idx int
		fmt.Sscanf(p, "PRO-%d", &idx)
		eq(t, idx%4, 0)
	}
}

func TestEach(t *testing.T) {
	d := db(t)
	w := seed(t, 11)
	n := 0
	batches := 0
	ok(t, fleetdb.Shipments.Query().Each(w.ctx, d, 4, func(_ context.Context, b []*fleet.Shipment) error {
		n += len(b)
		batches++
		return nil
	}))
	eq(t, n, 11)
	eq(t, batches, 3)
}

func TestCountExists(t *testing.T) {
	d := db(t)
	w := seed(t, 9)
	s := fleetdb.Shipments
	eq(t, must(s.Query().Count(w.ctx, d))(t), int64(9))
	eq(t, must(s.Query().CountUpTo(w.ctx, d, 5))(t), int64(5))
	eq(t, must(s.Where(s.Hazmat.IsTrue()).Exists(w.ctx, d))(t), true)
	eq(t, must(s.Where(s.ProNumber.Eq("x")).Exists(w.ctx, d))(t), false)
	groups := must(s.Group(s.Status).Count(w.ctx, d))(t)
	eq(t, groups, int64(4))
}

func TestBulkUpdateAndDelete(t *testing.T) {
	d := db(t)
	w := seed(t, 6)
	s := fleetdb.Shipments
	before := must(s.Get(w.ctx, d, w.shipments[0].ID))(t)

	time.Sleep(1100 * time.Millisecond)
	n := must(s.Where(s.Status.Eq(fleet.StatusNew)).
		Update(w.ctx, d, s.Status.To(fleet.StatusCanceled), s.CanceledAt.To(42)))(t)
	eq(t, n, int64(2))

	after := must(s.Get(w.ctx, d, w.shipments[0].ID))(t)
	eq(t, after.Status, fleet.StatusCanceled)
	eq(t, *after.CanceledAt, int64(42))
	eq(t, after.Version, before.Version+1)
	eq(t, after.UpdatedAt > before.UpdatedAt, true)

	n = must(s.Where(s.Customer().Code.Eq("C1")).Update(w.ctx, d, s.Priority.To(9)))(t)
	eq(t, n, int64(2))
	eq(t, must(s.Where(s.Priority.Eq(9)).Count(w.ctx, d))(t), int64(2))

	n = must(s.Where(s.Status.Eq(fleet.StatusCanceled)).Delete(w.ctx, d))(t)
	eq(t, n, int64(2))

	if _, err := s.Query().Update(w.ctx, d, s.Hazmat.To(true)); err == nil {
		t.Fatal("update without Where succeeded")
	}
	if _, err := s.Query().Delete(w.ctx, d); err == nil {
		t.Fatal("delete without Where succeeded")
	}
}

func TestUpsert(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	existing := newCustomer(t, ctx, "U1")

	rows := []*fleet.Customer{
		{Name: "Updated", Code: "U1"},
		{Name: "Inserted", Code: "U2"},
	}
	ins := must(c.Upsert(ctx, d, rows, keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).
		Update(c.Name)))(t)
	eq(t, ins[0], false)
	eq(t, ins[1], true)
	eq(t, rows[0].Version, int64(1))

	got := must(c.Get(ctx, d, existing.ID))(t)
	eq(t, got.Name, "Updated")

	n := must(c.CreateOrSkip(ctx, d, []*fleet.Customer{{Name: "x", Code: "U2"}, {Name: "y", Code: "U3"}},
		c.OrganizationID, c.BusinessUnitID, c.Code))(t)
	eq(t, n, int64(1))
}

func TestUpsertSet(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	existing := newCustomer(t, ctx, "S1")
	onCode := keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code)
	merge := onCode.Set(
		c.Name.ToExpr(keel.Raw("? || ' + ' || ?", c.Name, c.Name.Excluded())),
		c.Email.To("merged@example.com"),
	)

	one := []*fleet.Customer{{Name: "One", Code: "S1"}}
	ins := must(c.Upsert(ctx, d, one, merge))(t)
	eq(t, ins[0], false)
	eq(t, one[0].Version, int64(1))
	got := must(c.Get(ctx, d, existing.ID))(t)
	eq(t, got.Name, "Customer S1 + One")
	eq(t, *got.Email, "merged@example.com")

	many := []*fleet.Customer{{Name: "Two", Code: "S1"}, {Name: "Fresh", Code: "S2"}}
	ins = must(c.Upsert(ctx, d, many, merge))(t)
	eq(t, ins[0], false)
	eq(t, ins[1], true)
	got = must(c.Get(ctx, d, existing.ID))(t)
	eq(t, got.Name, "Customer S1 + One + Two")
	eq(t, got.Version, int64(2))
	eq(t, must(c.Get(ctx, d, many[1].ID))(t).Name, "Fresh")

	bump := []*fleet.Customer{{Name: "Three", Code: "S1"}}
	_ = must(c.Upsert(ctx, d, bump, onCode.Update(c.Name).Set(c.Version.Add(10))))(t)
	eq(t, bump[0].Version, int64(12))
	got = must(c.Get(ctx, d, existing.ID))(t)
	eq(t, got.Name, "Three")
	eq(t, got.Version, int64(12))

	if _, err := c.Upsert(ctx, d, bump, onCode.Update(c.Name).Set(c.Name.To("twice"))); err == nil {
		t.Fatal("assigning a column twice succeeded")
	}
	s := fleetdb.Shipments
	if _, err := c.Upsert(ctx, d, bump, onCode.Set(s.ProNumber.To("x"))); err == nil {
		t.Fatal("assigning another table's column succeeded")
	}
	if _, err := c.Upsert(ctx, d, bump, onCode.Where(c.Name.Eq("x")).Update(c.Name)); err == nil {
		t.Fatal("a conflict Where that binds a value succeeded")
	}
	_, err := c.Where(c.ID.Eq(existing.ID)).Update(ctx, d, c.Name.ToExpr(c.Name.Excluded()))
	if err == nil || !strings.Contains(err.Error(), "Excluded is valid only in Conflict.Set") {
		t.Fatalf("Excluded outside an upsert: got %v", err)
	}
}

func TestInto(t *testing.T) {
	d := db(t)
	w := seed(t, 12)
	s := fleetdb.Shipments
	type row struct {
		Status  fleet.Status
		Total   int64
		Hazmat  int64
		Revenue fleet.Money
	}
	rows := must(keel.Into[row](w.ctx, d, s.
		Group(s.Status).
		Pick(s.Status, keel.Count(), keel.Count().Filter(s.Hazmat.IsTrue()), s.Total.Sum()).
		Order(s.Status.Asc())))(t)
	eq(t, len(rows), 4)
	eq(t, rows[0].Status, fleet.StatusNew)
	eq(t, rows[0].Total, int64(3))

	n := must(keel.IntoOne[int64](w.ctx, d, s.Pick(keel.Count()).Where(s.Hazmat.IsTrue())))(t)
	eq(t, n, int64(3))
}

func TestCorrelatedSubqueryAndLateral(t *testing.T) {
	d := db(t)
	w := seed(t, 6)
	c, s := fleetdb.Customers, fleetdb.Shipments
	idle := must(c.Where(keel.NotExists(s.Where(s.CustomerID.EqCol(c.ID), s.Hazmat.IsTrue()))).
		Order(c.Code.Asc()).All(w.ctx, d))(t)
	eq(t, len(idle), 1)
	eq(t, idle[0].Code, "C1")

	m := fleetdb.Moves
	last := fleetdb.Stops.As("last_stop")
	type moveStop struct {
		Move     ids.ID
		Location ids.ID
		Sequence int32
	}
	rows := must(keel.Into[moveStop](w.ctx, d, m.
		LeftJoinLateral(last.
			Pick(last.LocationID, last.Sequence).
			Where(last.MoveID.EqCol(m.ID)).
			Order(last.Sequence.Desc()).
			Limit(1)).
		Pick(m.ID, last.LocationID, last.Sequence).
		Order(m.ID.Asc())))(t)
	eq(t, len(rows), 12)
	for _, r := range rows {
		eq(t, r.Sequence, int32(1))
		eq(t, r.Location, w.locations[1].ID)
	}

	inSub := must(c.Where(c.ID.InQuery(s.Pick(s.CustomerID).Where(s.Hazmat.IsTrue()))).Count(w.ctx, d))(t)
	eq(t, inSub, int64(2))
}

func TestInsertFrom(t *testing.T) {
	d := db(t)
	w := seed(t, 3)
	tg := fleetdb.Tags
	n := must(tg.InsertFrom(w.ctx, d,
		fleetdb.Customers.Pick(keel.Raw("'tag_' || ?", fleetdb.Customers.ID), fleetdb.Customers.Name),
		tg.ID, tg.Name))(t)
	eq(t, n, int64(3))
	eq(t, must(tg.Query().Count(w.ctx, d))(t), int64(5))
}

func TestLock(t *testing.T) {
	d := db(t)
	w := seed(t, 2)
	s := fleetdb.Shipments
	ok(t, keel.Write(w.ctx, d, func(ctx context.Context) error {
		got, err := s.Where(s.ID.Eq(w.shipments[0].ID)).Load(s.Customer()).Lock(keel.ForUpdate, keel.NoWait).One(ctx, d)
		if err != nil {
			return err
		}
		other := context.WithValue(w.ctx, struct{}{}, 1)
		_, err = s.Where(s.ID.Eq(got.ID)).Lock(keel.ForUpdate, keel.NoWait).One(other, d)
		if keel.Code(err) != "55P03" {
			return fmt.Errorf("concurrent lock: got %v, want lock_not_available", err)
		}
		skipped, err := s.Query().Lock(keel.ForUpdate, keel.SkipLocked).All(other, d)
		if err != nil {
			return err
		}
		if len(skipped) != 1 {
			return fmt.Errorf("skip locked returned %d rows", len(skipped))
		}
		return nil
	}))
}

func TestAllTenantsReachesLoadsAndAny(t *testing.T) {
	d := db(t)
	w := seed(t, 6)
	s := fleetdb.Shipments
	m := s.Moves()
	st := m.Stops()
	sys := systemCtx("report")
	org := w.shipments[0].OrganizationID

	rows := must(s.AllTenants().
		Where(s.OrganizationID.Eq(org), m.Any(m.Loaded.IsTrue()), m.None(m.Status.Eq(fleet.StatusCanceled))).
		Load(m.Where(st.Any(st.Kind.Eq("pickup"))).Order(m.Sequence.Asc()), st.Order(st.Sequence.Asc()),
			s.Tags()).
		Order(s.ProNumber.Asc()).
		All(sys, d))(t)
	eq(t, len(rows), 6)
	eq(t, len(rows[0].Moves), 2)
	eq(t, len(rows[0].Moves[0].Stops), 2)
	eq(t, len(rows[0].Tags), 2)

	c := fleetdb.Customers
	_, err := s.AllTenants().Where(keel.Exists(c.Where(c.ID.EqCol(s.CustomerID)))).Limit(1).List(sys, d)
	if !errors.Is(err, keel.ErrAllTenantsRequired) {
		t.Fatalf("explicit subquery without AllTenants: got %v, want ErrAllTenantsRequired", err)
	}
	_, err = s.Where(s.OrganizationID.Eq(org)).Load(m.AllTenants()).Limit(1).List(sys, d)
	if !errors.Is(err, keel.ErrAllTenantsRequired) {
		t.Fatalf("root without AllTenants: got %v, want ErrAllTenantsRequired", err)
	}
}
