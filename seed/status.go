package seed

import (
	"context"
	"slices"
	"time"

	"github.com/emoss08/keel"
)

// State is a seed's state on a database.
type State uint8

const (
	// Applied: the seed is applied with its current fingerprint, or is a fixture applied once.
	Applied State = iota + 1
	// Pending: the seed has never been applied.
	Pending
	// Changed: the reference seed was applied, but its fingerprint changed since; Status.Reason
	// says how.
	Changed
	// Removed: the database records the seed, but it is no longer registered. Prune deletes it.
	Removed
)

// String returns the state in lower case.
func (s State) String() string {
	switch s {
	case Applied:
		return "applied"
	case Pending:
		return "pending"
	case Changed:
		return "changed"
	case Removed:
		return "removed"
	}
	return "unknown"
}

// Status is one seed's state on a database.
type Status struct {
	// Name is the seed's name.
	Name string
	// Seed is the registered seed; nil when Removed.
	Seed *Seed
	// Kind is the seed's kind, read from the database when Removed; zero when only tenants of an
	// unfinished run are recorded.
	Kind Kind
	// State is the seed's state.
	State State
	// Reason is New for a pending seed, the change for a changed one, and UpToDate or
	// FixtureApplied for an applied one.
	Reason Reason
	// Changed lists the input paths added, removed or edited since the seed was applied.
	Changed []string
	// Detail explains the state in words.
	Detail string
	// AppliedAt is when the seed was last applied; zero when it never was.
	AppliedAt time.Time
	// Duration is how long the last applied run took.
	Duration time.Duration
	// Rows is the number of rows the last applied run reported.
	Rows int64
	// Tenants is the number of tenants a PerTenant seed has applied with its current fingerprint,
	// including those of an unfinished run.
	Tenants int
}

// Status returns the state of every registered seed, in dependency order, followed by the seeds the
// database records that are no longer registered.
func (g *Registry) Status(ctx context.Context, db *keel.DB) ([]Status, error) {
	gr, err := prepare(g.list())
	if err != nil {
		return nil, err
	}
	st, err := loadState(ctx, db.Pool())
	if err != nil {
		return nil, err
	}
	plan, err := gr.plan(st, Options{})
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(plan.Seeds))
	for _, it := range plan.Seeds {
		out = append(out, statusOf(it, st.seeds[it.Seed.Name]))
	}
	for _, name := range st.names {
		if _, ok := gr.byName[name]; !ok {
			out = append(out, removedStatus(st, name))
		}
	}
	return out, nil
}

func statusOf(it PlanItem, s *stored) Status {
	out := Status{Name: it.Seed.Name, Seed: it.Seed, Kind: it.Seed.Kind, Reason: it.Reason,
		Changed: it.Changed, Detail: it.Detail, Tenants: it.TenantsApplied}
	switch it.Reason {
	case New:
		out.State = Pending
	case UpToDate, FixtureApplied:
		out.State = Applied
	default:
		out.State = Changed
	}
	if s != nil {
		out.AppliedAt, out.Duration, out.Rows = s.appliedAt, s.duration, s.rows
	}
	return out
}

func removedStatus(st *state, name string) Status {
	out := Status{Name: name, State: Removed, Detail: "no longer registered"}
	s, ok := st.seeds[name]
	if !ok {
		for k, n := range st.tenants {
			if k.name == name {
				out.Tenants += n
			}
		}
		return out
	}
	out.Kind = Reference
	if s.kind == Fixture.String() {
		out.Kind = Fixture
	}
	out.AppliedAt, out.Duration, out.Rows = s.appliedAt, s.duration, s.rows
	out.Tenants = st.tenants[tenantKey{name, s.fingerprint}]
	return out
}

// Prune deletes the bookkeeping of seeds the database records that are no longer registered, and
// returns their names. It waits for a running Apply to finish.
func (g *Registry) Prune(ctx context.Context, db *keel.DB) ([]string, error) {
	names := map[string]bool{}
	for _, s := range g.list() {
		names[s.Name] = true
	}
	var pruned []string
	err := keel.Write(ctx, db, func(ctx context.Context) error {
		if err := keel.LockKey(ctx, db, lockKey); err != nil {
			return err
		}
		if err := createTables(ctx, db); err != nil {
			return err
		}
		st, err := loadState(ctx, db.Pool())
		if err != nil {
			return err
		}
		pruned = nil
		for _, name := range st.names {
			if !names[name] {
				pruned = append(pruned, name)
			}
		}
		if len(pruned) == 0 {
			return nil
		}
		if _, err := keel.Exec(ctx, db, deleteSeedTenants, pruned); err != nil {
			return err
		}
		_, err = keel.Exec(ctx, db, deleteSeeds, pruned)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(pruned)
	return pruned, nil
}
