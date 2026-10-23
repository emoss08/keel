package fake

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var errLockTimeout = errors.New("ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)")

// Status lists every migration with its state, missing ones first by version.
func (x Migrations) Status(context.Context) ([]migrateport.Status, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]migrateport.Status, 0, len(e.migrations))
	for _, m := range e.migrations {
		out = append(out, migrateport.Status{
			Migration:       m.m,
			State:           m.state,
			AppliedAt:       m.appliedAt,
			Duration:        m.duration,
			AppliedChecksum: m.appliedChecksum,
		})
	}
	return out, nil
}

// History lists the bookkeeping rows in the order they were applied.
func (x Migrations) History(context.Context) ([]migrateport.Record, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []migrateport.Record
	for _, m := range e.migrations {
		if m.appliedAt.IsZero() || m.state == migrateport.Pending {
			continue
		}
		out = append(out, migrateport.Record{
			Version:   m.m.Version,
			Name:      m.m.Name,
			Source:    m.m.Source,
			Checksum:  m.appliedChecksum,
			AppliedAt: m.appliedAt,
			Duration:  m.duration,
		})
	}
	return out, nil
}

// Plan lists the pending migrations up to opts.To, then due repeatable ones when no target is set.
func (x Migrations) Plan(_ context.Context, opts migrateport.PlanOptions) (migrateport.Plan, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	run, err := e.migrationPlanLocked(opts.To)
	if err != nil {
		return migrateport.Plan{}, err
	}
	p := migrateport.Plan{}
	for _, m := range run {
		p.Migrations = append(p.Migrations, m.m)
	}
	if len(p.Migrations) > 0 {
		p.After = []string{reconcile}
	}
	return p, nil
}

func (e *Engine) migrationPlanLocked(to string) ([]*migration, error) {
	if to != "" && e.migrationLocked(to) == nil {
		return nil, fmt.Errorf("fake: unknown version %s", to)
	}
	var run []*migration
	for _, m := range e.migrations {
		if m.state == migrateport.Pending && (to == "" || m.m.Version <= to) {
			run = append(run, m)
		}
	}
	if to == "" {
		for _, m := range e.migrations {
			if m.state == migrateport.RepeatableDue {
				run = append(run, m)
			}
		}
	}
	return run, nil
}

func (e *Engine) migrationLocked(version string) *migration {
	for _, m := range e.migrations {
		if m.m.Version == version || m.m.ID() == version {
			return m
		}
	}
	return nil
}

// Up applies the plan, reporting a lock held by another deploy, notx statements, lock-timeout
// retries and, in the failure scenario, a failing Go migration.
func (x Migrations) Up(ctx context.Context, opts migrateport.UpOptions, observe migrateport.Observer) error {
	e := x.e
	e.mu.Lock()
	for _, m := range e.migrations {
		if m.state == migrateport.Changed {
			e.mu.Unlock()
			return fmt.Errorf("fake: %s: applied file changed (checksum %s, recorded %s); run migrate repair",
				m.m.ID(), short(m.m.Checksum), short(m.appliedChecksum))
		}
	}
	run, err := e.migrationPlanLocked(opts.To)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	total, err := e.waitForLock(ctx, observe)
	if err != nil {
		observe(migrateport.Event{Kind: migrateport.RunEnd, Duration: total, Err: err})
		return err
	}
	for i, m := range run {
		d, err := e.apply(ctx, m, i+1, len(run), observe)
		total += d
		if err != nil {
			observe(migrateport.Event{Kind: migrateport.RunEnd, Total: i, Duration: total, Err: err})
			return err
		}
	}
	if len(run) > 0 {
		d := 12 * time.Millisecond
		if err := e.sleep(ctx, d); err != nil {
			observe(migrateport.Event{Kind: migrateport.RunEnd, Total: len(run), Duration: total, Err: err})
			return err
		}
		total += d
		observe(migrateport.Event{Kind: migrateport.After, Statement: 1, Statements: 1, Duration: d})
	}
	observe(migrateport.Event{Kind: migrateport.RunEnd, Total: len(run), Duration: total})
	return nil
}

func (e *Engine) waitForLock(ctx context.Context, observe migrateport.Observer) (time.Duration, error) {
	if e.opts.NoLockWait {
		observe(migrateport.Event{Kind: migrateport.LockAcquired})
		return 0, nil
	}
	holder := []migrateport.Blocker{{
		PID: 51203, Application: "trenova-db (deploy 418)", State: "active", Mode: "advisory",
		Age: 41 * time.Second, Query: "SELECT pg_advisory_lock(7372631836457123681)",
	}}
	step := 700 * time.Millisecond
	waited := time.Duration(0)
	for attempt := 1; attempt <= 3; attempt++ {
		holder[0].Age = 41*time.Second + waited
		observe(migrateport.Event{
			Kind: migrateport.LockWait, Attempt: attempt, Duration: waited, Blockers: cloneBlockers(holder),
		})
		if err := e.sleep(ctx, step); err != nil {
			return waited, err
		}
		waited += step
	}
	observe(migrateport.Event{Kind: migrateport.LockAcquired, Duration: waited})
	return waited, nil
}

func (e *Engine) apply(ctx context.Context, m *migration, index, total int, observe migrateport.Observer) (time.Duration, error) {
	mig := m.m
	observe(migrateport.Event{Kind: migrateport.MigrationStart, Migration: &mig, Index: index, Total: total})
	d, err := e.execute(ctx, m, &mig, index, total, observe)
	observe(migrateport.Event{
		Kind: migrateport.MigrationEnd, Migration: &mig, Index: index, Total: total, Duration: d, Err: err,
	})
	if err != nil {
		return d, err
	}
	e.mu.Lock()
	m.state = migrateport.Applied
	m.appliedAt = e.opts.Now()
	m.duration = d
	m.appliedChecksum = m.m.Checksum
	e.mu.Unlock()
	return d, nil
}

func (e *Engine) execute(ctx context.Context, m *migration, mig *migrateport.Migration, index, total int, observe migrateport.Observer) (time.Duration, error) {
	elapsed := time.Duration(0)
	if len(m.script.statements) > 0 {
		for i, d := range m.script.statements {
			observe(migrateport.Event{
				Kind: migrateport.Statement, Migration: mig, Index: index, Total: total,
				Statement: i + 1, Statements: len(m.script.statements),
			})
			if err := e.sleep(ctx, d); err != nil {
				return elapsed, err
			}
			elapsed += d
		}
		return elapsed, nil
	}
	lockTimeout := 5 * time.Second
	if m.m.Directives.LockTimeout > 0 {
		lockTimeout = m.m.Directives.LockTimeout
	}
	for attempt := 1; attempt <= m.script.retries; attempt++ {
		wait := lockTimeout / 4
		if err := e.sleep(ctx, wait); err != nil {
			return elapsed, err
		}
		elapsed += wait
		observe(migrateport.Event{
			Kind: migrateport.Retry, Migration: mig, Index: index, Total: total, Attempt: attempt,
			Err: errLockTimeout, Blockers: reportBlockers(elapsed),
		})
	}
	if err := e.sleep(ctx, m.script.duration); err != nil {
		return elapsed, err
	}
	elapsed += m.script.duration
	if m.script.fails {
		return elapsed, errors.New(`ERROR: null value in column "currency" of relation "shipments" violates not-null constraint (SQLSTATE 23502)`)
	}
	return elapsed, nil
}

func reportBlockers(elapsed time.Duration) []migrateport.Blocker {
	return []migrateport.Blocker{
		{
			PID: 48121, Application: "report-runner", State: "active", Mode: "AccessShareLock",
			Age:   4*time.Minute + 12*time.Second + elapsed,
			Query: "SELECT s.pro_number, sum(m.distance) FROM shipments s JOIN moves m ON m.shipment_id = s.id WHERE s.organization_id = $1 AND s.created_at >= $2 GROUP BY s.pro_number ORDER BY 2 DESC",
		},
		{
			PID: 48377, Application: "trenova-api", State: "idle in transaction", Mode: "RowExclusiveLock",
			Age: 38*time.Second + elapsed, Query: "UPDATE shipments SET status = $1 WHERE id = $2",
		},
	}
}

func cloneBlockers(bs []migrateport.Blocker) []migrateport.Blocker {
	return append([]migrateport.Blocker(nil), bs...)
}

// Create adds a pending migration named after the request.
func (x Migrations) Create(_ context.Context, req migrateport.CreateRequest) (migrateport.Created, error) {
	e := x.e
	if !namePattern.MatchString(req.Name) {
		return migrateport.Created{}, fmt.Errorf("fake: migration name %q: use lower case letters, digits and underscores", req.Name)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m := migrateport.Migration{Name: req.Name, Source: "core"}
	var path string
	switch req.Kind {
	case migrateport.RepeatableFile:
		m.Kind = migrateport.Repeatable
		m.SQL = "-- Re-applied whenever this file changes.\n"
		path = "schema/migrations/R_" + req.Name + ".sql"
	case migrateport.NoTx:
		m.Version = e.opts.Now().UTC().Format("20060102150405")
		m.Directives.NoTx = true
		m.SQL = "--keel:notx\n"
		path = "schema/migrations/" + m.ID() + ".sql"
	default:
		m.Version = e.opts.Now().UTC().Format("20060102150405")
		path = "schema/migrations/" + m.ID() + ".sql"
	}
	for _, x := range e.migrations {
		if x.m.ID() == m.ID() {
			return migrateport.Created{}, fmt.Errorf("fake: %s already exists", path)
		}
	}
	added := pending(m, script{duration: 30 * time.Millisecond})
	if m.Kind == migrateport.Repeatable {
		added.state = migrateport.RepeatableDue
	}
	e.migrations = append(e.migrations, added)
	return migrateport.Created{Path: path, Migration: added.m}, nil
}

// Squash replaces every applied versioned migration with one baseline.
func (x Migrations) Squash(ctx context.Context) (migrateport.Squashed, error) {
	e := x.e
	if err := e.sleep(ctx, 1500*time.Millisecond); err != nil {
		return migrateport.Squashed{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var keep []*migration
	replaced, newest := 0, ""
	var sql strings.Builder
	sql.WriteString("--keel:baseline\n")
	for _, m := range e.migrations {
		if m.state == migrateport.Applied && m.m.Kind != migrateport.Repeatable {
			replaced++
			newest = m.m.Version
			sql.WriteString(strings.TrimPrefix(m.m.SQL, "--keel:baseline\n"))
			continue
		}
		keep = append(keep, m)
	}
	if replaced == 0 {
		return migrateport.Squashed{}, errors.New("fake: nothing to squash")
	}
	base := applied(migrateport.Migration{
		Version: newest, Name: "baseline", Source: "core", Kind: migrateport.Baseline, SQL: sql.String(),
	}, e.opts.Now(), 0)
	e.migrations = append([]*migration{base}, keep...)
	return migrateport.Squashed{Path: "schema/migrations/" + base.m.ID() + ".sql", Version: newest, Replaced: replaced}, nil
}

// Stamp reports the schema version of the newest migration; the fake's stamp file matches the
// migrations the fake started with.
func (x Migrations) Stamp(_ context.Context, opts migrateport.StampOptions) (migrateport.Stamp, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	newest := ""
	var sums strings.Builder
	for _, m := range e.migrations {
		if m.m.Version > newest && m.state != migrateport.Missing {
			newest = m.m.Version
		}
		sums.WriteString(m.m.Checksum)
	}
	s := migrateport.Stamp{
		Path:        "schema/version_gen.go",
		Version:     newest,
		Fingerprint: checksum(sums.String())[:16],
		Current:     newest == e.stamped,
	}
	if !opts.Check {
		s.Current = true
	}
	return s, nil
}

// Check reports changed and missing files and pending migrations older than the newest applied
// one, or than opts.Base when set.
func (x Migrations) Check(_ context.Context, opts migrateport.CheckOptions) ([]migrateport.Problem, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	newest := opts.Base
	if newest == "" {
		for _, m := range e.migrations {
			if m.state == migrateport.Applied && m.m.Version > newest {
				newest = m.m.Version
			}
		}
	}
	var out []migrateport.Problem
	for _, m := range e.migrations {
		switch {
		case m.state == migrateport.Changed:
			out = append(out, migrateport.Problem{Kind: migrateport.ChangedFile, Migration: m.m,
				Message: fmt.Sprintf("checksum %s differs from the recorded %s", short(m.m.Checksum), short(m.appliedChecksum))})
		case m.state == migrateport.Missing:
			out = append(out, migrateport.Problem{Kind: migrateport.MissingFile, Migration: m.m,
				Message: "applied but no longer in any source"})
		case m.state == migrateport.Pending && m.m.Version != "" && m.m.Version < newest:
			out = append(out, migrateport.Problem{Kind: migrateport.OutOfOrder, Migration: m.m,
				Message: fmt.Sprintf("pending, older than the newest applied %s", newest)})
		}
	}
	return out, nil
}

// Lint runs the built-in rules over pending migrations.
func (x Migrations) Lint(context.Context) ([]migrateport.Finding, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []migrateport.Finding
	for _, m := range e.migrations {
		if m.state != migrateport.Pending {
			continue
		}
		out = append(out, lint(m.m)...)
	}
	return out, nil
}

func lint(m migrateport.Migration) []migrateport.Finding {
	var out []migrateport.Finding
	for i, line := range strings.Split(m.SQL, "\n") {
		upper := strings.ToUpper(line)
		at := i + 1
		switch {
		case strings.HasPrefix(upper, "CREATE INDEX ") && !strings.Contains(upper, "CONCURRENTLY") &&
			strings.Contains(upper, " ON SHIPMENTS"):
			out = append(out, migrateport.Finding{Rule: "index-concurrently", Migration: m, Line: at, Severity: migrateport.Error,
				Message: "CREATE INDEX on existing table shipments without CONCURRENTLY blocks writes while it builds"})
		case strings.Contains(upper, "ADD COLUMN") && strings.Contains(upper, "NOT NULL") && !strings.Contains(upper, "DEFAULT"):
			out = append(out, migrateport.Finding{Rule: "not-null-default", Migration: m, Line: at, Severity: migrateport.Error,
				Message: "ADD COLUMN … NOT NULL without a default fails on a table with rows"})
		case strings.HasPrefix(upper, "CREATE TABLE ") && strings.Contains(m.SQL, "organization_id"):
			out = append(out, migrateport.Finding{Rule: "tenant-rls", Migration: m, Line: at, Severity: migrateport.Warning,
				Message: "tenant table: reconcile() enables row-level security after this run; add policies in R_tenant_rls_helpers"})
		}
	}
	return out
}

// Baseline records every migration up to version as applied without running it.
func (x Migrations) Baseline(_ context.Context, version string) (migrateport.Baselined, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.migrationLocked(version) == nil {
		return migrateport.Baselined{}, fmt.Errorf("fake: unknown version %s", version)
	}
	var out migrateport.Baselined
	for _, m := range e.migrations {
		if m.state == migrateport.Pending && m.m.Version != "" && m.m.Version <= version {
			m.state = migrateport.Applied
			m.appliedAt = e.opts.Now()
			m.appliedChecksum = m.m.Checksum
			out.Marked = append(out.Marked, m.m)
		}
	}
	return out, nil
}

// Repair accepts the current checksum of a changed migration.
func (x Migrations) Repair(_ context.Context, version string) (migrateport.Repaired, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.migrationLocked(version)
	if m == nil {
		return migrateport.Repaired{}, fmt.Errorf("fake: unknown version %s", version)
	}
	if m.state != migrateport.Changed {
		return migrateport.Repaired{}, fmt.Errorf("fake: %s is %s, not changed", m.m.ID(), m.state)
	}
	out := migrateport.Repaired{Migration: m.m, OldChecksum: m.appliedChecksum, NewChecksum: m.m.Checksum}
	m.state = migrateport.Applied
	m.appliedChecksum = m.m.Checksum
	return out, nil
}

// Templates lists the template databases, current first.
func (x Migrations) Templates(context.Context) ([]migrateport.Template, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]migrateport.Template(nil), e.templates...), nil
}

// BuildTemplate builds the current template unless it exists.
func (x Migrations) BuildTemplate(ctx context.Context) (migrateport.Template, error) {
	e := x.e
	e.mu.Lock()
	for _, t := range e.templates {
		if t.Current {
			e.mu.Unlock()
			return t, nil
		}
	}
	e.mu.Unlock()
	if err := e.sleep(ctx, 2*time.Second); err != nil {
		return migrateport.Template{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t := migrateport.Template{Name: "keel_tpl_3e8b5f21", Fingerprint: "3e8b5f21", Current: true, Size: 49 << 20, CreatedAt: e.opts.Now()}
	e.templates = append([]migrateport.Template{t}, e.templates...)
	return t, nil
}

// PruneTemplates drops every template that is not current.
func (x Migrations) PruneTemplates(context.Context) ([]migrateport.Template, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	var keep, dropped []migrateport.Template
	for _, t := range e.templates {
		if t.Current {
			keep = append(keep, t)
		} else {
			dropped = append(dropped, t)
		}
	}
	e.templates = keep
	return dropped, nil
}

// Reset recreates the database from the current template, building it first when needed; every
// migration and reference seed is applied afterwards and fixtures are gone.
func (x Migrations) Reset(ctx context.Context) (migrateport.Reset, error) {
	e := x.e
	built := false
	e.mu.Lock()
	current := false
	for _, t := range e.templates {
		current = current || t.Current
	}
	e.mu.Unlock()
	if !current {
		if _, err := x.BuildTemplate(ctx); err != nil {
			return migrateport.Reset{}, err
		}
		built = true
	}
	d := 180 * time.Millisecond
	if err := e.sleep(ctx, d); err != nil {
		return migrateport.Reset{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.opts.Now()
	var keep []*migration
	for _, m := range e.migrations {
		if m.state == migrateport.Missing {
			continue
		}
		m.state, m.appliedAt, m.appliedChecksum = migrateport.Applied, now, m.m.Checksum
		keep = append(keep, m)
	}
	e.migrations = keep
	e.resetSeedsLocked(now)
	name := ""
	for _, t := range e.templates {
		if t.Current {
			name = t.Name
		}
	}
	return migrateport.Reset{Database: "trenova", Template: name, Built: built, Duration: d}, nil
}

func short(sum string) string {
	if len(sum) > 8 {
		return sum[:8]
	}
	return sum
}
