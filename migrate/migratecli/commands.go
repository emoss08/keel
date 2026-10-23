package migratecli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/emoss08/keel/migrate"
)

// Up applies pending migrations, printing each event as it happens. With o.DryRun it prints the
// plan and the SQL each migration would run instead.
func Up(ctx context.Context, w io.Writer, c Config, o migrate.UpOptions) error {
	if o.DryRun {
		return printPlan(ctx, w, c, o, true)
	}
	m, err := c.open(ctx, Printer(w, c.JSON))
	if err != nil {
		return err
	}
	defer m.Close()
	_, err = m.Up(ctx, o)
	return err
}

// Plan prints what Up would do without changing anything.
func Plan(ctx context.Context, w io.Writer, c Config, o migrate.UpOptions) error {
	return printPlan(ctx, w, c, o, false)
}

// planJSON is a plan in JSON output.
type planJSON struct {
	Marks       []*migrationJSON `json:"marks"`
	Pending     []*migrationJSON `json:"pending"`
	Repeatables []*migrationJSON `json:"repeatables"`
	After       []string         `json:"after"`
}

// printPlan prints the plan, with each migration's SQL when withSQL is set.
func printPlan(ctx context.Context, w io.Writer, c Config, o migrate.UpOptions, withSQL bool) error {
	m, err := c.open(ctx, nil)
	if err != nil {
		return err
	}
	defer m.Close()
	p, err := m.Plan(ctx, o)
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(w, planJSON{Marks: toMigrationsJSON(p.Marks, false), Pending: toMigrationsJSON(p.Pending, withSQL),
			Repeatables: toMigrationsJSON(p.Repeatables, withSQL), After: nonNil(p.After)})
	}
	if p.Empty() {
		_, err := fmt.Fprintln(w, "nothing to apply")
		return err
	}
	for _, mg := range p.Marks {
		fmt.Fprintf(w, "mark    %s\n", label(mg))
	}
	for _, mg := range append(append([]migrate.Migration(nil), p.Pending...), p.Repeatables...) {
		fmt.Fprintf(w, "apply   %s%s\n", label(mg), notes(mg))
		if withSQL {
			printSQL(w, mg)
		}
	}
	for _, s := range p.After {
		fmt.Fprintf(w, "after   %s\n", oneLine(s))
	}
	_, err = fmt.Fprintf(w, "%s to apply, %s to mark\n", plural(len(p.Pending)+len(p.Repeatables), "migration"), plural(len(p.Marks), "baseline"))
	return err
}

// printSQL prints the statements a migration runs.
func printSQL(w io.Writer, mg migrate.Migration) {
	if mg.Kind == migrate.KindGo {
		fmt.Fprintln(w, "        -- Go migration")
		return
	}
	for i, s := range mg.Statements() {
		if i > 0 {
			fmt.Fprintln(w, "        --keel:split")
		}
		for _, line := range strings.Split(s, "\n") {
			fmt.Fprintln(w, strings.TrimRight("        "+line, " \t\r"))
		}
	}
}

// nonNil returns s, or an empty slice for nil, so JSON shows [].
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// statusJSON is a status in JSON output.
type statusJSON struct {
	*migrationJSON
	State           migrate.State `json:"state"`
	AppliedAt       *time.Time    `json:"applied_at,omitempty"`
	DurationMS      int64         `json:"duration_ms,omitempty"`
	AppliedChecksum string        `json:"applied_checksum,omitempty"`
	OutOfOrder      bool          `json:"out_of_order,omitempty"`
	Mark            bool          `json:"mark,omitempty"`
	Gap             bool          `json:"gap,omitempty"`
}

// toStatusesJSON converts statuses.
func toStatusesJSON(ss []migrate.Status) []statusJSON {
	out := make([]statusJSON, len(ss))
	for i, s := range ss {
		out[i] = statusJSON{migrationJSON: toMigrationJSON(s.Migration, false), State: s.State,
			DurationMS: s.Duration.Milliseconds(), AppliedChecksum: s.AppliedChecksum, OutOfOrder: s.OutOfOrder, Mark: s.Mark, Gap: s.Gap}
		if !s.AppliedAt.IsZero() {
			at := s.AppliedAt.UTC()
			out[i].AppliedAt = &at
		}
	}
	return out
}

// Status prints every migration with its state.
func Status(ctx context.Context, w io.Writer, c Config) error {
	m, err := c.open(ctx, nil)
	if err != nil {
		return err
	}
	defer m.Close()
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(w, toStatusesJSON(st))
	}
	tw := table(w)
	fmt.Fprintln(tw, "STATE\tVERSION\tNAME\tSOURCE\tKIND\tAPPLIED\tDURATION\tNOTE")
	for _, s := range st {
		applied, dur := "", ""
		if !s.AppliedAt.IsZero() {
			applied, dur = s.AppliedAt.UTC().Format("2006-01-02 15:04:05"), ms(s.Duration)
		}
		version := s.Version
		if s.Kind == migrate.KindRepeatable {
			version = "R"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.State, version, s.Name, s.Source, s.Kind, applied, dur, statusNote(s))
	}
	return tw.Flush()
}

// statusNote explains a status.
func statusNote(s migrate.Status) string {
	notes := directiveNotes(s.Directives)
	switch {
	case s.Gap:
		notes = append(notes, "the database's history ends before this baseline")
	case s.Mark:
		notes = append(notes, "recorded without running")
	case s.OutOfOrder:
		notes = append(notes, "out of order")
	case s.State == migrate.StateChanged, s.State == migrate.StateRepeatableDue && s.AppliedChecksum != "":
		notes = append(notes, "checksum differs")
	}
	return strings.Join(notes, ", ")
}

// recordJSON is an applied migration in JSON output.
type recordJSON struct {
	Version    string       `json:"version,omitempty"`
	Name       string       `json:"name"`
	Source     string       `json:"source"`
	Kind       migrate.Kind `json:"kind"`
	Checksum   string       `json:"checksum"`
	AppliedAt  time.Time    `json:"applied_at"`
	DurationMS int64        `json:"duration_ms"`
}

// History prints every applied migration, oldest first.
func History(ctx context.Context, w io.Writer, c Config) error {
	m, err := c.open(ctx, nil)
	if err != nil {
		return err
	}
	defer m.Close()
	recs, err := m.History(ctx)
	if err != nil {
		return err
	}
	if c.JSON {
		out := make([]recordJSON, len(recs))
		for i, r := range recs {
			out[i] = recordJSON{Version: r.Version, Name: r.Name, Source: r.Source, Kind: r.Kind, Checksum: r.Checksum,
				AppliedAt: r.AppliedAt.UTC(), DurationMS: r.Duration.Milliseconds()}
		}
		return writeJSON(w, out)
	}
	tw := table(w)
	fmt.Fprintln(tw, "APPLIED\tVERSION\tNAME\tSOURCE\tKIND\tDURATION")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.AppliedAt.UTC().Format("2006-01-02 15:04:05"), r.Version, r.Name,
			r.Source, r.Kind, ms(r.Duration))
	}
	return tw.Flush()
}

// checkJSON is a check report in JSON output.
type checkJSON struct {
	OK         bool         `json:"ok"`
	Pending    []statusJSON `json:"pending"`
	OutOfOrder []statusJSON `json:"out_of_order"`
	Changed    []statusJSON `json:"changed"`
	Missing    []statusJSON `json:"missing"`
	Gaps       []statusJSON `json:"gaps"`
}

// Check compares the sources with the database's history, or with base, the migrations of the
// branch being merged into, when c.DSN is empty. It prints pending, out-of-order, changed and
// missing migrations and baseline gaps, and returns an error wrapping ErrProblems when any but
// pending ones exist.
func Check(ctx context.Context, w io.Writer, c Config, base *migrate.Set) error {
	var rep migrate.CheckReport
	var err error
	if c.DSN == "" && base != nil {
		rep, err = migrate.CheckAgainst(c.Sources, *base)
	} else {
		rep, err = checkDatabase(ctx, c)
	}
	if err != nil {
		return err
	}
	if c.JSON {
		if err := writeJSON(w, checkJSON{OK: rep.OK(), Pending: toStatusesJSON(rep.Pending), OutOfOrder: toStatusesJSON(rep.OutOfOrder),
			Changed: toStatusesJSON(rep.Changed), Missing: toStatusesJSON(rep.Missing), Gaps: toStatusesJSON(rep.Gaps)}); err != nil {
			return err
		}
	} else {
		printCheck(w, rep)
	}
	if rep.OK() {
		return nil
	}
	return fmt.Errorf("check: %s: %w", plural(len(rep.OutOfOrder)+len(rep.Changed)+len(rep.Missing)+len(rep.Gaps), "problem"), ErrProblems)
}

// checkDatabase checks against the database.
func checkDatabase(ctx context.Context, c Config) (migrate.CheckReport, error) {
	m, err := c.open(ctx, nil)
	if err != nil {
		return migrate.CheckReport{}, err
	}
	defer m.Close()
	return m.Check(ctx)
}

// printCheck prints a check report as lines.
func printCheck(w io.Writer, rep migrate.CheckReport) {
	groups := []struct {
		name, why string
		ss        []migrate.Status
	}{
		{"out of order", "older than the newest applied migration; give it a newer version", rep.OutOfOrder},
		{"changed", "edited after it was applied; add a new migration, or repair after a comment-only edit", rep.Changed},
		{"missing", "applied, but no longer in the sources", rep.Missing},
		{"gap", "the database's history ends before this baseline", rep.Gaps},
		{"pending", "not applied yet", rep.Pending},
	}
	for _, g := range groups {
		for _, s := range g.ss {
			fmt.Fprintf(w, "%-13s %s: %s\n", g.name, label(s.Migration), g.why)
		}
	}
	n := len(rep.OutOfOrder) + len(rep.Changed) + len(rep.Missing) + len(rep.Gaps)
	if n == 0 {
		fmt.Fprintf(w, "ok: %s pending\n", plural(len(rep.Pending), "migration"))
		return
	}
	fmt.Fprintf(w, "%s\n", plural(n, "problem"))
}

// findingJSON is a lint finding in JSON output.
type findingJSON struct {
	Rule     string           `json:"rule"`
	Severity migrate.Severity `json:"severity"`
	Version  string           `json:"version,omitempty"`
	Name     string           `json:"name"`
	Source   string           `json:"source"`
	File     string           `json:"file,omitempty"`
	Line     int              `json:"line"`
	Message  string           `json:"message"`
}

// Lint lints the pending migrations and due repeatable ones when c.DSN is set and all is not, or
// every migration otherwise. It returns an error wrapping ErrProblems when a finding is an error.
func Lint(ctx context.Context, w io.Writer, c Config, all bool, rules ...migrate.LintRule) error {
	ms, err := lintTargets(ctx, c, all)
	if err != nil {
		return err
	}
	findings := migrate.Lint(ms, rules...)
	errs := 0
	for _, f := range findings {
		if f.Severity == migrate.SeverityError {
			errs++
		}
	}
	if c.JSON {
		out := make([]findingJSON, len(findings))
		for i, f := range findings {
			out[i] = findingJSON{Rule: f.Rule, Severity: f.Severity, Version: f.Migration.Version, Name: f.Migration.Name,
				Source: f.Migration.Source, File: f.Migration.File, Line: f.Line, Message: f.Message}
		}
		if err := writeJSON(w, out); err != nil {
			return err
		}
	} else {
		for _, f := range findings {
			fmt.Fprintf(w, "%s/%s:%d: %s: %s: %s\n", f.Migration.Source, f.Migration.File, f.Line, f.Severity, f.Rule, f.Message)
		}
		fmt.Fprintf(w, "%s linted, %s\n", plural(len(ms), "migration"), plural(len(findings), "finding"))
	}
	if errs > 0 {
		return fmt.Errorf("lint: %s: %w", plural(errs, "error"), ErrProblems)
	}
	return nil
}

// lintTargets returns the migrations Lint checks.
func lintTargets(ctx context.Context, c Config, all bool) ([]migrate.Migration, error) {
	ms, err := c.Sources.Migrations()
	if err != nil || all || c.DSN == "" {
		return ms, err
	}
	m, err := c.open(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer m.Close()
	st, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	var out []migrate.Migration
	for _, s := range st {
		if s.State == migrate.StatePending || s.State == migrate.StateRepeatableDue {
			out = append(out, s.Migration)
		}
	}
	return out, nil
}

// Baseline records every migration up to version as applied without running it.
func Baseline(ctx context.Context, w io.Writer, c Config, version string) error {
	m, err := c.open(ctx, nil)
	if err != nil {
		return err
	}
	defer m.Close()
	marked, err := m.Baseline(ctx, version)
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(w, map[string]any{"marked": toMigrationsJSON(marked, false)})
	}
	for _, mg := range marked {
		fmt.Fprintf(w, "marked %s\n", label(mg))
	}
	_, err = fmt.Fprintf(w, "%s marked as applied\n", plural(len(marked), "migration"))
	return err
}

// Repair accepts the current checksum of the applied migration with version.
func Repair(ctx context.Context, w io.Writer, c Config, version string) error {
	m, err := c.open(ctx, nil)
	if err != nil {
		return err
	}
	defer m.Close()
	r, err := m.Repair(ctx, version)
	if err != nil {
		return err
	}
	if c.JSON {
		return writeJSON(w, map[string]any{"migration": toMigrationJSON(r.Migration, false), "old": r.Old, "new": r.New,
			"changed": r.Old != r.New})
	}
	if r.Old == r.New {
		_, err = fmt.Fprintf(w, "%s: checksum unchanged (%s)\n", label(r.Migration), r.New)
		return err
	}
	_, err = fmt.Fprintf(w, "repaired %s: checksum %s -> %s\n", label(r.Migration), r.Old, r.New)
	return err
}

// Create writes an empty migration in dir.
func Create(w io.Writer, dir, name string, o migrate.CreateOptions, asJSON bool) error {
	path, err := migrate.Create(dir, name, o)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(w, map[string]string{"path": path})
	}
	_, err = fmt.Fprintln(w, "created", path)
	return err
}

// Squash replaces the migrations up to o.To with one baseline file.
func Squash(ctx context.Context, w io.Writer, o migrate.SquashOptions, asJSON bool) error {
	r, err := migrate.Squash(ctx, o)
	if err != nil {
		return err
	}
	if asJSON {
		ids := make([]string, len(r.Squashed))
		for i, m := range r.Squashed {
			ids[i] = m.ID()
		}
		return writeJSON(w, map[string]any{"baseline": r.Baseline, "version": r.Version, "squashed": ids, "removed": r.Removed})
	}
	for _, p := range r.Removed {
		fmt.Fprintln(w, "removed", p)
	}
	_, err = fmt.Fprintf(w, "squashed %s into %s\n", plural(len(r.Squashed), "migration"), r.Baseline)
	return err
}

// StampOptions configure Stamp.
type StampOptions struct {
	// Package is the generated file's package name.
	Package string
	// Out is the path of the generated file.
	Out string
	// Check fails when the file is out of date instead of writing it.
	Check bool
}

// Stamp writes the Version and Fingerprint file for set, or checks it is current.
func Stamp(w io.Writer, set migrate.Set, o StampOptions, asJSON bool) error {
	src, err := migrate.Stamp(set, o.Package)
	if err != nil {
		return err
	}
	cur, err := os.ReadFile(o.Out)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stamp: %w", err)
	}
	current := bytes.Equal(cur, src)
	written := false
	if !current && !o.Check {
		if err := os.WriteFile(o.Out, src, 0o644); err != nil {
			return fmt.Errorf("stamp: %w", err)
		}
		written = true
	}
	if asJSON {
		if err := writeJSON(w, map[string]any{"path": o.Out, "version": set.Version(), "fingerprint": set.Fingerprint(),
			"current": current, "written": written}); err != nil {
			return err
		}
	} else {
		switch {
		case written:
			fmt.Fprintf(w, "wrote %s (version %s)\n", o.Out, set.Version())
		case current:
			fmt.Fprintf(w, "%s is up to date\n", o.Out)
		}
	}
	if o.Check && !current {
		return fmt.Errorf("stamp: %s is out of date; run keel migrate stamp: %w", o.Out, ErrProblems)
	}
	return nil
}

// templateJSON is a template in JSON output.
type templateJSON struct {
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	Created     time.Time `json:"created"`
	Fingerprint string    `json:"fingerprint"`
	Key         string    `json:"key,omitempty"`
}

// Templates lists the keel template databases on the server admin connects to.
func Templates(ctx context.Context, w io.Writer, admin string, asJSON bool) error {
	ts, err := migrate.Templates(ctx, admin)
	if err != nil {
		return err
	}
	if asJSON {
		out := make([]templateJSON, len(ts))
		for i, t := range ts {
			out[i] = templateJSON(t)
		}
		return writeJSON(w, out)
	}
	tw := table(w)
	fmt.Fprintln(tw, "NAME\tSIZE\tCREATED\tFINGERPRINT\tKEY")
	for _, t := range ts {
		created := ""
		if !t.Created.IsZero() {
			created = t.Created.UTC().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, size(t.Size), created, truncate(t.Fingerprint, 12), t.Key)
	}
	return tw.Flush()
}

// PruneTemplates drops the keel template databases not named in keep.
func PruneTemplates(ctx context.Context, w io.Writer, admin string, keep []string, asJSON bool) error {
	dropped, err := migrate.PruneTemplates(ctx, admin, keep)
	if asJSON {
		if jerr := writeJSON(w, map[string]any{"dropped": nonNil(dropped)}); jerr != nil {
			return errors.Join(err, jerr)
		}
		return err
	}
	for _, d := range dropped {
		fmt.Fprintln(w, "dropped", d)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s dropped\n", plural(len(dropped), "template"))
	return err
}

// size formats a byte count.
func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
