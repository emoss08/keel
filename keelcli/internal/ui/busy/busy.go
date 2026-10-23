// Package busy shows a spinner beside a label while one engine call runs, for the commands whose
// call takes a while but reports no progress: template build, squash, reset.
package busy

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/ui/theme"
)

// Options configures Run.
type Options struct {
	Label   string
	Theme   theme.Theme
	Program []tea.ProgramOption
	Now     func() time.Time
}

type doneMsg struct{ err error }

type model struct {
	opts    Options
	spinner spinner.Model
	started time.Time
	cancel  context.CancelFunc
	fn      func() error
	done    bool
	err     error
	took    time.Duration
}

// Run calls fn on its own goroutine and shows the label with a spinner until it returns; ctrl+c
// cancels fn's context.
func Run(ctx context.Context, opts Options, fn func(ctx context.Context) error) error {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := model{
		opts:    opts,
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(opts.Theme.Spinner)),
		started: opts.Now(),
		cancel:  cancel,
		fn:      func() error { return fn(ctx) },
	}
	final, err := tea.NewProgram(m, opts.Program...).Run()
	if err != nil {
		return err
	}
	return final.(model).err
}

// Init starts the spinner and the call.
func (m model) Init() tea.Cmd {
	fn := m.fn
	return tea.Batch(m.spinner.Tick, tea.RequestBackgroundColor, func() tea.Msg { return doneMsg{fn()} })
}

// Update records the result and quits, or cancels on ctrl+c.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		m.opts.Theme = theme.New(msg.IsDark())
		m.spinner.Style = m.opts.Theme.Spinner
	case doneMsg:
		m.done, m.err, m.took = true, msg.err, m.opts.Now().Sub(m.started)
		return m, tea.Quit
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.cancel()
		}
	case spinner.TickMsg:
		if m.done {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// View is the label with a spinner, then with the outcome.
func (m model) View() tea.View {
	t := m.opts.Theme
	switch {
	case !m.done:
		return tea.NewView(m.spinner.View() + " " + m.opts.Label + " " + t.Faint.Render(render.Duration(m.opts.Now().Sub(m.started))) + "\n")
	case m.err != nil:
		return tea.NewView(t.Glyph(render.GlyphFail) + " " + m.opts.Label + "\n")
	default:
		return tea.NewView(t.Glyph(render.GlyphOK) + " " + m.opts.Label + " " + t.Faint.Render(render.Duration(m.took)) + "\n")
	}
}
