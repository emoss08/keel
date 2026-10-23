package keelcli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/internal/ui/live"
	"github.com/emoss08/keel/keelcli/internal/ui/prompt"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

type problemsError string

// Error returns the message.
func (e problemsError) Error() string { return string(e) }

// Is matches ErrProblems.
func (e problemsError) Is(target error) bool { return target == ErrProblems }

func problemsf(format string, args ...any) error {
	return problemsError(fmt.Sprintf(format, args...))
}

func noArgs(name string, args []string) error {
	if len(args) > 0 {
		return Usagef("keelcli: %s takes no arguments, got %q", name, strings.Join(args, " "))
	}
	return nil
}

func setupUp(fs *flag.FlagSet) runner {
	to := fs.String("to", "", "stop after migration `version`; seeds are not applied")
	dry := fs.Bool("dry-run", false, "print what would run, with the SQL, instead of running it")
	env := fs.String("env", "", "mark an unmarked database with environment `name`, and refuse one marked otherwise")
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("up", args); err != nil {
			return err
		}
		if *env != "" {
			if err := s.markEnvironment(ctx, seedport.Environment(*env)); err != nil {
				return err
			}
		}
		withSeeds := s.app.Seeds != nil && *to == ""
		if *dry {
			return s.dryRun(ctx, *to, withSeeds)
		}
		jobs := []live.Job{live.MigrationJob(s.app.Migrations, migrateport.UpOptions{To: *to})}
		if withSeeds {
			jobs = append(jobs, live.SeedJob(s.app.Seeds, seedport.ApplyOptions{}))
		}
		return s.runJobs(ctx, jobs)
	}
}

func (s *session) markEnvironment(ctx context.Context, env seedport.Environment) error {
	if s.app.Seeds == nil {
		return Usagef("keelcli: -env needs an App with Seeds, whose engine records the mark")
	}
	current, err := s.app.Seeds.Environment(ctx)
	if err != nil {
		return fmt.Errorf("keelcli: read environment: %w", err)
	}
	switch current {
	case env:
		return nil
	case "":
		if err := s.app.Seeds.MarkEnvironment(ctx, env); err != nil {
			return fmt.Errorf("keelcli: mark environment: %w", err)
		}
		return s.say(render.Muted, "Marked the database as %s.", env)
	default:
		return fmt.Errorf("keelcli: the database is marked %s, not %s: check the connection settings", current, env)
	}
}

func (s *session) dryRun(ctx context.Context, to string, withSeeds bool) error {
	plan, err := s.app.Migrations.Plan(ctx, migrateport.PlanOptions{To: to})
	if err != nil {
		return err
	}
	var seeds []seedport.PlanItem
	if withSeeds {
		if seeds, err = s.app.Seeds.Plan(ctx, seedport.PlanOptions{}); err != nil {
			return err
		}
	}
	doc := map[string]any{"migrations": render.PlanJSON(plan, true)}
	if withSeeds {
		doc["seeds"] = render.SeedPlanJSON(seeds)
	}
	return s.emit(doc, func(p render.Painter, width int) string {
		out := render.PlanText(plan, true, p, width)
		if withSeeds {
			t := render.SeedPlanTable(seeds, !render.IsPlain(p))
			t.Width = width
			out += "\n" + p.Paint(render.Strong, "Seeds, as the database stands now:") + "\n" + t.String(p)
		}
		return out
	})
}

func setupMigrateUp(fs *flag.FlagSet) runner {
	to := fs.String("to", "", "stop after migration `version`")
	dry := fs.Bool("dry-run", false, "print the plan with its SQL instead of applying it")
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("migrate up", args); err != nil {
			return err
		}
		if *dry {
			return s.dryRun(ctx, *to, false)
		}
		return s.runJobs(ctx, []live.Job{live.MigrationJob(s.app.Migrations, migrateport.UpOptions{To: *to})})
	}
}

func setupMigratePlan(fs *flag.FlagSet) runner {
	to := fs.String("to", "", "stop after migration `version`")
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("migrate plan", args); err != nil {
			return err
		}
		plan, err := s.app.Migrations.Plan(ctx, migrateport.PlanOptions{To: *to})
		if err != nil {
			return err
		}
		return s.emit(render.PlanJSON(plan, false), func(p render.Painter, width int) string {
			return render.PlanText(plan, false, p, width)
		})
	}
}

func setupMigrateStatus(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("migrate status", args); err != nil {
			return err
		}
		rows, err := s.app.Migrations.Status(ctx)
		if err != nil {
			return err
		}
		return s.emit(render.MigrationStatusJSON(rows), func(p render.Painter, width int) string {
			t := render.MigrationTable(rows, !render.IsPlain(p))
			t.Width = width
			return t.String(p) + "\n" + statusSummary(rows, p) + "\n"
		})
	}
}

func statusSummary(rows []migrateport.Status, p render.Painter) string {
	counts := map[migrateport.State]int{}
	for _, r := range rows {
		counts[r.State]++
	}
	var parts []string
	for _, st := range []migrateport.State{migrateport.Applied, migrateport.Pending, migrateport.RepeatableDue, migrateport.Changed, migrateport.Missing} {
		if n := counts[st]; n > 0 {
			g := render.MigrationGlyph(st)
			parts = append(parts, p.Paint(g.Tone, fmt.Sprintf("%d %s", n, st)))
		}
	}
	return strings.Join(parts, p.Paint(render.Faint, " · "))
}

func setupMigrateHistory(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("migrate history", args); err != nil {
			return err
		}
		rows, err := s.app.Migrations.History(ctx)
		if err != nil {
			return err
		}
		return s.emit(render.HistoryJSON(rows), func(p render.Painter, width int) string {
			t := render.HistoryTable(rows)
			t.Width = width
			return t.String(p)
		})
	}
}

func parseCreateKind(s string) (migrateport.CreateKind, error) {
	switch s {
	case "", "tx":
		return migrateport.Transactional, nil
	case "notx":
		return migrateport.NoTx, nil
	case "repeatable":
		return migrateport.RepeatableFile, nil
	default:
		return 0, Usagef("keelcli: -kind %q: use tx, notx or repeatable", s)
	}
}

func setupMigrateCreate(fs *flag.FlagSet) runner {
	kindFlag := fs.String("kind", "", "`kind` of migration: tx (the default), notx or repeatable")
	return func(ctx context.Context, s *session, args []string) error {
		if len(args) > 1 {
			return Usagef("keelcli: migrate create takes one name, got %q", strings.Join(args, " "))
		}
		kind, err := parseCreateKind(*kindFlag)
		if err != nil {
			return err
		}
		name := ""
		if len(args) == 1 {
			name = args[0]
		}
		if name == "" {
			if !s.canPrompt() {
				return Usagef("keelcli: migrate create needs a name: migrate create [-kind tx|notx|repeatable] <name>")
			}
			if err := prompt.Run(ctx, prompt.Create(s.programTheme(), &name, &kind), s.programOptions(inlineHeight)...); err != nil {
				return err
			}
		}
		if err := prompt.ValidateName(name); err != nil {
			return Usagef("keelcli: migration name %q: %v", name, err)
		}
		created, err := s.app.Migrations.Create(ctx, migrateport.CreateRequest{Name: name, Kind: kind})
		if err != nil {
			return err
		}
		doc := map[string]any{"path": created.Path, "migration": render.MigrationJSON(created.Migration, false)}
		return s.emit(doc, func(p render.Painter, _ int) string {
			return p.Paint(render.OK, "Created ") + created.Path + "\n"
		})
	}
}

func setupMigrateSquash(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("migrate squash", args); err != nil {
			return err
		}
		err := s.confirm(ctx, guard.Action{
			Verb:  "squash migrations",
			Title: "Squash every applied migration into one baseline?",
			Detail: []string{
				"Applies every migration to a scratch database, dumps it with pg_dump and",
				"replaces the applied migration files with <version>_baseline.sql.",
			},
		})
		if err != nil {
			return err
		}
		var out migrateport.Squashed
		if err := s.busy(ctx, "Squashing migrations", func(ctx context.Context) (err error) {
			out, err = s.app.Migrations.Squash(ctx)
			return err
		}); err != nil {
			return err
		}
		doc := map[string]any{"path": out.Path, "version": out.Version, "replaced": out.Replaced}
		return s.emit(doc, func(p render.Painter, _ int) string {
			return fmt.Sprintf("%s %s, replacing %s.\n", p.Paint(render.OK, "Wrote"), out.Path, render.Plural(out.Replaced, "migration", "migrations"))
		})
	}
}

func setupMigrateStamp(fs *flag.FlagSet) runner {
	check := fs.Bool("check", false, "write nothing; fail when the stamp does not match the migrations")
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("migrate stamp", args); err != nil {
			return err
		}
		st, err := s.app.Migrations.Stamp(ctx, migrateport.StampOptions{Check: *check})
		if err != nil {
			return err
		}
		doc := map[string]any{"path": st.Path, "version": st.Version, "fingerprint": st.Fingerprint, "current": st.Current}
		if err := s.emit(doc, func(p render.Painter, _ int) string {
			switch {
			case !*check:
				return fmt.Sprintf("%s %s: version %s, fingerprint %s\n", p.Paint(render.OK, "Wrote"), st.Path, st.Version, st.Fingerprint)
			case st.Current:
				return fmt.Sprintf("%s %s matches the migrations (version %s)\n", p.Paint(render.OK, "✓"), st.Path, st.Version)
			default:
				return fmt.Sprintf("%s %s is stale: the migrations end at %s; run migrate stamp\n", p.Paint(render.Fail, "✗"), st.Path, st.Version)
			}
		}); err != nil {
			return err
		}
		if *check && !st.Current {
			return problemsf("keelcli: %s is stale", st.Path)
		}
		return nil
	}
}

func setupMigrateCheck(fs *flag.FlagSet) runner {
	base := fs.String("base", "", "compare against `version`, the newest on the base branch, instead of the database")
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("migrate check", args); err != nil {
			return err
		}
		ps, err := s.app.Migrations.Check(ctx, migrateport.CheckOptions{Base: *base})
		if err != nil {
			return err
		}
		if err := s.emit(render.ProblemsJSON(ps), func(p render.Painter, width int) string {
			return render.ProblemsText(ps, p, width)
		}); err != nil {
			return err
		}
		if len(ps) > 0 {
			return problemsf("keelcli: migrate check found %s", render.Plural(len(ps), "problem", "problems"))
		}
		return nil
	}
}

func setupMigrateLint(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("migrate lint", args); err != nil {
			return err
		}
		fs, err := s.app.Migrations.Lint(ctx)
		if err != nil {
			return err
		}
		if err := s.emit(render.FindingsJSON(fs), func(p render.Painter, width int) string {
			return render.FindingsText(fs, p, width)
		}); err != nil {
			return err
		}
		errs := 0
		for _, f := range fs {
			if f.Severity == migrateport.Error {
				errs++
			}
		}
		if errs > 0 {
			return problemsf("keelcli: migrate lint found %s", render.Plural(errs, "error", "errors"))
		}
		return nil
	}
}

func oneVersion(name string, args []string) (string, error) {
	if len(args) != 1 {
		return "", Usagef("keelcli: %s needs one version: %s <version>", name, name)
	}
	return args[0], nil
}

func setupMigrateBaseline(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		version, err := oneVersion("migrate baseline", args)
		if err != nil {
			return err
		}
		if err := s.confirm(ctx, guard.Action{
			Verb:   "baseline migrations",
			Title:  "Record every migration up to " + version + " as applied without running it?",
			Detail: []string{"Use this on a database whose schema already matches those migrations."},
		}); err != nil {
			return err
		}
		out, err := s.app.Migrations.Baseline(ctx, version)
		if err != nil {
			return err
		}
		ids := make([]any, 0, len(out.Marked))
		for _, m := range out.Marked {
			ids = append(ids, render.MigrationJSON(m, false))
		}
		return s.emit(map[string]any{"marked": ids}, func(p render.Painter, _ int) string {
			var b strings.Builder
			fmt.Fprintf(&b, "%s %s as applied.\n", p.Paint(render.OK, "Recorded"), render.Plural(len(out.Marked), "migration", "migrations"))
			for _, m := range out.Marked {
				fmt.Fprintf(&b, "  %s\n", m.ID())
			}
			return b.String()
		})
	}
}

func setupMigrateRepair(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		version, err := oneVersion("migrate repair", args)
		if err != nil {
			return err
		}
		if err := s.confirm(ctx, guard.Action{
			Verb:   "repair a migration checksum",
			Title:  "Accept the current checksum of " + version + "?",
			Detail: []string{"Only for a comment-only or whitespace edit: the migration does not run again."},
		}); err != nil {
			return err
		}
		out, err := s.app.Migrations.Repair(ctx, version)
		if err != nil {
			return err
		}
		doc := map[string]any{"migration": render.MigrationJSON(out.Migration, false), "old_checksum": out.OldChecksum, "new_checksum": out.NewChecksum}
		return s.emit(doc, func(p render.Painter, _ int) string {
			return fmt.Sprintf("%s %s: checksum %s → %s\n", p.Paint(render.OK, "Repaired"), out.Migration.ID(), out.OldChecksum, out.NewChecksum)
		})
	}
}
