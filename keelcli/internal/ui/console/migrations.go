package console

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/ui/live"
	"github.com/emoss08/keel/keelcli/internal/ui/prompt"
	"github.com/emoss08/keel/keelcli/internal/ui/sqlview"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
	"github.com/emoss08/keel/keelcli/migrateport"
)

type migrationsTab struct {
	rows          []migrateport.Status
	err           error
	ready         bool
	cur           cursor
	filter        textinput.Model
	filtering     bool
	previewOffset int
	markID        string
	markLine      int
	plan          *migrateport.Plan
	planErr       error
	focus         string
}

func filterStyles(t theme.Theme) textinput.Styles {
	s := textinput.DefaultStyles(t.Dark)
	s.Focused.Prompt = t.Accent
	s.Focused.Text = t.Text
	s.Focused.Placeholder = t.Faint
	s.Blurred.Prompt = t.Muted
	s.Blurred.Text = t.Muted
	s.Cursor.Color = t.Palette.Accent
	s.Cursor.Blink = false
	return s
}

func newFilter(t theme.Theme) textinput.Model {
	f := textinput.New()
	f.Prompt = "/ "
	f.Placeholder = "filter"
	f.SetStyles(filterStyles(t))
	return f
}

func newMigrationsTab(t theme.Theme) migrationsTab {
	return migrationsTab{filter: newFilter(t)}
}

func (mt migrationsTab) loaded(msg migrationsMsg) migrationsTab {
	mt.rows, mt.err, mt.ready = msg.rows, msg.err, true
	if mt.focus != "" {
		for i, idx := range mt.visible() {
			if mt.rows[idx].Migration.ID() == mt.focus || mt.rows[idx].Migration.Version == mt.focus {
				mt.cur.pos = i
			}
		}
		mt.focus = ""
	}
	return mt
}

func (mt migrationsTab) planned(msg planMsg) migrationsTab {
	mt.plan, mt.planErr, mt.previewOffset = &msg.plan, msg.err, 0
	return mt
}

func (mt migrationsTab) schema() string {
	newest := ""
	for _, r := range mt.rows {
		if r.Recorded() && r.Migration.Version > newest {
			newest = r.Migration.Version
		}
	}
	return newest
}

func (mt migrationsTab) due() int {
	n := 0
	for _, r := range mt.rows {
		if r.State == migrateport.Pending || r.State == migrateport.RepeatableDue {
			n++
		}
	}
	return n
}

func (mt migrationsTab) visible() []int {
	q := strings.ToLower(strings.TrimSpace(mt.filter.Value()))
	var out []int
	for i, r := range mt.rows {
		hay := strings.ToLower(strings.Join([]string{r.Migration.ID(), r.Migration.Source, r.State.String(), render.MigrationDetail(r.Migration)}, " "))
		if q == "" || strings.Contains(hay, q) {
			out = append(out, i)
		}
	}
	return out
}

func (mt migrationsTab) selected() (migrateport.Status, bool) {
	vis := mt.visible()
	if len(vis) == 0 {
		return migrateport.Status{}, false
	}
	return mt.rows[vis[clamp(mt.cur.pos, 0, len(vis)-1)]], true
}

func (mt migrationsTab) fit(height int) migrationsTab {
	mt.cur = mt.cur.visible(height-1, len(mt.visible()))
	return mt
}

func (mt migrationsTab) endFilter(clear bool) migrationsTab {
	mt.filtering = false
	mt.filter.Blur()
	if clear {
		mt.filter.SetValue("")
	}
	mt.cur = mt.cur.to(0, len(mt.visible()))
	return mt
}

func (m Model) migrationListHeight(body int) int {
	return clamp(body*2/5, 4, max(body-6, 4))
}

func (m Model) migrationsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	mt := &m.migrations
	n := len(mt.visible())
	switch {
	case key.Matches(msg, k.up):
		mt.cur, mt.previewOffset, mt.plan = mt.cur.move(-1, n), 0, nil
	case key.Matches(msg, k.down):
		mt.cur, mt.previewOffset, mt.plan = mt.cur.move(1, n), 0, nil
	case key.Matches(msg, k.top):
		mt.cur, mt.previewOffset, mt.plan = mt.cur.to(0, n), 0, nil
	case key.Matches(msg, k.bottom):
		mt.cur, mt.previewOffset, mt.plan = mt.cur.to(n-1, n), 0, nil
	case key.Matches(msg, k.pageDown):
		mt.previewOffset += max(m.bodyHeight()-m.migrationListHeight(m.bodyHeight())-3, 1)
	case key.Matches(msg, k.pageUp):
		mt.previewOffset = max(mt.previewOffset-max(m.bodyHeight()-m.migrationListHeight(m.bodyHeight())-3, 1), 0)
	case key.Matches(msg, k.filter):
		mt.filtering = true
		return m, mt.filter.Focus()
	case key.Matches(msg, k.clear):
		mt.plan, mt.markID = nil, ""
		mt.filter.SetValue("")
	case key.Matches(msg, k.enter):
		mt.plan = nil
	case key.Matches(msg, k.plan):
		return m, m.loadPlan()
	case key.Matches(msg, k.apply):
		return m.startRun(live.MigrationJob(m.opts.Migrations, migrateport.UpOptions{}))
	case key.Matches(msg, k.applyTo):
		return m.applyToSelected()
	case key.Matches(msg, k.create):
		ans := &answer{ok: true}
		return m.openForm(prompt.Create(m.theme, &ans.name, &ans.kind), ans, m.createMigration)
	case key.Matches(msg, k.repair):
		return m.repairSelected()
	}
	return m.resized(), nil
}

func (m Model) applyToSelected() (tea.Model, tea.Cmd) {
	s, ok := m.migrations.selected()
	if !ok || s.Migration.Version == "" {
		m.flash = &flash{text: "Select a versioned migration to apply up to it.", tone: render.Warn, at: m.opts.Now()}
		return m, nil
	}
	return m.startRun(live.MigrationJob(m.opts.Migrations, migrateport.UpOptions{To: s.Migration.Version}))
}

func (m Model) createMigration(ans *answer) tea.Cmd {
	eng := m.opts.Migrations
	return m.action(func(ctx context.Context) (string, string, error) {
		c, err := eng.Create(ctx, migrateport.CreateRequest{Name: ans.name, Kind: ans.kind})
		if err != nil {
			return "", "", err
		}
		return "Created " + c.Path, c.Migration.ID(), nil
	})
}

func (m Model) repairSelected() (tea.Model, tea.Cmd) {
	s, ok := m.migrations.selected()
	if !ok || s.State != migrateport.Changed {
		m.flash = &flash{text: "Only a changed migration can be repaired.", tone: render.Warn, at: m.opts.Now()}
		return m, nil
	}
	version, eng := s.Migration.Version, m.opts.Migrations
	return m.confirm(guard.Action{
		Verb:   "repair a migration checksum",
		Title:  "Accept the current checksum of " + s.Migration.ID() + "?",
		Detail: []string{"Only for a comment-only or whitespace edit: the migration does not run again."},
	}, func(*answer) tea.Cmd {
		return m.action(func(ctx context.Context) (string, string, error) {
			r, err := eng.Repair(ctx, version)
			if err != nil {
				return "", "", err
			}
			return fmt.Sprintf("Repaired %s: %s → %s", r.Migration.ID(), short(r.OldChecksum), short(r.NewChecksum)), "", nil
		})
	})
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

func (m Model) migrationsView(h int) string {
	t := m.theme
	mt := m.migrations
	switch {
	case mt.err != nil:
		return block([]string{t.Fail.Render(mt.err.Error())}, m.width, h)
	case !mt.ready:
		return block([]string{m.spinner.View() + " " + m.note("Loading migrations…")}, m.width, h)
	}
	listH := m.migrationListHeight(h)
	lines := m.migrationList(listH)
	lines = append(lines, t.Rule.Render(strings.Repeat("─", m.width)))
	lines = append(lines, m.preview(h-listH-1)...)
	return block(lines, m.width, h)
}

func (m Model) migrationList(h int) []string {
	t := m.theme
	mt := m.migrations
	vis := mt.visible()
	rows := make([]migrateport.Status, len(vis))
	for i, idx := range vis {
		rows[i] = mt.rows[idx]
	}
	tbl := render.MigrationTable(rows, true)
	tbl.Width = m.width - gutter
	styled := tbl.Lines(t)
	plain := tbl.Lines(render.Plain)
	head := "  " + styled[0]
	if mt.filtering || mt.filter.Value() != "" {
		head = mt.filter.View() + t.Faint.Render(fmt.Sprintf("  %d of %d", len(vis), len(mt.rows)))
	}
	body := m.rows(styled[1:], plain[1:], mt.cur.pos)
	if len(vis) == 0 {
		body = append(body, m.note("  No migrations match."))
	}
	return append([]string{head}, window(body, mt.cur.offset, h-1)...)
}

func (m Model) preview(h int) []string {
	t := m.theme
	mt := m.migrations
	if mt.plan != nil || mt.planErr != nil {
		if mt.planErr != nil {
			return []string{t.Fail.Render(mt.planErr.Error())}
		}
		lines := strings.Split(strings.TrimRight(render.PlanText(*mt.plan, false, t, m.width), "\n"), "\n")
		return window(append([]string{t.Header.Render("PLAN") + t.Faint.Render("  esc returns to the SQL")}, lines...), mt.previewOffset, h)
	}
	s, ok := mt.selected()
	if !ok {
		return nil
	}
	mig := s.Migration
	title := t.Header.Render(mig.ID()) + t.Faint.Render(" · "+mig.Source+" · "+s.State.String())
	if d := render.MigrationDetail(mig); d != "" {
		title += t.Faint.Render(" · " + d)
	}
	if s.State == migrateport.Changed {
		title += t.Warn.Render(fmt.Sprintf(" · recorded %s, file %s", short(s.AppliedChecksum), short(mig.Checksum)))
	}
	var body []string
	switch {
	case s.State == migrateport.Missing:
		body = []string{m.note("Recorded as applied, but no source has this file any more.")}
	case mig.Kind == migrateport.Go:
		body = []string{m.note("Go migration: its function runs in the migration's transaction; there is no SQL to show.")}
	case strings.TrimSpace(mig.SQL) == "":
		body = []string{m.note("Empty file.")}
	default:
		mark := 0
		if mt.markID == mig.ID() {
			mark = mt.markLine
		}
		body = sqlview.Render(mig.SQL, t, m.width, mark)
	}
	offset := clamp(mt.previewOffset, 0, max(len(body)-(h-1), 0))
	return append([]string{title}, window(body, offset, h-1)...)
}
