package keelcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	xterm "github.com/charmbracelet/x/term"

	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/term"
	"github.com/emoss08/keel/keelcli/internal/ui/prompt"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
)

type globals struct {
	json          bool
	plain         bool
	noColor       bool
	yes           bool
	yesProduction bool
}

type session struct {
	app   *App
	st    streams
	g     globals
	theme *theme.Theme
}

func newSession(app *App, st streams) *session {
	return &session{app: app, st: st}
}

func (s *session) bindGlobals(fs *flag.FlagSet) {
	fs.BoolVar(&s.g.json, "json", s.g.json, "write JSON: one document per result, one line per event")
	fs.BoolVar(&s.g.plain, "plain", s.g.plain, "write plain text even in a terminal")
	fs.BoolVar(&s.g.noColor, "no-color", s.g.noColor, "do not colour output (NO_COLOR also works)")
	fs.BoolVar(&s.g.yes, "yes", s.g.yes, "answer yes to confirmations")
	fs.BoolVar(&s.g.yesProduction, "yes-production", s.g.yesProduction, "allow destructive commands on a production database")
}

func (s *session) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s.bindGlobals(fs)
	return fs
}

func (s *session) parse(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return errHelp
	case err != nil:
		return Usagef("keelcli: %s: %v", fs.Name(), err)
	}
	return nil
}

var errHelp = errors.New("keelcli: help")

func (s *session) interactive() bool {
	return !s.g.json && !s.g.plain && s.st.inTTY && s.st.outTTY
}

func (s *session) canPrompt() bool {
	return !s.g.json && s.st.inTTY && s.st.outTTY
}

func (s *session) noColor() bool {
	if s.g.noColor {
		return true
	}
	for _, kv := range s.st.environ {
		if v, ok := strings.CutPrefix(kv, "NO_COLOR="); ok && v != "" {
			return true
		}
	}
	return false
}

func (s *session) programTheme() theme.Theme {
	if s.theme != nil {
		return *s.theme
	}
	return theme.New(true)
}

func (s *session) currentTheme() theme.Theme {
	if s.theme == nil {
		dark := true
		in, inOK := s.st.in.(xterm.File)
		out, outOK := s.st.out.(xterm.File)
		if inOK && outOK && s.st.inTTY && s.st.outTTY {
			dark = lipgloss.HasDarkBackground(in, out)
		}
		t := theme.New(dark)
		s.theme = &t
	}
	return *s.theme
}

func (s *session) painter() render.Painter {
	if !s.interactive() {
		return render.Plain
	}
	return s.currentTheme()
}

func (s *session) width() int {
	if !s.interactive() {
		return 0
	}
	return s.st.width
}

func (s *session) out() io.Writer {
	if !s.interactive() {
		return s.st.out
	}
	w := colorprofile.NewWriter(s.st.out, s.st.environ)
	if s.noColor() {
		w.Profile = colorprofile.Ascii
	}
	return w
}

const (
	inlineHeight  = 200
	consoleHeight = 40
)

func (s *session) programOptions(height int) []tea.ProgramOption {
	opts := []tea.ProgramOption{
		tea.WithInput(s.st.in),
		tea.WithOutput(s.st.out),
		tea.WithEnvironment(s.st.environ),
		tea.WithoutSignalHandler(),
	}
	if s.noColor() {
		opts = append(opts, tea.WithColorProfile(colorprofile.Ascii))
	}
	if !term.IsTerminal(s.st.out) {
		opts = append(opts, tea.WithWindowSize(max(s.st.width, 80), height))
	}
	return opts
}

func (s *session) emit(doc any, text func(p render.Painter, width int) string) error {
	if s.g.json {
		return render.NewJSON(s.st.out).Write(doc)
	}
	_, err := io.WriteString(s.out(), text(s.painter(), s.width()))
	return err
}

func (s *session) say(tone render.Tone, format string, args ...any) error {
	if s.g.json {
		return nil
	}
	_, err := fmt.Fprintln(s.out(), s.painter().Paint(tone, fmt.Sprintf(format, args...)))
	return err
}

func (s *session) target(ctx context.Context) (guard.Target, error) {
	src := guard.Sources{Facts: s.app.Facts}
	if s.app.Seeds != nil {
		src.Seeds = s.app.Seeds
	}
	return guard.Read(ctx, src)
}

func (s *session) confirm(ctx context.Context, a guard.Action) error {
	t, err := s.target(ctx)
	if err != nil {
		return err
	}
	if err := guard.Check(t, a, s.g.yesProduction); err != nil {
		return err
	}
	if s.g.yes {
		return nil
	}
	if !s.canPrompt() {
		return Usagef("keelcli: %s needs confirmation and there is no terminal to ask on: pass -yes", a.Verb)
	}
	ok := false
	err = prompt.Run(ctx, prompt.Confirm(s.programTheme(), t, a, &ok), s.programOptions(inlineHeight)...)
	if errors.Is(err, prompt.ErrAborted) || err == nil && !ok {
		return fmt.Errorf("%w: %s", guard.ErrDeclined, a.Verb)
	}
	return err
}

func (s *session) checkCommands() error {
	for _, c := range s.app.Commands {
		if slices.ContainsFunc(builtins(s), func(b builtin) bool { return b.name == c.Name }) {
			return fmt.Errorf("keelcli: application command %q reuses a built-in name", c.Name)
		}
	}
	return nil
}
