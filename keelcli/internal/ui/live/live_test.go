package live

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/fake"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

var t0 = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newModel(width int, c *clock, jobs ...Job) Model {
	if len(jobs) == 0 {
		jobs = []Job{{Title: "Migrations", Noun: "migration"}}
	}
	return New(Options{Jobs: jobs, Theme: theme.New(true), Now: c.Now, Width: width})
}

func feed(m Model, msgs ...tea.Msg) Model {
	m, _ = m.Update(streamMsg{msgs: msgs, ch: closed()})
	return m
}

func plain(m Model) string { return ansi.Strip(m.Render()) }

func closed() <-chan tea.Msg {
	ch := make(chan tea.Msg)
	close(ch)
	return ch
}

var (
	ediIndex = migrateport.Migration{Version: "20261025110000", Name: "edi_partner_index", Source: "core",
		Directives: migrateport.Directives{NoTx: true}}
	rateTables = migrateport.Migration{Version: "20261025120000", Name: "rate_tables", Source: "core"}
	blockers   = []migrateport.Blocker{{PID: 48121, Application: "report-runner", State: "active",
		Mode: "AccessShareLock", Age: 4 * time.Minute, Query: "SELECT s.pro_number,\n  sum(m.distance) FROM shipments s"}}
)

func TestMigrationEvents(t *testing.T) {
	c := &clock{now: t0}
	m := newModel(100, c)
	m = feed(m, PlanMsg{Plan: migrateport.Plan{Migrations: []migrateport.Migration{ediIndex, rateTables}}})
	if out := plain(m); !strings.Contains(out, "·  20261025110000  edi_partner_index") || !strings.Contains(out, "rate_tables") {
		t.Fatalf("planned rows missing:\n%s", out)
	}

	m = feed(m, MigrationMsg{migrateport.Event{Kind: migrateport.LockWait, Attempt: 2, Duration: time.Second, Blockers: blockers}})
	out := plain(m)
	for _, want := range []string{"Waiting for the migration lock", "attempt 2", "48121", "report-runner", "SELECT s.pro_number, sum(m.distance)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lock panel lacks %q:\n%s", want, out)
		}
	}

	m = feed(m, MigrationMsg{migrateport.Event{Kind: migrateport.LockAcquired, Duration: 2 * time.Second}})
	if out := plain(m); strings.Contains(out, "Waiting for") || !strings.Contains(out, "migration lock acquired after 2s") {
		t.Fatalf("lock panel should close:\n%s", out)
	}

	m = feed(m,
		MigrationMsg{migrateport.Event{Kind: migrateport.MigrationStart, Migration: &ediIndex, Index: 1, Total: 2}},
		MigrationMsg{migrateport.Event{Kind: migrateport.Statement, Migration: &ediIndex, Index: 1, Total: 2, Statement: 1, Statements: 2}},
	)
	c.now = t0.Add(1500 * time.Millisecond)
	if out := plain(m); !strings.Contains(out, "statement 1/2") || !strings.Contains(out, "1.5s") {
		t.Fatalf("running row:\n%s", out)
	}

	m = feed(m,
		MigrationMsg{migrateport.Event{Kind: migrateport.MigrationEnd, Migration: &ediIndex, Index: 1, Total: 2, Duration: 4 * time.Second}},
		MigrationMsg{migrateport.Event{Kind: migrateport.MigrationStart, Migration: &rateTables, Index: 2, Total: 2}},
		MigrationMsg{migrateport.Event{Kind: migrateport.Retry, Migration: &rateTables, Index: 2, Total: 2, Attempt: 1,
			Err: errors.New("lock timeout"), Blockers: blockers}},
	)
	out = plain(m)
	for _, want := range []string{"✓  20261025110000", "2 statements", "4s", "lock timeout · retry 1", "rate_tables waits on a lock"} {
		if !strings.Contains(out, want) {
			t.Fatalf("after retry, view lacks %q:\n%s", want, out)
		}
	}

	m = feed(m,
		MigrationMsg{migrateport.Event{Kind: migrateport.MigrationEnd, Migration: &rateTables, Index: 2, Total: 2, Duration: 3 * time.Second}},
		MigrationMsg{migrateport.Event{Kind: migrateport.After, Statement: 1, Statements: 1, Duration: 12 * time.Millisecond}},
		MigrationMsg{migrateport.Event{Kind: migrateport.RunEnd, Total: 2, Duration: 9 * time.Second}},
	)
	var cmd tea.Cmd
	m, cmd = m.Update(streamMsg{msgs: []tea.Msg{jobDoneMsg{job: 0}}, ch: closed()})
	if !m.Finished() || m.Err() != nil || m.Cancelled() {
		t.Fatalf("finished %v err %v cancelled %v", m.Finished(), m.Err(), m.Cancelled())
	}
	if fin := findFinished(cmd); fin == nil || fin.Err != nil {
		t.Fatalf("no FinishedMsg: %#v", fin)
	}
	out = plain(m)
	for _, want := range []string{"Summary", "2 migrations applied", "1 retry", "after statements 1/1 in 12ms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "rate_tables waits") {
		t.Fatalf("lock panel left after the run:\n%s", out)
	}
}

func findFinished(cmd tea.Cmd) *FinishedMsg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case FinishedMsg:
		return &msg
	case tea.BatchMsg:
		for _, c := range msg {
			if f := findFinished(c); f != nil {
				return f
			}
		}
	}
	return nil
}

func TestSeedEventsShowTenantProgress(t *testing.T) {
	c := &clock{now: t0}
	m := newModel(100, c, Job{Title: "Seeds", Noun: "seed"})
	perms := seedport.Seed{Name: "organization_role_permissions", Kind: seedport.Reference, PerTenant: true}
	states := seedport.Seed{Name: "us_states", Kind: seedport.Reference}
	fixture := seedport.Seed{Name: "workers", Kind: seedport.Fixture}
	m = feed(m,
		SeedMsg{seedport.Event{Kind: seedport.Planned, Seed: &states, Reason: seedport.Reason{Kind: seedport.UpToDate}, Index: 1, Total: 3}},
		SeedMsg{seedport.Event{Kind: seedport.Planned, Seed: &perms, Reason: seedport.Reason{Kind: seedport.CodeChanged}, Index: 2, Total: 3}},
		SeedMsg{seedport.Event{Kind: seedport.Planned, Seed: &fixture, Reason: seedport.Reason{Kind: seedport.Refused}, Index: 3, Total: 3}},
		SeedMsg{seedport.Event{Kind: seedport.SeedStart, Seed: &perms, Index: 1, Total: 1, Tenants: 9000}},
		SeedMsg{seedport.Event{Kind: seedport.TenantEnd, Seed: &perms, Tenant: "org_04213", Tenants: 9000, TenantsDone: 4213, Rows: 101112}},
	)
	out := plain(m)
	for _, want := range []string{"4,213 / 9,000 tenants", "━", "101,112 rows", "1 up to date: us_states", "refused: database is production"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view lacks %q:\n%s", want, out)
		}
	}
	m = feed(m,
		SeedMsg{seedport.Event{Kind: seedport.TenantEnd, Seed: &perms, Tenant: "org_04214", Tenants: 9000, TenantsDone: 4213, Err: errors.New("tenant org_04214: duplicate key")}},
		SeedMsg{seedport.Event{Kind: seedport.SeedEnd, Seed: &perms, Rows: 101112, Duration: 3 * time.Second, Err: errors.New("tenant org_04214: duplicate key")}},
		SeedMsg{seedport.Event{Kind: seedport.RunEnd, Err: errors.New("tenant org_04214: duplicate key")}},
		jobDoneMsg{job: 0, err: errors.New("tenant org_04214: duplicate key")},
	)
	out = plain(m)
	for _, want := range []string{"failed at tenant org_04214", "duplicate key", "failed after 0 seeds"} {
		if !strings.Contains(out, want) {
			t.Fatalf("failure view lacks %q:\n%s", want, out)
		}
	}
}

func TestCancelListsWhatWasLeftUndone(t *testing.T) {
	c := &clock{now: t0}
	m := newModel(100, c, Job{Title: "Migrations", Noun: "migration"}, Job{Title: "Seeds", Noun: "seed"})
	m = feed(m,
		PlanMsg{Plan: migrateport.Plan{Migrations: []migrateport.Migration{ediIndex, rateTables}}},
		MigrationMsg{migrateport.Event{Kind: migrateport.MigrationStart, Migration: &ediIndex, Index: 1, Total: 2}},
	)
	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !m.Cancelling() {
		t.Fatal("ctrl+c did not start cancelling")
	}
	if !strings.Contains(plain(m), "Cancelling") {
		t.Fatalf("footer:\n%s", plain(m))
	}
	m = feed(m, jobDoneMsg{job: 0, err: context.Canceled})
	if !m.Finished() || !m.Cancelled() {
		t.Fatal("not finished as cancelled")
	}
	out := plain(m)
	for _, want := range []string{"not run", "Left undone", "migrations: 20261025110000_edi_partner_index, 20261025120000_rate_tables", "seeds: not started", "cancelled after 0 migrations"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view lacks %q:\n%s", want, out)
		}
	}
}

func TestNarrowTerminals(t *testing.T) {
	for _, width := range []int{120, 80, 60, 40} {
		c := &clock{now: t0}
		m := newModel(0, c)
		m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		m = feed(m,
			PlanMsg{Plan: migrateport.Plan{Migrations: []migrateport.Migration{ediIndex, rateTables}}},
			MigrationMsg{migrateport.Event{Kind: migrateport.MigrationStart, Migration: &rateTables, Index: 2, Total: 2}},
			MigrationMsg{migrateport.Event{Kind: migrateport.Retry, Migration: &rateTables, Index: 2, Total: 2, Attempt: 3, Blockers: blockers}},
		)
		for i, line := range strings.Split(m.Render(), "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Fatalf("width %d: line %d is %d cells: %q", width, i, w, ansi.Strip(line))
			}
		}
	}
}

type program struct{ m Model }

func (p program) Init() tea.Cmd { return p.m.Init() }

func (p program) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(FinishedMsg); ok {
		return p, tea.Quit
	}
	var cmd tea.Cmd
	p.m, cmd = p.m.Update(msg)
	return p, cmd
}

func (p program) View() tea.View { return p.m.View() }

func run(t *testing.T, opts fake.Options, jobs func(*fake.Engine) []Job) (Model, string) {
	t.Helper()
	e := fake.New(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := New(Options{Jobs: jobs(e), Theme: theme.New(true), Context: ctx, Life: ctx})
	var out bytes.Buffer
	final, err := tea.NewProgram(program{m}, tea.WithInput(strings.NewReader("")), tea.WithOutput(&out),
		tea.WithWindowSize(80, 100), tea.WithoutSignals(), tea.WithContext(ctx)).Run()
	if err != nil {
		t.Fatal(err)
	}
	return final.(program).m, ansi.Strip(out.String())
}

func TestRunsToCompletionAgainstTheFake(t *testing.T) {
	m, out := run(t, fake.Options{Tenants: 500}, func(e *fake.Engine) []Job {
		return []Job{MigrationJob(e.Migrations(), migrateport.UpOptions{}), SeedJob(e.Seeds(), seedport.ApplyOptions{})}
	})
	if !m.Finished() || m.Err() != nil {
		t.Fatalf("finished %v err %v", m.Finished(), m.Err())
	}
	for _, want := range []string{"4 migrations applied", "7 seeds applied", "500 tenants"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestStopsAtTheFirstFailure(t *testing.T) {
	m, out := run(t, fake.Options{FailMigration: true}, func(e *fake.Engine) []Job {
		return []Job{MigrationJob(e.Migrations(), migrateport.UpOptions{}), SeedJob(e.Seeds(), seedport.ApplyOptions{})}
	})
	if m.Err() == nil || m.Cancelled() {
		t.Fatalf("err %v cancelled %v", m.Err(), m.Cancelled())
	}
	if !strings.Contains(out, "seeds: not started") || !strings.Contains(out, "R_tenant_rls_helpers") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestNoJobsFinishesAtOnce(t *testing.T) {
	m := New(Options{Theme: theme.New(false)})
	if !m.Finished() || findFinished(m.Init()) == nil {
		t.Fatal("a model without jobs should finish at once")
	}
}
