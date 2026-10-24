package keel_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// versionsDB opens a DB on the test database with data versions on, written every interval.
func versionsDB(t *testing.T, interval time.Duration, rec *recorder) *keel.DB {
	t.Helper()
	db(t)
	cfg := testConfig(testDSN)
	cfg.DataVersions = keel.DataVersionsConfig{Enabled: true, Interval: interval}
	if rec != nil {
		cfg.Observers = []keel.Observer{rec}
	}
	d, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

// versionOf flushes d and returns the version of table for ctx's tenant.
func versionOf(t *testing.T, d *keel.DB, ctx context.Context, table string) int64 {
	t.Helper()
	ok(t, d.FlushDataVersions(context.Background()))
	v := must(d.DataVersions(context.Background(), d.TenantKey(ctx), table))(t)
	return v[table]
}

func TestDataVersionsEveryWriteTerminal(t *testing.T) {
	d := versionsDB(t, time.Hour, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	tg := fleetdb.Tags
	first := &fleet.Customer{Name: "First", Code: "F1"}
	steps := []struct {
		name  string
		table string
		write func() error
	}{
		{"Create", "customers", func() error { return c.Create(ctx, d, first) }},
		{"CreateMany", "customers", func() error {
			return c.CreateMany(ctx, d, []*fleet.Customer{{Name: "A", Code: "A"}, {Name: "B", Code: "B"}})
		}},
		{"Save", "customers", func() error {
			first.Name = "First, renamed"
			return c.Save(ctx, d, first, c.Name)
		}},
		{"Upsert", "customers", func() error {
			_, err := c.Upsert(ctx, d, []*fleet.Customer{{ID: first.ID, Name: "Upserted", Code: "F1"}},
				keel.OnConflict(c.ID, c.OrganizationID, c.BusinessUnitID).Update(c.Name))
			return err
		}},
		{"CreateOrSkip", "customers", func() error {
			_, err := c.CreateOrSkip(ctx, d, []*fleet.Customer{{Name: "C", Code: "C"}},
				c.ID, c.OrganizationID, c.BusinessUnitID)
			return err
		}},
		{"Copy", "customers", func() error {
			_, err := c.Copy(ctx, d, []*fleet.Customer{{Name: "D", Code: "D"}})
			return err
		}},
		{"Update", "customers", func() error {
			_, err := c.Where(c.Code.Eq("A")).Update(ctx, d, c.Name.To("A2"))
			return err
		}},
		{"Delete", "customers", func() error {
			_, err := c.Where(c.Code.Eq("B")).Delete(ctx, d)
			return err
		}},
		{"DeleteRow", "customers", func() error { return c.DeleteRow(ctx, d, first) }},
		{"InsertFrom", "tags", func() error {
			_, err := tg.InsertFrom(ctx, d, c.Pick(keel.Raw("'tag_' || ?", c.ID), c.Name), tg.ID, tg.Name)
			return err
		}},
		{"MarkChanged", "tags", func() error {
			if _, err := keel.Exec(ctx, d, "SELECT 1"); err != nil {
				return err
			}
			keel.MarkChanged(ctx, d, "tags")
			return nil
		}},
	}
	want := map[string]int64{}
	for _, st := range steps {
		ok(t, st.write())
		want[st.table]++
		if got := versionOf(t, d, ctx, st.table); got != want[st.table] {
			t.Fatalf("%s: %s version %d, want %d", st.name, st.table, got, want[st.table])
		}
	}
	org, _ := tenantOf(ctx)
	_ = must(keel.Exec(ctx, d, "UPDATE tags SET name = name WHERE organization_id = $1", string(org)))(t)
	eq(t, versionOf(t, d, ctx, "tags"), want["tags"])
	eq(t, versionOf(t, d, newTenant(), "customers"), int64(0))
}

func TestDataVersionsDebounce(t *testing.T) {
	rec := &recorder{}
	d := versionsDB(t, time.Hour, rec)
	ctx := newTenant()
	c := fleetdb.Customers
	for i := range 50 {
		ok(t, c.Create(ctx, d, &fleet.Customer{Name: "C", Code: fmt.Sprint(i)}))
	}
	rec.reset()
	ok(t, d.FlushDataVersions(context.Background()))
	eq(t, versionUpserts(rec), 1)
	eq(t, versionOf(t, d, ctx, "customers"), int64(1))

	timed := versionsDB(t, 100*time.Millisecond, rec)
	rec.reset()
	start := time.Now()
	for i := 0; time.Since(start) < time.Second; i++ {
		ok(t, c.Create(ctx, timed, &fleet.Customer{Name: "C", Code: fmt.Sprint("timed-", i)}))
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)
	ok(t, timed.FlushDataVersions(context.Background()))
	upserts := versionUpserts(rec)
	limit := int(elapsed/(100*time.Millisecond)) + 2
	if upserts < 2 || upserts > limit {
		t.Fatalf("%d upserts in %v, want 2 to %d", upserts, elapsed, limit)
	}
	got := versionOf(t, timed, ctx, "customers")
	if got != 1+int64(upserts) {
		t.Fatalf("version %d after %d upserts, want %d", got, upserts, 1+upserts)
	}
}

// versionUpserts counts the statements that wrote keel_data_versions.
func versionUpserts(rec *recorder) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, e := range rec.events {
		if e.Table == "keel_data_versions" && e.Op == "insert" {
			n++
		}
	}
	return n
}

func TestDataVersionsAreWrittenOutsideTheWriteTransaction(t *testing.T) {
	d := versionsDB(t, time.Hour, nil)
	a, b := newTenant(), newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(a, d, &fleet.Customer{Name: "A", Code: "A"}))

	inTx := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- keel.Write(a, d, func(ctx context.Context) error {
			if err := c.Create(ctx, d, &fleet.Customer{Name: "A2", Code: "A2"}); err != nil {
				return err
			}
			if _, err := c.Where(c.Code.Eq("A")).Update(ctx, d, c.Name.To("held")); err != nil {
				return err
			}
			close(inTx)
			<-release
			return nil
		})
	}()
	<-inTx
	ok(t, c.Create(b, d, &fleet.Customer{Name: "B", Code: "B"}))
	fctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	ok(t, d.FlushDataVersions(fctx))
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("flush waited %v on an open write transaction", took)
	}
	eq(t, versionOf(t, d, a, "customers"), int64(1))
	eq(t, versionOf(t, d, b, "customers"), int64(1))

	close(release)
	ok(t, <-done)
	eq(t, versionOf(t, d, a, "customers"), int64(2))
}

func TestDataVersionsSkipRolledBackWrites(t *testing.T) {
	d := versionsDB(t, time.Hour, nil)
	ctx := newTenant()
	boom := errors.New("boom")
	err := keel.Write(ctx, d, func(ctx context.Context) error {
		if err := fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "X", Code: "X"}); err != nil {
			return err
		}
		keel.MarkChanged(ctx, d, "tags")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	eq(t, versionOf(t, d, ctx, "customers"), int64(0))
	eq(t, versionOf(t, d, ctx, "tags"), int64(0))

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		return fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "Y", Code: "Y"})
	}))
	eq(t, versionOf(t, d, ctx, "customers"), int64(1))
}

func TestDataVersionsSystemScopeRecordsEachRowsTenant(t *testing.T) {
	d := versionsDB(t, time.Hour, nil)
	a, b := newTenant(), newTenant()
	orgA, buA := tenantOf(a)
	orgB, buB := tenantOf(b)
	sys := systemCtx("data versions test")
	c := fleetdb.Customers
	rows := []*fleet.Customer{
		{OrganizationID: orgA, BusinessUnitID: buA, Name: "A", Code: "SYS-A-" + string(orgA)},
		{OrganizationID: orgB, BusinessUnitID: buB, Name: "B1", Code: "SYS-B1-" + string(orgA)},
		{OrganizationID: orgB, BusinessUnitID: buB, Name: "B2", Code: "SYS-B2-" + string(orgA)},
	}
	ok(t, c.CreateMany(sys, d, rows))
	eq(t, versionOf(t, d, a, "customers"), int64(1))
	eq(t, versionOf(t, d, b, "customers"), int64(1))

	mine := c.Code.In("SYS-A-"+string(orgA), "SYS-B1-"+string(orgA), "SYS-B2-"+string(orgA))
	n := must(c.Where(mine).AllTenants().Update(sys, d, c.Name.To("renamed")))(t)
	eq(t, n, int64(3))
	eq(t, versionOf(t, d, a, "customers"), int64(2))
	eq(t, versionOf(t, d, b, "customers"), int64(2))
	eq(t, must(d.DataVersions(context.Background(), "", "customers"))(t)["customers"], int64(0))

	only := c.Code.Eq("SYS-B2-" + string(orgA))
	n = must(c.Where(only).AllTenants().Delete(sys, d))(t)
	eq(t, n, int64(1))
	eq(t, versionOf(t, d, a, "customers"), int64(2))
	eq(t, versionOf(t, d, b, "customers"), int64(3))

	saved := must(c.Where(c.ID.Eq(rows[0].ID)).AllTenants().One(sys, d))(t)
	saved.Name = "saved"
	ok(t, c.Save(sys, d, saved, c.Name))
	eq(t, versionOf(t, d, a, "customers"), int64(3))

	tg := fleetdb.Tags
	n = must(tg.InsertFrom(sys, d,
		c.Pick(keel.Raw("'tag_' || ?", c.ID), c.Name, c.OrganizationID, c.BusinessUnitID).Where(mine).AllTenants(),
		tg.ID, tg.Name, tg.OrganizationID, tg.BusinessUnitID))(t)
	eq(t, n, int64(2))
	eq(t, versionOf(t, d, a, "tags"), int64(1))
	eq(t, versionOf(t, d, b, "tags"), int64(1))

	ok(t, fleetdb.Plants.Create(a, d, &fleet.Plant{Code: "PL-" + string(orgA), Name: "Plant"}))
	shared := versionOf(t, d, b, "plants")
	if shared < 1 {
		t.Fatalf("a shared table's change is not visible to another tenant: version %d", shared)
	}
}

func TestDataVersionsSystemScopeBulkWriteWithHooks(t *testing.T) {
	d := versionsDB(t, time.Hour, nil)
	c := fleetdb.Customers
	var seen int
	keel.OnUpdate(d, c, keel.AfterWrite, func(context.Context, *keel.Mutation[fleet.Customer]) error {
		seen++
		return nil
	})
	a, b := newTenant(), newTenant()
	org, _ := tenantOf(a)
	codeA, codeB := "VH-A-"+string(org), "VH-B-"+string(org)
	ok(t, c.Create(a, d, &fleet.Customer{Name: "A", Code: codeA}))
	ok(t, c.Create(b, d, &fleet.Customer{Name: "B", Code: codeB}))
	eq(t, versionOf(t, d, a, "customers"), int64(1))
	n := must(c.Where(c.Code.In(codeA, codeB)).AllTenants().Update(systemCtx("hooks"), d, c.Name.To("x")))(t)
	eq(t, n, int64(2))
	eq(t, seen, 2)
	eq(t, versionOf(t, d, a, "customers"), int64(2))
	eq(t, versionOf(t, d, b, "customers"), int64(2))
}

func TestCloseFlushesDataVersions(t *testing.T) {
	db(t)
	cfg := testConfig(testDSN)
	cfg.DataVersions = keel.DataVersionsConfig{Enabled: true, Interval: time.Hour}
	d, err := keel.Open(context.Background(), cfg)
	ok(t, err)
	ctx := newTenant()
	ok(t, fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "C", Code: "C"}))
	d.Close()
	d.Close()
	reader := versionsDB(t, time.Hour, nil)
	eq(t, versionOf(t, reader, ctx, "customers"), int64(1))
}

func TestDataVersionsOff(t *testing.T) {
	d := db(t)
	if _, err := d.DataVersions(context.Background(), "", "customers"); err == nil {
		t.Fatal("DataVersions works with data versions off")
	}
	ok(t, d.FlushDataVersions(context.Background()))
	keel.MarkChanged(newTenant(), d, "customers")
}

func TestCloseStopsBackgroundGoroutines(t *testing.T) {
	db(t)
	cfg := testConfig(testDSN)
	cfg.DataVersions = keel.DataVersionsConfig{Enabled: true, Interval: 10 * time.Millisecond}
	cfg.Replicas = []keel.ReplicaConfig{{DSN: testDSN}}
	before := runtime.NumGoroutine()
	d, err := keel.Open(context.Background(), cfg)
	ok(t, err)
	waitFor(t, "a replica check", func() bool { return !d.Replicas()[0].CheckedAt.IsZero() })
	if !strings.Contains(stacks(), "keel.(*replicaSet).run") || !strings.Contains(stacks(), "keel.(*versioner).run") {
		t.Fatal("background goroutines not running")
	}
	d.Close()
	if s := stacks(); strings.Contains(s, "keel.(*replicaSet)") || strings.Contains(s, "keel.(*versioner)") {
		t.Fatalf("goroutines left after Close:\n%s", s)
	}
	waitFor(t, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= before })
}

func stacks() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

// waitFor polls cond for up to 15 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDataVersionsConcurrentWriters(t *testing.T) {
	d := versionsDB(t, 20*time.Millisecond, nil)
	tenants := make([]context.Context, 8)
	for i := range tenants {
		tenants[i] = newTenant()
	}
	var wg sync.WaitGroup
	for i, ctx := range tenants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				if err := fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "C", Code: fmt.Sprint(i, j)}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, ctx := range tenants {
		if v := versionOf(t, d, ctx, "customers"); v < 1 || v > 20 {
			t.Fatalf("version %d after 20 writes", v)
		}
	}
}

func TestDataVersionsSystemScopeBulkWriteToATableKeyedByPartOfTheTenant(t *testing.T) {
	d := versionsDB(t, time.Hour, nil)
	a, b := newTenant(), newTenant()
	_, buA := tenantOf(a)
	_, buB := tenantOf(b)
	sys := systemCtx("data versions test")
	dp := fleetdb.Depots
	ok(t, dp.CreateMany(sys, d, []*fleet.Depot{
		{BusinessUnitID: buA, Name: "North"},
		{BusinessUnitID: buB, Name: "South"},
	}))

	created := versionOf(t, d, a, "depots")

	both := dp.BusinessUnitID.In(buA, buB)
	n := must(dp.Where(both).AllTenants().Update(sys, d, dp.Name.To("renamed")))(t)
	eq(t, n, int64(2))
	if v := versionOf(t, d, a, "depots"); v <= created {
		t.Fatalf("the update is not visible to the first business unit: version %d after %d", v, created)
	}
	if v := versionOf(t, d, b, "depots"); v <= created {
		t.Fatalf("the update is not visible to the second business unit: version %d after %d", v, created)
	}

	n = must(dp.Where(both).AllTenants().Delete(sys, d))(t)
	eq(t, n, int64(2))
}
