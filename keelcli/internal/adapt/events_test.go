package adapt

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/seed"
)

func describeMigration(e migrateport.Event) string {
	id := ""
	if e.Migration != nil {
		id = " " + e.Migration.ID()
	}
	return fmt.Sprintf("%s%s i=%d/%d stmt=%d/%d attempt=%d d=%s err=%v", e.Kind, id, e.Index, e.Total, e.Statement,
		e.Statements, e.Attempt, e.Duration, e.Err != nil)
}

func TestMigrationEventsFollowThePort(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	base := &migrate.Migration{Version: "20260101000000", Name: "baseline", Kind: migrate.KindBaseline}
	a := &migrate.Migration{Version: "20260102000000", Name: "a"}
	b := &migrate.Migration{Version: "20260103000000", Name: "b"}
	timeout := errors.New("lock timeout")
	var got []string
	tr := newMigrationEvents(func(e migrateport.Event) { got = append(got, describeMigration(e)) }, 2, clock)
	for _, e := range []migrate.Event{
		{Kind: migrate.LockWait, Duration: time.Millisecond},
		{Kind: migrate.LockWait, Duration: time.Second},
		{Kind: migrate.LockAcquired, Duration: 1100 * time.Millisecond},
		{Kind: migrate.BaselineMarked, Migration: base, Index: 0, Total: 3},
		{Kind: migrate.MigrationStart, Migration: a, Index: 1, Total: 3},
		{Kind: migrate.Retry, Migration: a, Index: 1, Total: 3, Attempt: 2, Err: timeout},
		{Kind: migrate.MigrationEnd, Migration: a, Index: 1, Total: 3, Duration: time.Second},
		{Kind: migrate.MigrationStart, Migration: b, Index: 2, Total: 3},
		{Kind: migrate.After, Index: 2, Total: 3, SQL: "SELECT 1"},
		{Kind: migrate.After, Index: 2, Total: 3, SQL: "SELECT 2"},
		{Kind: migrate.Retry, Migration: b, Index: 2, Total: 3, Attempt: 2, Err: timeout},
		{Kind: migrate.After, Index: 2, Total: 3, SQL: "SELECT 1"},
		{Kind: migrate.After, Index: 2, Total: 3, SQL: "SELECT 2"},
		{Kind: migrate.MigrationEnd, Migration: b, Index: 2, Total: 3, Duration: time.Second},
		{Kind: migrate.RunEnd, Total: 3, Duration: 3 * time.Second},
	} {
		if e.Kind == migrate.After {
			now = now.Add(5 * time.Millisecond)
		}
		tr.event(e)
	}
	want := []string{
		"lock_wait i=0/0 stmt=0/0 attempt=1 d=1ms err=false",
		"lock_wait i=0/0 stmt=0/0 attempt=2 d=1s err=false",
		"lock_acquired i=0/0 stmt=0/0 attempt=0 d=1.1s err=false",
		"baseline_marked 20260101000000_baseline i=0/0 stmt=0/0 attempt=0 d=0s err=false",
		"migration_start 20260102000000_a i=1/2 stmt=0/0 attempt=0 d=0s err=false",
		"retry 20260102000000_a i=1/2 stmt=0/0 attempt=1 d=0s err=true",
		"migration_end 20260102000000_a i=1/2 stmt=0/0 attempt=0 d=1s err=false",
		"migration_start 20260103000000_b i=2/2 stmt=0/0 attempt=0 d=0s err=false",
		"after i=0/0 stmt=1/2 attempt=0 d=5ms err=false",
		"retry 20260103000000_b i=2/2 stmt=0/0 attempt=1 d=0s err=true",
		"after i=0/0 stmt=1/2 attempt=0 d=5ms err=false",
		"after i=0/0 stmt=2/2 attempt=0 d=0s err=false",
		"migration_end 20260103000000_b i=2/2 stmt=0/0 attempt=0 d=1s err=false",
		"run_end i=0/2 stmt=0/0 attempt=0 d=3s err=false",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestMigrationEventsWithoutAWait(t *testing.T) {
	var got []migrateport.Event
	tr := newMigrationEvents(func(e migrateport.Event) { got = append(got, e) }, 1, time.Now)
	a := &migrate.Migration{Version: "20260102000000", Name: "a"}
	tr.event(migrate.Event{Kind: migrate.LockAcquired, Duration: 3 * time.Millisecond})
	tr.event(migrate.Event{Kind: migrate.MigrationStart, Migration: a, Total: 1})
	tr.event(migrate.Event{Kind: migrate.After, Total: 1, SQL: "SELECT 1"})
	tr.event(migrate.Event{Kind: migrate.MigrationEnd, Migration: a, Total: 1, Err: errors.New("after failed")})
	tr.event(migrate.Event{Kind: migrate.Retry, Attempt: 2, Err: errors.New("lock timeout")})
	tr.event(migrate.Event{Kind: migrate.RunEnd, Total: 1, Err: errors.New("failed")})
	kinds := make([]string, len(got))
	for i, e := range got {
		kinds[i] = e.Kind.String()
	}
	if strings.Join(kinds, ",") != "lock_acquired,migration_start,migration_end,retry,run_end" {
		t.Fatalf("kinds %v", kinds)
	}
	if got[0].Duration != 0 || got[3].Migration != nil || got[3].Attempt != 1 || got[4].Total != 0 {
		t.Fatalf("events %+v", got)
	}
}

func TestSeedEventsFollowThePort(t *testing.T) {
	ref := &seed.Seed{Name: "states", Kind: seed.Reference}
	per := &seed.Seed{Name: "settings", Kind: seed.Reference, PerTenant: true, After: []*seed.Seed{ref}}
	fix := &seed.Seed{Name: "demo", Kind: seed.Fixture}
	var got []string
	tr := newSeedEvents(func(e seedport.Event) {
		name := ""
		if e.Seed != nil {
			name = " " + e.Seed.Name
		}
		got = append(got, fmt.Sprintf("%s%s i=%d/%d tenants=%d/%d rows=%d reason=%s d=%s err=%v", e.Kind, name, e.Index, e.Total,
			e.TenantsDone, e.Tenants, e.Rows, e.Reason.Kind, e.Duration, e.Err != nil))
	})
	plan := &seed.Plan{Seeds: []seed.PlanItem{
		{Seed: ref, Reason: seed.InputChanged, Run: true, Changed: []string{"states.yaml"}},
		{Seed: per, Reason: seed.New, Run: true},
		{Seed: fix, Reason: seed.FixtureApplied},
	}}
	for _, e := range []seed.Event{
		{Kind: seed.LockAcquired, Duration: 2 * time.Millisecond},
		{Kind: seed.Planned, Plan: plan},
		{Kind: seed.SeedStart, Seed: ref, Reason: seed.InputChanged, Index: 0, Total: 2},
		{Kind: seed.SeedEnd, Seed: ref, Index: 0, Total: 2, Rows: 3, Duration: time.Second},
		{Kind: seed.SeedStart, Seed: per, Reason: seed.New, Index: 1, Total: 2, Tenants: 2},
		{Kind: seed.TenantStart, Seed: per, Index: 1, Total: 2, Tenant: "t1", Tenants: 2},
		{Kind: seed.TenantEnd, Seed: per, Index: 1, Total: 2, Tenant: "t1", Tenants: 2, TenantsDone: 1, Rows: 4},
		{Kind: seed.TenantStart, Seed: per, Index: 1, Total: 2, Tenant: "t2", Tenants: 2, TenantsDone: 1},
		{Kind: seed.TenantEnd, Seed: per, Index: 1, Total: 2, Tenant: "t2", Tenants: 2, TenantsDone: 2, Err: errors.New("boom")},
		{Kind: seed.SeedEnd, Seed: per, Index: 1, Total: 2, Tenants: 2, Rows: 4, Err: errors.New("boom")},
		{Kind: seed.RunEnd, Duration: 2 * time.Second, Err: errors.New("boom")},
	} {
		tr.event(e)
	}
	want := []string{
		"lock_acquired i=0/0 tenants=0/0 rows=0 reason=new d=0s err=false",
		"planned states i=1/3 tenants=0/0 rows=0 reason=input changed d=0s err=false",
		"planned settings i=2/3 tenants=0/0 rows=0 reason=new d=0s err=false",
		"planned demo i=3/3 tenants=0/0 rows=0 reason=fixture applied d=0s err=false",
		"seed_start states i=1/2 tenants=0/0 rows=0 reason=input changed d=0s err=false",
		"seed_end states i=1/2 tenants=0/0 rows=3 reason=new d=1s err=false",
		"seed_start settings i=2/2 tenants=0/2 rows=0 reason=new d=0s err=false",
		"tenant_start settings i=2/2 tenants=0/2 rows=0 reason=new d=0s err=false",
		"tenant_end settings i=2/2 tenants=1/2 rows=4 reason=new d=0s err=false",
		"tenant_start settings i=2/2 tenants=1/2 rows=4 reason=new d=0s err=false",
		"tenant_end settings i=2/2 tenants=1/2 rows=4 reason=new d=0s err=true",
		"seed_end settings i=2/2 tenants=0/2 rows=4 reason=new d=0s err=true",
		"run_end i=0/1 tenants=0/0 rows=0 reason=new d=2s err=true",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSeedRunEndCountsTheSeedsApplied(t *testing.T) {
	s := &seed.Seed{Name: "states", Kind: seed.Reference}
	var last seedport.Event
	tr := newSeedEvents(func(e seedport.Event) { last = e })
	tr.event(seed.Event{Kind: seed.LockWait, Duration: time.Second})
	tr.event(seed.Event{Kind: seed.LockAcquired, Duration: 1500 * time.Millisecond})
	if last.Duration != 1500*time.Millisecond {
		t.Fatalf("lock acquired after a wait: %+v", last)
	}
	tr.event(seed.Event{Kind: seed.SeedEnd, Seed: s, Total: 1})
	tr.event(seed.Event{Kind: seed.RunEnd})
	if last.Kind != seedport.RunEnd || last.Total != 1 {
		t.Fatalf("run end %+v", last)
	}
}
