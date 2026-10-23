package fake

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

var t0 = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

func newFake(opts Options) *Engine {
	opts.Now = func() time.Time { return t0 }
	return New(opts)
}

type recorder[E any] struct {
	mu     sync.Mutex
	events []E
}

func (r *recorder[E]) add(e E) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func kinds(es []migrateport.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Kind.String()
	}
	return out
}

func TestUpEmitsTheScriptedRun(t *testing.T) {
	e := newFake(Options{})
	var r recorder[migrateport.Event]
	if err := e.Migrations().Up(context.Background(), migrateport.UpOptions{}, r.add); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(kinds(r.events), " ")
	want := "lock_wait lock_wait lock_wait lock_acquired " +
		"migration_start statement statement migration_end " +
		"migration_start retry retry migration_end " +
		"migration_start migration_end migration_start migration_end after run_end"
	if got != want {
		t.Fatalf("events:\n%s\nwant:\n%s", got, want)
	}
	last := r.events[len(r.events)-1]
	if last.Total != 4 || last.Err != nil {
		t.Fatalf("run end %+v", last)
	}
	for _, ev := range r.events {
		if ev.Kind == migrateport.Retry && (len(ev.Blockers) == 0 || ev.Blockers[0].PID != 48121) {
			t.Fatalf("retry without blockers: %+v", ev)
		}
	}
	rows, _ := e.Migrations().Status(context.Background())
	for _, s := range rows {
		if s.State != migrateport.Applied {
			t.Fatalf("%s is %s after up", s.Migration.ID(), s.State)
		}
	}
	plan, _ := e.Migrations().Plan(context.Background(), migrateport.PlanOptions{})
	if !plan.Empty() {
		t.Fatalf("plan after up: %+v", plan)
	}
}

func TestUpTarget(t *testing.T) {
	e := newFake(Options{NoLockWait: true})
	var r recorder[migrateport.Event]
	if err := e.Migrations().Up(context.Background(), migrateport.UpOptions{To: "20261025120000"}, r.add); err != nil {
		t.Fatal(err)
	}
	if last := r.events[len(r.events)-1]; last.Total != 2 {
		t.Fatalf("applied %d, want 2", last.Total)
	}
	if _, err := e.Migrations().Plan(context.Background(), migrateport.PlanOptions{To: "1999"}); err == nil {
		t.Fatal("unknown target accepted")
	}
}

func TestUpFailureAndDrift(t *testing.T) {
	e := newFake(Options{FailMigration: true, NoLockWait: true})
	var r recorder[migrateport.Event]
	err := e.Migrations().Up(context.Background(), migrateport.UpOptions{}, r.add)
	if err == nil || !strings.Contains(err.Error(), "not-null") {
		t.Fatalf("err %v", err)
	}
	if last := r.events[len(r.events)-1]; last.Kind != migrateport.RunEnd || last.Total != 2 || last.Err == nil {
		t.Fatalf("run end %+v", last)
	}

	d := newFake(Options{Drift: true})
	if err := d.Migrations().Up(context.Background(), migrateport.UpOptions{}, func(migrateport.Event) {}); err == nil {
		t.Fatal("changed migration did not block up")
	}
	ps, _ := d.Migrations().Check(context.Background(), migrateport.CheckOptions{})
	if len(ps) != 2 {
		t.Fatalf("problems %+v", ps)
	}
	rep, err := d.Migrations().Repair(context.Background(), "20261024150000")
	if err != nil || rep.OldChecksum == rep.NewChecksum {
		t.Fatalf("repair %+v %v", rep, err)
	}
	if _, err := d.Migrations().Repair(context.Background(), "20261024150000"); err == nil {
		t.Fatal("repaired twice")
	}
}

func TestUpCancels(t *testing.T) {
	e := newFake(Options{Speed: 1, NoLockWait: true})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := e.Migrations().Up(ctx, migrateport.UpOptions{}, func(migrateport.Event) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

func TestSeedsResumeAfterFailure(t *testing.T) {
	e := newFake(Options{FailSeed: true, Tenants: 200})
	var r recorder[seedport.Event]
	err := e.Seeds().Apply(context.Background(), seedport.ApplyOptions{}, r.add)
	if err == nil || !strings.Contains(err.Error(), "org_00095") {
		t.Fatalf("err %v", err)
	}
	st, _ := e.Seeds().Status(context.Background())
	i := slices.IndexFunc(st, func(s seedport.Status) bool { return s.Seed.Name == "organization_role_permissions" })
	if st[i].State != seedport.Partial || st[i].Tenants != 94 {
		t.Fatalf("status %+v", st[i])
	}
	var again recorder[seedport.Event]
	if err := e.Seeds().Apply(context.Background(), seedport.ApplyOptions{}, again.add); err != nil {
		t.Fatal(err)
	}
	first := -1
	for _, ev := range again.events {
		if ev.Kind == seedport.TenantStart && first < 0 {
			first = ev.TenantsDone
		}
	}
	if first != 94 {
		t.Fatalf("resumed at %d, want 94", first)
	}
	plan, _ := e.Seeds().Plan(context.Background(), seedport.PlanOptions{})
	for _, p := range plan {
		if p.Reason.Runs() {
			t.Fatalf("%s still runs: %s", p.Seed.Name, p.Reason.Kind)
		}
	}
}

func TestSeedPlanSelectionAndProduction(t *testing.T) {
	e := newFake(Options{Environment: seedport.Production})
	plan, err := e.Seeds().Plan(context.Background(), seedport.PlanOptions{Only: []string{"shipments"}, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]seedport.ReasonKind{}
	for _, p := range plan {
		reasons[p.Seed.Name] = p.Reason.Kind
	}
	if reasons["shipments"] != seedport.Refused || reasons["us_states"] != seedport.UpToDate || reasons["hazmat_references"] != seedport.Excluded {
		t.Fatalf("reasons %v", reasons)
	}
	if _, err := e.Seeds().Plan(context.Background(), seedport.PlanOptions{Only: []string{"nope"}}); err == nil {
		t.Fatal("unknown seed accepted")
	}
}

func TestTemplatesAndReset(t *testing.T) {
	e := newFake(Options{})
	m := e.Migrations()
	dropped, _ := m.PruneTemplates(context.Background())
	if len(dropped) != 2 {
		t.Fatalf("dropped %d", len(dropped))
	}
	r, err := m.Reset(context.Background())
	if err != nil || r.Built || r.Template != "keel_tpl_9f2c41d0" {
		t.Fatalf("reset %+v %v", r, err)
	}
	st, _ := e.Seeds().Status(context.Background())
	for _, s := range st {
		if s.Seed.Kind == seedport.Fixture && s.State != seedport.Due {
			t.Fatalf("fixture %s survived the reset", s.Seed.Name)
		}
	}
}

func TestCreate(t *testing.T) {
	m := newFake(Options{}).Migrations()
	c, err := m.Create(context.Background(), migrateport.CreateRequest{Name: "add_holds", Kind: migrateport.NoTx})
	if err != nil || c.Path != "schema/migrations/20261027093000_add_holds.sql" || !c.Migration.Directives.NoTx {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := m.Create(context.Background(), migrateport.CreateRequest{Name: "Bad Name"}); err == nil {
		t.Fatal("bad name accepted")
	}
	r, _ := m.Create(context.Background(), migrateport.CreateRequest{Name: "grants", Kind: migrateport.RepeatableFile})
	if r.Path != "schema/migrations/R_grants.sql" {
		t.Fatalf("path %s", r.Path)
	}
	s, _ := m.Stamp(context.Background(), migrateport.StampOptions{Check: true})
	if s.Current {
		t.Fatal("stamp current after a new migration")
	}
}
