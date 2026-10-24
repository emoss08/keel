package live

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/migrateport"
)

const defaultWidth = 80

func (m Model) w() int {
	if m.width <= 0 {
		return defaultWidth
	}
	return m.width
}

// View renders the run inline, below the shell prompt.
func (m Model) View() tea.View {
	return tea.NewView(m.Render())
}

// Render is the view's text.
func (m Model) Render() string {
	var parts []string
	for i := range m.sections {
		parts = append(parts, m.renderSection(&m.sections[i]))
	}
	if m.lock != nil && !m.finished {
		parts = append(parts, m.renderLock())
	}
	switch {
	case m.finished:
		parts = append(parts, m.renderSummary())
	case !m.embedded:
		parts = append(parts, m.renderFooter())
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func (m Model) renderSection(s *section) string {
	t := m.theme
	width := m.w()
	var lines []string
	right := m.sectionProgress(s)
	title := t.Strong.Render(s.title)
	gap := width - lipgloss.Width(title) - lipgloss.Width(right)
	lines = append(lines, title+strings.Repeat(" ", max(gap, 1))+right)
	if len(s.rows) == 0 && len(s.skipped) == 0 && s.ended && s.err == nil {
		lines = append(lines, "  "+t.Muted.Render("Nothing to apply."))
	}
	if len(s.rows) == 0 && !s.ended && !m.finished {
		lines = append(lines, "  "+t.Faint.Render("Planning…"))
	}
	if len(s.rows) == 0 && m.finished && !s.ended {
		lines = append(lines, "  "+t.Faint.Render("Not started."))
	}
	table := m.rowsTable(s)
	for i, line := range table.Lines(t) {
		lines = append(lines, line)
		r := s.rows[i]
		if r.state == running && r.tenants > 0 {
			lines = append(lines, m.tenantBar(r))
		}
		if r.state == failed && r.err != nil {
			lines = append(lines, m.errorLines(r.err, 6)...)
		}
	}
	if len(s.skipped) > 0 {
		text := fmt.Sprintf("%d up to date: %s", len(s.skipped), strings.Join(s.skipped, ", "))
		lines = append(lines, "  "+t.Faint.Render(render.GlyphSkipped.Symbol+" "+render.Fit(text, width-4)))
	}
	for _, n := range s.notes {
		lines = append(lines, "  "+t.Faint.Render(render.Fit(n, width-2)))
	}
	if s.after != "" {
		lines = append(lines, "  "+t.Faint.Render(render.Fit(s.after, width-2)))
	}
	return strings.Join(lines, "\n")
}

func (m Model) sectionProgress(s *section) string {
	t := m.theme
	if !s.ended && len(s.rows) == 0 {
		return ""
	}
	finished := 0
	for _, r := range s.rows {
		if r.state == done {
			finished++
		}
	}
	took := s.took
	if !s.ended {
		took = m.now().Sub(s.started)
	}
	return t.Muted.Render(fmt.Sprintf("%d/%d · %s", finished, len(s.rows), render.Duration(took)))
}

func (m Model) rowsTable(s *section) render.Table {
	tbl := render.Table{NoHeader: true, Width: m.w(), Indent: 2, Columns: []render.Column{
		{Title: "", Min: 1, Max: 1},
		{Title: "KEY", Max: 14},
		{Title: "NAME", Min: 10, Max: 34, Flex: true},
		{Title: "DETAIL", Min: 6, Max: 30, Flex: true, Drop: 1},
		{Title: "STATUS", Min: 8, Max: 40, Flex: true},
		{Title: "TIME", Right: true, Min: 5},
	}}
	for _, r := range s.rows {
		g, status := m.rowStatus(r)
		key := r.key
		if key == r.name {
			key = ""
		}
		tbl.Rows = append(tbl.Rows, []render.Cell{
			g,
			render.C(key, render.Muted),
			render.C(r.name, nameTone(r)),
			render.C(r.detail, render.Faint),
			status,
			render.C(m.rowTime(r), render.Muted),
		})
	}
	return tbl
}

func nameTone(r row) render.Tone {
	switch r.state {
	case queued, notRun:
		return render.Muted
	case running:
		return render.Strong
	default:
		return render.Normal
	}
}

func (m Model) rowStatus(r row) (render.Cell, render.Cell) {
	switch r.state {
	case running:
		frame := m.spinner.Spinner.Frames[0]
		if v := ansi.Strip(m.spinner.View()); v != "" {
			frame = v
		}
		return render.C(frame, render.Accent), render.C(runningStatus(r), render.Accent)
	case done:
		return glyphCell(render.GlyphOK), render.C(doneStatus(r), render.Muted)
	case failed:
		text := "failed"
		if r.failedTenant != "" {
			text = "failed at tenant " + r.failedTenant
		}
		return glyphCell(render.GlyphFail), render.C(text, render.Fail)
	case cancelled:
		return glyphCell(render.GlyphStopped), render.C("cancelled", render.Warn)
	case notRun:
		return glyphCell(render.GlyphStopped), render.C("not run", render.Faint)
	case refused:
		return glyphCell(render.GlyphFail), render.C(r.reason, render.Fail)
	default:
		return glyphCell(render.GlyphQueued), render.C(r.reason, render.Faint)
	}
}

func glyphCell(g render.Glyph) render.Cell { return render.C(g.Symbol, g.Tone) }

func runningStatus(r row) string {
	switch {
	case r.retries > 0:
		return fmt.Sprintf("lock timeout · retry %d", r.retries)
	case r.statements > 0:
		return fmt.Sprintf("statement %d/%d", r.statement, r.statements)
	case r.tenants > 0:
		return fmt.Sprintf("%s rows", render.Count(r.rows))
	default:
		return "running"
	}
}

func doneStatus(r row) string {
	var parts []string
	if r.reason != "" && r.rows == 0 {
		parts = append(parts, r.reason)
	}
	if r.rows > 0 {
		parts = append(parts, render.Plural(int(r.rows), "row", "rows"))
	}
	if r.tenants > 0 {
		parts = append(parts, render.Plural(r.tenantsDone, "tenant", "tenants"))
	}
	if r.statements > 0 {
		parts = append(parts, render.Plural(r.statements, "statement", "statements"))
	}
	if r.retries > 0 {
		parts = append(parts, render.Plural(r.retries, "retry", "retries"))
	}
	if len(parts) == 0 {
		return "applied"
	}
	return strings.Join(parts, " · ")
}

func (m Model) rowTime(r row) string {
	switch r.state {
	case running:
		return render.Duration(m.now().Sub(r.started))
	case done, failed, cancelled:
		return render.Duration(r.took)
	default:
		return ""
	}
}

func (m Model) tenantBar(r row) string {
	t := m.theme
	label := fmt.Sprintf(" %s / %s tenants", render.Count(int64(r.tenantsDone)), render.Count(int64(r.tenants)))
	width := min(m.w()-6-lipgloss.Width(label), 48)
	if width < 10 {
		return "    " + t.Muted.Render(strings.TrimSpace(label))
	}
	bar := m.bar
	bar.SetWidth(width)
	pct := 0.0
	if r.tenants > 0 {
		pct = float64(r.tenantsDone) / float64(r.tenants)
	}
	return "    " + bar.ViewAs(pct) + t.Muted.Render(label)
}

func (m Model) errorLines(err error, indent int) []string {
	width := max(m.w()-indent, 20)
	var out []string
	for _, l := range render.Wrap(err.Error(), width) {
		out = append(out, strings.Repeat(" ", indent)+m.theme.Fail.Render(l))
	}
	return out
}

func (m Model) renderLock() string {
	t := m.theme
	l := m.lock
	inner := max(m.w()-4, 20)
	head := t.PanelTitle.Render("⧗ "+l.title) + t.Muted.Render(" · "+joinNonEmpty(l.detail, render.Duration(m.now().Sub(l.since))))
	lines := []string{ansi.Truncate(head, inner, "…")}
	if len(l.blockers) > 0 {
		lines = append(lines, BlockersTable(l.blockers, inner).Lines(t)...)
	} else {
		lines = append(lines, t.Muted.Render("Another run holds the lock; this one starts when it ends."))
	}
	return t.WarnPanel.Width(m.w()).Render(strings.Join(lines, "\n"))
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// BlockersTable lists the backends a lock waits for, fitted to width.
func BlockersTable(bs []migrateport.Blocker, width int) render.Table {
	tbl := render.Table{Width: width, Columns: []render.Column{
		{Title: "PID", Right: true},
		{Title: "APPLICATION", Min: 8, Max: 24, Flex: true},
		{Title: "STATE", Min: 6, Max: 20, Drop: 2},
		{Title: "LOCK", Max: 18, Drop: 3},
		{Title: "AGE", Right: true},
		{Title: "QUERY", Min: 12, Flex: true},
	}}
	for _, b := range bs {
		tone := render.Muted
		if b.Age >= 5*time.Minute {
			tone = render.Warn
		}
		tbl.Rows = append(tbl.Rows, []render.Cell{
			render.C(fmt.Sprint(b.PID), render.Normal), render.C(b.Application, render.Normal),
			render.C(b.State, render.Muted), render.C(b.Mode, render.Faint),
			render.C(render.Duration(b.Age), tone), render.C(render.OneLine(b.Query), render.Faint),
		})
	}
	return tbl
}

func (m Model) renderFooter() string {
	t := m.theme
	if m.cancelling {
		return t.Warn.Render("Cancelling: waiting for the engine to stop the current step…")
	}
	return t.Help.ShortKey.Render(m.cancelKey.Help().Key) + " " + t.Help.ShortDesc.Render(m.cancelKey.Help().Desc)
}

func (m Model) renderSummary() string {
	t := m.theme
	tbl := render.Table{NoHeader: true, Width: m.w(), Indent: 2, Columns: []render.Column{
		{Title: "", Max: 1}, {Title: "PHASE"}, {Title: "RESULT", Min: 10, Flex: true}, {Title: "TIME", Right: true},
	}}
	var undone []string
	for _, s := range m.sections {
		g, result := m.sectionResult(s)
		took := ""
		if s.ended {
			took = render.Duration(s.took)
		}
		tbl.Rows = append(tbl.Rows, []render.Cell{glyphCell(g), render.C(s.title, render.Strong), result, render.C(took, render.Muted)})
		var left []string
		for _, r := range s.rows {
			if r.state == notRun || r.state == cancelled {
				left = append(left, r.id)
			}
		}
		if len(left) > 0 {
			undone = append(undone, fmt.Sprintf("%s: %s", strings.ToLower(s.title), strings.Join(left, ", ")))
		}
		if !s.ended && len(s.rows) == 0 {
			undone = append(undone, strings.ToLower(s.title)+": not started")
		}
	}
	lines := []string{t.Strong.Render("Summary")}
	lines = append(lines, tbl.Lines(t)...)
	if m.err != nil && !m.cancelled {
		lines = append(lines, m.errorLines(m.err, 2)...)
	}
	if len(undone) > 0 {
		lines = append(lines, "", t.Warn.Render("Left undone"))
		for _, u := range undone {
			for _, l := range render.Wrap(u, m.w()-4) {
				lines = append(lines, "  "+t.Muted.Render(l))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) sectionResult(s section) (render.Glyph, render.Cell) {
	noun := func(n int) string { return render.Plural(n, s.noun, s.noun+"s") }
	switch {
	case !s.ended && len(s.rows) == 0:
		return render.GlyphStopped, render.C("not started", render.Faint)
	case m.cancelled && hasState(s, cancelled, notRun):
		return render.GlyphStopped, render.C(fmt.Sprintf("cancelled after %s", noun(count(s, done))), render.Warn)
	case s.err != nil:
		return render.GlyphFail, render.C(fmt.Sprintf("failed after %s", noun(count(s, done))), render.Fail)
	case len(s.rows) == 0:
		return render.GlyphOK, render.C("up to date", render.Muted)
	default:
		return render.GlyphOK, render.C(noun(count(s, done))+" applied", render.OK)
	}
}

func count(s section, st rowState) int {
	n := 0
	for _, r := range s.rows {
		if r.state == st {
			n++
		}
	}
	return n
}

func hasState(s section, states ...rowState) bool {
	for _, r := range s.rows {
		for _, st := range states {
			if r.state == st {
				return true
			}
		}
	}
	return false
}
