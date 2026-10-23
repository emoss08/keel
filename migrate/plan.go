package migrate

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Plan is what Up would do.
type Plan struct {
	// Marks are baselines recorded as applied without running, because the database's history
	// already holds what they replace.
	Marks []Migration
	// Pending are the versioned, Go and baseline migrations Up runs, in version order.
	Pending []Migration
	// Repeatables are the repeatable migrations Up runs after Pending, in name order.
	Repeatables []Migration
	// After are the Options.After statements, set when Up runs anything.
	After []string
}

// Total is the number of migrations the plan marks or runs.
func (p Plan) Total() int { return len(p.Marks) + len(p.Pending) + len(p.Repeatables) }

// Empty reports whether the plan does nothing.
func (p Plan) Empty() bool { return p.Total() == 0 }

// State is a migration's state in the database.
type State uint8

const (
	// StateApplied is applied with the current checksum.
	StateApplied State = iota
	// StatePending is not applied yet.
	StatePending
	// StateChanged is applied, but its file changed since.
	StateChanged
	// StateRepeatableDue is a repeatable migration never applied, or changed since it was.
	StateRepeatableDue
	// StateMissing is applied in the database but no longer in the set.
	StateMissing
)

// stateNames are the names of the states.
var stateNames = [...]string{"applied", "pending", "changed", "repeatable-due", "missing"}

// String returns the state's name, such as repeatable-due.
func (s State) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return fmt.Sprintf("State(%d)", s)
}

// MarshalText encodes the state as its name.
func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Status is a migration with its state in the database. For a missing migration, Migration holds
// what the database recorded.
type Status struct {
	Migration
	State State
	// AppliedAt and Duration are set for an applied, changed or missing migration, and for a
	// repeatable migration applied before.
	AppliedAt time.Time
	Duration  time.Duration
	// AppliedChecksum is the checksum the database recorded.
	AppliedChecksum string
	// OutOfOrder marks a pending migration older than the newest applied one.
	OutOfOrder bool
	// Mark marks a pending baseline that Up records without running.
	Mark bool
	// Gap marks a pending baseline that cannot apply, because the database's history ends before it.
	Gap bool
}

// CheckReport is what Check found.
type CheckReport struct {
	Pending    []Status
	OutOfOrder []Status
	Changed    []Status
	Missing    []Status
	Gaps       []Status
}

// OK reports whether the report holds no problem; pending migrations are not one.
func (r CheckReport) OK() bool {
	return len(r.OutOfOrder)+len(r.Changed)+len(r.Missing)+len(r.Gaps) == 0
}

// CheckAgainst compares set with base, the migrations on the branch it merges into, without a
// database: a migration of set missing from base and older than base's newest version is out of
// order, a file that differs is changed, and a migration of base missing from set is missing.
func CheckAgainst(set, base Set) (CheckReport, error) {
	if set.err != nil {
		return CheckReport{}, set.err
	}
	if base.err != nil {
		return CheckReport{}, base.err
	}
	recs := make([]Record, len(base.migrations))
	for i, m := range base.migrations {
		recs[i] = Record{Version: m.Version, Name: m.Name, Source: m.Source, Kind: m.Kind, Checksum: m.Checksum}
	}
	return analyze(set.migrations, recs).report(), nil
}

// analysis compares a set with a database's history.
type analysis struct {
	statuses []Status
	marks    []Migration
	pending  []Migration
	due      []Migration
}

// analyze computes every migration's state.
func analyze(ms []Migration, recs []Record) analysis {
	byVersion, byName := map[string]Record{}, map[string]Record{}
	newest := ""
	for _, r := range recs {
		if r.Kind == KindRepeatable {
			byName[r.Name] = r
			continue
		}
		byVersion[r.Version] = r
		newest = max(newest, r.Version)
	}
	var a analysis
	covered := ""
	seen, seenNames := map[string]bool{}, map[string]bool{}
	for _, m := range ms {
		if m.Kind == KindRepeatable {
			seenNames[m.Name] = true
			a.repeatable(m, byName)
			continue
		}
		seen[m.Version] = true
		r, ok := byVersion[m.Version]
		if m.Kind == KindBaseline {
			covered = m.Version
			a.baseline(m, r, ok, newest)
			continue
		}
		if !ok {
			a.statuses = append(a.statuses, Status{Migration: m, State: StatePending, OutOfOrder: m.Version < newest})
			a.pending = append(a.pending, m)
			continue
		}
		a.statuses = append(a.statuses, applied(m, r))
	}
	for _, r := range recs {
		gone := (r.Kind == KindRepeatable && !seenNames[r.Name]) ||
			(r.Kind != KindRepeatable && !seen[r.Version] && r.Version > covered)
		if gone {
			a.statuses = append(a.statuses, Status{
				Migration: Migration{Version: r.Version, Name: r.Name, Source: r.Source, Kind: r.Kind, Checksum: r.Checksum},
				State:     StateMissing, AppliedAt: r.AppliedAt, Duration: r.Duration, AppliedChecksum: r.Checksum,
			})
		}
	}
	sort.SliceStable(a.statuses, func(i, j int) bool {
		x, y := a.statuses[i], a.statuses[j]
		if (x.Kind == KindRepeatable) != (y.Kind == KindRepeatable) {
			return y.Kind == KindRepeatable
		}
		if x.Version != y.Version {
			return x.Version < y.Version
		}
		return x.Name < y.Name
	})
	return a
}

// applied returns the status of a migration the database recorded as r.
func applied(m Migration, r Record) Status {
	s := Status{Migration: m, State: StateApplied, AppliedAt: r.AppliedAt, Duration: r.Duration, AppliedChecksum: r.Checksum}
	if r.Checksum != m.Checksum {
		s.State = StateChanged
	}
	return s
}

// repeatable adds a repeatable migration.
func (a *analysis) repeatable(m Migration, byName map[string]Record) {
	r, ok := byName[m.Name]
	s := Status{Migration: m, State: StateRepeatableDue}
	if ok {
		s.AppliedAt, s.Duration, s.AppliedChecksum = r.AppliedAt, r.Duration, r.Checksum
		if r.Checksum == m.Checksum {
			s.State = StateApplied
		}
	}
	if s.State == StateRepeatableDue {
		a.due = append(a.due, m)
	}
	a.statuses = append(a.statuses, s)
}

// baseline adds a baseline: applied when recorded as one, marked when the database's history
// reaches it, run on an empty history, and a gap otherwise.
func (a *analysis) baseline(m Migration, r Record, ok bool, newest string) {
	switch {
	case ok && r.Kind == KindBaseline:
		a.statuses = append(a.statuses, applied(m, r))
	case ok || (newest != "" && newest >= m.Version):
		a.statuses = append(a.statuses, Status{Migration: m, State: StatePending, Mark: true})
		a.marks = append(a.marks, m)
	case newest == "":
		a.statuses = append(a.statuses, Status{Migration: m, State: StatePending})
		a.pending = append(a.pending, m)
	default:
		a.statuses = append(a.statuses, Status{Migration: m, State: StatePending, Gap: true})
	}
}

// report sorts the statuses into a CheckReport.
func (a analysis) report() CheckReport {
	var r CheckReport
	for _, s := range a.statuses {
		switch {
		case s.Gap:
			r.Gaps = append(r.Gaps, s)
		case s.State == StatePending && s.OutOfOrder:
			r.OutOfOrder = append(r.OutOfOrder, s)
		case s.State == StatePending || s.State == StateRepeatableDue:
			r.Pending = append(r.Pending, s)
		case s.State == StateChanged:
			r.Changed = append(r.Changed, s)
		case s.State == StateMissing:
			r.Missing = append(r.Missing, s)
		}
	}
	return r
}

// plan turns an analysis into what Up does, or the error that stops it.
func plan(ms []Migration, recs []Record, o Options, uo UpOptions) (Plan, error) {
	a := analyze(ms, recs)
	rep := a.report()
	if len(rep.Changed) > 0 {
		return Plan{}, fmt.Errorf("%w: %s; add a new migration instead, or run repair after a comment-only edit",
			ErrChanged, ids(rep.Changed))
	}
	if len(rep.Gaps) > 0 {
		return Plan{}, fmt.Errorf("%w: %s; apply the migrations it replaced from an earlier release first",
			ErrBaselineGap, ids(rep.Gaps))
	}
	if len(rep.OutOfOrder) > 0 && !o.AllowOutOfOrder {
		return Plan{}, fmt.Errorf("%w: %s; give it a newer version, or set AllowOutOfOrder", ErrOutOfOrder, ids(rep.OutOfOrder))
	}
	p := Plan{Marks: a.marks, Pending: a.pending, Repeatables: a.due}
	if uo.To != "" {
		if !hasVersion(ms, uo.To) {
			return Plan{}, fmt.Errorf("migrate: no migration has version %s", uo.To)
		}
		p.Marks, p.Pending = upTo(p.Marks, uo.To), upTo(p.Pending, uo.To)
		if len(p.Pending) < len(a.pending) {
			p.Repeatables = nil
		}
	}
	if len(p.Pending)+len(p.Repeatables) > 0 {
		p.After = o.After
	}
	return p, nil
}

// upTo returns the migrations with a version up to to.
func upTo(ms []Migration, to string) []Migration {
	var out []Migration
	for _, m := range ms {
		if m.Version <= to {
			out = append(out, m)
		}
	}
	return out
}

// hasVersion reports whether ms has a non-repeatable migration with version.
func hasVersion(ms []Migration, version string) bool {
	for _, m := range ms {
		if m.Kind != KindRepeatable && m.Version == version {
			return true
		}
	}
	return false
}

// ids joins the statuses' IDs and sources.
func ids(ss []Status) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = s.ID() + " (" + s.Source + ")"
	}
	return strings.Join(parts, ", ")
}
