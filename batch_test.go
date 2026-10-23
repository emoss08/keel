package keel_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

type statusCount struct {
	Status fleet.Status
	Total  int64
}

func TestBatchItemsMatchTerminals(t *testing.T) {
	d := db(t)
	w := seed(t, 12)
	s, c := fleetdb.Shipments, fleetdb.Customers
	list := func() *keel.Query[fleet.Shipment] {
		return s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned)).Order(s.ProNumber.Asc()).Limit(4)
	}
	hazmat := func() *keel.Query[fleet.Shipment] { return s.Where(s.Hazmat.IsTrue()).Order(s.ProNumber.Asc()) }
	byStatus := func() *keel.Query[fleet.Shipment] {
		return s.Group(s.Status).Pick(s.Status, keel.Count()).Order(s.Status.Asc())
	}
	paged := func() *keel.Query[fleet.Shipment] { return s.Order(s.ProNumber.Asc()) }

	var (
		rows, all        []*fleet.Shipment
		one, first       *fleet.Shipment
		page             keel.Page[fleet.Shipment]
		n, upTo, hazmats int64
		exists, missing  bool
		groups           []statusCount
		cust             *fleet.Customer
	)
	ok(t, keel.Batch(w.ctx, d,
		keel.ListInto(list(), &rows),
		keel.AllInto(hazmat(), &all),
		keel.OneInto(s.Where(s.ProNumber.Eq("PRO-0003")), &one),
		keel.FirstInto(s.Order(s.ProNumber.Desc()), &first),
		keel.PageInto(paged(), keel.After("", 5).Total(100), &page),
		keel.CountInto(s.Query(), &n),
		keel.CountUpToInto(s.Query(), 5, &upTo),
		keel.ExistsInto(s.Where(s.Hazmat.IsTrue()), &exists),
		keel.ExistsInto(s.Where(s.ProNumber.Eq("nope")), &missing),
		keel.IntoSlice(byStatus(), &groups),
		keel.IntoValue(s.Pick(keel.Count()).Where(s.Hazmat.IsTrue()), &hazmats),
		keel.OneInto(c.Where(c.Code.Eq("C1")), &cust),
	))

	eq(t, slices.Equal(pros(rows), pros(must(list().List(w.ctx, d))(t))), true)
	eq(t, len(rows), 4)
	eq(t, slices.Equal(pros(all), pros(must(hazmat().All(w.ctx, d))(t))), true)
	eq(t, one.ID, w.shipments[3].ID)
	eq(t, first.ProNumber, must(s.Order(s.ProNumber.Desc()).First(w.ctx, d))(t).ProNumber)

	want := must(paged().Page(w.ctx, d, keel.After("", 5).Total(100)))(t)
	eq(t, slices.Equal(pros(page.Items), pros(want.Items)), true)
	eq(t, page.Next, want.Next)
	eq(t, page.Total, int64(12))
	eq(t, page.TotalCapped, false)
	var next keel.Page[fleet.Shipment]
	ok(t, keel.Batch(w.ctx, d, keel.PageInto(paged(), keel.After(page.Next, 5), &next)))
	eq(t, next.Items[0].ProNumber, "PRO-0005")

	eq(t, n, must(s.Query().Count(w.ctx, d))(t))
	eq(t, upTo, int64(5))
	eq(t, exists, true)
	eq(t, missing, false)
	wantGroups := must(keel.Into[statusCount](w.ctx, d, byStatus()))(t)
	eq(t, slices.Equal(groups, wantGroups), true)
	eq(t, hazmats, int64(3))
	eq(t, cust.ID, w.customers[1].ID)
}

func TestBatchIsOneRoundTrip(t *testing.T) {
	rec := &recorder{}
	scoped := scopedDB(t, rec)
	w := seed(t, 6)
	s, c := fleetdb.Shipments, fleetdb.Customers
	org := string(w.shipments[0].OrganizationID)
	inTx := func() keel.Pred { return keel.Raw("current_setting('keel.org') = ?", org) }

	var rows []*fleet.Shipment
	var total int64
	var groups []statusCount
	var cust *fleet.Customer
	rec.reset()
	ok(t, keel.Batch(w.ctx, scoped,
		keel.ListInto(s.Where(inTx()).Order(s.ProNumber.Asc()).Limit(50), &rows),
		keel.CountInto(s.Where(inTx()), &total),
		keel.IntoSlice(s.Where(inTx()).Group(s.Status).Pick(s.Status, keel.Count()), &groups),
		keel.OneInto(c.Where(c.Code.Eq("C1"), inTx()), &cust),
	))
	eq(t, len(rows), 6)
	eq(t, total, int64(6))
	eq(t, len(groups), 4)
	eq(t, cust.Code, "C1")
	eq(t, rec.statements(), 4)
	eq(t, rec.trips(), 1)

	plainRec := &recorder{}
	cfg := testConfig(testDSN)
	cfg.Observers = []keel.Observer{plainRec}
	plain, err := keel.Open(context.Background(), cfg)
	ok(t, err)
	t.Cleanup(plain.Close)
	rows, total, groups, cust = nil, 0, nil, nil
	ok(t, keel.Batch(w.ctx, plain,
		keel.ListInto(s.Order(s.ProNumber.Asc()).Limit(50), &rows),
		keel.CountInto(s.Query(), &total),
		keel.IntoSlice(s.Group(s.Status).Pick(s.Status, keel.Count()), &groups),
		keel.OneInto(c.Where(c.Code.Eq("C1")), &cust),
	))
	eq(t, len(rows), 6)
	eq(t, total, int64(6))
	eq(t, len(groups), 4)
	eq(t, cust.Code, "C1")
	eq(t, plainRec.statements(), 4)
	eq(t, plainRec.trips(), 1)

	plainRec.reset()
	ok(t, keel.Batch(w.ctx, plain))
	eq(t, plainRec.statements(), 0)
}

func TestBatchLoadsRelations(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	w := seed(t, 8)
	s, mv := fleetdb.Shipments, fleetdb.Moves
	m := s.Moves()

	var ships []*fleet.Shipment
	var moves []*fleet.Move
	var n int64
	rec.reset()
	ok(t, keel.Batch(w.ctx, d,
		keel.AllInto(s.
			Where(s.ProNumber.In("PRO-0000", "PRO-0001", "PRO-0007")).
			Load(s.Customer(), m.Order(m.Sequence.Desc()), m.Stops().Location(), s.Tags()).
			Order(s.ProNumber.Asc()), &ships),
		keel.ListInto(mv.Where(mv.Sequence.Eq(0)).Load(mv.Stops(), mv.Shipment()).Order(mv.ID.Asc()).Limit(3), &moves),
		keel.CountInto(s.Query(), &n),
	))
	eq(t, rec.statements(), 7)
	eq(t, rec.trips(), 3)

	eq(t, len(ships), 3)
	eq(t, ships[0].Customer.Code, "C0")
	eq(t, len(ships[0].Moves), 2)
	eq(t, ships[0].Moves[0].Sequence, int32(1))
	eq(t, len(ships[0].Moves[0].Stops), 2)
	eq(t, ships[0].Moves[0].Stops[0].Location != nil, true)
	eq(t, len(ships[0].Tags), 2)
	eq(t, len(ships[1].Tags), 1)
	eq(t, len(ships[2].Moves), 0)

	eq(t, len(moves), 3)
	for _, mo := range moves {
		eq(t, len(mo.Stops), 2)
		eq(t, mo.Shipment != nil, true)
	}
	eq(t, n, int64(8))
}

func TestBatchHooksInterceptorsAndTimeout(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	w := seed(t, 6)
	s, wk := fleetdb.Shipments, fleetdb.Workers
	d.Intercept(s, func(_ context.Context, q *keel.Intercepted) error {
		if q.Read {
			q.Where(s.Hazmat.IsFalse())
		}
		return nil
	})
	boss := &fleet.Worker{FirstName: "Grace", LastName: "Hopper"}
	ok(t, wk.Create(w.ctx, d, boss))

	var n int64
	var rows []*fleet.Shipment
	var got *fleet.Worker
	ok(t, keel.Batch(w.ctx, d,
		keel.CountInto(s.Query(), &n),
		keel.ListInto(s.Order(s.ProNumber.Asc()).Limit(10), &rows),
		keel.OneInto(wk.Where(wk.ID.Eq(boss.ID)), &got),
	))
	eq(t, n, int64(4))
	eq(t, len(rows), 4)
	eq(t, got.Initials, "GH")

	start := time.Now()
	err := keel.Batch(w.ctx, d,
		keel.CountInto(s.Query(), &n),
		keel.CountInto(s.Where(keel.Raw("pg_sleep(?) IS NOT NULL", 1)).Timeout(50*time.Millisecond), &n),
	)
	if err == nil {
		t.Fatal("a batch with a timed out item succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a deadline error", err)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("the item's Timeout did not bound the batch")
	}
}

func TestBatchErrors(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	w := seed(t, 4)
	s := fleetdb.Shipments

	n := int64(-1)
	var rows []*fleet.Shipment
	rec.reset()
	err := keel.Batch(w.ctx, d, keel.CountInto(s.Query(), &n), keel.ListInto(s.Query(), &rows))
	if !errors.Is(err, keel.ErrUnbounded) {
		t.Fatalf("got %v, want ErrUnbounded", err)
	}
	eq(t, rec.statements(), 0)
	eq(t, n, int64(-1))

	var one *fleet.Shipment
	err = keel.Batch(w.ctx, d, keel.CountInto(s.Query(), &n), keel.OneInto(s.Where(s.ProNumber.Eq("nope")), &one))
	if !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	eq(t, n, int64(-1))

	err = keel.Batch(w.ctx, d, keel.OneInto(s.Load(s.Moves()), &one))
	if !errors.Is(err, keel.ErrNotSingular) {
		t.Fatalf("got %v, want ErrNotSingular", err)
	}
	eq(t, one == nil, true)

	err = keel.Batch(w.ctx, d, keel.FirstInto(s.Where(s.ProNumber.Eq("nope")), &one))
	if !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}

	var pro string
	err = keel.Batch(w.ctx, d, keel.IntoValue(s.Pick(s.ProNumber).Where(s.ProNumber.Eq("nope")), &pro))
	if !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}

	rec.reset()
	err = keel.Batch(context.Background(), d, keel.CountInto(s.Query(), &n))
	if !errors.Is(err, keel.ErrNoScope) {
		t.Fatalf("got %v, want ErrNoScope", err)
	}
	eq(t, rec.statements(), 0)

	if err := keel.Batch(w.ctx, d, keel.ListInto(s.Limit(1), nil)); err == nil {
		t.Fatal("a nil destination was accepted")
	}
	if err := keel.Batch(w.ctx, d, keel.BatchItem{}); err == nil {
		t.Fatal("an empty item was accepted")
	}
	var page keel.Page[fleet.Shipment]
	if err := keel.Batch(w.ctx, d, keel.PageInto(s.Query(), keel.After("", 0), &page)); err == nil {
		t.Fatal("a page without a size was accepted")
	}
}

func TestBatchInReadUsesTransaction(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	w := seed(t, 3)
	s := fleetdb.Shipments
	inTx := func() keel.Pred {
		return keel.And(
			keel.Raw("current_setting('lock_timeout') = ?", "250ms"),
			keel.Raw("current_setting('transaction_read_only') = ?", "on"),
		)
	}

	rec.reset()
	ok(t, keel.Read(w.ctx, d, func(ctx context.Context) error {
		var n, again int64
		var rows []*fleet.Shipment
		if err := keel.Batch(ctx, d,
			keel.CountInto(s.Where(inTx()), &n),
			keel.ListInto(s.Where(inTx()).Limit(10), &rows),
		); err != nil {
			return err
		}
		eq(t, n, int64(3))
		eq(t, len(rows), 3)
		if err := keel.Batch(ctx, d, keel.CountInto(s.Where(inTx()), &again)); err != nil {
			return err
		}
		eq(t, again, int64(3))
		return nil
	}, keel.LockTimeout(250*time.Millisecond)))
	eq(t, rec.statements(), 3)
	eq(t, rec.trips(), 2)
}
