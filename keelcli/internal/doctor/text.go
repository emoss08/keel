package doctor

import (
	"fmt"
	"strings"
	"time"

	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/internal/render"
)

// ChecksTable lists the checks; glyphs adds a level symbol column.
func ChecksTable(r Report, glyphs bool) render.Table {
	t := render.Table{Columns: []render.Column{
		{Title: "CHECK", Min: 10},
		{Title: "RESULT", Min: 4},
		{Title: "DETAIL", Min: 16, Flex: true},
	}}
	if glyphs {
		t.Columns = append([]render.Column{{Title: " ", Max: 1}}, t.Columns...)
	}
	for _, c := range r.Checks {
		g := c.Level.Glyph()
		row := []render.Cell{render.C(c.Name, render.Normal), render.C(c.Level.String(), g.Tone), render.C(c.Detail, render.Muted)}
		if glyphs {
			row = append([]render.Cell{render.C(g.Symbol, g.Tone)}, row...)
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// BackendsTable lists backends with their lock and query.
func BackendsTable(bs []factsport.Backend) render.Table {
	t := render.Table{Columns: []render.Column{
		{Title: "PID", Right: true},
		{Title: "APPLICATION", Min: 8, Max: 24, Flex: true},
		{Title: "STATE", Min: 6, Max: 20, Drop: 3},
		{Title: "LOCK", Max: 20, Drop: 2},
		{Title: "AGE", Right: true},
		{Title: "QUERY", Min: 12, Flex: true},
	}}
	for _, b := range bs {
		state := b.State
		if b.Waiting {
			state = "waiting on " + pids(b.BlockedBy)
		}
		lock := b.Mode
		if b.Relation != "" {
			lock += " " + b.Relation
		}
		tone := render.Muted
		if b.Age >= 5*time.Minute {
			tone = render.Warn
		}
		t.Rows = append(t.Rows, []render.Cell{
			render.C(fmt.Sprint(b.PID), render.Normal), render.C(b.Application, render.Normal),
			render.C(state, render.Muted), render.C(lock, render.Faint), render.C(render.Duration(b.Age), tone),
			render.C(render.OneLine(b.Query), render.Faint),
		})
	}
	return t
}

func pids(ps []int32) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, ",")
}

// Text renders the report for a terminal or a log.
func Text(r Report, p render.Painter, width int, glyphs bool) string {
	var b strings.Builder
	checks := ChecksTable(r, glyphs)
	checks.Width = width
	b.WriteString(checks.String(p))
	section := func(title string, bs []factsport.Backend) {
		if len(bs) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s\n", p.Paint(render.Strong, title))
		t := BackendsTable(bs)
		t.Width = width
		b.WriteString(t.String(p))
	}
	section("Lock holders", r.Activity.LockHolders)
	section("Long-running queries", r.Activity.LongQueries)
	return b.String()
}

// JSON is the document of doctor.
func JSON(r Report) any {
	type check struct {
		Name   string `json:"name"`
		Level  string `json:"level"`
		Detail string `json:"detail"`
	}
	type backend struct {
		PID         int32   `json:"pid"`
		Application string  `json:"application"`
		User        string  `json:"user,omitempty"`
		State       string  `json:"state"`
		Mode        string  `json:"mode,omitempty"`
		Relation    string  `json:"relation,omitempty"`
		AgeMS       int64   `json:"age_ms"`
		Query       string  `json:"query"`
		Waiting     bool    `json:"waiting,omitempty"`
		BlockedBy   []int32 `json:"blocked_by,omitempty"`
	}
	conv := func(bs []factsport.Backend) []backend {
		out := make([]backend, 0, len(bs))
		for _, b := range bs {
			out = append(out, backend{b.PID, b.Application, b.User, b.State, b.Mode, b.Relation, b.Age.Milliseconds(), b.Query, b.Waiting, b.BlockedBy})
		}
		return out
	}
	checks := make([]check, 0, len(r.Checks))
	for _, c := range r.Checks {
		checks = append(checks, check{c.Name, c.Level.String(), c.Detail})
	}
	return map[string]any{
		"ok":           !r.Failed(),
		"server":       r.Connection.ServerVersion,
		"database":     r.Connection.Database,
		"environment":  string(r.Environment),
		"schema":       r.Schema,
		"checks":       checks,
		"lock_holders": conv(r.Activity.LockHolders),
		"long_queries": conv(r.Activity.LongQueries),
	}
}
