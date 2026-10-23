package seed_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/seed"
)

func upsertPlants(path string) func(context.Context, *seed.Run) error {
	return func(ctx context.Context, r *seed.Run) error {
		plants, err := seed.YAML[[]*fleet.Plant](r, path)
		if err != nil {
			return err
		}
		p := fleetdb.Plants
		if _, err := p.Upsert(ctx, r.DB, plants, keel.OnConflict(p.Code)); err != nil {
			return err
		}
		r.Count(int64(len(plants)))
		return nil
	}
}

func TestFingerprintComponents(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	fsys := fstest.MapFS{
		"data/a.yaml": {Data: []byte("- code: A\n  name: Alpha\n")},
		"data/b.yaml": {Data: []byte("- code: B\n  name: Bravo\n")},
	}
	code := "v1"
	c := newCounter()
	g := seed.NewRegistry()
	inputs := g.Add(seed.Seed{Name: "inputs", Kind: seed.Reference, Inputs: seed.Files(fsys, "data/a.yaml", "data/b.yaml"),
		Run: func(ctx context.Context, r *seed.Run) error {
			c.hit("inputs")
			if err := upsertPlants("data/a.yaml")(ctx, r); err != nil {
				return err
			}
			if len(r.Inputs()) == 1 {
				return nil
			}
			return upsertPlants("data/b.yaml")(ctx, r)
		}})
	version := g.Add(seed.Seed{Name: "version", Kind: seed.Reference, Version: "1", Run: c.run("version")})
	g.Add(seed.Seed{Name: "code", Kind: seed.Reference, Fingerprint: func() string { return code },
		Run: c.run("code")})
	g.Add(seed.Seed{Name: "steady", Kind: seed.Reference, Run: c.run("steady")})

	rep := must(g.Apply(ctx, d, opts(nil)))(t)
	for _, res := range rep.Results {
		eq(t, res.Ran, true)
		eq(t, res.Reason, seed.New)
	}
	eq(t, result(t, rep, "inputs").Rows, int64(2))
	eq(t, scalar[int64](t, d, "SELECT rows FROM keel_seeds WHERE name = 'inputs'"), int64(2))

	rep = must(g.Apply(ctx, d, opts(nil)))(t)
	for _, res := range rep.Results {
		eq(t, res.Ran, false)
		eq(t, res.Reason, seed.UpToDate)
	}

	expectOnly := func(name string, reason seed.Reason, changed string) {
		t.Helper()
		plan := must(g.Plan(ctx, d, opts(nil)))(t)
		for _, it := range plan.Seeds {
			if it.Seed.Name != name {
				eq(t, it.Reason, seed.UpToDate)
				continue
			}
			eq(t, it.Reason, reason)
			eq(t, it.Run, true)
			eq(t, strings.Join(it.Changed, ","), changed)
		}
		before := map[string]int{}
		for _, n := range []string{"inputs", "version", "code", "steady"} {
			before[n] = c.count(n)
		}
		rep := must(g.Apply(ctx, d, opts(nil)))(t)
		eq(t, result(t, rep, name).Ran, true)
		for n, was := range before {
			want := was
			if n == name {
				want++
			}
			eq(t, c.count(n), want)
		}
		eq(t, len(must(g.Plan(ctx, d, opts(nil)))(t).Runs()), 0)
	}

	fsys["data/b.yaml"] = &fstest.MapFile{Data: []byte("- code: B\n  name: Bravo Two\n")}
	expectOnly("inputs", seed.InputChanged, "data/b.yaml")
	eq(t, must(fleetdb.Plants.Get(ctx, d, "B"))(t).Name, "Bravo Two")

	inputs.Inputs = seed.Files(fsys, "data/a.yaml")
	expectOnly("inputs", seed.InputChanged, "data/b.yaml")

	version.Version = "2"
	expectOnly("version", seed.VersionChanged, "")

	code = "v2"
	expectOnly("code", seed.CodeChanged, "")
}

func TestFixturesRunOnce(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	c := newCounter()
	g := seed.NewRegistry()
	fx := g.Add(seed.Seed{Name: "fixture", Kind: seed.Fixture, Version: "1", Run: c.run("fixture")})
	must(g.Apply(ctx, d, opts(nil)))(t)
	must(g.Apply(ctx, d, opts(nil)))(t)
	eq(t, c.count("fixture"), 1)

	fx.Version = "2"
	plan := must(g.Plan(ctx, d, opts(nil)))(t)
	eq(t, item(t, plan, "fixture").Reason, seed.FixtureApplied)
	must(g.Apply(ctx, d, opts(nil)))(t)
	eq(t, c.count("fixture"), 1)

	o := opts(nil)
	o.Force = true
	rep := must(g.Apply(ctx, d, o))(t)
	eq(t, result(t, rep, "fixture").Reason, seed.Forced)
	eq(t, c.count("fixture"), 2)
}

func TestAfterOrder(t *testing.T) {
	ctx := context.Background()
	for _, parallel := range []int{1, 4} {
		t.Run(fmt.Sprint("parallel ", parallel), func(t *testing.T) {
			d := newDB(t)
			var mu sync.Mutex
			spans := map[string][2]time.Time{}
			var log []string
			run := func(name string) func(context.Context, *seed.Run) error {
				return func(context.Context, *seed.Run) error {
					start := time.Now()
					mu.Lock()
					log = append(log, name)
					mu.Unlock()
					time.Sleep(20 * time.Millisecond)
					mu.Lock()
					spans[name] = [2]time.Time{start, time.Now()}
					mu.Unlock()
					return nil
				}
			}
			g := seed.NewRegistry()
			c := g.Add(seed.Seed{Name: "c", Kind: seed.Reference, Run: run("c")})
			g.Add(seed.Seed{Name: "d", Kind: seed.Reference, Run: run("d")})
			b := g.Add(seed.Seed{Name: "b", Kind: seed.Reference, Run: run("b")})
			a := g.Add(seed.Seed{Name: "a", Kind: seed.Reference, Run: run("a")})
			c.After = []*seed.Seed{b}
			b.After = []*seed.Seed{a}
			o := opts(nil)
			o.Parallel = parallel
			must(g.Apply(ctx, d, o))(t)
			if parallel == 1 {
				eq(t, strings.Join(log, ","), "d,a,b,c")
			}
			for _, edge := range [][2]string{{"a", "b"}, {"b", "c"}} {
				if spans[edge[1]][0].Before(spans[edge[0]][1]) {
					t.Fatalf("%s started before %s ended", edge[1], edge[0])
				}
			}
		})
	}
}

func TestParallelRunsIndependentSeedsTogether(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		parallel int
		overlap  bool
	}{{1, false}, {2, true}} {
		t.Run(fmt.Sprint("parallel ", tc.parallel), func(t *testing.T) {
			d := newDB(t)
			var mu sync.Mutex
			active := 0
			both := make(chan struct{})
			var overlapped atomic.Bool
			run := func(context.Context, *seed.Run) error {
				mu.Lock()
				active++
				if active == 2 {
					close(both)
				}
				mu.Unlock()
				select {
				case <-both:
					overlapped.Store(true)
				case <-time.After(300 * time.Millisecond):
				}
				mu.Lock()
				active--
				mu.Unlock()
				return nil
			}
			g := seed.NewRegistry()
			g.Add(seed.Seed{Name: "one", Kind: seed.Reference, Run: run})
			g.Add(seed.Seed{Name: "two", Kind: seed.Reference, Run: run})
			o := opts(nil)
			o.Parallel = tc.parallel
			rep := must(g.Apply(ctx, d, o))(t)
			eq(t, result(t, rep, "one").Ran, true)
			eq(t, result(t, rep, "two").Ran, true)
			eq(t, overlapped.Load(), tc.overlap)
		})
	}
}

func TestFailingSeedRollsBack(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	boom := errors.New("boom")
	g := seed.NewRegistry()
	g.Add(seed.Seed{Name: "good", Kind: seed.Reference, Run: func(ctx context.Context, r *seed.Run) error {
		return fleetdb.Plants.Create(ctx, r.DB, &fleet.Plant{Code: "GOOD", Name: "kept"})
	}})
	bad := g.Add(seed.Seed{Name: "bad", Kind: seed.Reference, Run: func(ctx context.Context, r *seed.Run) error {
		if err := fleetdb.Plants.Create(ctx, r.DB, &fleet.Plant{Code: "BAD", Name: "lost"}); err != nil {
			return err
		}
		r.Count(1)
		return boom
	}})
	g.Add(seed.Seed{Name: "later", Kind: seed.Reference, After: []*seed.Seed{bad}, Run: noop})
	g.Add(seed.Seed{Name: "panics", Kind: seed.Fixture, Run: func(context.Context, *seed.Run) error {
		panic("kaboom")
	}})

	rep, err := g.Apply(ctx, d, opts(nil))
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	eq(t, result(t, rep, "good").Ran, true)
	badRes := result(t, rep, "bad")
	eq(t, badRes.Ran, false)
	eq(t, errors.Is(badRes.Err, boom), true)
	eq(t, badRes.Rows, int64(0))
	eq(t, errors.Is(result(t, rep, "later").Err, seed.ErrAborted), true)
	_, err = fleetdb.Plants.Get(ctx, d, "BAD")
	eq(t, errors.Is(err, keel.ErrNotFound), true)
	eq(t, must(fleetdb.Plants.Get(ctx, d, "GOOD"))(t).Name, "kept")
	eq(t, scalar[string](t, d, "SELECT string_agg(name, ',' ORDER BY name) FROM keel_seeds"), "good")

	bad.Run = noop
	_, err = g.Apply(ctx, d, opts(nil))
	if err == nil || !strings.Contains(err.Error(), "panic: kaboom") {
		t.Fatalf("panic: got %v", err)
	}
	eq(t, scalar[string](t, d, "SELECT string_agg(name, ',' ORDER BY name) FROM keel_seeds"), "bad,good,later")
}

func TestTimeout(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	g := seed.NewRegistry()
	g.Add(seed.Seed{Name: "slow", Kind: seed.Reference, Timeout: 200 * time.Millisecond,
		Run: func(ctx context.Context, r *seed.Run) error {
			if err := fleetdb.Plants.Create(ctx, r.DB, &fleet.Plant{Code: "SLOW", Name: "lost"}); err != nil {
				return err
			}
			_, err := keel.Exec(ctx, r.DB, "SELECT pg_sleep(5)")
			return err
		}})
	start := time.Now()
	_, err := g.Apply(ctx, d, opts(nil))
	if err == nil || !strings.Contains(err.Error(), "seed: slow: timed out after 200ms") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not enforced: %v", time.Since(start))
	}
	_, err = fleetdb.Plants.Get(ctx, d, "SLOW")
	eq(t, errors.Is(err, keel.ErrNotFound), true)
	eq(t, scalar[int64](t, d, "SELECT count(*) FROM keel_seeds"), int64(0))
}

func TestPerTenantResume(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	draws := map[string]uint64{}
	g := seed.NewRegistry()
	g.Add(seed.Seed{Name: "customers", Kind: seed.Reference, PerTenant: true,
		Run: func(ctx context.Context, r *seed.Run) error {
			mu.Lock()
			draws[r.Tenant.Key] = r.Rand().Uint64()
			mu.Unlock()
			cust := &fleet.Customer{Code: "C-" + r.Tenant.Key, Name: r.Tenant.Name}
			if err := fleetdb.Customers.Create(ctx, r.DB, cust); err != nil {
				return err
			}
			r.Count(1)
			if r.Tenant.Key == "t3" && fail.Load() {
				return errors.New("tenant three is broken")
			}
			return nil
		}})
	o := opts(nil)
	o.Tenants = tenantList("t1", "t2", "t3", "t4")
	rec := &recorder{}
	o.Observer = rec.observe

	rep, err := g.Apply(ctx, d, o)
	if err == nil || !strings.Contains(err.Error(), `seed: customers: tenant "t3": tenant three is broken`) {
		t.Fatalf("got %v", err)
	}
	res := result(t, rep, "customers")
	eq(t, res.Tenants, 2)
	eq(t, res.Rows, int64(2))
	var started []string
	for _, e := range rec.of(seed.TenantStart) {
		started = append(started, e.Tenant)
		eq(t, e.Tenants, 4)
	}
	eq(t, strings.Join(started, ","), "t1,t2,t3")
	ends := rec.of(seed.TenantEnd)
	eq(t, len(ends), 3)
	eq(t, ends[2].TenantsDone, 3)
	eq(t, ends[2].Err != nil, true)
	eq(t, ends[0].Rows, int64(1))
	countIn := func(key string) int64 {
		return must(fleetdb.Customers.Query().Count(tenantContext(ctx, key), d))(t)
	}
	eq(t, countIn("t1"), int64(1))
	eq(t, countIn("t2"), int64(1))
	eq(t, countIn("t3"), int64(0))
	eq(t, countIn("t4"), int64(0))
	eq(t, scalar[int64](t, d, "SELECT count(*) FROM keel_seeds"), int64(0))
	plan := must(g.Plan(ctx, d, o))(t)
	eq(t, item(t, plan, "customers").Reason, seed.New)
	eq(t, item(t, plan, "customers").TenantsApplied, 2)

	fail.Store(false)
	rec2 := &recorder{}
	o.Observer = rec2.observe
	rep = must(g.Apply(ctx, d, o))(t)
	eq(t, result(t, rep, "customers").Tenants, 2)
	started = nil
	for _, e := range rec2.of(seed.TenantStart) {
		started = append(started, e.Tenant)
		eq(t, e.Tenants, 2)
	}
	eq(t, strings.Join(started, ","), "t3,t4")
	eq(t, rec2.of(seed.SeedStart)[0].Tenants, 2)
	end := rec2.of(seed.SeedEnd)[0]
	eq(t, end.TenantsDone, 2)
	eq(t, end.Rows, int64(2))
	for _, k := range []string{"t1", "t2", "t3", "t4"} {
		eq(t, countIn(k), int64(1))
	}
	eq(t, scalar[int64](t, d, "SELECT rows FROM keel_seeds WHERE name = 'customers'"), int64(2))
	eq(t, scalar[int64](t, d, "SELECT count(*) FROM keel_seed_tenants"), int64(4))

	rep = must(g.Apply(ctx, d, o))(t)
	eq(t, result(t, rep, "customers").Reason, seed.UpToDate)
	eq(t, draws["t1"] != draws["t2"], true)

	o.Parallel = 3
	o.Force = true
	rep, err = g.Apply(ctx, d, o)
	eq(t, keel.IsUnique(err), true)
	eq(t, result(t, rep, "customers").Reason, seed.Forced)
	eq(t, result(t, rep, "customers").Ran, false)
	eq(t, keel.IsUnique(result(t, rep, "customers").Err), true)
}

func TestConcurrentApplySerializes(t *testing.T) {
	ctx := context.Background()
	dsn := newDatabase(t)
	first, second := openDB(t, dsn), openDB(t, dsn)
	started := make(chan struct{})
	release := make(chan struct{})
	c := newCounter()
	g := seed.NewRegistry()
	g.Add(seed.Seed{Name: "hold", Kind: seed.Reference, Run: func(context.Context, *seed.Run) error {
		c.hit("hold")
		close(started)
		<-release
		return nil
	}})

	firstDone := make(chan error, 1)
	go func() {
		_, err := g.Apply(ctx, first, opts(nil))
		firstDone <- err
	}()
	<-started

	waiting := make(chan struct{})
	var once sync.Once
	rec := &recorder{}
	o := opts(nil)
	o.Observer = func(e seed.Event) {
		rec.observe(e)
		if e.Kind == seed.LockWait {
			once.Do(func() { close(waiting) })
		}
	}
	secondDone := make(chan error, 1)
	go func() {
		rep, err := g.Apply(ctx, second, o)
		if err == nil && result(t, rep, "hold").Reason != seed.UpToDate {
			err = fmt.Errorf("second run planned %v", result(t, rep, "hold").Reason)
		}
		secondDone <- err
	}()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("second run never waited for the lock")
	}
	time.Sleep(1100 * time.Millisecond)
	close(release)
	ok(t, <-firstDone)
	ok(t, <-secondDone)
	eq(t, c.count("hold"), 1)

	waits := rec.of(seed.LockWait)
	if len(waits) < 2 {
		t.Fatalf("got %d LockWait events, want at least 2", len(waits))
	}
	if waits[0].Holder == nil || waits[0].Holder.PID == 0 {
		t.Fatalf("LockWait without the holder: %+v", waits[0].Holder)
	}
	if waits[1].Duration < 900*time.Millisecond {
		t.Fatalf("second LockWait after %v", waits[1].Duration)
	}
	var kinds []string
	for _, e := range rec.list() {
		if e.Kind != seed.LockWait {
			kinds = append(kinds, e.Kind.String())
		}
	}
	eq(t, strings.Join(kinds, ","), "lock acquired,planned,run end")
}

func TestEnvironment(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	eq(t, must(seed.Environment(ctx, d))(t), seed.Env(""))
	ok(t, seed.MarkEnvironment(ctx, d, seed.Development, false))
	ok(t, seed.MarkEnvironment(ctx, d, seed.Development, false))
	eq(t, must(seed.Environment(ctx, d))(t), seed.Development)
	err := seed.MarkEnvironment(ctx, d, seed.Production, false)
	if !errors.Is(err, seed.ErrEnvironment) {
		t.Fatalf("got %v", err)
	}
	if seed.MarkEnvironment(ctx, d, seed.Env("qa"), false) == nil {
		t.Fatal("unknown environment accepted")
	}

	c := newCounter()
	refs := seed.NewRegistry()
	refs.Add(seed.Seed{Name: "ref", Kind: seed.Reference, Run: c.run("ref")})
	fixtures := seed.NewRegistry()
	fixtures.Add(seed.Seed{Name: "fix", Kind: seed.Fixture, Run: c.run("fix")})
	all := seed.Merge(refs, fixtures)

	o := opts(nil)
	o.Environment = seed.Staging
	_, err = all.Apply(ctx, d, o)
	if !errors.Is(err, seed.ErrEnvironment) {
		t.Fatalf("got %v", err)
	}
	_, err = all.Plan(ctx, d, o)
	if !errors.Is(err, seed.ErrEnvironment) {
		t.Fatalf("plan: got %v", err)
	}

	ok(t, seed.MarkEnvironment(ctx, d, seed.Production, true))
	o.Environment = ""
	plan := must(all.Plan(ctx, d, o))(t)
	eq(t, plan.Environment, seed.Production)
	eq(t, item(t, plan, "fix").Reason, seed.Refused)
	eq(t, item(t, plan, "fix").Run, false)
	rep, err := all.Apply(ctx, d, o)
	if !errors.Is(err, seed.ErrRefused) {
		t.Fatalf("got %v", err)
	}
	eq(t, result(t, rep, "fix").Reason, seed.Refused)
	eq(t, c.count("ref")+c.count("fix"), 0)

	o.Environment = seed.Production
	o.Force = true
	_, err = all.Apply(ctx, d, o)
	eq(t, errors.Is(err, seed.ErrRefused), true)
	rep = must(refs.Apply(ctx, d, o))(t)
	eq(t, result(t, rep, "ref").Ran, true)
	eq(t, rep.Environment, seed.Production)

	fresh := newDB(t)
	o = opts(nil)
	o.Environment = seed.Production
	_, err = fixtures.Apply(ctx, fresh, o)
	eq(t, errors.Is(err, seed.ErrRefused), true)
	eq(t, must(seed.Environment(ctx, fresh))(t), seed.Production)

	marked := newDB(t)
	rep = must(refs.Apply(ctx, marked, opts(nil)))(t)
	eq(t, rep.Environment, seed.Test)
	eq(t, must(seed.Environment(ctx, marked))(t), seed.Test)

	eq(t, must(seed.ParseEnv("staging"))(t), seed.Staging)
	_, err = seed.ParseEnv("prod")
	eq(t, err != nil, true)
}

func TestTypedKeys(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	plantKey := seed.Key[*fleet.Plant]("plant")
	countKey := seed.Key[int]("count")
	missing := seed.Key[string]("missing")
	var got *fleet.Plant
	var gotCount int
	var missingFound bool
	var envSeen seed.Env
	g := seed.NewRegistry()
	maker := g.Add(seed.Seed{Name: "maker", Kind: seed.Fixture, Run: func(ctx context.Context, r *seed.Run) error {
		p := &fleet.Plant{Code: "KEY", Name: "keyed"}
		if err := fleetdb.Plants.Create(ctx, r.DB, p); err != nil {
			return err
		}
		seed.Put(r, plantKey, p)
		seed.Put(r, countKey, 7)
		return nil
	}})
	user := g.Add(seed.Seed{Name: "user", Kind: seed.Fixture, After: []*seed.Seed{maker},
		Run: func(_ context.Context, r *seed.Run) error {
			got = seed.Must(r, plantKey)
			gotCount = seed.Must(r, countKey)
			_, missingFound = seed.Get(r, missing)
			envSeen = r.Environment
			return nil
		}})
	must(g.Apply(ctx, d, opts(nil)))(t)
	eq(t, got.Name, "keyed")
	eq(t, gotCount, 7)
	eq(t, missingFound, false)
	eq(t, envSeen, seed.Test)

	o := opts(nil)
	o.Only = []string{user.Name}
	o.Force = true
	rep, err := g.Apply(ctx, d, o)
	if err == nil || !strings.Contains(err.Error(), `seed: user needs key "plant", which no seed put in this run`) {
		t.Fatalf("got %v", err)
	}
	eq(t, result(t, rep, "maker").Reason, seed.FixtureApplied)

	wrong := seed.NewRegistry()
	w := wrong.Add(seed.Seed{Name: "put", Kind: seed.Fixture, Run: func(_ context.Context, r *seed.Run) error {
		seed.Put(r, seed.Key[int]("shared"), 1)
		return nil
	}})
	wrong.Add(seed.Seed{Name: "get", Kind: seed.Fixture, After: []*seed.Seed{w}, Run: func(_ context.Context, r *seed.Run) error {
		seed.Must(r, seed.Key[string]("shared"))
		return nil
	}})
	_, err = wrong.Apply(ctx, newDB(t), opts(nil))
	if err == nil || !strings.Contains(err.Error(), `reads key "shared" as string, but it holds a int`) {
		t.Fatalf("got %v", err)
	}
}

func TestDeterministicRand(t *testing.T) {
	ctx := context.Background()
	draw := func(d *keel.DB, name string, tenants ...string) map[string][]uint64 {
		var mu sync.Mutex
		out := map[string][]uint64{}
		g := seed.NewRegistry()
		g.Add(seed.Seed{Name: name, Kind: seed.Fixture, PerTenant: len(tenants) > 0,
			Run: func(_ context.Context, r *seed.Run) error {
				var v []uint64
				for range 4 {
					v = append(v, r.Rand().Uint64())
				}
				mu.Lock()
				out[r.Tenant.Key] = v
				mu.Unlock()
				return nil
			}})
		o := opts(nil)
		o.Tenants = tenantList(tenants...)
		must(g.Apply(ctx, d, o))(t)
		return out
	}
	a := draw(newDB(t), "volume")
	b := draw(newDB(t), "volume")
	eq(t, fmt.Sprint(a), fmt.Sprint(b))
	other := draw(newDB(t), "volume2")
	eq(t, fmt.Sprint(a) != fmt.Sprint(other), true)

	ta := draw(newDB(t), "volume", "x", "y")
	tb := draw(newDB(t), "volume", "x", "y")
	eq(t, fmt.Sprint(ta), fmt.Sprint(tb))
	eq(t, fmt.Sprint(ta["x"]) != fmt.Sprint(ta["y"]), true)
	eq(t, fmt.Sprint(ta["x"]) != fmt.Sprint(a[""]), true)
}

func TestCSVStreamsIntoCopy(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	var b strings.Builder
	b.WriteString("code,name\n")
	for i := range 2500 {
		fmt.Fprintf(&b, "P%04d,Plant %d\n", i, i)
	}
	fsys := fstest.MapFS{"data/plants.csv": {Data: []byte(b.String())}}
	batches := 0
	g := seed.NewRegistry()
	g.Add(seed.Seed{Name: "plants", Kind: seed.Reference, Inputs: seed.Files(fsys, "data/plants.csv"),
		Run: func(ctx context.Context, r *seed.Run) error {
			batch := make([]*fleet.Plant, 0, 1000)
			flush := func() error {
				n, err := fleetdb.Plants.Copy(ctx, r.DB, batch)
				r.Count(n)
				batch = batch[:0]
				batches++
				return err
			}
			err := seed.CSVEach(r, "data/plants.csv", func(p fleet.Plant) error {
				batch = append(batch, &p)
				if len(batch) == cap(batch) {
					return flush()
				}
				return nil
			})
			if err == nil && len(batch) > 0 {
				err = flush()
			}
			return err
		}})
	rep := must(g.Apply(ctx, d, opts(nil)))(t)
	eq(t, result(t, rep, "plants").Rows, int64(2500))
	eq(t, batches, 3)
	eq(t, must(fleetdb.Plants.Query().Count(ctx, d))(t), int64(2500))
	eq(t, must(fleetdb.Plants.Get(ctx, d, "P2499"))(t).Name, "Plant 2499")
}

func TestOnlyForceDryRun(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	c := newCounter()
	g := seed.NewRegistry()
	a := g.Add(seed.Seed{Name: "a", Kind: seed.Reference, Run: c.run("a")})
	b := g.Add(seed.Seed{Name: "b", Kind: seed.Reference, After: []*seed.Seed{a}, Run: c.run("b")})
	g.Add(seed.Seed{Name: "c", Kind: seed.Reference, Run: c.run("c")})

	o := opts(nil)
	o.DryRun = true
	rec := &recorder{}
	o.Observer = rec.observe
	rep := must(g.Apply(ctx, d, o))(t)
	for _, res := range rep.Results {
		eq(t, res.Reason, seed.New)
		eq(t, res.Ran, false)
	}
	eq(t, len(c.order()), 0)
	eq(t, scalar[bool](t, d, "SELECT to_regclass('keel_seeds') IS NULL"), true)
	eq(t, len(rec.of(seed.Planned)), 1)
	eq(t, len(rec.list()), 2)

	o = opts(nil)
	o.Only = []string{b.Name}
	rep = must(g.Apply(ctx, d, o))(t)
	eq(t, strings.Join(c.order(), ","), "a,b")
	eq(t, result(t, rep, "c").Reason, seed.Excluded)

	o.Force = true
	rep = must(g.Apply(ctx, d, o))(t)
	eq(t, result(t, rep, "a").Reason, seed.UpToDate)
	eq(t, result(t, rep, "b").Reason, seed.Forced)
	eq(t, strings.Join(c.order(), ","), "a,b,b")

	o.Only = []string{"nope"}
	_, err := g.Apply(ctx, d, o)
	if err == nil || !strings.Contains(err.Error(), `no seed named "nope"`) {
		t.Fatalf("got %v", err)
	}

	o = opts(nil)
	o.Force = true
	o.DryRun = true
	rep = must(g.Apply(ctx, d, o))(t)
	eq(t, result(t, rep, "c").Reason, seed.New)
	eq(t, result(t, rep, "a").Reason, seed.Forced)
	eq(t, len(c.order()), 3)
}

func TestStatusAndPrune(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	fsys := fstest.MapFS{"data/plants.yaml": {Data: []byte("- code: S\n  name: Status\n")}}
	kept := seed.NewRegistry()
	kept.Add(seed.Seed{Name: "ref", Kind: seed.Reference, Inputs: seed.Files(fsys, "data/plants.yaml"),
		Run: upsertPlants("data/plants.yaml")})
	fix := kept.Add(seed.Seed{Name: "fix", Kind: seed.Fixture, Run: noop})
	per := kept.Add(seed.Seed{Name: "per", Kind: seed.Reference, PerTenant: true, Run: noop})
	kept.Add(seed.Seed{Name: "pending", Kind: seed.Reference, Run: noop})
	removed := seed.NewRegistry()
	removed.Add(seed.Seed{Name: "gone", Kind: seed.Reference, Run: noop})
	removed.Add(seed.Seed{Name: "half", Kind: seed.Reference, PerTenant: true,
		Run: func(_ context.Context, r *seed.Run) error {
			if r.Tenant.Key == "t2" {
				return errors.New("no")
			}
			return nil
		}})
	o := opts(nil)
	o.Tenants = tenantList("t1", "t2")
	o.Only = []string{"gone", "ref", "fix", "per"}
	must(seed.Merge(kept, removed).Apply(ctx, d, o))(t)
	o.Only = []string{"half"}
	_, err := removed.Apply(ctx, d, o)
	eq(t, err != nil, true)

	fix.Version = "2"
	fsys["data/plants.yaml"] = &fstest.MapFile{Data: []byte("- code: S\n  name: Changed\n")}
	status := must(kept.Status(ctx, d))(t)
	got := map[string]seed.Status{}
	var order []string
	for _, s := range status {
		got[s.Name] = s
		order = append(order, s.Name)
	}
	eq(t, strings.Join(order, ","), "ref,fix,per,pending,gone,half")
	eq(t, got["ref"].State, seed.Changed)
	eq(t, got["ref"].Reason, seed.InputChanged)
	eq(t, strings.Join(got["ref"].Changed, ","), "data/plants.yaml")
	eq(t, got["ref"].Rows, int64(1))
	eq(t, got["ref"].AppliedAt.IsZero(), false)
	eq(t, got["fix"].State, seed.Applied)
	eq(t, got["fix"].Reason, seed.FixtureApplied)
	eq(t, got["per"].State, seed.Applied)
	eq(t, got["per"].Tenants, 2)
	eq(t, got["pending"].State, seed.Pending)
	eq(t, got["pending"].Reason, seed.New)
	eq(t, got["gone"].State, seed.Removed)
	eq(t, got["gone"].Kind, seed.Reference)
	eq(t, got["gone"].Seed == nil, true)
	eq(t, got["half"].State, seed.Removed)
	eq(t, got["half"].Tenants, 1)

	per.Version = "2"
	status = must(kept.Status(ctx, d))(t)
	eq(t, status[2].State, seed.Changed)
	eq(t, status[2].Reason, seed.VersionChanged)
	eq(t, status[2].Tenants, 0)

	pruned := must(kept.Prune(ctx, d))(t)
	eq(t, strings.Join(pruned, ","), "gone,half")
	eq(t, len(must(kept.Status(ctx, d))(t)), 4)
	eq(t, len(must(kept.Prune(ctx, d))(t)), 0)
	eq(t, scalar[int64](t, d, "SELECT count(*) FROM keel_seed_tenants WHERE name = 'half'"), int64(0))
	eq(t, scalar[int64](t, d, "SELECT count(*) FROM keel_seed_tenants WHERE name = 'per'"), int64(2))
}

func TestEventSequence(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	g := seed.NewRegistry()
	first := g.Add(seed.Seed{Name: "first", Kind: seed.Reference, Run: func(ctx context.Context, r *seed.Run) error {
		if _, err := fleetdb.Customers.AllTenants().Count(ctx, r.DB); err != nil {
			return err
		}
		r.Count(3)
		return nil
	}})
	g.Add(seed.Seed{Name: "skipped", Kind: seed.Fixture, Run: noop})
	g.Add(seed.Seed{Name: "tenants", Kind: seed.Reference, PerTenant: true, After: []*seed.Seed{first},
		Run: func(_ context.Context, r *seed.Run) error {
			r.Count(2)
			return nil
		}})
	o := opts(nil)
	o.Tenants = tenantList("t1", "t2")
	o.Only = []string{"tenants"}
	rec := &recorder{}
	o.Observer = rec.observe
	rep := must(g.Apply(ctx, d, o))(t)

	type step struct {
		kind               seed.EventKind
		seed, tenant       string
		index, total       int
		tenants, done      int
		rows               int64
		reason             seed.Reason
		hasPlan, hasHolder bool
	}
	var got []step
	for _, e := range rec.list() {
		s := step{kind: e.Kind, tenant: e.Tenant, index: e.Index, total: e.Total, tenants: e.Tenants,
			done: e.TenantsDone, rows: e.Rows, reason: e.Reason, hasPlan: e.Plan != nil, hasHolder: e.Holder != nil}
		if e.Seed != nil {
			s.seed = e.Seed.Name
		}
		if e.Err != nil {
			t.Fatalf("event %v failed: %v", e.Kind, e.Err)
		}
		got = append(got, s)
	}
	want := []step{
		{kind: seed.LockAcquired},
		{kind: seed.Planned, hasPlan: true},
		{kind: seed.SeedStart, seed: "first", index: 0, total: 2, reason: seed.New},
		{kind: seed.SeedEnd, seed: "first", index: 0, total: 2, rows: 3, reason: seed.New},
		{kind: seed.SeedStart, seed: "tenants", index: 1, total: 2, tenants: 2, reason: seed.New},
		{kind: seed.TenantStart, seed: "tenants", tenant: "t1", index: 1, total: 2, tenants: 2, reason: seed.New},
		{kind: seed.TenantEnd, seed: "tenants", tenant: "t1", index: 1, total: 2, tenants: 2, done: 1, rows: 2,
			reason: seed.New},
		{kind: seed.TenantStart, seed: "tenants", tenant: "t2", index: 1, total: 2, tenants: 2, done: 1,
			reason: seed.New},
		{kind: seed.TenantEnd, seed: "tenants", tenant: "t2", index: 1, total: 2, tenants: 2, done: 2, rows: 2,
			reason: seed.New},
		{kind: seed.SeedEnd, seed: "tenants", index: 1, total: 2, tenants: 2, done: 2, rows: 4, reason: seed.New},
		{kind: seed.RunEnd},
	}
	if len(got) != len(want) {
		t.Fatalf("events:\n%+v\nwant:\n%+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	planned := rec.of(seed.Planned)[0].Plan
	eq(t, len(planned.Seeds), 3)
	eq(t, item(t, *planned, "skipped").Reason, seed.Excluded)
	events := rec.list()
	eq(t, events[len(events)-1].Duration, rep.Duration)
	for _, e := range rec.of(seed.SeedEnd) {
		if e.Duration <= 0 {
			t.Fatalf("SeedEnd without a duration: %+v", e)
		}
	}
	eq(t, result(t, rep, "tenants").Tenants, 2)
	eq(t, result(t, rep, "tenants").Rows, int64(4))
	eq(t, result(t, rep, "skipped").Ran, false)
}
