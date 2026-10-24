// Package console is keelcli's full-screen console: tabs for migrations, seeds, templates, lint
// findings and doctor checks over the same ports the commands call. Applying runs the live view
// inside the console; destructive actions confirm with the same guard as the commands.
package console

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/ui/live"
	"github.com/emoss08/keel/keelcli/internal/ui/prompt"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

type tab uint8

const (
	tabMigrations tab = iota
	tabSeeds
	tabTemplates
	tabLint
	tabDoctor
)

var tabNames = []string{"Migrations", "Seeds", "Templates", "Lint", "Doctor"}

// Options configures the console. Seeds and Facts may be nil; their tabs are then left out.
type Options struct {
	Name          string
	Version       string
	Migrations    migrateport.Engine
	Seeds         seedport.Engine
	Facts         factsport.Facts
	Extensions    []string
	YesProduction bool
	Theme         theme.Theme
	Now           func() time.Time
	Context       context.Context
	RefreshEvery  time.Duration
}

type answer struct {
	ok   bool
	name string
	kind migrateport.CreateKind
}

// Model is the console.
type Model struct {
	opts     Options
	ctx      context.Context
	theme    theme.Theme
	keys     keys
	help     help.Model
	spinner  spinner.Model
	width    int
	height   int
	tab      tab
	showHelp bool

	target    guard.Target
	server    string
	targetErr error

	migrations migrationsTab
	seeds      seedsTab
	templates  templatesTab
	lint       lintTab
	doctor     doctorTab

	form      *huh.Form
	answer    *answer
	onConfirm func(*answer) tea.Cmd
	run       *live.Model
	busy      string
	spinning  bool
	flash     *flash
	quitting  bool
}

// New makes the console.
func New(opts Options) Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.RefreshEvery <= 0 {
		opts.RefreshEvery = 3 * time.Second
	}
	m := Model{
		opts:     opts,
		ctx:      opts.Context,
		theme:    opts.Theme,
		keys:     newKeys(),
		help:     help.New(),
		spinner:  spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(opts.Theme.Spinner)),
		width:    100,
		height:   30,
		spinning: true,
	}
	m.help.Styles = opts.Theme.Help
	m.migrations = newMigrationsTab(opts.Theme)
	m.seeds = newSeedsTab(opts.Theme)
	return m
}

// Init loads every tab and starts the doctor's refresh.
func (m Model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.refreshAll(), m.doctorTick(), m.spinner.Tick, m.watch())
}

func (m Model) waiting() bool {
	return m.busy != "" || !m.migrations.ready || !m.templates.ready || !m.lint.ready ||
		m.opts.Seeds != nil && !m.seeds.ready || m.opts.Facts != nil && !m.doctor.ready
}

func (m Model) setBusy(label string) (Model, tea.Cmd) {
	m.busy, m.flash = label, nil
	if m.spinning {
		return m, nil
	}
	m.spinning = true
	return m, m.spinner.Tick
}

func (m Model) watch() tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		<-ctx.Done()
		return tea.QuitMsg{}
	}
}

func (m Model) setTheme(t theme.Theme) Model {
	m.theme = t
	m.help.Styles = t.Help
	m.spinner.Style = t.Spinner
	m.migrations.filter.SetStyles(filterStyles(t))
	m.seeds.filter.SetStyles(filterStyles(t))
	if m.run != nil {
		r := m.run.SetTheme(t)
		m.run = &r
	}
	return m
}

func (m Model) bodyHeight() int {
	helpHeight := 1
	if m.showHelp {
		helpHeight = strings.Count(m.help.FullHelpView(m.helpKeys().FullHelp()), "\n") + 1
	}
	return max(m.height-4-helpHeight-strings.Count(m.status(), "\n")-1, 3)
}

// Update applies a message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		if m.run != nil {
			r := m.run.SetWidth(msg.Width)
			m.run = &r
		}
		return m.resized(), nil
	case tea.BackgroundColorMsg:
		return m.setTheme(theme.New(msg.IsDark())), nil
	case spinner.TickMsg:
		var cmds []tea.Cmd
		var cmd tea.Cmd
		if msg.ID == m.spinner.ID() {
			if !m.waiting() {
				m.spinning = false
				return m, nil
			}
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}
		if m.run != nil {
			r, cmd := m.run.Update(msg)
			m.run = &r
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	case targetMsg:
		m.target, m.server, m.targetErr = msg.target, msg.server, msg.err
		return m, nil
	case migrationsMsg:
		m.migrations = m.migrations.loaded(msg)
		return m.resized(), nil
	case seedsMsg:
		m.seeds = m.seeds.loaded(msg)
		return m.resized(), nil
	case templatesMsg:
		m.templates = m.templates.loaded(msg)
		return m.resized(), nil
	case lintMsg:
		m.lint = m.lint.loaded(msg)
		return m.resized(), nil
	case doctorMsg:
		m.doctor = m.doctor.loaded(msg)
		return m, nil
	case doctorTickMsg:
		if m.tab == tabDoctor && m.run == nil {
			return m, tea.Batch(m.loadDoctor(), m.doctorTick())
		}
		return m, m.doctorTick()
	case planMsg:
		m.migrations = m.migrations.planned(msg)
		return m, nil
	case seedPlanMsg:
		m.seeds = m.seeds.planned(msg)
		return m, nil
	case resultMsg:
		return m.result(msg)
	case live.FinishedMsg:
		return m.runFinished(msg)
	case tea.MouseWheelMsg:
		if m.form != nil || m.run != nil {
			return m, nil
		}
		return m.wheel(msg), nil
	case tea.KeyPressMsg:
		if m.form == nil {
			return m.key(msg)
		}
	}
	if m.form != nil {
		return m.updateForm(msg)
	}
	if m.run != nil {
		r, cmd := m.run.Update(msg)
		m.run = &r
		return m, cmd
	}
	if m.tab == tabMigrations && m.migrations.filtering {
		var cmd tea.Cmd
		m.migrations.filter, cmd = m.migrations.filter.Update(msg)
		return m, cmd
	}
	if m.tab == tabSeeds && m.seeds.filtering {
		var cmd tea.Cmd
		m.seeds.filter, cmd = m.seeds.filter.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) resized() Model {
	h := m.bodyHeight()
	m.migrations = m.migrations.fit(m.migrationListHeight(h))
	m.seeds = m.seeds.fit(m.seedListHeight(h))
	m.templates.cur = m.templates.cur.visible(h-1, len(m.templates.templates))
	m.lint = m.lint.fit(h)
	return m
}

func (m Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	if m.run != nil {
		return m.runKey(msg)
	}
	if m.filtering() {
		return m.filterKey(msg)
	}
	switch {
	case key.Matches(msg, k.quit):
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, k.help):
		m.showHelp = !m.showHelp
		return m.resized(), nil
	case key.Matches(msg, k.next):
		return m.step(1)
	case key.Matches(msg, k.prev):
		return m.step(-1)
	case key.Matches(msg, k.tab1, k.tab2, k.tab3, k.tab4, k.tab5):
		if i := int(msg.String()[0] - '1'); i < len(m.tabList()) {
			m.tab = m.tabList()[i]
		}
		return m.enterTab()
	case key.Matches(msg, k.refresh):
		m.flash = nil
		return m, m.refreshAll()
	}
	switch m.tab {
	case tabMigrations:
		return m.migrationsKey(msg)
	case tabSeeds:
		return m.seedsKey(msg)
	case tabTemplates:
		return m.templatesKey(msg)
	case tabLint:
		return m.lintKey(msg)
	case tabDoctor:
		return m.doctorKey(msg)
	}
	return m, nil
}

// tabList lists the tabs the console shows, leaving out those without an engine behind them.
func (m Model) tabList() []tab {
	out := []tab{tabMigrations}
	if m.opts.Seeds != nil {
		out = append(out, tabSeeds)
	}
	out = append(out, tabTemplates, tabLint)
	if m.opts.Facts != nil {
		out = append(out, tabDoctor)
	}
	return out
}

// step moves to the next or previous tab.
func (m Model) step(delta int) (tea.Model, tea.Cmd) {
	tabs := m.tabList()
	at := 0
	for i, t := range tabs {
		if t == m.tab {
			at = i
		}
	}
	m.tab = tabs[(at+delta+len(tabs))%len(tabs)]
	return m.enterTab()
}

func (m Model) enterTab() (tea.Model, tea.Cmd) {
	if m.tab == tabDoctor {
		return m, m.loadDoctor()
	}
	return m, nil
}

func (m Model) filtering() bool {
	return m.tab == tabMigrations && m.migrations.filtering || m.tab == tabSeeds && m.seeds.filtering
}

func (m Model) filterKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.tab == tabMigrations {
			m.migrations = m.migrations.endFilter(true)
		} else {
			m.seeds = m.seeds.endFilter(true)
		}
		return m.resized(), nil
	case "enter", "down", "up":
		if m.tab == tabMigrations {
			m.migrations = m.migrations.endFilter(false)
		} else {
			m.seeds = m.seeds.endFilter(false)
		}
		return m.resized(), nil
	}
	var cmd tea.Cmd
	if m.tab == tabMigrations {
		m.migrations.filter, cmd = m.migrations.filter.Update(msg)
		m.migrations.cur = m.migrations.cur.to(0, len(m.migrations.visible()))
	} else {
		m.seeds.filter, cmd = m.seeds.filter.Update(msg)
		m.seeds.cur = m.seeds.cur.to(0, len(m.seeds.visible()))
	}
	return m.resized(), cmd
}

func (m Model) runKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.run.Running() {
		if key.Matches(msg, m.keys.cancel) {
			r := m.run.Cancel()
			m.run = &r
		}
		return m, nil
	}
	if key.Matches(msg, m.keys.back, m.keys.clear) || key.Matches(msg, m.keys.quit) {
		m.run = nil
		return m, nil
	}
	return m, nil
}

func (m Model) startRun(jobs ...live.Job) (tea.Model, tea.Cmd) {
	r := live.New(live.Options{Jobs: jobs, Theme: m.theme, Now: m.opts.Now, Context: m.ctx, Life: m.ctx, Width: m.width, Embedded: true})
	m.run = &r
	m.flash = nil
	return m, r.Init()
}

func (m Model) runFinished(msg live.FinishedMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Cancelled:
		m.flash = &flash{text: "Run cancelled; what was left undone is listed below.", tone: render.Warn, at: m.opts.Now()}
	case msg.Err != nil:
		m.flash = &flash{text: "Run failed: " + msg.Err.Error(), tone: render.Fail, at: m.opts.Now()}
	default:
		m.flash = &flash{text: "Run finished.", tone: render.OK, at: m.opts.Now()}
	}
	return m, m.refreshAll()
}

func (m Model) result(msg resultMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		m.flash = &flash{text: msg.err.Error(), tone: render.Fail, at: m.opts.Now()}
		return m, nil
	}
	m.flash = &flash{text: msg.text, tone: render.OK, at: m.opts.Now()}
	if msg.focus != "" {
		m.migrations.focus = msg.focus
	}
	return m, m.refreshAll()
}

func (m Model) confirm(a guard.Action, then func(*answer) tea.Cmd) (tea.Model, tea.Cmd) {
	if err := guard.Check(m.target, a, m.opts.YesProduction); err != nil {
		m.flash = &flash{text: err.Error(), tone: render.Fail, at: m.opts.Now()}
		return m, nil
	}
	m.answer = &answer{}
	f := prompt.Confirm(m.theme, m.target, a, &m.answer.ok).WithWidth(min(72, m.width-8))
	m.form = f
	m.onConfirm = then
	return m, f.Init()
}

func (m Model) openForm(f *huh.Form, ans *answer, then func(*answer) tea.Cmd) (tea.Model, tea.Cmd) {
	m.answer = ans
	m.form = f.WithWidth(min(72, m.width-8))
	m.onConfirm = then
	return m, m.form.Init()
}

func (m Model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.form.Update(msg)
	if f, ok := next.(*huh.Form); ok {
		m.form = f
	}
	switch m.form.State {
	case huh.StateAborted:
		m.form, m.onConfirm = nil, nil
		m.flash = &flash{text: "Cancelled.", tone: render.Muted, at: m.opts.Now()}
		return m, nil
	case huh.StateCompleted:
		then, ans := m.onConfirm, m.answer
		m.form, m.onConfirm = nil, nil
		if !ans.ok {
			m.flash = &flash{text: "Cancelled.", tone: render.Muted, at: m.opts.Now()}
			return m, nil
		}
		var spin tea.Cmd
		m, spin = m.setBusy("Working…")
		return m, tea.Batch(then(ans), spin)
	}
	return m, cmd
}

func (m Model) wheel(msg tea.MouseWheelMsg) Model {
	delta := 0
	switch msg.Button {
	case tea.MouseWheelUp:
		delta = -3
	case tea.MouseWheelDown:
		delta = 3
	default:
		return m
	}
	h := m.bodyHeight()
	switch m.tab {
	case tabMigrations:
		listEnd := 3 + m.migrationListHeight(h)
		if msg.Y > listEnd {
			m.migrations.previewOffset = max(m.migrations.previewOffset+delta, 0)
			return m
		}
		m.migrations.cur = m.migrations.cur.move(delta/3, len(m.migrations.visible()))
	case tabSeeds:
		m.seeds.cur = m.seeds.cur.move(delta/3, len(m.seeds.visible()))
	case tabTemplates:
		m.templates.cur = m.templates.cur.move(delta/3, len(m.templates.templates))
	case tabLint:
		m.lint = m.lint.move(delta / 3)
	case tabDoctor:
		m.doctor.offset = max(m.doctor.offset+delta, 0)
	}
	return m.resized()
}

// View renders the console on the alternate screen.
func (m Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = m.opts.Name
	if m.quitting {
		return v
	}
	v.SetContent(m.Render())
	return v
}

// Render is the console's text.
func (m Model) Render() string {
	h := m.bodyHeight()
	body := m.body(h)
	rule := m.theme.Rule.Render(strings.Repeat("─", m.width))
	parts := []string{m.header(), m.tabs(), rule, body, rule, m.status(), m.helpView()}
	return strings.Join(parts, "\n")
}

func (m Model) body(h int) string {
	switch {
	case m.form != nil:
		return lipgloss.Place(m.width, h, lipgloss.Center, lipgloss.Center, m.theme.Modal.Render(m.form.View()))
	case m.run != nil:
		lines := strings.Split(strings.TrimRight(m.run.Render(), "\n"), "\n")
		if len(lines) > h {
			lines = lines[len(lines)-h:]
		}
		return block(lines, m.width, h)
	}
	switch m.tab {
	case tabSeeds:
		return m.seedsView(h)
	case tabTemplates:
		return m.templatesView(h)
	case tabLint:
		return m.lintView(h)
	case tabDoctor:
		return m.doctorView(h)
	default:
		return m.migrationsView(h)
	}
}

func (m Model) header() string {
	t := m.theme
	title := t.Title.Render(m.opts.Name)
	var meta []string
	switch {
	case m.target.Production():
		meta = append(meta, t.Production.Render("PRODUCTION"))
	case m.target.Environment != "":
		meta = append(meta, t.Environment.Render(string(m.target.Environment)))
	default:
		meta = append(meta, t.Faint.Render("unmarked"))
	}
	if schema := m.migrations.schema(); schema != "" {
		meta = append(meta, t.Accent.Render("v"+schema))
	}
	if m.targetErr != nil {
		meta = append(meta, t.Fail.Render(m.targetErr.Error()))
	}
	if m.target.DSN != "" {
		meta = append(meta, t.TitleMeta.Render(m.target.DSN))
	} else if m.target.Database != "" {
		meta = append(meta, t.TitleMeta.Render(m.target.Name()))
	}
	if m.server != "" {
		meta = append(meta, t.Faint.Render("PostgreSQL "+m.server))
	}
	line := title + " " + strings.Join(meta, t.Faint.Render(" · "))
	return fitLines([]string{line}, m.width)[0]
}

func (m Model) tabs() string {
	t := m.theme
	var out []string
	for i, tb := range m.tabList() {
		label := fmt.Sprintf("%d %s", i+1, tabNames[tb])
		if badge := m.tabBadge(tb); badge != "" {
			label += " " + badge
		}
		if tb == m.tab {
			out = append(out, t.ActiveTab.Render(label))
		} else {
			out = append(out, t.Tab.Render(label))
		}
	}
	return fitLines([]string{strings.Join(out, " ")}, m.width)[0]
}

func (m Model) tabBadge(tb tab) string {
	switch tb {
	case tabMigrations:
		if n := m.migrations.due(); n > 0 {
			return fmt.Sprintf("(%d)", n)
		}
	case tabSeeds:
		if n := m.seeds.due(); n > 0 {
			return fmt.Sprintf("(%d)", n)
		}
	case tabLint:
		if n := len(m.lint.findings); n > 0 {
			return fmt.Sprintf("(%d)", n)
		}
	case tabDoctor:
		if m.doctor.report.Failed() {
			return "(!)"
		}
	}
	return ""
}

func (m Model) status() string {
	t := m.theme
	switch {
	case m.busy != "":
		return m.spinner.View() + " " + t.Muted.Render(m.busy)
	case m.flash != nil:
		lines := render.Wrap(m.flash.text, m.width)
		if len(lines) > 2 {
			lines = lines[:2]
		}
		for i, l := range lines {
			lines[i] = t.Tone(m.flash.tone).Render(l)
		}
		return strings.Join(fitLines(lines, m.width), "\n")
	case m.run != nil && m.run.Running():
		return t.Muted.Render("Running; ctrl+c cancels the run.")
	case m.run != nil:
		return t.Muted.Render("Press enter to return.")
	default:
		return ""
	}
}

func (m Model) helpView() string {
	if m.showHelp {
		return m.help.FullHelpView(m.helpKeys().FullHelp())
	}
	return m.help.ShortHelpView(m.helpKeys().ShortHelp())
}

func (m Model) note(text string) string {
	return m.theme.Muted.Render(text)
}
