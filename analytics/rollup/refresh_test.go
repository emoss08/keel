package rollup_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// pendingFor counts the dirty rows of the fixture's tenant.
func (f *fixture) pendingFor(name string) int {
	f.t.Helper()
	var n int
	ok(f.t, f.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM keel_rollup_dirty WHERE rollup = $1 AND tenant_key = $2",
		name, keel.TenantKeyOf(f.tn.org, f.tn.bu)).Scan(&n))
	return n
}

// refreshAll refreshes both rollups until nothing is left.
func refreshAll(t testing.TB, d *keel.DB) rollup.Report {
	t.Helper()
	rep, err := rollup.Refresh(context.Background(), d, daily, monthly)
	ok(t, err)
	return rep
}

func TestWritesMarkSlicesAndRefreshConverges(t *testing.T) {
	clk := &clock{}
	d := openDB(t, clk)
	f := newFixture(t, d, clk)
	a := f.shipment(0, 0, fleet.StatusNew, 1000, ptr[int64](10))
	f.shipment(0, 0, fleet.StatusNew, 2550, nil)
	f.shipment(1, 1, fleet.StatusDelivered, 99, ptr[int64](3))
	c := f.shipment(2, 2, fleet.StatusAssigned, 700, ptr[int64](7))
	if n := f.pendingFor("shipments_daily"); n != 4 {
		t.Fatalf("pending marks = %d, want one per write transaction and bucket: 4", n)
	}
	rep := refreshAll(t, d)
	if rep.Slices == 0 || rep.Rows == 0 || rep.Rollups["shipments_daily"].Slices < 3 {
		t.Fatalf("report = %+v", rep)
	}
	f.converged()
	if n := f.pendingFor("shipments_daily"); n != 0 {
		t.Fatalf("%d slices still pending after a refresh", n)
	}

	a.Status = fleet.StatusDelivered
	a.Total = fleet.Money{Cents: 1234}
	ok(t, fleetdb.Shipments.Save(f.ctx, d, a))
	ok(t, fleetdb.Shipments.DeleteRow(f.ctx, d, c))
	n, err := fleetdb.Shipments.Where(s.CustomerID.Eq(f.customers[1].ID)).
		Update(f.ctx, d, s.CreatedAt.To(f.base.AddDate(0, 0, 5).Unix()))
	ok(t, err)
	if n != 1 {
		t.Fatalf("bulk update wrote %d rows", n)
	}
	_, err = keel.Exec(f.ctx, d, `INSERT INTO shipments (id, organization_id, business_unit_id, customer_id, pro_number, status, total, created_at, updated_at)
		VALUES ('shp_raw', $1, $2, $3, 'RAW', 'New', 5, $4, $4)`, string(f.tn.org), string(f.tn.bu), string(f.customers[0].ID), f.base.AddDate(0, 0, 9).Unix())
	ok(t, err)
	refreshAll(t, d)
	f.converged()

	ok(t, fleetdb.Shipments.Save(f.ctx, d, a, s.ProNumber))
	if n := f.pendingFor("shipments_daily"); n != 0 {
		t.Fatalf("an update of no rollup column marked %d slices", n)
	}
}

func TestNullGrainAndWholeTenantRebuild(t *testing.T) {
	clk := &clock{}
	d := openDB(t, clk)
	f := newFixture(t, d, clk)
	f.shipment(0, 0, fleet.StatusNew, 100, nil)
	f.shipment(3, 1, fleet.StatusNew, 200, nil)
	refreshAll(t, d)
	_, err := d.Pool().Exec(context.Background(), "DELETE FROM shipments_daily WHERE organization_id = $1", string(f.tn.org))
	ok(t, err)
	rep, err := rollup.Rebuild(context.Background(), d, rollup.RefreshOptions{}, daily, f.tn.org, f.tn.bu)
	ok(t, err)
	if rep.Tenants != 1 || rep.Rows != 2 {
		t.Fatalf("rebuild report = %+v", rep)
	}
	f.converged()
	if _, err := rollup.Rebuild(context.Background(), d, rollup.RefreshOptions{}, daily, f.tn.org); err == nil {
		t.Fatal("a rebuild with a partial tenant was accepted")
	}

	_, err = d.Pool().Exec(context.Background(), "DELETE FROM shipments_daily WHERE organization_id = $1", string(f.tn.org))
	ok(t, err)
	marked, err := rollup.MarkAll(context.Background(), d, daily)
	ok(t, err)
	if marked == 0 || f.pendingFor("shipments_daily") != 1 {
		t.Fatalf("MarkAll marked %d rows, %d for this tenant", marked, f.pendingFor("shipments_daily"))
	}
	pending, err := rollup.Pending(context.Background(), d, daily, monthly)
	ok(t, err)
	if pending["shipments_daily"] == 0 {
		t.Fatalf("Pending = %v", pending)
	}
	refreshAll(t, d)
	f.converged()
}

func TestRefreshBatchesAndBounds(t *testing.T) {
	clk := &clock{}
	d := openDB(t, clk)
	f := newFixture(t, d, clk)
	for day := range 12 {
		f.shipment(day, day%3, fleet.StatusInTransit, int64(day*100), ptr(int64(day)))
	}
	refreshAll(t, d)
	for day := range 12 {
		f.shipment(day, 0, fleet.StatusNew, 1, nil)
	}
	rep, err := rollup.RefreshWith(context.Background(), d, rollup.RefreshOptions{BatchSize: 5, MaxBatches: 1, StatementTimeout: time.Minute}, daily)
	ok(t, err)
	if rep.Batches != 1 || rep.Slices > 5 {
		t.Fatalf("a bounded refresh did %+v", rep)
	}
	var began atomic.Int32
	rep, err = rollup.RefreshWith(context.Background(), d, rollup.RefreshOptions{
		BatchSize: 5,
		OnBegin: func(ctx context.Context, tx pgx.Tx) error {
			began.Add(1)
			return nil
		},
	}, daily)
	ok(t, err)
	if int(began.Load()) != rep.Batches || rep.Batches < 2 {
		t.Fatalf("OnBegin ran %d times for %d batches", began.Load(), rep.Batches)
	}
	f.converged()
	if _, err := rollup.Refresh(context.Background(), d); err == nil {
		t.Fatal("a refresh of no rollups was accepted")
	}
}

func TestRefreshConvergesUnderConcurrentWrites(t *testing.T) {
	clk := &clock{}
	d := openDB(t, clk)
	fixtures := []*fixture{newFixture(t, d, clk), newFixture(t, d, clk)}
	var mu sync.Mutex
	var shipments []*fleet.Shipment
	stop := make(chan struct{})
	var writers, refreshers sync.WaitGroup
	errs := make(chan error, 64)
	for w := range 6 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			for i := range 40 {
				f := fixtures[(w+i)%2]
				if err := concurrentWrite(f, rng, &mu, &shipments); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	var refreshed atomic.Int64
	for range 3 {
		refreshers.Add(1)
		go func() {
			defer refreshers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rep, err := rollup.RefreshWith(context.Background(), d, rollup.RefreshOptions{BatchSize: 7}, daily, monthly)
				if err != nil {
					errs <- err
					return
				}
				refreshed.Add(int64(rep.Slices + rep.Tenants))
			}
		}()
	}
	writers.Wait()
	close(stop)
	refreshers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	refreshAll(t, d)
	for _, f := range fixtures {
		f.converged()
	}
	if refreshed.Load() == 0 {
		t.Fatal("the concurrent refreshers never refreshed anything")
	}
}

// concurrentWrite makes one random write for f: a create, a save that may move the shipment to
// another day, customer and status, a bulk update or a delete.
func concurrentWrite(f *fixture, rng *rand.Rand, mu *sync.Mutex, all *[]*fleet.Shipment) error {
	ctx := f.ctx
	statuses := []fleet.Status{fleet.StatusNew, fleet.StatusAssigned, fleet.StatusDelivered}
	pick := func() *fleet.Shipment {
		mu.Lock()
		defer mu.Unlock()
		for range 5 {
			if len(*all) == 0 {
				return nil
			}
			sh := (*all)[rng.IntN(len(*all))]
			if sh.OrganizationID == f.tn.org {
				return sh
			}
		}
		return nil
	}
	switch op := rng.IntN(10); {
	case op < 5:
		sh := &fleet.Shipment{
			CustomerID: f.customers[rng.IntN(3)].ID, ProNumber: "P" + randHex(), Status: statuses[rng.IntN(3)],
			Total: fleet.Money{Cents: rng.Int64N(10000)}, Labels: []string{}, Attributes: map[string]any{},
		}
		if rng.IntN(2) == 0 {
			sh.Weight = ptr(rng.Int64N(500))
		}
		if err := fleetdb.Shipments.Create(ctx, f.db, sh); err != nil {
			return err
		}
		mu.Lock()
		*all = append(*all, sh)
		mu.Unlock()
	case op < 8:
		sh := pick()
		if sh == nil {
			return nil
		}
		return keel.Write(ctx, f.db, func(ctx context.Context) error {
			_, err := fleetdb.Shipments.Where(s.ID.Eq(sh.ID)).Update(ctx, f.db,
				s.Status.To(statuses[rng.IntN(3)]),
				s.CreatedAt.To(f.base.AddDate(0, 0, rng.IntN(6)).Unix()),
				s.CustomerID.To(f.customers[rng.IntN(3)].ID))
			return err
		})
	default:
		sh := pick()
		if sh == nil {
			return nil
		}
		_, err := fleetdb.Shipments.Where(s.ID.Eq(sh.ID)).Delete(ctx, f.db)
		if err != nil {
			return fmt.Errorf("delete: %w", err)
		}
	}
	return nil
}

func TestTimeGrainInAZoneWithNulls(t *testing.T) {
	clk := &clock{}
	d := openDB(t, clk)
	ctx, tn := newTenant()
	chicago := mustLoad("America/Chicago")
	late := time.Date(2026, 3, 8, 23, 30, 0, 0, chicago)
	for i, at := range []*time.Time{&late, ptr(late.Add(time.Hour)), nil, ptr(late.Add(-48 * time.Hour))} {
		st := &fleet.Stop{MoveID: "mov_x", LocationID: "loc_x", Sequence: int32(i), Kind: []string{"pickup", "delivery"}[i%2], ArrivedAt: at}
		ok(t, fleetdb.Stops.Create(ctx, d, st))
	}
	_, err := rollup.Refresh(context.Background(), d, stopsDaily)
	ok(t, err)
	f := &fixture{t: t, db: d, tn: tn}
	got := f.rows(`SELECT concat_ws('|', bucket AT TIME ZONE 'America/Chicago', kind, row_count, sequence_max) FROM stops_daily
		WHERE organization_id = $1 AND business_unit_id = $2`)
	want := f.rows(`SELECT concat_ws('|', date_trunc('day', arrived_at, 'America/Chicago') AT TIME ZONE 'America/Chicago', kind, count(*), max(sequence))
		FROM stops WHERE organization_id = $1 AND business_unit_id = $2
		GROUP BY date_trunc('day', arrived_at, 'America/Chicago'), kind`)
	if strings.Join(got, "\n") != strings.Join(want, "\n") || len(got) != 4 {
		t.Fatalf("rollup:\n%s\nsource:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	_, err = fleetdb.Stops.Where(fleetdb.Stops.ArrivedAt.IsNull()).Update(ctx, d, fleetdb.Stops.ArrivedAt.To(late))
	ok(t, err)
	_, err = rollup.Refresh(context.Background(), d, stopsDaily)
	ok(t, err)
	got = f.rows(`SELECT concat_ws('|', bucket AT TIME ZONE 'America/Chicago', kind, row_count) FROM stops_daily
		WHERE organization_id = $1 AND business_unit_id = $2`)
	if len(got) != 3 || !strings.Contains(strings.Join(got, ","), "2026-03-08 00:00:00|pickup|2") {
		t.Fatalf("after filling the null: %v", got)
	}
}
