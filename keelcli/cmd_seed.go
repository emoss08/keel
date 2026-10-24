package keelcli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/ui/live"
	"github.com/emoss08/keel/keelcli/seedport"
)

func setupSeedApply(fs *flag.FlagSet) runner {
	force := fs.Bool("force", false, "run the named seeds, or all, whatever their fingerprints say")
	dry := fs.Bool("dry-run", false, "print the plan instead of applying it")
	return func(ctx context.Context, s *session, args []string) error {
		if *dry {
			return s.seedPlan(ctx, seedport.PlanOptions{Only: args, Force: *force})
		}
		return s.runJobs(ctx, []live.Job{live.SeedJob(s.seeds(), seedport.ApplyOptions{Only: args, Force: *force})})
	}
}

func setupSeedPlan(fs *flag.FlagSet) runner {
	force := fs.Bool("force", false, "plan as if -force were given to seed apply")
	return func(ctx context.Context, s *session, args []string) error {
		return s.seedPlan(ctx, seedport.PlanOptions{Only: args, Force: *force})
	}
}

func (s *session) seedPlan(ctx context.Context, opts seedport.PlanOptions) error {
	items, err := s.seeds().Plan(ctx, opts)
	if err != nil {
		return err
	}
	return s.emit(render.SeedPlanJSON(items), func(p render.Painter, width int) string {
		t := render.SeedPlanTable(items, !render.IsPlain(p))
		t.Width = width
		runs := 0
		for _, it := range items {
			if it.Reason.Runs() {
				runs++
			}
		}
		return t.String(p) + "\n" + p.Paint(render.Muted, render.Plural(runs, "seed runs", "seeds run")+".") + "\n"
	})
}

func setupSeedStatus(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("seed status", args); err != nil {
			return err
		}
		rows, err := s.seeds().Status(ctx)
		if err != nil {
			return err
		}
		return s.emit(render.SeedStatusJSON(rows), func(p render.Painter, width int) string {
			t := render.SeedStatusTable(rows, !render.IsPlain(p))
			t.Width = width
			return t.String(p)
		})
	}
}

func setupSeedPrune(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("seed prune", args); err != nil {
			return err
		}
		rows, err := s.seeds().Status(ctx)
		if err != nil {
			return err
		}
		var names []string
		for _, r := range rows {
			if r.State == seedport.Orphaned {
				names = append(names, r.Seed.Name)
			}
		}
		if len(names) == 0 {
			return s.emit(render.SeedStatusJSON(nil), func(p render.Painter, _ int) string {
				return p.Paint(render.OK, "Nothing to prune: every recorded seed is registered.") + "\n"
			})
		}
		if err := s.confirm(ctx, guard.Action{
			Verb:   "prune seed records",
			Title:  "Delete the records of " + render.Plural(len(names), "seed", "seeds") + " no longer registered?",
			Detail: []string{"Seeds: " + strings.Join(names, ", "), "Their data stays; only keel_seeds forgets them."},
		}); err != nil {
			return err
		}
		pruned, err := s.seeds().Prune(ctx)
		if err != nil {
			return err
		}
		return s.emit(render.SeedStatusJSON(pruned), func(p render.Painter, _ int) string {
			return fmt.Sprintf("%s %s.\n", p.Paint(render.OK, "Pruned"), render.Plural(len(pruned), "seed record", "seed records"))
		})
	}
}
