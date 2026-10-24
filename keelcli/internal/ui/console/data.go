package console

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/internal/doctor"
	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

type targetMsg struct {
	target guard.Target
	server string
	err    error
}

type migrationsMsg struct {
	rows []migrateport.Status
	err  error
}

type seedsMsg struct {
	graph  []seedport.Seed
	status []seedport.Status
	err    error
}

type templatesMsg struct {
	templates []migrateport.Template
	err       error
}

type lintMsg struct {
	findings []migrateport.Finding
	err      error
}

type doctorMsg struct {
	report doctor.Report
}

type doctorTickMsg struct{}

type planMsg struct {
	plan migrateport.Plan
	err  error
}

type seedPlanMsg struct {
	items []seedport.PlanItem
	err   error
}

type resultMsg struct {
	text  string
	focus string
	err   error
}

type flash struct {
	text string
	tone render.Tone
	at   time.Time
}

func (m Model) loadTarget() tea.Cmd {
	ctx, facts, seeds := m.ctx, m.opts.Facts, m.opts.Seeds
	return func() tea.Msg {
		src := guard.Sources{Facts: facts}
		if seeds != nil {
			src.Seeds = seeds
		}
		t, err := guard.Read(ctx, src)
		server := ""
		if err == nil && facts != nil {
			if c, cerr := facts.Connection(ctx); cerr == nil {
				server = c.ServerVersion
			}
		}
		return targetMsg{target: t, server: server, err: err}
	}
}

func (m Model) loadMigrations() tea.Cmd {
	ctx, eng := m.ctx, m.opts.Migrations
	return func() tea.Msg {
		rows, err := eng.Status(ctx)
		return migrationsMsg{rows: rows, err: err}
	}
}

func (m Model) loadSeeds() tea.Cmd {
	if m.opts.Seeds == nil {
		return nil
	}
	ctx, eng := m.ctx, m.opts.Seeds
	return func() tea.Msg {
		status, err := eng.Status(ctx)
		return seedsMsg{graph: eng.Graph(), status: status, err: err}
	}
}

func (m Model) loadTemplates() tea.Cmd {
	ctx, eng := m.ctx, m.opts.Migrations
	return func() tea.Msg {
		ts, err := eng.Templates(ctx)
		return templatesMsg{templates: ts, err: err}
	}
}

func (m Model) loadLint() tea.Cmd {
	ctx, eng := m.ctx, m.opts.Migrations
	return func() tea.Msg {
		fs, err := eng.Lint(ctx, migrateport.LintOptions{})
		return lintMsg{findings: fs, err: err}
	}
}

func (m Model) loadDoctor() tea.Cmd {
	if m.opts.Facts == nil {
		return nil
	}
	ctx, src := m.ctx, doctor.Sources{
		Facts: m.opts.Facts, Migrations: m.opts.Migrations, Extensions: m.opts.Extensions, Now: m.opts.Now,
	}
	return func() tea.Msg { return doctorMsg{report: doctor.Run(ctx, src)} }
}

func (m Model) doctorTick() tea.Cmd {
	return tea.Tick(m.opts.RefreshEvery, func(time.Time) tea.Msg { return doctorTickMsg{} })
}

func (m Model) refreshAll() tea.Cmd {
	return tea.Batch(m.loadTarget(), m.loadMigrations(), m.loadSeeds(), m.loadTemplates(), m.loadLint(), m.loadDoctor())
}

func (m Model) loadPlan() tea.Cmd {
	ctx, eng := m.ctx, m.opts.Migrations
	return func() tea.Msg {
		p, err := eng.Plan(ctx, migrateport.PlanOptions{})
		return planMsg{plan: p, err: err}
	}
}

func (m Model) loadSeedPlan(opts seedport.PlanOptions) tea.Cmd {
	ctx, eng := m.ctx, m.opts.Seeds
	return func() tea.Msg {
		items, err := eng.Plan(ctx, opts)
		return seedPlanMsg{items: items, err: err}
	}
}

func (m Model) action(fn func(ctx context.Context) (string, string, error)) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		text, focus, err := fn(ctx)
		return resultMsg{text: text, focus: focus, err: err}
	}
}
