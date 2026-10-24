package keel_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/jackc/pgx/v5"
)

// replicaDSN returns the DSN of the streaming replica of the test server, on the test database,
// skipping the test when KEEL_TEST_REPLICA_DSN is not set (testdata/replica/replica.sh up starts
// one).
func replicaDSN(t *testing.T) string {
	t.Helper()
	db(t)
	dsn := os.Getenv("KEEL_TEST_REPLICA_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_REPLICA_DSN is not set; start a replica with testdata/replica/replica.sh up")
	}
	return replaceDB(dsn, dbName)
}

// replicaDB opens a DB on the test database with the replica, its configuration changed by edit,
// and waits until the replica has the test database and is healthy.
func replicaDB(t *testing.T, rec *recorder, maxLag time.Duration, edit func(*keel.Config)) *keel.DB {
	t.Helper()
	dsn := replicaDSN(t)
	cfg := testConfig(testDSN)
	cfg.Replicas = []keel.ReplicaConfig{{DSN: dsn}}
	cfg.MaxReplicaLag = maxLag
	if rec != nil {
		cfg.Observers = []keel.Observer{rec}
	}
	if edit != nil {
		edit(&cfg)
	}
	d, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	waitFor(t, "a healthy replica", func() bool {
		st := d.Replicas()[0]
		return st.Healthy && st.Lag <= maxLag
	})
	return d
}

// onReplica reports whether the reads of rec ran on a replica: every one did (true) or none did
// (false). It fails the test when they were mixed or there were none.
func onReplica(t *testing.T, rec *recorder) bool {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) == 0 {
		t.Fatal("no statements recorded")
	}
	first := rec.events[0].Replica != ""
	for _, e := range rec.events {
		if (e.Replica != "") != first {
			t.Fatalf("statements of one read ran on the primary and a replica: %+v", rec.events)
		}
	}
	return first
}

// recovery is a server's recovery state and a row count, read by inRecovery.
type recovery struct {
	InRecovery bool
	Rows       int64
}

// inRecovery asks the server a read runs on whether it is a standby.
func inRecovery(t *testing.T, ctx context.Context, d *keel.DB) bool {
	t.Helper()
	c := fleetdb.Customers
	return must(keel.IntoOne[recovery](ctx, d, c.Pick(keel.Raw("pg_is_in_recovery()"), keel.Count())))(t).InRecovery
}

// caughtUp waits until the replica has replayed ctx's commit position.
func caughtUp(t *testing.T, ctx context.Context, d *keel.DB) {
	t.Helper()
	pos := keel.CommitPosition(ctx, d)
	waitFor(t, "the replica to replay "+pos.String(), func() bool {
		st := d.Replicas()[0]
		return st.Healthy && st.Position >= pos
	})
}

func TestReplicaRoutesMarkedReads(t *testing.T) {
	rec := &recorder{}
	d := replicaDB(t, rec, 5*time.Second, nil)
	ctx := keel.OnReplica(newTenant())
	c := fleetdb.Customers
	s := fleetdb.Shipments
	cust := &fleet.Customer{Name: "Routed", Code: "R"}
	ok(t, c.Create(ctx, d, cust))
	ok(t, s.CreateMany(ctx, d, []*fleet.Shipment{
		{CustomerID: cust.ID, ProNumber: "R-1", Status: fleet.StatusNew},
		{CustomerID: cust.ID, ProNumber: "R-2", Status: fleet.StatusAssigned},
	}))
	if keel.CommitPosition(ctx, d) == 0 {
		t.Fatal("writes on a marked context recorded no commit position")
	}
	caughtUp(t, ctx, d)

	rec.reset()
	if !inRecovery(t, ctx, d) || !onReplica(t, rec) {
		t.Fatal("a marked read ran on the primary")
	}
	rec.reset()
	n := must(s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned)).Count(ctx, d))(t)
	eq(t, n, int64(2))
	if !onReplica(t, rec) {
		t.Fatal("an enum In count ran on the primary")
	}
	rec.reset()
	var rows []*fleet.Shipment
	var cnt int64
	ok(t, keel.Batch(ctx, d,
		keel.ListInto(s.Load(s.Customer()).Order(s.ProNumber.Asc()).Limit(10), &rows),
		keel.CountInto(c.Query(), &cnt)))
	eq(t, len(rows), 2)
	eq(t, rows[0].Customer.Name, "Routed")
	if !onReplica(t, rec) {
		t.Fatal("a batch ran on the primary")
	}
	if want := d.Replicas()[0].Name; !strings.Contains(want, ":") || rec.events[0].Replica != want {
		t.Fatalf("event replica %q, stats name %q", rec.events[0].Replica, want)
	}

	plain := context.WithoutCancel(newTenant())
	rec.reset()
	if inRecovery(t, plain, d) || onReplica(t, rec) {
		t.Fatal("an unmarked read ran on a replica")
	}

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		if inRecovery(t, ctx, d) {
			return fmt.Errorf("a read inside Write ran on a replica")
		}
		return nil
	}))
	ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
		if inRecovery(t, ctx, d) {
			return fmt.Errorf("a read inside Read ran on a replica")
		}
		return nil
	}))
}

func TestReplicaTx(t *testing.T) {
	rec := &recorder{}
	d := replicaDB(t, rec, 5*time.Second, nil)
	ctx := keel.OnReplica(newTenant())
	c := fleetdb.Customers
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Tx", Code: "TX"}))
	caughtUp(t, ctx, d)
	rec.reset()
	ok(t, keel.Read(newTenant(), d, func(tctx context.Context) error {
		if !inRecovery(t, tctx, d) {
			return fmt.Errorf("a ReplicaTx read ran on the primary")
		}
		return nil
	}, keel.ReplicaTx(), keel.RepeatableRead()))
	if !onReplica(t, rec) {
		t.Fatal("ReplicaTx statements ran on the primary")
	}
	eq(t, d.Replicas()[0].InFlight, int64(0))
}

func TestReplicaReadYourWrites(t *testing.T) {
	rec := &recorder{}
	d := replicaDB(t, rec, 5*time.Second, nil)
	base := newTenant()
	ctx := keel.OnReplica(base)
	c := fleetdb.Customers
	onPrimary := 0
	for i := range 40 {
		cust := &fleet.Customer{Name: "RYW", Code: fmt.Sprint("RYW-", i)}
		ok(t, c.Create(ctx, d, cust))
		rec.reset()
		got, err := c.Get(ctx, d, cust.ID)
		if err != nil {
			t.Fatalf("read %d after its write: %v", i, err)
		}
		eq(t, got.Code, cust.Code)
		if !onReplica(t, rec) {
			onPrimary++
		}
	}
	t.Logf("40 reads at once after their writes: %d on the primary, %d on the replica", onPrimary, 40-onPrimary)

	guarded := 0
	for i := range 10 {
		cust := &fleet.Customer{Name: "RYW", Code: fmt.Sprint("RYW-later-", i)}
		ok(t, c.Create(ctx, d, cust))
		time.Sleep(d.Replicas()[0].Lag + 30*time.Millisecond)
		rec.reset()
		got, err := c.Get(ctx, d, cust.ID)
		if err != nil {
			t.Fatalf("read %d after its write: %v", i, err)
		}
		eq(t, got.Code, cust.Code)
		if onReplica(t, rec) {
			guarded++
		}
	}
	t.Logf("10 reads 30ms past the replica lag after their writes: %d on the replica through the guard", guarded)
	if guarded == 0 {
		t.Fatal("no read went to the replica through the guard")
	}

	pos := keel.CommitPosition(ctx, d)
	caughtUp(t, ctx, d)
	rec.reset()
	eq(t, must(c.Query().Count(ctx, d))(t), int64(50))
	if !onReplica(t, rec) {
		t.Fatal("a read past the commit position ran on the primary")
	}

	later := keel.OnReplicaAfter(base, d, pos)
	eq(t, keel.CommitPosition(later, d), pos)
	ahead := keel.OnReplicaAfter(context.WithoutCancel(base), d, pos+1<<40)
	before := d.ReplicaFallbacks()
	rec.reset()
	eq(t, must(c.Query().Count(ahead, d))(t), int64(50))
	if onReplica(t, rec) || d.ReplicaFallbacks() != before+1 {
		t.Fatal("a read whose guard failed ran on the replica")
	}
	eq(t, rec.statements(), 1)
	eq(t, d.Replicas()[0].InFlight, int64(0))

	parsed := must(keel.ParsePosition(pos.String()))(t)
	eq(t, parsed, pos)
}

func TestReplicaWithOnBegin(t *testing.T) {
	rec := &recorder{}
	d := replicaDB(t, rec, 5*time.Second, func(cfg *keel.Config) {
		cfg.OnBegin = func(ctx context.Context, tx keel.TxInfo) ([]keel.Statement, error) {
			org := "system"
			if v, ok := ctx.Value(tenantKey{}).(tenant); ok {
				org = string(v.org)
			}
			return []keel.Statement{{SQL: "SELECT set_config('keel.org', $1, true)", Args: []any{org}}}, nil
		}
	})
	base := newTenant()
	ctx := keel.OnReplica(base)
	c := fleetdb.Customers
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "OnBegin", Code: "OB"}))
	caughtUp(t, ctx, d)
	org, _ := tenantOf(base)
	scoped := c.Query().Where(keel.Raw("current_setting('keel.org') = ?", string(org)))

	rec.reset()
	eq(t, must(scoped.Count(ctx, d))(t), int64(1))
	if !onReplica(t, rec) {
		t.Fatal("an implicit transaction did not run on the replica")
	}

	ahead := keel.OnReplicaAfter(context.WithoutCancel(base), d, keel.CommitPosition(ctx, d)+1<<40)
	rec.reset()
	eq(t, must(scoped.Count(ahead, d))(t), int64(1))
	if onReplica(t, rec) {
		t.Fatal("an implicit transaction whose guard failed stayed on the replica")
	}
	eq(t, d.Replicas()[0].InFlight, int64(0))
	eq(t, must(scoped.Count(ctx, d))(t), int64(1))
}

func TestReplicaLagFallsBackToPrimary(t *testing.T) {
	rec := &recorder{}
	d := replicaDB(t, rec, time.Second, nil)
	admin, err := pgx.Connect(context.Background(), replicaDSN(t))
	ok(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "SELECT pg_wal_replay_resume()")
		_ = admin.Close(context.Background())
	})
	_, err = admin.Exec(context.Background(), "SELECT pg_wal_replay_pause()")
	ok(t, err)

	ctx := newTenant()
	var writes atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if err := fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "L", Code: fmt.Sprint("LAG-", i)}); err == nil {
				writes.Add(1)
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	waitFor(t, "the replica to lag", func() bool { return d.Replicas()[0].Lag > time.Second })
	st := d.Replicas()[0]
	if !st.Healthy {
		t.Fatalf("a paused replica is unhealthy: %v", st.Err)
	}
	marked := keel.OnReplica(newTenant())
	before := d.ReplicaFallbacks()
	rec.reset()
	if inRecovery(t, marked, d) || onReplica(t, rec) {
		t.Fatal("a read ran on a replica past the lag bound")
	}
	if d.ReplicaFallbacks() <= before {
		t.Fatal("the fallback was not counted")
	}

	_, err = admin.Exec(context.Background(), "SELECT pg_wal_replay_resume()")
	ok(t, err)
	waitFor(t, "the replica to catch up", func() bool {
		st := d.Replicas()[0]
		return st.Healthy && st.Lag <= time.Second
	})
	rec.reset()
	if !inRecovery(t, marked, d) || !onReplica(t, rec) {
		t.Fatal("a read did not return to the replica after it caught up")
	}
	if writes.Load() == 0 {
		t.Fatal("no writes while the replica was paused")
	}
}

func TestReplicaThatIsNotInRecoveryIsNotUsed(t *testing.T) {
	db(t)
	rec := &recorder{}
	var connects atomic.Int64
	cfg := testConfig(testDSN)
	cfg.Replicas = []keel.ReplicaConfig{{DSN: testDSN, Weight: 2}}
	cfg.Observers = []keel.Observer{rec}
	cfg.AfterConnect = func(context.Context, *pgx.Conn) error {
		connects.Add(1)
		return nil
	}
	d, err := keel.Open(context.Background(), cfg)
	ok(t, err)
	t.Cleanup(d.Close)
	waitFor(t, "a replica check", func() bool { return !d.Replicas()[0].CheckedAt.IsZero() })
	st := d.Replicas()[0]
	if st.Healthy || st.Err == nil || !strings.Contains(st.Err.Error(), "not in recovery") {
		t.Fatalf("a primary listed as a replica: healthy %v, error %v", st.Healthy, st.Err)
	}
	eq(t, st.Weight, 2)
	if connects.Load() < 2 {
		t.Fatalf("AfterConnect ran %d times, want once on the primary pool and once on the replica's",
			connects.Load())
	}
	w := seed(t, 2)
	rec.reset()
	n := must(fleetdb.Shipments.Query().Count(keel.OnReplica(w.ctx), d))(t)
	eq(t, n, int64(2))
	if onReplica(t, rec) || d.ReplicaFallbacks() != 1 {
		t.Fatalf("read on a replica, or %d fallbacks", d.ReplicaFallbacks())
	}
}

func TestUnreachableReplicaFallsBack(t *testing.T) {
	db(t)
	cfg := testConfig(testDSN)
	cfg.Replicas = []keel.ReplicaConfig{{DSN: "postgres://keel:keel@127.0.0.1:1/keel?sslmode=disable&connect_timeout=1"}}
	d, err := keel.Open(context.Background(), cfg)
	ok(t, err)
	t.Cleanup(d.Close)
	waitFor(t, "a replica check", func() bool { return !d.Replicas()[0].CheckedAt.IsZero() })
	st := d.Replicas()[0]
	if st.Healthy || st.Err == nil {
		t.Fatalf("an unreachable replica: healthy %v, error %v", st.Healthy, st.Err)
	}
	w := seed(t, 1)
	eq(t, must(fleetdb.Shipments.Query().Count(keel.OnReplica(w.ctx), d))(t), int64(1))
	ok(t, keel.Read(w.ctx, d, func(ctx context.Context) error {
		_, err := fleetdb.Shipments.Query().Count(ctx, d)
		return err
	}, keel.ReplicaTx()))
}

func TestReplicaTxOptions(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	if err := keel.Write(ctx, d, func(context.Context) error { return nil }, keel.ReplicaTx()); err == nil {
		t.Fatal("Write accepted ReplicaTx")
	}
	if err := keel.Read(ctx, d, func(context.Context) error { return nil },
		keel.ReplicaTx(), keel.Serializable()); err == nil {
		t.Fatal("ReplicaTx accepted Serializable")
	}
	ok(t, keel.Read(ctx, d, func(context.Context) error { return nil }, keel.ReplicaTx()))
	if keel.CommitPosition(keel.OnReplica(ctx), d) != 0 || d.Replicas() != nil || d.ReplicaFallbacks() != 0 {
		t.Fatal("a DB without replicas reports replica state")
	}
}

func TestOpenRefusesBadReplicas(t *testing.T) {
	db(t)
	cfg := testConfig(testDSN)
	cfg.Replicas = []keel.ReplicaConfig{{}}
	if _, err := keel.Open(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "replica 0") {
		t.Fatalf("a replica without a DSN: %v", err)
	}
	cfg.Replicas = []keel.ReplicaConfig{{DSN: testDSN, Weight: -1}}
	if _, err := keel.Open(context.Background(), cfg); err == nil {
		t.Fatal("a negative weight was accepted")
	}
}
