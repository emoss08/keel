package live

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

// Job is one engine call the view runs on its own goroutine. Run delivers the call's events
// through send, which blocks until the view takes them.
type Job struct {
	Title string
	Noun  string
	Run   func(ctx context.Context, send func(tea.Msg)) error
}

// PlanMsg lists the migrations a run will apply, so the view shows them before they start.
type PlanMsg struct {
	Plan migrateport.Plan
}

// MigrationMsg carries one migration event.
type MigrationMsg struct {
	Event migrateport.Event
}

// SeedMsg carries one seed event.
type SeedMsg struct {
	Event seedport.Event
}

// Migrations is the job that plans and applies migrations.
type Migrations interface {
	migrateport.Planner
	migrateport.Runner
}

// MigrationJob plans, then applies migrations up to opts.To.
func MigrationJob(engine Migrations, opts migrateport.UpOptions) Job {
	return Job{
		Title: "Migrations",
		Noun:  "migration",
		Run: func(ctx context.Context, send func(tea.Msg)) error {
			plan, err := engine.Plan(ctx, migrateport.PlanOptions(opts))
			if err != nil {
				return err
			}
			send(PlanMsg{Plan: plan})
			return engine.Up(ctx, opts, func(e migrateport.Event) { send(MigrationMsg{Event: e}) })
		},
	}
}

// SeedJob applies seeds.
func SeedJob(engine seedport.Applier, opts seedport.ApplyOptions) Job {
	return Job{
		Title: "Seeds",
		Noun:  "seed",
		Run: func(ctx context.Context, send func(tea.Msg)) error {
			return engine.Apply(ctx, opts, func(e seedport.Event) { send(SeedMsg{Event: e}) })
		},
	}
}

const maxBatch = 1024

type streamMsg struct {
	msgs []tea.Msg
	ch   <-chan tea.Msg
}

type jobDoneMsg struct {
	job int
	err error
}

func start(ctx, life context.Context, index int, job Job) tea.Cmd {
	return func() tea.Msg {
		ch := make(chan tea.Msg, 512)
		go func() {
			defer close(ch)
			send := func(msg tea.Msg) {
				select {
				case ch <- msg:
				case <-life.Done():
				}
			}
			err := job.Run(ctx, send)
			send(jobDoneMsg{job: index, err: err})
		}()
		return next(ch)()
	}
}

func next(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		batch := []tea.Msg{msg}
		for len(batch) < maxBatch {
			select {
			case msg, ok := <-ch:
				if !ok {
					return streamMsg{msgs: batch, ch: ch}
				}
				batch = append(batch, msg)
			default:
				return streamMsg{msgs: batch, ch: ch}
			}
		}
		return streamMsg{msgs: batch, ch: ch}
	}
}
