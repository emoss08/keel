package console

import (
	"fmt"

	"charm.land/bubbles/v2/key"
)

type keys struct {
	next, prev, tab1, tab2, tab3, tab4, tab5 key.Binding
	up, down, top, bottom, pageUp, pageDown  key.Binding
	filter, clear, enter, help, quit, cancel key.Binding
	refresh                                  key.Binding

	apply, applyTo, plan, create, repair key.Binding
	toggle, applySeeds, force            key.Binding
	build, prune, reset                  key.Binding
	jump                                 key.Binding
	back                                 key.Binding
}

func b(keysAndHelp ...string) key.Binding {
	n := len(keysAndHelp)
	return key.NewBinding(key.WithKeys(keysAndHelp[:n-2]...), key.WithHelp(keysAndHelp[n-2], keysAndHelp[n-1]))
}

func newKeys() keys {
	return keys{
		next:     b("tab", "tab", "next tab"),
		prev:     b("shift+tab", "shift+tab", "previous tab"),
		tab1:     b("1", "1-5", "go to tab"),
		tab2:     b("2", "2", "seeds"),
		tab3:     b("3", "3", "templates"),
		tab4:     b("4", "4", "lint"),
		tab5:     b("5", "5", "doctor"),
		up:       b("up", "k", "↑/k", "up"),
		down:     b("down", "j", "↓/j", "down"),
		top:      b("home", "g", "g", "top"),
		bottom:   b("end", "G", "G", "bottom"),
		pageUp:   b("pgup", "ctrl+u", "pgup", "scroll preview up"),
		pageDown: b("pgdown", "ctrl+d", "pgdn", "scroll preview down"),
		filter:   b("/", "/", "filter"),
		clear:    b("esc", "esc", "clear"),
		enter:    b("enter", "enter", "view SQL"),
		help:     b("?", "?", "help"),
		quit:     b("q", "ctrl+c", "q", "quit"),
		cancel:   b("ctrl+c", "ctrl+c", "cancel run"),
		refresh:  b("r", "r", "refresh"),

		apply:   b("u", "u", "apply all"),
		applyTo: b("U", "U", "apply to selected"),
		plan:    b("p", "p", "plan"),
		create:  b("c", "c", "create"),
		repair:  b("x", "x", "repair checksum"),

		toggle:     b("space", "space", "select"),
		applySeeds: b("a", "a", "apply"),
		force:      b("f", "f", "force"),

		build: b("b", "b", "build"),
		prune: b("x", "x", "prune"),
		reset: b("R", "R", "reset database"),

		jump: b("enter", "enter", "jump to line"),
		back: b("enter", "esc", "enter", "back"),
	}
}

type helpKeys struct {
	short []key.Binding
	full  [][]key.Binding
}

// ShortHelp is the help bar.
func (h helpKeys) ShortHelp() []key.Binding { return h.short }

// FullHelp is the help shown by ?.
func (h helpKeys) FullHelp() [][]key.Binding { return h.full }

func (m Model) helpKeys() helpKeys {
	k := m.keys
	nav := []key.Binding{k.up, k.down, k.top, k.bottom}
	k.tab1.SetHelp(fmt.Sprintf("1-%d", len(m.tabList())), "go to tab")
	global := []key.Binding{k.next, k.tab1, k.refresh, k.help, k.quit}
	if m.run != nil {
		if m.run.Running() {
			return helpKeys{short: []key.Binding{k.cancel}, full: [][]key.Binding{{k.cancel}}}
		}
		return helpKeys{short: []key.Binding{k.back}, full: [][]key.Binding{{k.back}}}
	}
	var actions []key.Binding
	last := k.filter
	switch m.tab {
	case tabMigrations:
		actions = []key.Binding{k.apply, k.applyTo, k.plan, k.create, k.repair, k.enter}
		nav = append(nav, k.filter, k.pageUp, k.pageDown)
	case tabSeeds:
		actions = []key.Binding{k.toggle, k.applySeeds, k.force, k.plan}
		nav = append(nav, k.filter)
	case tabTemplates:
		actions = []key.Binding{k.build, k.prune, k.reset}
		last = k.refresh
	case tabLint:
		actions = []key.Binding{k.jump}
		last = k.refresh
	case tabDoctor:
		last = k.refresh
	}
	short := append(append([]key.Binding{}, actions...), last, k.next, k.help, k.quit)
	return helpKeys{short: short, full: [][]key.Binding{actions, nav, global}}
}
