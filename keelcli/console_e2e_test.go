package keelcli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/internal/ui/console"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
)

type screen struct {
	mu   sync.Mutex
	text string
}

func (s *screen) set(text string) {
	s.mu.Lock()
	s.text = text
	s.mu.Unlock()
}

func (s *screen) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text
}

type probe struct {
	m      console.Model
	screen *screen
}

func (p probe) Init() tea.Cmd { return p.m.Init() }

func (p probe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := p.m.Update(msg)
	p.m = next.(console.Model)
	p.screen.set(ansi.Strip(p.m.Render()))
	return p, cmd
}

func (p probe) View() tea.View { return p.m.View() }

type consoleRun struct {
	t      *testing.T
	p      *tea.Program
	screen *screen
	done   chan struct{}
	err    error
}

func startConsole(t *testing.T, app App) *consoleRun {
	t.Helper()
	s := newSession(&app, streams{now: time.Now})
	if err := s.setup(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := console.New(console.Options{
		Name: app.Name, Migrations: s.migrations(), Seeds: s.seeds(), Facts: s.facts(), Extensions: app.Extensions,
		Theme: theme.New(true), Now: time.Now, Context: ctx, RefreshEvery: time.Hour,
	})
	sc := &screen{}
	p := tea.NewProgram(probe{m: m, screen: sc}, tea.WithInput(nil), tea.WithOutput(discard{}),
		tea.WithWindowSize(110, 34), tea.WithoutSignalHandler())
	r := &consoleRun{t: t, p: p, screen: sc, done: make(chan struct{})}
	go func() {
		_, r.err = p.Run()
		close(r.done)
	}()
	t.Cleanup(func() {
		p.Quit()
		<-r.done
		cancel()
		s.close()
	})
	return r
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func (r *consoleRun) wait(wants ...string) string {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		text := r.screen.get()
		missing := ""
		for _, w := range wants {
			if !strings.Contains(text, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			return text
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("console never showed %q:\n%s", missing, text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *consoleRun) press(keys ...string) {
	for _, k := range keys {
		switch k {
		case "enter":
			r.p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
		default:
			r.p.Send(tea.KeyPressMsg{Code: []rune(k)[0], Text: k})
		}
	}
}

func TestSampleConsole(t *testing.T) {
	s := newSample(t)
	c := startConsole(t, s.app)
	c.wait(s.name, "1 Migrations (4)", "2 Seeds", "5 Doctor", "item_indexes", "pending", "CREATE TABLE tenants")

	c.press("u")
	c.wait("Run finished.", "Summary", "4 migrations applied")
	c.press("enter")
	c.wait("v20260103000000", "applied")

	c.press("2")
	c.wait("states", "tenant_settings", "demo_items", "new")
	c.press("a")
	c.wait("Run finished.", "3 seeds applied")
	c.press("enter")
	c.wait("up to date", "fixture applied")

	c.press("5")
	c.wait("connection", "migrations       pass    up to date at 20260103000000")

	c.press("3", "b")
	c.wait("keel_tpl_", "current")
	c.press("q")
	<-c.done
	if c.err != nil {
		t.Fatal(c.err)
	}
}

func TestSampleConsoleWithoutSeeds(t *testing.T) {
	s := newSample(t)
	s.app.Seeds = nil
	c := startConsole(t, s.app)
	text := c.wait("1 Migrations (4)", "2 Templates", "3 Lint", "4 Doctor")
	if strings.Contains(text, "Seeds") {
		t.Fatalf("console shows a seeds tab without seeds:\n%s", text)
	}
	c.press("u")
	c.wait("Run finished.", "4 migrations applied")
}
