package keelcli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/ui/busy"
	"github.com/emoss08/keel/keelcli/internal/ui/live"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

var errCancelled = errors.New("keelcli: cancelled")

func (s *session) runJobs(ctx context.Context, jobs []live.Job) error {
	if s.interactive() {
		return s.runLive(ctx, jobs)
	}
	return s.runPlain(ctx, jobs)
}

type liveProgram struct {
	live live.Model
}

// Init starts the view and asks for the background colour.
func (p liveProgram) Init() tea.Cmd {
	return tea.Batch(p.live.Init(), tea.RequestBackgroundColor)
}

// Update quits when the run ends or on a second ctrl+c, and passes everything else on.
func (p liveProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case live.FinishedMsg:
		return p, tea.Quit
	case tea.KeyPressMsg:
		if key.Matches(msg, p.live.CancelKey()) && p.live.Cancelling() {
			return p, tea.Quit
		}
	}
	var cmd tea.Cmd
	p.live, cmd = p.live.Update(msg)
	return p, cmd
}

// View renders the live view.
func (p liveProgram) View() tea.View {
	return p.live.View()
}

func (s *session) runLive(ctx context.Context, jobs []live.Job) error {
	life, stop := context.WithCancel(ctx)
	defer stop()
	m := live.New(live.Options{
		Jobs: jobs, Theme: s.programTheme(), Now: s.st.now, Context: ctx, Life: life, Width: s.st.width,
	})
	final, err := tea.NewProgram(liveProgram{live: m}, s.programOptions(inlineHeight)...).Run()
	if err != nil {
		return fmt.Errorf("keelcli: %w", err)
	}
	lm := final.(liveProgram).live
	switch {
	case !lm.Finished():
		return reported{errCancelled}
	case lm.Cancelled():
		return reported{errCancelled}
	case lm.Err() != nil:
		return reported{lm.Err()}
	}
	return nil
}

type plainRun struct {
	s       *session
	lines   *render.Lines
	json    *render.JSON
	planned []string
	ended   map[string]bool
}

func (s *session) runPlain(ctx context.Context, jobs []live.Job) error {
	r := &plainRun{s: s, lines: render.NewLines(s.st.out), json: render.NewJSON(s.st.out), ended: map[string]bool{}}
	for _, j := range jobs {
		if err := j.Run(ctx, r.send); err != nil {
			r.undone(jobs, j)
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %v", errCancelled, err)
			}
			return err
		}
	}
	return nil
}

func (r *plainRun) send(msg tea.Msg) {
	switch m := msg.(type) {
	case live.PlanMsg:
		for _, mig := range m.Plan.Migrations {
			r.planned = append(r.planned, mig.ID())
		}
	case live.MigrationMsg:
		r.migration(m.Event)
	case live.SeedMsg:
		r.seed(m.Event)
	}
}

func (r *plainRun) migration(e migrateport.Event) {
	if e.Kind == migrateport.MigrationEnd && e.Err == nil {
		r.ended[e.Migration.ID()] = true
	}
	if r.s.g.json {
		r.json.Migration(e)
		return
	}
	r.lines.Migration(e)
}

func (r *plainRun) seed(e seedport.Event) {
	switch {
	case e.Kind == seedport.Planned && e.Reason.Runs():
		r.planned = append(r.planned, e.Seed.Name)
	case e.Kind == seedport.SeedEnd && e.Err == nil:
		r.ended[e.Seed.Name] = true
	}
	if r.s.g.json {
		r.json.Seed(e)
		return
	}
	r.lines.Seed(e)
}

func (r *plainRun) undone(jobs []live.Job, failed live.Job) {
	if r.s.g.json {
		return
	}
	var left []string
	for _, id := range r.planned {
		if !r.ended[id] {
			left = append(left, id)
		}
	}
	if len(left) > 0 {
		fmt.Fprintf(r.s.st.out, "%-8sleft undone: %s\n", "", strings.Join(left, ", "))
	}
	after := false
	for _, j := range jobs {
		if after {
			fmt.Fprintf(r.s.st.out, "%-8s%s not started\n", "", strings.ToLower(j.Title))
		}
		after = after || j.Title == failed.Title
	}
}

func (s *session) busy(ctx context.Context, label string, fn func(ctx context.Context) error) error {
	if !s.interactive() {
		return fn(ctx)
	}
	return busy.Run(ctx, busy.Options{Label: label, Theme: s.programTheme(), Program: s.programOptions(inlineHeight), Now: s.st.now}, fn)
}
