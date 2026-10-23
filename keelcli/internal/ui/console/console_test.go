package console

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/fake"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
	"github.com/emoss08/keel/keelcli/seedport"
)

var t0 = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

type driver struct {
	t *testing.T
	m Model
}

func newDriver(t *testing.T, opts fake.Options, width, height int) *driver {
	t.Helper()
	opts.Now = func() time.Time { return t0 }
	e := fake.New(opts)
	m := New(Options{
		Name: "trenova-devdb", Migrations: e.Migrations(), Seeds: e.Seeds(), Facts: e.Facts(),
		Theme: theme.New(true), Now: func() time.Time { return t0 }, Context: context.Background(),
	})
	d := &driver{t: t, m: m}
	d.send(tea.WindowSizeMsg{Width: width, Height: height})
	d.do(m.refreshAll())
	return d
}

func (d *driver) send(msg tea.Msg) {
	next, cmd := d.m.Update(msg)
	d.m = next.(Model)
	d.do(cmd)
}

func (d *driver) do(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(50 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			d.do(c)
		}
	default:
		if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeOf(tea.Cmd(nil)) {
			for i := range v.Len() {
				d.do(v.Index(i).Interface().(tea.Cmd))
			}
			return
		}
		d.send(msg)
	}
}

func (d *driver) keys(keys ...string) {
	for _, k := range keys {
		d.send(keyMsg(k))
	}
}

func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	r := []rune(k)[0]
	return tea.KeyPressMsg{Code: r, Text: k}
}

func (d *driver) screen() string {
	return ansi.Strip(d.m.Render())
}

func (d *driver) expect(wants ...string) {
	d.t.Helper()
	s := d.screen()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			d.t.Fatalf("screen lacks %q:\n%s", w, s)
		}
	}
}

func TestTabsRender(t *testing.T) {
	d := newDriver(t, fake.Options{}, 100, 32)
	d.expect("trenova-devdb", "development", "v20261024150000", "postgres://trenova:***@localhost:5432/trenova",
		"1 Migrations (4)", "2 Seeds (7)", "4 Lint (3)", "edi_partner_index", "--keel:baseline", "u apply all")
	d.keys("2")
	d.expect("SEED", "└─ ifta_jurisdictions", "organization_role_permissions", "input changed (data/hazmat.yaml)", "legacy_rate_codes")
	d.keys("3")
	d.expect("keel_tpl_9f2c41d0", "current", "stale", "48 MB")
	d.keys("4")
	d.expect("not-null-default", "index-concurrently", "20261025120000_rate_tables:10")
	d.keys("5")
	d.expect("plan_cache_mode", "Lock holders", "report-runner", "Long-running queries")
	for i, line := range strings.Split(d.m.Render(), "\n") {
		if w := ansi.StringWidth(line); w > 100 {
			t.Fatalf("line %d is %d cells", i, w)
		}
	}
	if n := strings.Count(d.m.Render(), "\n") + 1; n != 32 {
		t.Fatalf("screen is %d lines, want 32", n)
	}
}

func TestNarrowAndResize(t *testing.T) {
	d := newDriver(t, fake.Options{}, 80, 24)
	for _, size := range [][2]int{{80, 24}, {60, 20}, {120, 40}} {
		d.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, tb := range []string{"1", "2", "3", "4", "5"} {
			d.keys(tb)
			lines := strings.Split(d.m.Render(), "\n")
			if len(lines) != size[1] {
				t.Fatalf("%dx%d tab %s: %d lines", size[0], size[1], tb, len(lines))
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Fatalf("%dx%d tab %s line %d: %d cells: %q", size[0], size[1], tb, i, w, ansi.Strip(l))
				}
			}
		}
	}
}

func TestFilterAndPreview(t *testing.T) {
	d := newDriver(t, fake.Options{}, 100, 32)
	d.keys("/", "r", "a", "t", "e", "_", "t", "enter")
	s := d.screen()
	if !strings.Contains(s, "1 of 7") || strings.Contains(s, "shipment_holds") {
		t.Fatalf("filter:\n%s", s)
	}
	d.expect("ALTER TABLE shipments ADD COLUMN currency text NOT NULL")
	d.keys("esc")
	d.expect("shipment_holds")
	d.keys("p")
	d.expect("PLAN", "Would apply 4 migrations", "SELECT trenova_rls.reconcile()")
}

func TestLintJumpsToTheLine(t *testing.T) {
	d := newDriver(t, fake.Options{}, 100, 32)
	d.keys("4", "j", "enter")
	if d.m.tab != tabMigrations {
		t.Fatalf("tab %d", d.m.tab)
	}
	d.expect("20261025120000_rate_tables · core · pending", " 9 ▶ALTER TABLE shipments ADD COLUMN currency text NOT NULL")
}

func TestApplyRunsInline(t *testing.T) {
	d := newDriver(t, fake.Options{Tenants: 50}, 100, 40)
	d.keys("u")
	if d.m.run == nil {
		t.Fatal("no run")
	}
	deadline := time.Now().Add(10 * time.Second)
	for d.m.run.Running() && time.Now().Before(deadline) {
		d.do(func() tea.Msg { return nil })
		time.Sleep(10 * time.Millisecond)
	}
	d.expect("Summary", "4 migrations applied", "Run finished.")
	d.keys("enter")
	if d.m.run != nil {
		t.Fatal("enter did not close the run")
	}
	d.expect("1 Migrations ", "v20261026080000")
	if strings.Contains(d.screen(), "Migrations (") {
		t.Fatalf("migrations still due:\n%s", d.screen())
	}
}

func TestSeedSelectionAndForce(t *testing.T) {
	d := newDriver(t, fake.Options{Tenants: 50}, 100, 40)
	d.keys("2", "space")
	d.expect("1 selected", "[x]")
	d.keys("p")
	d.expect("PLAN", "excluded")
	d.keys("esc", "f")
	if d.m.run == nil {
		t.Fatal("force did not start a run")
	}
	for i := 0; d.m.run.Running() && i < 500; i++ {
		d.do(func() tea.Msg { return nil })
		time.Sleep(5 * time.Millisecond)
	}
	d.expect("1 seed applied", "us_states")
}

func TestConfirmAndProductionRefusal(t *testing.T) {
	d := newDriver(t, fake.Options{}, 100, 32)
	d.keys("3", "x")
	if d.m.form == nil {
		t.Fatal("prune did not ask")
	}
	d.expect("Drop 2 stale templates?", "Environment: development", "keel_tpl_51ab07e3")
	d.keys("esc")
	if d.m.form != nil {
		t.Fatal("esc did not close the form")
	}
	d.expect("Cancelled.", "keel_tpl_51ab07e3")
	d.keys("x", "y")
	d.expect("Dropped 2 templates")
	if strings.Contains(d.screen(), "keel_tpl_51ab07e3") {
		t.Fatalf("stale template still listed:\n%s", d.screen())
	}

	p := newDriver(t, fake.Options{Environment: seedport.Production}, 100, 32)
	p.expect("PRODUCTION")
	p.keys("3", "R")
	if p.m.form != nil {
		t.Fatal("reset asked instead of refusing")
	}
	p.expect("refusing to reset the database", "is marked production")
	p.keys("x")
	p.expect("-yes-production to proceed")
}

func TestCreateFromTheForm(t *testing.T) {
	d := newDriver(t, fake.Options{}, 100, 32)
	d.keys("c")
	if d.m.form == nil {
		t.Fatal("create did not open the form")
	}
	for _, r := range "add_rate_index" {
		d.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	d.keys("enter", "down", "enter")
	d.expect("Created schema/migrations/20261027093000_add_rate_index.sql", "add_rate_index", "--keel:notx")
}

func TestQuit(t *testing.T) {
	d := newDriver(t, fake.Options{}, 100, 32)
	next, cmd := d.m.Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("q returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("q did not quit")
	}
	if v := next.(Model).View(); v.Content != "" || !v.AltScreen {
		t.Fatal("quitting view should be empty on the alt screen")
	}
}
