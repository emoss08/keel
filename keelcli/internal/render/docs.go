package render

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

func glyphColumn(glyphs bool) []Column {
	if !glyphs {
		return nil
	}
	return []Column{{Title: " ", Min: 1, Max: 1}}
}

func glyphCell(glyphs bool, g Glyph) []Cell {
	if !glyphs {
		return nil
	}
	return []Cell{{Text: g.Symbol, Tone: g.Tone}}
}

func stateTone(g Glyph) Tone {
	if g.Tone == OK {
		return Normal
	}
	return g.Tone
}

// MigrationTable lists migrations with their state; glyphs adds a state symbol column.
func MigrationTable(rows []migrateport.Status, glyphs bool) Table {
	t := Table{Columns: append(glyphColumn(glyphs),
		Column{Title: "VERSION", Min: 14},
		Column{Title: "NAME", Min: 12, Max: 40, Flex: true},
		Column{Title: "SOURCE", Min: 4, Drop: 3},
		Column{Title: "STATE", Min: 7},
		Column{Title: "APPLIED", Drop: 2},
		Column{Title: "TOOK", Right: true, Drop: 4},
		Column{Title: "DETAIL", Min: 8, Flex: true, Drop: 1},
	)}
	for _, s := range rows {
		g := MigrationGlyph(s.State)
		took := ""
		if s.Duration > 0 {
			took = Duration(s.Duration)
		}
		version := s.Migration.Version
		if version == "" {
			version = "R"
		}
		t.Rows = append(t.Rows, append(glyphCell(glyphs, g),
			C(version, Muted),
			C(s.Migration.Name, Normal),
			C(s.Migration.Source, Muted),
			C(s.State.String(), stateTone(g)),
			C(Time(s.AppliedAt), Muted),
			C(took, Muted),
			C(StateDetail(s), Faint),
		))
	}
	return t
}

// HistoryTable lists bookkeeping rows.
func HistoryTable(rows []migrateport.Record) Table {
	t := Table{Columns: []Column{
		{Title: "VERSION", Min: 14},
		{Title: "NAME", Min: 12, Flex: true},
		{Title: "SOURCE", Drop: 2},
		{Title: "APPLIED", Min: 16},
		{Title: "TOOK", Right: true, Drop: 1},
		{Title: "CHECKSUM", Drop: 3},
	}}
	for _, r := range rows {
		t.Rows = append(t.Rows, []Cell{
			C(r.Version, Muted), C(r.Name, Normal), C(r.Source, Muted), C(Time(r.AppliedAt), Normal),
			C(Duration(r.Duration), Muted), C(shortSum(r.Checksum), Faint),
		})
	}
	return t
}

// PlanText describes a migration plan, with each migration's SQL for a dry run.
func PlanText(p migrateport.Plan, withSQL bool, pt Painter, width int) string {
	if p.Empty() {
		return pt.Paint(OK, "Nothing to apply: the database is up to date.") + "\n"
	}
	var b strings.Builder
	t := Table{NoHeader: true, Width: width, Indent: 2, Columns: []Column{
		{Title: "#", Right: true}, {Title: "VERSION", Min: 14}, {Title: "NAME", Flex: true, Min: 10},
		{Title: "SOURCE", Drop: 2}, {Title: "DETAIL", Flex: true, Drop: 1},
	}}
	for i, m := range p.Migrations {
		version := m.Version
		if version == "" {
			version = "R"
		}
		t.Rows = append(t.Rows, []Cell{C(fmt.Sprint(i+1), Faint), C(version, Muted), C(m.Name, Normal),
			C(m.Source, Muted), C(MigrationDetail(m), Faint)})
	}
	if len(p.Migrations) > 0 {
		fmt.Fprintf(&b, "%s\n", pt.Paint(Strong, "Would apply "+Plural(len(p.Migrations), "migration", "migrations")+":"))
		b.WriteString(t.String(pt))
	}
	if len(p.Baseline) > 0 {
		fmt.Fprintf(&b, "%s\n", pt.Paint(Strong, "Would record as applied without running:"))
		for _, m := range p.Baseline {
			fmt.Fprintf(&b, "  %s\n", m.ID())
		}
	}
	if len(p.After) > 0 {
		fmt.Fprintf(&b, "%s\n", pt.Paint(Strong, "Then, in the last migration's transaction:"))
		for _, s := range p.After {
			fmt.Fprintf(&b, "  %s\n", pt.Paint(Muted, s))
		}
	}
	if withSQL {
		for _, m := range p.Migrations {
			fmt.Fprintf(&b, "\n%s\n", pt.Paint(Header, "-- "+m.ID()+" ("+m.Source+")"))
			sql := strings.TrimRight(m.SQL, "\n")
			if sql == "" {
				sql = "-- Go migration: runs its function; no SQL to show."
			}
			b.WriteString(sql + "\n")
		}
	}
	return b.String()
}

// FindingsText lists lint findings grouped by rule.
func FindingsText(fs []migrateport.Finding, pt Painter, width int) string {
	if len(fs) == 0 {
		return pt.Paint(OK, "No lint findings.") + "\n"
	}
	var b strings.Builder
	for _, group := range GroupFindings(fs) {
		fmt.Fprintf(&b, "%s %s\n", pt.Paint(Header, group.Rule), pt.Paint(Faint, "("+Plural(len(group.Findings), "finding", "findings")+")"))
		t := Table{NoHeader: true, Width: width, Indent: 2, Columns: []Column{
			{Title: "SEVERITY"}, {Title: "WHERE", Min: 20, Max: 48, Flex: true}, {Title: "MESSAGE", Min: 20, Flex: true},
		}}
		for _, f := range group.Findings {
			tone := Warn
			if f.Severity == migrateport.Error {
				tone = Fail
			}
			t.Rows = append(t.Rows, []Cell{C(f.Severity.String(), tone), C(fmt.Sprintf("%s:%d", f.Migration.ID(), f.Line), Muted), C(f.Message, Normal)})
		}
		b.WriteString(t.String(pt))
	}
	return b.String()
}

// FindingGroup is the findings of one rule.
type FindingGroup struct {
	Rule     string
	Findings []migrateport.Finding
}

// GroupFindings groups findings by rule, rules in order of first appearance.
func GroupFindings(fs []migrateport.Finding) []FindingGroup {
	var out []FindingGroup
	for _, f := range fs {
		i := slices.IndexFunc(out, func(g FindingGroup) bool { return g.Rule == f.Rule })
		if i < 0 {
			out = append(out, FindingGroup{Rule: f.Rule})
			i = len(out) - 1
		}
		out[i].Findings = append(out[i].Findings, f)
	}
	return out
}

// ProblemsText lists check problems.
func ProblemsText(ps []migrateport.Problem, pt Painter, width int) string {
	if len(ps) == 0 {
		return pt.Paint(OK, "No problems: ordering and checksums are consistent.") + "\n"
	}
	t := Table{Width: width, Columns: []Column{
		{Title: "PROBLEM"}, {Title: "MIGRATION", Min: 20, Flex: true}, {Title: "DETAIL", Min: 20, Flex: true},
	}}
	for _, p := range ps {
		t.Rows = append(t.Rows, []Cell{C(p.Kind.String(), Fail), C(p.Migration.ID(), Normal), C(p.Message, Muted)})
	}
	return t.String(pt)
}

// TemplateTable lists template databases.
func TemplateTable(ts []migrateport.Template, now time.Time, glyphs bool) Table {
	t := Table{Columns: append(glyphColumn(glyphs),
		Column{Title: "TEMPLATE", Min: 12, Flex: true},
		Column{Title: "STATE"},
		Column{Title: "SIZE", Right: true},
		Column{Title: "AGE", Right: true},
		Column{Title: "CREATED", Drop: 1},
	)}
	for _, tp := range ts {
		g, state := GlyphOK, "current"
		if !tp.Current {
			g, state = Glyph{"–", Muted}, "stale"
		}
		age := ""
		if !tp.CreatedAt.IsZero() {
			age = Duration(now.Sub(tp.CreatedAt))
		}
		t.Rows = append(t.Rows, append(glyphCell(glyphs, g),
			C(tp.Name, Normal), C(state, stateTone(g)), C(Size(tp.Size), Muted), C(age, Muted), C(Time(tp.CreatedAt), Faint)))
	}
	return t
}

// SeedStatusTable lists seeds with their state.
func SeedStatusTable(rows []seedport.Status, glyphs bool) Table {
	t := Table{Columns: append(glyphColumn(glyphs),
		Column{Title: "SEED", Min: 12, Flex: true},
		Column{Title: "KIND", Drop: 3},
		Column{Title: "STATE", Min: 7},
		Column{Title: "REASON", Min: 10, Flex: true},
		Column{Title: "APPLIED", Drop: 2},
		Column{Title: "ROWS", Right: true, Drop: 1},
		Column{Title: "TENANTS", Right: true, Drop: 4},
	)}
	for _, s := range rows {
		g := SeedGlyph(s.State)
		tenants := ""
		if s.Seed.PerTenant || s.Tenants > 0 {
			tenants = Count(int64(s.Tenants))
		}
		rowsText := ""
		if s.Rows > 0 {
			rowsText = Count(s.Rows)
		}
		t.Rows = append(t.Rows, append(glyphCell(glyphs, g),
			C(s.Seed.Name, Normal), C(SeedKind(s.Seed), Muted), C(s.State.String(), stateTone(g)),
			C(Reason(s.Reason), ReasonTone(s.Reason)), C(Time(s.AppliedAt), Muted), C(rowsText, Muted), C(tenants, Muted)))
	}
	return t
}

// SeedPlanTable lists a seed plan.
func SeedPlanTable(items []seedport.PlanItem, glyphs bool) Table {
	t := Table{Columns: append(glyphColumn(glyphs),
		Column{Title: "SEED", Min: 12, Flex: true},
		Column{Title: "KIND", Drop: 1},
		Column{Title: "RUNS"},
		Column{Title: "REASON", Min: 10, Flex: true},
	)}
	for _, p := range items {
		g, runs := Glyph{"–", Faint}, "no"
		if p.Reason.Runs() {
			g, runs = GlyphPending, "yes"
		}
		if p.Reason.Kind == seedport.Refused {
			g = GlyphFail
		}
		t.Rows = append(t.Rows, append(glyphCell(glyphs, g),
			C(p.Seed.Name, Normal), C(SeedKind(p.Seed), Muted), C(runs, stateTone(g)), C(Reason(p.Reason), ReasonTone(p.Reason))))
	}
	return t
}

// SeedKind is a seed's kind, marked when it runs per tenant.
func SeedKind(s seedport.Seed) string {
	if s.PerTenant {
		return s.Kind.String() + " · tenants"
	}
	return s.Kind.String()
}

func shortSum(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}
