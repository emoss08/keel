package console

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/ui/live"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
	"github.com/emoss08/keel/keelcli/seedport"
)

type node struct {
	seed   seedport.Seed
	status seedport.Status
	prefix string
}

type seedsTab struct {
	nodes     []node
	err       error
	ready     bool
	cur       cursor
	filter    textinput.Model
	filtering bool
	selected  map[string]bool
	plan      []seedport.PlanItem
	planErr   error
}

func newSeedsTab(t theme.Theme) seedsTab {
	return seedsTab{filter: newFilter(t), selected: map[string]bool{}}
}

func (st seedsTab) loaded(msg seedsMsg) seedsTab {
	st.err, st.ready = msg.err, true
	if msg.err == nil {
		st.nodes = tree(msg.graph, msg.status)
	}
	return st
}

func (st seedsTab) planned(msg seedPlanMsg) seedsTab {
	st.plan, st.planErr = msg.items, msg.err
	return st
}

func (st seedsTab) due() int {
	n := 0
	for _, nd := range st.nodes {
		if nd.status.State == seedport.Due || nd.status.State == seedport.Partial {
			n++
		}
	}
	return n
}

func tree(graph []seedport.Seed, status []seedport.Status) []node {
	byName := map[string]seedport.Status{}
	for _, s := range status {
		byName[s.Seed.Name] = s
	}
	depth := map[string]int{}
	var depthOf func(s seedport.Seed) int
	index := map[string]seedport.Seed{}
	for _, s := range graph {
		index[s.Name] = s
	}
	depthOf = func(s seedport.Seed) int {
		if d, ok := depth[s.Name]; ok {
			return d
		}
		depth[s.Name] = 0
		d := 0
		for _, a := range s.After {
			if p, ok := index[a]; ok {
				d = max(d, depthOf(p)+1)
			}
		}
		depth[s.Name] = d
		return d
	}
	children := map[string][]seedport.Seed{}
	var roots []seedport.Seed
	for _, s := range graph {
		best := ""
		for _, a := range s.After {
			if _, ok := index[a]; ok && (best == "" || depthOf(index[a]) > depthOf(index[best])) {
				best = a
			}
		}
		if best == "" {
			roots = append(roots, s)
			continue
		}
		children[best] = append(children[best], s)
	}
	var out []node
	var visit func(s seedport.Seed, lead string, last, root bool)
	visit = func(s seedport.Seed, lead string, last, root bool) {
		prefix, next := "", ""
		if !root {
			prefix, next = lead+"├─ ", lead+"│  "
			if last {
				prefix, next = lead+"└─ ", lead+"   "
			}
		}
		out = append(out, node{seed: s, status: byName[s.Name], prefix: prefix})
		kids := children[s.Name]
		for i, c := range kids {
			visit(c, next, i == len(kids)-1, false)
		}
	}
	for _, r := range roots {
		visit(r, "", true, true)
	}
	for _, s := range status {
		if s.State == seedport.Orphaned {
			out = append(out, node{seed: s.Seed, status: s})
		}
	}
	return out
}

func (st seedsTab) visible() []int {
	q := strings.ToLower(strings.TrimSpace(st.filter.Value()))
	var out []int
	for i, n := range st.nodes {
		hay := strings.ToLower(n.seed.Name + " " + n.seed.Kind.String() + " " + n.status.State.String() + " " + render.Reason(n.status.Reason))
		if q == "" || strings.Contains(hay, q) {
			out = append(out, i)
		}
	}
	return out
}

func (st seedsTab) current() (node, bool) {
	vis := st.visible()
	if len(vis) == 0 {
		return node{}, false
	}
	return st.nodes[vis[clamp(st.cur.pos, 0, len(vis)-1)]], true
}

func (st seedsTab) fit(height int) seedsTab {
	st.cur = st.cur.visible(height-1, len(st.visible()))
	return st
}

func (st seedsTab) endFilter(clear bool) seedsTab {
	st.filtering = false
	st.filter.Blur()
	if clear {
		st.filter.SetValue("")
	}
	st.cur = st.cur.to(0, len(st.visible()))
	return st
}

func (st seedsTab) selection() []string {
	var out []string
	for _, n := range st.nodes {
		if st.selected[n.seed.Name] {
			out = append(out, n.seed.Name)
		}
	}
	return out
}

func (m Model) seedListHeight(body int) int {
	return clamp(body*3/5, 4, max(body-5, 4))
}

func (m Model) seedsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	st := &m.seeds
	n := len(st.visible())
	switch {
	case key.Matches(msg, k.up):
		st.cur = st.cur.move(-1, n)
	case key.Matches(msg, k.down):
		st.cur = st.cur.move(1, n)
	case key.Matches(msg, k.top):
		st.cur = st.cur.to(0, n)
	case key.Matches(msg, k.bottom):
		st.cur = st.cur.to(n-1, n)
	case key.Matches(msg, k.filter):
		st.filtering = true
		return m, st.filter.Focus()
	case key.Matches(msg, k.clear):
		st.selected, st.plan, st.planErr = map[string]bool{}, nil, nil
		st.filter.SetValue("")
	case key.Matches(msg, k.toggle):
		if nd, ok := st.current(); ok && nd.status.State != seedport.Orphaned {
			sel := map[string]bool{}
			for k, v := range st.selected {
				sel[k] = v
			}
			sel[nd.seed.Name] = !sel[nd.seed.Name]
			st.selected = sel
			st.cur = st.cur.move(1, n)
		}
	case key.Matches(msg, k.plan):
		return m, m.loadSeedPlan(seedport.PlanOptions{Only: st.selection()})
	case key.Matches(msg, k.applySeeds):
		return m.startRun(live.SeedJob(m.opts.Seeds, seedport.ApplyOptions{Only: st.selection()}))
	case key.Matches(msg, k.force):
		only := st.selection()
		if len(only) == 0 {
			if nd, ok := st.current(); ok && nd.status.State != seedport.Orphaned {
				only = []string{nd.seed.Name}
			}
		}
		if len(only) == 0 {
			return m, nil
		}
		return m.startRun(live.SeedJob(m.opts.Seeds, seedport.ApplyOptions{Only: only, Force: true}))
	}
	return m.resized(), nil
}

func (m Model) seedsView(h int) string {
	t := m.theme
	st := m.seeds
	switch {
	case st.err != nil:
		return block([]string{t.Fail.Render(st.err.Error())}, m.width, h)
	case !st.ready:
		return block([]string{m.spinner.View() + " " + m.note("Loading seeds…")}, m.width, h)
	}
	listH := m.seedListHeight(h)
	lines := m.seedList(listH)
	lines = append(lines, t.Rule.Render(strings.Repeat("─", m.width)))
	lines = append(lines, m.seedDetail(h-listH-1)...)
	return block(lines, m.width, h)
}

func (m Model) seedList(h int) []string {
	t := m.theme
	st := m.seeds
	vis := st.visible()
	tbl := render.Table{Width: m.width - gutter, Columns: []render.Column{
		{Title: " ", Max: 3},
		{Title: " ", Max: 1},
		{Title: "SEED", Min: 14, Max: 44, Flex: true},
		{Title: "KIND", Drop: 2},
		{Title: "STATE", Min: 7},
		{Title: "REASON", Min: 10, Max: 40, Flex: true},
		{Title: "ROWS", Right: true, Drop: 1},
		{Title: "TENANTS", Right: true, Drop: 3},
	}}
	for _, idx := range vis {
		n := st.nodes[idx]
		box := "[ ]"
		if st.selected[n.seed.Name] {
			box = "[x]"
		}
		if n.status.State == seedport.Orphaned {
			box = "   "
		}
		g := render.SeedGlyph(n.status.State)
		rows, tenants := "", ""
		if n.status.Rows > 0 {
			rows = render.Count(n.status.Rows)
		}
		if n.seed.PerTenant {
			tenants = render.Count(int64(n.status.Tenants))
		}
		stateTone := g.Tone
		if stateTone == render.OK {
			stateTone = render.Normal
		}
		tbl.Rows = append(tbl.Rows, []render.Cell{
			render.C(box, render.Accent), render.C(g.Symbol, g.Tone), render.C(n.prefix+n.seed.Name, render.Normal),
			render.C(render.SeedKind(n.seed), render.Muted), render.C(n.status.State.String(), stateTone),
			render.C(render.Reason(n.status.Reason), render.ReasonTone(n.status.Reason)), render.C(rows, render.Muted),
			render.C(tenants, render.Muted),
		})
	}
	styled, plain := tbl.Lines(t), tbl.Lines(render.Plain)
	head := "  " + styled[0]
	if st.filtering || st.filter.Value() != "" {
		head = st.filter.View() + t.Faint.Render(fmt.Sprintf("  %d of %d", len(vis), len(st.nodes)))
	} else if sel := len(st.selection()); sel > 0 {
		head = "  " + render.Fit(styled[0], m.width-16) + t.Accent.Render(fmt.Sprintf("  %d selected", sel))
	}
	body := m.rows(styled[1:], plain[1:], st.cur.pos)
	return append([]string{head}, window(body, st.cur.offset, h-1)...)
}

func (m Model) seedDetail(h int) []string {
	t := m.theme
	st := m.seeds
	if st.planErr != nil {
		return []string{t.Fail.Render(st.planErr.Error())}
	}
	if st.plan != nil {
		tbl := render.SeedPlanTable(st.plan, true)
		tbl.Width = m.width
		return window(append([]string{t.Header.Render("PLAN") + t.Faint.Render("  esc clears")}, tbl.Lines(t)...), 0, h)
	}
	n, ok := st.current()
	if !ok {
		return nil
	}
	s := n.status
	lines := []string{t.Header.Render(n.seed.Name) + t.Faint.Render(" · "+render.SeedKind(n.seed)+" · "+s.State.String())}
	if n.seed.Description != "" {
		lines = append(lines, t.Text.Render(n.seed.Description))
	}
	if len(n.seed.After) > 0 {
		lines = append(lines, t.Muted.Render("after "+strings.Join(n.seed.After, ", ")))
	}
	lines = append(lines, t.Muted.Render("reason: ")+t.Tone(render.ReasonTone(s.Reason)).Render(render.Reason(s.Reason)))
	if !s.AppliedAt.IsZero() {
		detail := fmt.Sprintf("applied %s in %s", render.Time(s.AppliedAt), render.Duration(s.Duration))
		if s.Rows > 0 {
			detail += " · " + render.Plural(int(s.Rows), "row", "rows")
		}
		lines = append(lines, t.Muted.Render(detail))
	}
	if n.seed.PerTenant {
		lines = append(lines, t.Muted.Render(fmt.Sprintf("tenants recorded: %s", render.Count(int64(s.Tenants)))))
	}
	return window(lines, 0, h)
}
