package analytics_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
)

// explainCounter counts the EXPLAIN statements a DB sends.
type explainCounter struct{ n atomic.Int64 }

func (c *explainCounter) BeforeQuery(ctx context.Context, e *keel.QueryEvent) context.Context {
	if e.Op == "explain" {
		c.n.Add(1)
	}
	return ctx
}

func (c *explainCounter) AfterQuery(context.Context, *keel.QueryEvent) {}

func TestCostGuardRefusesAndCachesThePlan(t *testing.T) {
	w := seedWorld(t)
	counter := &explainCounter{}
	cfg := testConfig(testDSN)
	cfg.Observers = []keel.Observer{counter}
	d, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cat := catalog(t)
	report := def(t, manyMeasures)
	cheap := must[*analytics.Runner](t)(analytics.NewRunner(d, cat, analytics.RunnerOptions{MaxCost: 1e9, MaxPlanRows: 1e9}))
	for range 3 {
		if _, err := cheap.Preview(w.ctx, analytics.Request{Definition: report}); err != nil {
			t.Fatal(err)
		}
	}
	if counter.n.Load() != 1 {
		t.Fatalf("%d EXPLAINs for one statement shape", counter.n.Load())
	}
	costly := must[*analytics.Runner](t)(analytics.NewRunner(d, cat, analytics.RunnerOptions{MaxCost: 1}))
	if _, err := costly.Preview(w.ctx, analytics.Request{Definition: report}); analytics.ErrorCode(err) != analytics.CodeTooCostly {
		t.Fatalf("cost: %v", err)
	}
	wide := must[*analytics.Runner](t)(analytics.NewRunner(d, cat, analytics.RunnerOptions{MaxPlanRows: 0.5}))
	if _, err := wide.Count(w.ctx, analytics.Request{Definition: report}); analytics.ErrorCode(err) != analytics.CodeTooCostly {
		t.Fatalf("rows: %v", err)
	}
}

func TestComplexityGuard(t *testing.T) {
	w := seedWorld(t)
	r := runner(t, analytics.RunnerOptions{Complexity: analytics.ComplexityLimits{MaxPreAggs: 1}})
	_, err := r.Preview(w.ctx, analytics.Request{Definition: def(t, manyMeasures)})
	if analytics.ErrorCode(err) != analytics.CodeTooComplex {
		t.Fatalf("complexity: %v", err)
	}
	comp := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, catalog(t), nil, def(t, manyMeasures), nil, analytics.CompileOptions{}))
	if c := comp.Complexity; c.Joins != 1 || c.ToMany != 2 || c.PreAggs != 2 || c.Dimensions != 1 || c.Statements != 1 {
		t.Fatalf("complexity %+v", c)
	}
}

// holdSlot takes a tenant's advisory lock slot on its own connection until release is called.
func holdSlot(t *testing.T, w *world, slot int32) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := db(t).Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1, $2)",
		analytics.TenantLock(db(t).TenantKey(w.ctx)), slot).Scan(&got); err != nil || !got {
		t.Fatalf("hold slot: %v %v", got, err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			conn.Release()
		})
	}
	t.Cleanup(release)
	return release
}

func TestSemaphoreLimitsATenant(t *testing.T) {
	w := seedWorld(t)
	other := seedRows(t, fixture[:1])
	release := holdSlot(t, w, 0)
	r := runner(t, analytics.RunnerOptions{Slots: 1})
	report := def(t, lanes)
	if _, err := r.Preview(w.ctx, analytics.Request{Definition: report}); analytics.ErrorCode(err) != analytics.CodeBusy {
		t.Fatalf("saturated: %v", err)
	}
	if _, err := r.Preview(other.ctx, analytics.Request{Definition: report}); err != nil {
		t.Fatalf("another tenant: %v", err)
	}
	waiting := runner(t, analytics.RunnerOptions{Slots: 1, SlotWait: 5 * time.Second})
	done := make(chan error, 1)
	go func() {
		_, err := waiting.Preview(w.ctx, analytics.Request{Definition: report})
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	release()
	if err := <-done; err != nil {
		t.Fatalf("waited for a slot: %v", err)
	}
	two := runner(t, analytics.RunnerOptions{Slots: 2})
	holdSlot(t, w, 1)
	if _, err := two.Preview(w.ctx, analytics.Request{Definition: report}); err != nil {
		t.Fatalf("a free slot: %v", err)
	}
}

func TestSupersedeCancelsThePreviousRun(t *testing.T) {
	w := seedWorld(t)
	report := def(t, lanes)
	release := holdSlot(t, w, 0)
	r := runner(t, analytics.RunnerOptions{Slots: 1, SlotWait: 10 * time.Second})
	first := make(chan error, 1)
	go func() {
		_, err := r.Preview(w.ctx, analytics.Request{Definition: report, Key: "user-1/preview"})
		first <- err
	}()
	time.Sleep(150 * time.Millisecond)
	second := make(chan error, 1)
	go func() {
		_, err := r.Preview(w.ctx, analytics.Request{Definition: report, Key: "user-1/preview"})
		second <- err
	}()
	if err := <-first; analytics.ErrorCode(err) != analytics.CodeSuperseded {
		t.Fatalf("the first run: %v", err)
	}
	release()
	if err := <-second; err != nil {
		t.Fatalf("the second run: %v", err)
	}
	conn, err := pgx.Connect(context.Background(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tag := analytics.SupersedeTag(db(t).TenantKey(w.ctx), "user-1/preview")
	if _, err := conn.Exec(context.Background(), "SELECT set_config('application_name', $1, false)", tag); err != nil {
		t.Fatal(err)
	}
	sleeping := make(chan error, 1)
	go func() {
		_, err := conn.Exec(context.Background(), "SELECT pg_sleep(30)")
		sleeping <- err
	}()
	time.Sleep(150 * time.Millisecond)
	other := runner(t, analytics.RunnerOptions{})
	if _, err := other.Preview(w.ctx, analytics.Request{Definition: report, Key: "user-1/preview"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-sleeping:
		if keel.Code(err) != "57014" {
			t.Fatalf("the other process's statement: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the other process's statement was not cancelled")
	}
}

func TestStatementTimeout(t *testing.T) {
	w := seedWorld(t)
	r := runner(t, analytics.RunnerOptions{})
	comp := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, catalog(t), nil, def(t, lanes), nil, analytics.CompileOptions{}))
	start := time.Now()
	err := analytics.SleepThroughRunner(w.ctx, r, comp, 5, 200*time.Millisecond)
	if analytics.ErrorCode(err) != analytics.CodeTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout after %v: %v", time.Since(start), err)
	}
	var ae *analytics.Error
	if !errors.As(err, &ae) || keel.Code(ae.Err) != "57014" {
		t.Fatalf("the server did not stop the statement: %v", err)
	}
}
