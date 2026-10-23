package migratecli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emoss08/keel/migrate"
)

// Printer returns an Observer that prints events: one line each, or one JSON object each when
// asJSON is set. It is the non-interactive rendering of a run.
func Printer(w io.Writer, asJSON bool) func(migrate.Event) {
	if asJSON {
		enc := json.NewEncoder(w)
		return func(e migrate.Event) { _ = enc.Encode(toEventJSON(e)) }
	}
	p := &plain{w: w}
	return p.event
}

// plain prints events as lines and counts what the run did.
type plain struct {
	w       io.Writer
	waited  bool
	applied int
	marked  int
}

// event prints one event.
func (p *plain) event(e migrate.Event) {
	step := fmt.Sprintf("[%d/%d]", e.Index+1, e.Total)
	switch e.Kind {
	case migrate.LockWait:
		p.waited = true
		fmt.Fprintf(p.w, "waiting for the migration lock (%s)\n", ms(e.Duration))
		p.blockers("held by", e.Blockers)
	case migrate.LockAcquired:
		if p.waited {
			fmt.Fprintf(p.w, "migration lock acquired after %s\n", ms(e.Duration))
		}
	case migrate.MigrationStart:
		fmt.Fprintf(p.w, "%s applying %s%s\n", step, label(*e.Migration), notes(*e.Migration))
	case migrate.Statement:
		if e.Statements > 1 {
			fmt.Fprintf(p.w, "%s   statement %d/%d: %s\n", step, e.Statement, e.Statements, truncate(oneLine(e.SQL), 100))
		}
	case migrate.Retry:
		fmt.Fprintf(p.w, "%s lock timeout; retrying, attempt %d\n", step, e.Attempt)
		p.blockers("blocked by", e.Blockers)
	case migrate.MigrationEnd:
		p.end(step, e)
	case migrate.BaselineMarked:
		p.marked++
		fmt.Fprintf(p.w, "%s marked %s as applied\n", step, label(*e.Migration))
	case migrate.After:
		fmt.Fprintf(p.w, "after: %s\n", truncate(oneLine(e.SQL), 100))
	case migrate.RunEnd:
		p.runEnd(e)
	}
}

// end prints a MigrationEnd.
func (p *plain) end(step string, e migrate.Event) {
	if e.Err != nil {
		fmt.Fprintf(p.w, "%s failed %s after %s\n", step, label(*e.Migration), ms(e.Duration))
		return
	}
	p.applied++
	fmt.Fprintf(p.w, "%s applied %s in %s\n", step, label(*e.Migration), ms(e.Duration))
}

// runEnd prints the summary.
func (p *plain) runEnd(e migrate.Event) {
	switch {
	case e.Err != nil:
		fmt.Fprintf(p.w, "failed after %s: %s applied\n", ms(e.Duration), plural(p.applied, "migration"))
	case p.applied == 0 && p.marked == 0:
		fmt.Fprintln(p.w, "nothing to apply")
	case p.marked > 0:
		fmt.Fprintf(p.w, "applied %s and marked %s in %s\n", plural(p.applied, "migration"), plural(p.marked, "baseline"), ms(e.Duration))
	default:
		fmt.Fprintf(p.w, "applied %s in %s\n", plural(p.applied, "migration"), ms(e.Duration))
	}
}

// blockers prints one line per blocker.
func (p *plain) blockers(verb string, bs []migrate.Blocker) {
	for _, b := range bs {
		fmt.Fprintf(p.w, "  %s %s\n", verb, b)
	}
}

// notes formats a migration's kind and directives for a start line.
func notes(m migrate.Migration) string {
	var parts []string
	if m.Kind != migrate.KindVersioned {
		parts = append(parts, m.Kind.String())
	}
	parts = append(parts, directiveNotes(m.Directives)...)
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, ", ") + "]"
}

// truncate shortens s to n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// eventJSON is an event in JSON output.
type eventJSON struct {
	Event      string         `json:"event"`
	Migration  *migrationJSON `json:"migration,omitempty"`
	Index      int            `json:"index"`
	Total      int            `json:"total"`
	Statement  int            `json:"statement,omitempty"`
	Statements int            `json:"statements,omitempty"`
	Attempt    int            `json:"attempt,omitempty"`
	Blockers   []blockerJSON  `json:"blockers,omitempty"`
	DurationMS *float64       `json:"duration_ms,omitempty"`
	SQL        string         `json:"sql,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// blockerJSON is a blocker in JSON output.
type blockerJSON struct {
	PID         int32   `json:"pid"`
	Application string  `json:"application"`
	State       string  `json:"state"`
	Mode        string  `json:"mode"`
	AgeMS       float64 `json:"age_ms"`
	Query       string  `json:"query"`
}

// toEventJSON converts an event.
func toEventJSON(e migrate.Event) eventJSON {
	out := eventJSON{Event: e.Kind.String(), Index: e.Index, Total: e.Total, Statement: e.Statement,
		Statements: e.Statements, Attempt: e.Attempt, SQL: e.SQL}
	if e.Migration != nil {
		out.Migration = toMigrationJSON(*e.Migration, false)
	}
	for _, b := range e.Blockers {
		out.Blockers = append(out.Blockers, blockerJSON{PID: b.PID, Application: b.Application, State: b.State,
			Mode: b.Mode, AgeMS: msFloat(b.Age), Query: b.Query})
	}
	switch e.Kind {
	case migrate.LockWait, migrate.LockAcquired, migrate.MigrationEnd, migrate.RunEnd:
		d := msFloat(e.Duration)
		out.DurationMS = &d
	}
	if e.Err != nil {
		out.Error = e.Err.Error()
	}
	return out
}

// msFloat returns d in milliseconds with microsecond precision.
func msFloat(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
