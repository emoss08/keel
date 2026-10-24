package keelcli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/emoss08/keel/keelcli/internal/doctor"
	"github.com/emoss08/keel/keelcli/internal/guard"
	"github.com/emoss08/keel/keelcli/internal/render"
	"github.com/emoss08/keel/keelcli/migrateport"
)

func resetAction(database string) guard.Action {
	return guard.Action{
		Verb:   "reset the database",
		Title:  "Recreate " + database + " from the current template?",
		Detail: []string{"Every row is lost: the database becomes a copy of the template,", "with migrations and reference seeds applied and no fixtures."},
		Never:  true,
	}
}

func setupReset(fs *flag.FlagSet) runner {
	admin := adminFlag(fs)
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("reset", args); err != nil {
			return err
		}
		s.admin = *admin
		t, err := s.target(ctx)
		if err != nil {
			return err
		}
		if err := s.confirm(ctx, resetAction(t.Name())); err != nil {
			return err
		}
		var out migrateport.Reset
		if err := s.busy(ctx, "Resetting "+t.Name(), func(ctx context.Context) (err error) {
			out, err = s.migrations().Reset(ctx)
			return err
		}); err != nil {
			return err
		}
		doc := map[string]any{"database": out.Database, "template": out.Template, "built": out.Built, "duration_ms": out.Duration.Milliseconds()}
		return s.emit(doc, func(p render.Painter, _ int) string {
			built := ""
			if out.Built {
				built = ", building it first"
			}
			return fmt.Sprintf("%s %s from %s in %s%s.\n", p.Paint(render.OK, "Reset"), out.Database, out.Template, render.Duration(out.Duration), built)
		})
	}
}

func setupTemplate(fs *flag.FlagSet) runner {
	admin := adminFlag(fs)
	return func(ctx context.Context, s *session, args []string) error {
		if len(args) > 0 {
			return Usagef("keelcli: unknown template subcommand %q: use build or prune", strings.Join(args, " "))
		}
		s.admin = *admin
		return s.listTemplates(ctx)
	}
}

func setupTemplateBuild(fs *flag.FlagSet) runner {
	admin := adminFlag(fs)
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("template build", args); err != nil {
			return err
		}
		s.admin = *admin
		return s.buildTemplate(ctx)
	}
}

func setupTemplatePrune(fs *flag.FlagSet) runner {
	admin := adminFlag(fs)
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("template prune", args); err != nil {
			return err
		}
		s.admin = *admin
		return s.pruneTemplates(ctx)
	}
}

func (s *session) listTemplates(ctx context.Context) error {
	ts, err := s.migrations().Templates(ctx)
	if err != nil {
		return err
	}
	return s.emit(render.TemplatesJSON(ts), func(p render.Painter, width int) string {
		if len(ts) == 0 {
			return p.Paint(render.Muted, "No templates: template build makes the current one.") + "\n"
		}
		t := render.TemplateTable(ts, s.st.now(), !render.IsPlain(p))
		t.Width = width
		return t.String(p)
	})
}

func (s *session) buildTemplate(ctx context.Context) error {
	var t migrateport.Template
	if err := s.busy(ctx, "Building the current template", func(ctx context.Context) (err error) {
		t, err = s.migrations().BuildTemplate(ctx)
		return err
	}); err != nil {
		return err
	}
	return s.emit(render.TemplatesJSON([]migrateport.Template{t}), func(p render.Painter, _ int) string {
		return fmt.Sprintf("%s %s (%s).\n", p.Paint(render.OK, "Template ready:"), t.Name, render.Size(t.Size))
	})
}

func (s *session) pruneTemplates(ctx context.Context) error {
	ts, err := s.migrations().Templates(ctx)
	if err != nil {
		return err
	}
	var stale []string
	for _, t := range ts {
		if !t.Current {
			stale = append(stale, t.Name)
		}
	}
	if len(stale) == 0 {
		return s.emit(render.TemplatesJSON(nil), func(p render.Painter, _ int) string {
			return p.Paint(render.OK, "Nothing to prune: no stale templates.") + "\n"
		})
	}
	if err := s.confirm(ctx, guard.Action{
		Verb:   "prune templates",
		Title:  "Drop " + render.Plural(len(stale), "stale template", "stale templates") + "?",
		Detail: []string{"Templates: " + strings.Join(stale, ", ")},
	}); err != nil {
		return err
	}
	dropped, err := s.migrations().PruneTemplates(ctx)
	if err != nil {
		return err
	}
	return s.emit(render.TemplatesJSON(dropped), func(p render.Painter, _ int) string {
		return fmt.Sprintf("%s %s.\n", p.Paint(render.OK, "Dropped"), render.Plural(len(dropped), "template", "templates"))
	})
}

func (s *session) doctorSources() doctor.Sources {
	return doctor.Sources{Facts: s.facts(), Migrations: s.migrations(), Extensions: s.app.Extensions, Now: s.st.now}
}

func setupDoctor(*flag.FlagSet) runner {
	return func(ctx context.Context, s *session, args []string) error {
		if err := noArgs("doctor", args); err != nil {
			return err
		}
		r := doctor.Run(ctx, s.doctorSources())
		if err := s.emit(doctor.JSON(r), func(p render.Painter, width int) string {
			return doctor.Text(r, p, width, !render.IsPlain(p))
		}); err != nil {
			return err
		}
		if r.Failed() {
			return problemsf("keelcli: doctor found failing checks")
		}
		return nil
	}
}

func setupVersion(*flag.FlagSet) runner {
	return func(_ context.Context, s *session, args []string) error {
		if err := noArgs("version", args); err != nil {
			return err
		}
		version := s.app.Version
		if version == "" {
			version = "(devel)"
		}
		return s.emit(map[string]string{"name": s.app.Name, "version": version}, func(render.Painter, int) string {
			return s.app.Name + " " + version + "\n"
		})
	}
}
