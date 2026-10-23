// Package live is the view of a running deploy step: one row per migration or seed with a spinner,
// elapsed time, retries, rows and per-tenant progress, a panel naming the backends a lock waits
// for, and a summary of what ran and what was left undone. The up, migrate up and seed apply
// commands run it standalone; the console embeds it.
package live

import (
	"context"
	"errors"
	"fmt"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

type rowState uint8

const (
	queued rowState = iota
	running
	done
	failed
	cancelled
	notRun
	refused
)

type row struct {
	key          string
	name         string
	detail       string
	reason       string
	state        rowState
	started      time.Time
	took         time.Duration
	retries      int
	statement    int
	statements   int
	rows         int64
	tenants      int
	tenantsDone  int
	failedTenant string
	err          error
}

type section struct {
	title   string
	noun    string
	rows    []row
	skipped []string
	notes   []string
	after   string
	started time.Time
	ended   bool
	applied int
	took    time.Duration
	err     error
	waiting bool
}

func (s *section) find(key string) int {
	for i := range s.rows {
		if s.rows[i].key == key {
			return i
		}
	}
	return -1
}

func (s *section) upsert(r row) int {
	if i := s.find(r.key); i >= 0 {
		return i
	}
	s.rows = append(s.rows, r)
	return len(s.rows) - 1
}

type lockState struct {
	title    string
	detail   string
	since    time.Time
	blockers []migrateport.Blocker
}

// FinishedMsg reports that the run ended: Err is the failure that stopped it and Cancelled marks
// a run stopped by the user or the context.
type FinishedMsg struct {
	Err       error
	Cancelled bool
}

// Options configures a Model. Context bounds the engine calls; Life bounds the goroutines that
// deliver events and ends when the program that shows the view ends.
type Options struct {
	Jobs     []Job
	Theme    theme.Theme
	Now      func() time.Time
	Context  context.Context
	Life     context.Context
	Width    int
	Embedded bool
}

// Model is the live view.
type Model struct {
	theme      theme.Theme
	now        func() time.Time
	width      int
	jobs       []Job
	job        int
	ctx        context.Context
	cancel     context.CancelFunc
	life       context.Context
	spinner    spinner.Model
	bar        progress.Model
	sections   []section
	lock       *lockState
	cancelKey  key.Binding
	cancelling bool
	finished   bool
	cancelled  bool
	err        error
	started    time.Time
	embedded   bool
}

// New makes the view; Init starts the first job.
func New(opts Options) Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.Life == nil {
		opts.Life = opts.Context
	}
	ctx, cancel := context.WithCancel(opts.Context)
	m := Model{
		theme:     opts.Theme,
		now:       opts.Now,
		width:     opts.Width,
		embedded:  opts.Embedded,
		jobs:      opts.Jobs,
		ctx:       ctx,
		cancel:    cancel,
		life:      opts.Life,
		spinner:   spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		cancelKey: key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "cancel")),
		started:   opts.Now(),
	}
	m.bar = m.newBar()
	if len(m.jobs) == 0 {
		m.finished = true
		return m
	}
	m.sections = []section{m.newSection(m.jobs[0])}
	return m
}

func (m Model) newBar() progress.Model {
	p := m.theme.Palette
	b := progress.New(progress.WithColors(p.Accent), progress.WithoutPercentage(), progress.WithFillCharacters('━', '━'))
	b.EmptyColor = p.Border
	return b
}

func (m Model) newSection(j Job) section {
	return section{title: j.Title, noun: j.Noun, started: m.now()}
}

// Init starts the spinner and the first job.
func (m Model) Init() tea.Cmd {
	if m.finished {
		return finished(nil, false)
	}
	return tea.Batch(m.spinner.Tick, start(m.ctx, m.life, 0, m.jobs[0]))
}

func finished(err error, cancelled bool) tea.Cmd {
	return func() tea.Msg { return FinishedMsg{Err: err, Cancelled: cancelled} }
}

// Finished reports whether every job ended.
func (m Model) Finished() bool { return m.finished }

// Err is the failure that stopped the run.
func (m Model) Err() error { return m.err }

// Cancelled reports whether the run was cancelled.
func (m Model) Cancelled() bool { return m.cancelled }

// Running reports whether a job is still running.
func (m Model) Running() bool { return !m.finished }

// Cancelling reports whether the run was asked to stop and has not yet.
func (m Model) Cancelling() bool { return m.cancelling && !m.finished }

// CancelKey is the binding that cancels the run.
func (m Model) CancelKey() key.Binding { return m.cancelKey }

// SetTheme restyles the view.
func (m Model) SetTheme(t theme.Theme) Model {
	m.theme = t
	m.bar = m.newBar()
	return m
}

// SetWidth sets the width the view renders to.
func (m Model) SetWidth(w int) Model {
	m.width = w
	return m
}

// Cancel stops the run; the view keeps running until the engine returns.
func (m Model) Cancel() Model {
	if !m.finished && !m.cancelling {
		m.cancelling = true
		m.cancel()
	}
	return m
}

// Update applies a message.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.BackgroundColorMsg:
		m = m.SetTheme(theme.New(msg.IsDark()))
	case tea.KeyPressMsg:
		if key.Matches(msg, m.cancelKey) {
			m = m.Cancel()
		}
	case spinner.TickMsg:
		if m.finished {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case streamMsg:
		cmds := []tea.Cmd{next(msg.ch)}
		for _, inner := range msg.msgs {
			var cmd tea.Cmd
			m, cmd = m.handle(inner)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) handle(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case PlanMsg:
		m.plan(msg.Plan)
	case MigrationMsg:
		m.migration(msg.Event)
	case SeedMsg:
		m.seed(msg.Event)
	case jobDoneMsg:
		return m.jobDone(msg)
	}
	return m, nil
}

func (m *Model) current() *section {
	return &m.sections[len(m.sections)-1]
}

func (m *Model) plan(p migrateport.Plan) {
	s := m.current()
	for _, mig := range p.Migrations {
		s.upsert(migrationRow(mig))
	}
}

func migrationRow(mig migrateport.Migration) row {
	key := mig.Version
	if key == "" {
		key = "R"
	}
	detail := mig.Source
	if d := render.MigrationDetail(mig); d != "" {
		detail += " · " + d
	}
	return row{key: key, name: mig.Name, detail: detail}
}

func (m *Model) migration(e migrateport.Event) {
	s := m.current()
	now := m.now()
	var r *row
	if e.Migration != nil {
		r = &s.rows[s.upsert(migrationRow(*e.Migration))]
	}
	switch e.Kind {
	case migrateport.LockWait:
		s.waiting = true
		m.lock = &lockState{title: "Waiting for the migration lock", detail: fmt.Sprintf("attempt %d", e.Attempt),
			since: now.Add(-e.Duration), blockers: e.Blockers}
	case migrateport.LockAcquired:
		m.lock = nil
		s.waiting = false
		if e.Duration > 0 {
			s.notes = append(s.notes, "migration lock acquired after "+render.Duration(e.Duration))
		}
	case migrateport.MigrationStart:
		m.lock = nil
		r.state, r.started = running, now
	case migrateport.Statement:
		r.statement, r.statements = e.Statement, e.Statements
	case migrateport.Retry:
		r.retries = e.Attempt
		m.lock = &lockState{title: r.name + " waits on a lock", detail: fmt.Sprintf("lock timeout · retry %d", e.Attempt),
			since: r.started, blockers: e.Blockers}
	case migrateport.MigrationEnd:
		m.lock = nil
		r.took, r.err = e.Duration, e.Err
		r.state = endState(e.Err)
	case migrateport.BaselineMarked:
		r.state, r.reason = done, "baseline recorded"
	case migrateport.After:
		s.after = fmt.Sprintf("after statements %d/%d in %s", e.Statement, e.Statements, render.Duration(e.Duration))
	case migrateport.RunEnd:
		s.ended, s.applied, s.took, s.err = true, e.Total, e.Duration, e.Err
	}
}

func (m *Model) seed(e seedport.Event) {
	s := m.current()
	now := m.now()
	var r *row
	if e.Seed != nil && e.Kind != seedport.Planned {
		r = &s.rows[s.upsert(row{key: e.Seed.Name, name: e.Seed.Name, detail: render.SeedKind(*e.Seed)})]
	}
	switch e.Kind {
	case seedport.Planned:
		switch {
		case e.Reason.Kind == seedport.Refused:
			s.upsert(row{key: e.Seed.Name, name: e.Seed.Name, detail: render.SeedKind(*e.Seed), state: refused, reason: "refused: database is production"})
		case e.Reason.Runs():
			s.upsert(row{key: e.Seed.Name, name: e.Seed.Name, detail: render.SeedKind(*e.Seed), reason: render.Reason(e.Reason)})
		case e.Reason.Kind != seedport.Excluded:
			s.skipped = append(s.skipped, e.Seed.Name)
		}
	case seedport.LockWait:
		s.waiting = true
		m.lock = &lockState{title: "Waiting for the seed lock", since: now.Add(-e.Duration)}
	case seedport.LockAcquired:
		m.lock = nil
		s.waiting = false
		if e.Duration > 0 {
			s.notes = append(s.notes, "seed lock acquired after "+render.Duration(e.Duration))
		}
	case seedport.SeedStart:
		r.state, r.started, r.tenants = running, now, e.Tenants
	case seedport.TenantStart:
		r.tenants, r.tenantsDone, r.rows = e.Tenants, e.TenantsDone, e.Rows
	case seedport.TenantEnd:
		r.tenants, r.tenantsDone, r.rows = e.Tenants, e.TenantsDone, e.Rows
		if e.Err != nil {
			r.failedTenant = e.Tenant
		}
	case seedport.SeedEnd:
		r.took, r.rows, r.err = e.Duration, e.Rows, e.Err
		r.state = endState(e.Err)
	case seedport.RunEnd:
		s.ended, s.applied, s.took, s.err = true, e.Total, e.Duration, e.Err
	}
}

func endState(err error) rowState {
	switch {
	case err == nil:
		return done
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return cancelled
	default:
		return failed
	}
}

func (m Model) jobDone(d jobDoneMsg) (Model, tea.Cmd) {
	m.lock = nil
	s := m.current()
	s.waiting = false
	if !s.ended {
		s.ended, s.took = true, m.now().Sub(s.started)
	}
	if s.err == nil {
		s.err = d.err
	}
	stopped := m.ctx.Err() != nil
	if d.err != nil || stopped {
		m.err = d.err
		m.cancelled = stopped || errors.Is(d.err, context.Canceled)
		m.stopRows()
		for _, j := range m.jobs[d.job+1:] {
			m.sections = append(m.sections, section{title: j.Title, noun: j.Noun, started: m.now()})
		}
		return m.finish()
	}
	if d.job+1 < len(m.jobs) {
		m.job = d.job + 1
		m.sections = append(m.sections, m.newSection(m.jobs[m.job]))
		return m, start(m.ctx, m.life, m.job, m.jobs[m.job])
	}
	return m.finish()
}

func (m *Model) stopRows() {
	for i := range m.sections {
		for j := range m.sections[i].rows {
			r := &m.sections[i].rows[j]
			switch r.state {
			case running:
				r.state = cancelled
				r.took = m.now().Sub(r.started)
			case queued:
				r.state = notRun
			}
		}
	}
}

func (m Model) finish() (Model, tea.Cmd) {
	m.finished = true
	m.cancel()
	return m, finished(m.err, m.cancelled)
}
