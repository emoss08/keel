package console

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/internal/doctor"
	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/migrateport"
)

type templatesTab struct {
	templates []migrateport.Template
	err       error
	ready     bool
	cur       cursor
}

func (tt templatesTab) loaded(msg templatesMsg) templatesTab {
	tt.templates, tt.err, tt.ready = msg.templates, msg.err, true
	return tt
}

func (m Model) templatesKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	n := len(m.templates.templates)
	eng := m.opts.Migrations
	switch {
	case key.Matches(msg, k.up):
		m.templates.cur = m.templates.cur.move(-1, n)
	case key.Matches(msg, k.down):
		m.templates.cur = m.templates.cur.move(1, n)
	case key.Matches(msg, k.build):
		var spin tea.Cmd
		m, spin = m.setBusy("Building the current template")
		return m, tea.Batch(spin, m.action(func(ctx context.Context) (string, string, error) {
			t, err := eng.BuildTemplate(ctx)
			if err != nil {
				return "", "", err
			}
			return fmt.Sprintf("Template ready: %s (%s)", t.Name, render.Size(t.Size)), "", nil
		}))
	case key.Matches(msg, k.prune):
		var stale []string
		for _, t := range m.templates.templates {
			if !t.Current {
				stale = append(stale, t.Name)
			}
		}
		if len(stale) == 0 {
			m.flash = &flash{text: "Nothing to prune: no stale templates.", tone: render.Muted, at: m.opts.Now()}
			return m, nil
		}
		return m.confirm(guard.Action{
			Verb:   "prune templates",
			Title:  "Drop " + render.Plural(len(stale), "stale template", "stale templates") + "?",
			Detail: []string{"Templates: " + strings.Join(stale, ", ")},
		}, func(*answer) tea.Cmd {
			return m.action(func(ctx context.Context) (string, string, error) {
				dropped, err := eng.PruneTemplates(ctx)
				return "Dropped " + render.Plural(len(dropped), "template", "templates"), "", err
			})
		})
	case key.Matches(msg, k.reset):
		return m.confirm(guard.Action{
			Verb:   "reset the database",
			Title:  "Recreate " + m.target.Name() + " from the current template?",
			Detail: []string{"Every row is lost: the database becomes a copy of the template,", "with migrations and reference seeds applied and no fixtures."},
			Never:  true,
		}, func(*answer) tea.Cmd {
			return m.action(func(ctx context.Context) (string, string, error) {
				r, err := eng.Reset(ctx)
				if err != nil {
					return "", "", err
				}
				return fmt.Sprintf("Reset %s from %s in %s", r.Database, r.Template, render.Duration(r.Duration)), "", nil
			})
		})
	}
	return m.resized(), nil
}

func (m Model) templatesView(h int) string {
	t := m.theme
	tt := m.templates
	switch {
	case tt.err != nil:
		return block([]string{t.Fail.Render(tt.err.Error())}, m.width, h)
	case !tt.ready:
		return block([]string{m.spinner.View() + " " + m.note("Loading templates…")}, m.width, h)
	case len(tt.templates) == 0:
		return block([]string{m.note("No templates yet: press b to build the current one.")}, m.width, h)
	}
	tbl := render.TemplateTable(tt.templates, m.opts.Now(), true)
	tbl.Width = m.width - gutter
	styled, plain := tbl.Lines(t), tbl.Lines(render.Plain)
	lines := []string{"  " + styled[0]}
	body := m.rows(styled[1:], plain[1:], tt.cur.pos)
	lines = append(lines, window(body, tt.cur.offset, min(len(body), h-4))...)
	lines = append(lines, "", m.note("Each test and reset copies the current template, so a fresh database takes milliseconds;"),
		m.note("stale templates belong to migrations that have since changed."))
	return block(lines, m.width, h)
}

type lintEntry struct {
	rule    string
	finding *migrateport.Finding
}

type lintTab struct {
	findings []migrateport.Finding
	entries  []lintEntry
	err      error
	ready    bool
	sel      int
	offset   int
}

func (lt lintTab) loaded(msg lintMsg) lintTab {
	lt.findings, lt.err, lt.ready = msg.findings, msg.err, true
	lt.entries = nil
	for _, g := range render.GroupFindings(msg.findings) {
		lt.entries = append(lt.entries, lintEntry{rule: g.Rule})
		for i := range g.Findings {
			lt.entries = append(lt.entries, lintEntry{finding: &g.Findings[i]})
		}
	}
	return lt.move(0)
}

func (lt lintTab) positions() []int {
	var out []int
	for i, e := range lt.entries {
		if e.finding != nil {
			out = append(out, i)
		}
	}
	return out
}

func (lt lintTab) move(delta int) lintTab {
	lt.sel = clamp(lt.sel+delta, 0, len(lt.positions())-1)
	return lt
}

func (lt lintTab) pos() int {
	ps := lt.positions()
	if len(ps) == 0 {
		return -1
	}
	return ps[clamp(lt.sel, 0, len(ps)-1)]
}

func (lt lintTab) fit(height int) lintTab {
	pos := lt.pos()
	if pos > 0 && lt.entries[pos-1].finding == nil {
		pos--
	}
	c := cursor{pos: pos, offset: lt.offset}.visible(height, len(lt.entries))
	lt.offset = c.offset
	return lt
}

func (m Model) lintKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	switch {
	case key.Matches(msg, k.up):
		m.lint = m.lint.move(-1)
	case key.Matches(msg, k.down):
		m.lint = m.lint.move(1)
	case key.Matches(msg, k.jump):
		return m.jumpToFinding()
	}
	return m.resized(), nil
}

func (m Model) jumpToFinding() (tea.Model, tea.Cmd) {
	pos := m.lint.pos()
	if pos < 0 {
		return m, nil
	}
	f := m.lint.entries[pos].finding
	mt := &m.migrations
	mt.filter.SetValue("")
	mt.plan = nil
	for i, idx := range mt.visible() {
		if mt.rows[idx].Migration.ID() == f.Migration.ID() {
			mt.cur = mt.cur.to(i, len(mt.rows))
		}
	}
	mt.markID, mt.markLine = f.Migration.ID(), f.Line
	previewH := m.bodyHeight() - m.migrationListHeight(m.bodyHeight()) - 2
	mt.previewOffset = max(f.Line-previewH/2, 0)
	m.tab = tabMigrations
	return m.resized(), nil
}

func (m Model) lintView(h int) string {
	t := m.theme
	lt := m.lint
	switch {
	case lt.err != nil:
		return block([]string{t.Fail.Render(lt.err.Error())}, m.width, h)
	case !lt.ready:
		return block([]string{m.spinner.View() + " " + m.note("Linting pending migrations…")}, m.width, h)
	case len(lt.findings) == 0:
		return block([]string{t.OK.Render("✓ No findings in pending migrations.")}, m.width, h)
	}
	var lines []string
	for i, e := range lt.entries {
		if e.finding == nil {
			lines = append(lines, t.Header.Render(e.rule))
			continue
		}
		f := e.finding
		g := render.GlyphWarn
		if f.Severity == migrateport.Error {
			g = render.GlyphFail
		}
		where := fmt.Sprintf("%s:%d", f.Migration.ID(), f.Line)
		plain := fmt.Sprintf("%s %-7s %s  %s", g.Symbol, f.Severity, where, f.Message)
		styled := t.Glyph(g) + " " + t.Tone(g.Tone).Render(fmt.Sprintf("%-7s", f.Severity)) + " " + t.Muted.Render(where) + "  " + t.Text.Render(f.Message)
		pos := -1
		if i == lt.pos() {
			pos = 0
		}
		lines = append(lines, m.rows([]string{styled}, []string{plain}, pos)...)
	}
	return block(window(lines, lt.offset, h), m.width, h)
}

type doctorTab struct {
	report doctor.Report
	ready  bool
	offset int
}

func (dt doctorTab) loaded(msg doctorMsg) doctorTab {
	dt.report, dt.ready = msg.report, true
	return dt
}

func (m Model) doctorKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	switch {
	case key.Matches(msg, k.up):
		m.doctor.offset = max(m.doctor.offset-1, 0)
	case key.Matches(msg, k.down):
		m.doctor.offset++
	}
	return m, nil
}

func (m Model) doctorView(h int) string {
	t := m.theme
	switch {
	case m.opts.Facts == nil:
		return block([]string{m.note("This application provides no database facts, so there is nothing to check.")}, m.width, h)
	case !m.doctor.ready:
		return block([]string{m.spinner.View() + " " + m.note("Running checks…")}, m.width, h)
	}
	text := doctor.Text(m.doctor.report, t, m.width, true)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	stamp := t.Faint.Render(fmt.Sprintf("checked %s · refreshes every %s", m.doctor.report.At.Local().Format("15:04:05"), render.Duration(m.opts.RefreshEvery)))
	lines = append(lines, "", stamp)
	offset := clamp(m.doctor.offset, 0, max(len(lines)-h, 0))
	return block(window(lines, offset, h), m.width, h)
}
