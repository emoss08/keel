package adapt

import (
	"sync"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/seed"
)

// migrationEvents translates migrate's events into migrateport's.
//
// migrate counts Index from 0 over every migration it marks or runs and reports RunEnd.Total as
// the size of the plan; the port counts from 1 over the migrations the run executes and reports
// RunEnd.Total as the migrations applied. migrate's Retry.Attempt is the attempt about to run; the
// port's is the attempt that failed. migrate's LockWait has no attempt and its LockAcquired always
// carries the time spent taking the lock; the port numbers the waits and reports zero when the
// lock was free. migrate sends After before each statement with its SQL; the port sends it once
// the statement finished, with its duration, so a statement is reported when the next event shows
// it succeeded.
type migrationEvents struct {
	observe migrateport.Observer
	after   int
	now     func() time.Time

	waits   int
	marks   int
	index   int
	applied int
	afterN  int
	pending *pendingAfter
}

// pendingAfter is an After statement running, reported once the next event shows it finished.
type pendingAfter struct {
	n     int
	start time.Time
}

// newMigrationEvents translates for observe; after is the number of After statements.
func newMigrationEvents(observe migrateport.Observer, after int, now func() time.Time) *migrationEvents {
	return &migrationEvents{observe: observe, after: after, now: now}
}

// event translates one migrate event.
func (t *migrationEvents) event(e migrate.Event) {
	switch e.Kind {
	case migrate.LockWait:
		t.waits++
		t.observe(migrateport.Event{Kind: migrateport.LockWait, Attempt: t.waits, Duration: e.Duration, Blockers: blockers(e.Blockers)})
	case migrate.LockAcquired:
		out := migrateport.Event{Kind: migrateport.LockAcquired}
		if t.waits > 0 {
			out.Duration = e.Duration
		}
		t.observe(out)
	case migrate.BaselineMarked:
		t.marks++
		t.observe(migrateport.Event{Kind: migrateport.BaselineMarked, Migration: portMigration(e.Migration)})
	case migrate.MigrationStart:
		t.index++
		t.observe(t.step(migrateport.MigrationStart, e))
	case migrate.Statement:
		out := t.step(migrateport.Statement, e)
		out.Statement, out.Statements = e.Statement, e.Statements
		t.observe(out)
	case migrate.Retry:
		t.retry(e)
	case migrate.MigrationEnd:
		t.end(e)
	case migrate.After:
		t.flush(true)
		t.afterN++
		t.pending = &pendingAfter{n: t.afterN, start: t.now()}
	case migrate.RunEnd:
		t.flush(e.Err == nil)
		t.observe(migrateport.Event{Kind: migrateport.RunEnd, Total: t.applied, Duration: e.Duration, Err: e.Err})
	}
}

// step is the port event of a migration step, numbered among the migrations the run executes.
func (t *migrationEvents) step(kind migrateport.EventKind, e migrate.Event) migrateport.Event {
	return migrateport.Event{Kind: kind, Migration: portMigration(e.Migration), Index: t.index, Total: e.Total - t.marks}
}

// retry reports the attempt that failed. A retry runs the After statements again, so their count
// restarts.
func (t *migrationEvents) retry(e migrate.Event) {
	t.flush(false)
	t.afterN = 0
	out := migrateport.Event{Kind: migrateport.Retry, Attempt: e.Attempt - 1, Err: e.Err, Blockers: blockers(e.Blockers)}
	if e.Migration != nil {
		out = t.step(migrateport.Retry, e)
		out.Attempt, out.Err, out.Blockers = e.Attempt-1, e.Err, blockers(e.Blockers)
		out.Statement, out.Statements = e.Statement, e.Statements
	}
	t.observe(out)
}

// end reports a migration's end, after the After statements its transaction ran.
func (t *migrationEvents) end(e migrate.Event) {
	t.flush(e.Err == nil)
	if e.Err == nil {
		t.applied++
	}
	out := t.step(migrateport.MigrationEnd, e)
	out.Duration, out.Err = e.Duration, e.Err
	t.observe(out)
}

// flush reports the running After statement when ok, the next event having shown it succeeded.
func (t *migrationEvents) flush(ok bool) {
	p := t.pending
	t.pending = nil
	if p == nil || !ok {
		return
	}
	t.observe(migrateport.Event{Kind: migrateport.After, Statement: p.n, Statements: t.after, Duration: t.now().Sub(p.start)})
}

// portMigration converts an event's migration, which is nil for After statements.
func portMigration(m *migrate.Migration) *migrateport.Migration {
	if m == nil {
		return nil
	}
	out := migration(*m)
	return &out
}

// seedEvents translates seed's events into seedport's.
//
// seed reports its plan as one Planned event once it holds the lock; the port wants one per seed,
// numbered from 1. seed counts Index from 0, reports a tenant's own rows on TenantEnd and counts a
// failed tenant as done, and leaves RunEnd.Total unset; the port counts from 1, reports the seed's
// rows so far and the tenants applied, and reports RunEnd.Total as the seeds applied. seed's
// LockAcquired carries the time spent taking the lock; the port's is zero when the lock was free.
// seed calls the observer from several goroutines when seeds run in parallel.
type seedEvents struct {
	observe seedport.Observer

	mu      sync.Mutex
	waited  bool
	applied int
	rows    map[*seed.Seed]int64
	done    map[*seed.Seed]int
}

// newSeedEvents translates for observe.
func newSeedEvents(observe seedport.Observer) *seedEvents {
	return &seedEvents{observe: observe, rows: map[*seed.Seed]int64{}, done: map[*seed.Seed]int{}}
}

// event translates one seed event.
func (t *seedEvents) event(e seed.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch e.Kind {
	case seed.LockWait:
		t.waited = true
		t.observe(seedport.Event{Kind: seedport.LockWait, Duration: e.Duration})
	case seed.LockAcquired:
		out := seedport.Event{Kind: seedport.LockAcquired}
		if t.waited {
			out.Duration = e.Duration
		}
		t.observe(out)
	case seed.Planned:
		t.planned(e.Plan)
	case seed.SeedStart:
		out := t.step(seedport.SeedStart, e)
		out.Tenants = e.Tenants
		t.observe(out)
	case seed.TenantStart, seed.TenantEnd:
		t.tenant(e)
	case seed.SeedEnd:
		if e.Err == nil {
			t.applied++
		}
		out := t.step(seedport.SeedEnd, e)
		out.Tenants, out.Rows, out.Duration, out.Err = e.Tenants, e.Rows, e.Duration, e.Err
		t.observe(out)
	case seed.RunEnd:
		t.observe(seedport.Event{Kind: seedport.RunEnd, Total: t.applied, Duration: e.Duration, Err: e.Err})
	}
}

// planned reports the plan one seed at a time.
func (t *seedEvents) planned(p *seed.Plan) {
	if p == nil {
		return
	}
	for i, it := range p.Seeds {
		s := seedDef(it.Seed)
		t.observe(seedport.Event{Kind: seedport.Planned, Seed: &s, Reason: reason(it.Reason, it.Changed), Index: i + 1, Total: len(p.Seeds)})
	}
}

// step is the port event of a seed, numbered from 1.
func (t *seedEvents) step(kind seedport.EventKind, e seed.Event) seedport.Event {
	s := seedDef(e.Seed)
	return seedport.Event{Kind: kind, Seed: &s, Reason: reason(e.Reason, nil), Index: e.Index + 1, Total: e.Total}
}

// tenant reports a tenant with the seed's rows and tenants applied so far.
func (t *seedEvents) tenant(e seed.Event) {
	kind := seedport.TenantStart
	if e.Kind == seed.TenantEnd {
		kind = seedport.TenantEnd
		if e.Err == nil {
			t.rows[e.Seed] += e.Rows
			t.done[e.Seed]++
		}
	}
	out := t.step(kind, e)
	out.Tenant, out.Tenants, out.TenantsDone, out.Rows, out.Err = e.Tenant, e.Tenants, t.done[e.Seed], t.rows[e.Seed], e.Err
	t.observe(out)
}
