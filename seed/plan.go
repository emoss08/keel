package seed

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/emoss08/keel"
)

// Reason says why a seed runs, or why it does not.
type Reason uint8

const (
	// New: the seed has never been applied to the database.
	New Reason = iota + 1
	// InputChanged: an input file was added, removed or edited since the seed was applied;
	// PlanItem.Changed names the paths.
	InputChanged
	// VersionChanged: the seed's Version differs from the applied one.
	VersionChanged
	// CodeChanged: the seed's Fingerprint function returns a different value.
	CodeChanged
	// Forced: Options.Force runs the seed whatever its state.
	Forced
	// UpToDate: the reference seed was applied with its current fingerprint; it does not run.
	UpToDate
	// FixtureApplied: the fixture was applied once; fixtures never run again.
	FixtureApplied
	// Excluded: the seed is not named in Options.Only, nor needed by a seed that is.
	Excluded
	// Refused: the seed is a fixture and the database is marked production.
	Refused
)

// String returns the reason in lower case.
func (r Reason) String() string {
	switch r {
	case New:
		return "new"
	case InputChanged:
		return "input changed"
	case VersionChanged:
		return "version changed"
	case CodeChanged:
		return "code changed"
	case Forced:
		return "forced"
	case UpToDate:
		return "up to date"
	case FixtureApplied:
		return "fixture applied"
	case Excluded:
		return "excluded"
	case Refused:
		return "refused"
	}
	return "unknown"
}

// Runs reports whether a seed with this reason runs.
func (r Reason) Runs() bool { return r >= New && r <= Forced }

// Plan is what Apply would do.
type Plan struct {
	// Environment is the environment recorded on the database; empty when it is not marked.
	Environment Env
	// Seeds lists every registered seed in dependency order.
	Seeds []PlanItem
}

// Runs returns the items of the seeds that run, in the order they start with Parallel 1.
func (p Plan) Runs() []PlanItem {
	var out []PlanItem
	for _, it := range p.Seeds {
		if it.Run {
			out = append(out, it)
		}
	}
	return out
}

// PlanItem is one seed of a plan.
type PlanItem struct {
	// Seed is the registered seed.
	Seed *Seed
	// Reason is why the seed runs or does not.
	Reason Reason
	// Run reports whether the seed runs.
	Run bool
	// Changed lists the input paths added, removed or edited since the seed was applied.
	Changed []string
	// Detail explains the reason in words, naming every change found.
	Detail string
	// Fingerprint is the seed's current fingerprint.
	Fingerprint string
	// AppliedAt is when the seed was last applied; zero when it never was.
	AppliedAt time.Time
	// TenantsApplied is the number of tenants a PerTenant seed has applied with its current
	// fingerprint, including those of an unfinished run.
	TenantsApplied int
}

// Plan returns what Apply would do with opts, without writing anything or taking the lock.
func (g *Registry) Plan(ctx context.Context, db *keel.DB, opts Options) (Plan, error) {
	gr, err := prepare(g.list())
	if err != nil {
		return Plan{}, err
	}
	st, err := loadState(ctx, db.Pool())
	if err != nil {
		return Plan{}, err
	}
	return gr.plan(st, opts)
}

// plan decides, for every seed, whether it runs and why.
func (gr *graph) plan(st *state, opts Options) (Plan, error) {
	if err := checkEnv(opts.Environment, st.env); err != nil {
		return Plan{}, err
	}
	included, forced, err := gr.selection(opts.Only, opts.Force)
	if err != nil {
		return Plan{}, err
	}
	production := st.env == Production || opts.Environment == Production
	p := Plan{Environment: st.env, Seeds: make([]PlanItem, len(gr.seeds))}
	for i, ps := range gr.seeds {
		it := decide(ps, st.seeds[ps.seed.Name], included[ps.seed], forced[ps.seed], production)
		it.TenantsApplied = st.tenants[tenantKey{ps.seed.Name, ps.fingerprint}]
		p.Seeds[i] = it
	}
	return p, nil
}

// selection returns the seeds a run includes (those named in only and their After seeds, or every
// seed) and those it forces.
func (gr *graph) selection(only []string, force bool) (included, forced map[*Seed]bool, err error) {
	included = map[*Seed]bool{}
	forced = map[*Seed]bool{}
	if len(only) == 0 {
		for _, p := range gr.seeds {
			included[p.seed] = true
			forced[p.seed] = force
		}
		return included, forced, nil
	}
	var errs []error
	var include func(s *Seed)
	include = func(s *Seed) {
		if included[s] {
			return
		}
		included[s] = true
		for _, a := range s.After {
			include(a)
		}
	}
	for _, name := range only {
		p, ok := gr.byName[name]
		if !ok {
			errs = append(errs, fmt.Errorf("seed: no seed named %q is registered", name))
			continue
		}
		include(p.seed)
		forced[p.seed] = force
	}
	return included, forced, errors.Join(errs...)
}

// decide returns a seed's plan item from its stored row.
func decide(p *prepared, s *stored, included, forced, production bool) PlanItem {
	it := PlanItem{Seed: p.seed, Fingerprint: p.fingerprint}
	if s != nil {
		it.AppliedAt = s.appliedAt
	}
	switch {
	case !included:
		it.Reason, it.Detail = Excluded, "not selected"
	case p.seed.Kind == Fixture && production:
		it.Reason, it.Detail = Refused, "fixtures never run against a production database"
	case s == nil:
		it.Reason, it.Detail = New, "never applied"
	case forced:
		it.Reason, it.Detail = Forced, "forced"
	case p.seed.Kind == Fixture:
		it.Reason, it.Detail = FixtureApplied, "fixtures run once"
	case s.fingerprint == p.fingerprint:
		it.Reason, it.Detail = UpToDate, "fingerprint unchanged"
	default:
		it.Reason, it.Changed, it.Detail = changes(p, s)
	}
	it.Run = it.Reason.Runs()
	return it
}

// changes explains why a seed's fingerprint differs from the stored one. Every change is named in
// the detail; the reason is the first of input, version and code changes.
func changes(p *prepared, s *stored) (Reason, []string, string) {
	var reasons []Reason
	var details []string
	changed := changedInputs(p.inputs, s.inputs)
	if len(changed) > 0 {
		reasons = append(reasons, InputChanged)
		details = append(details, "inputs changed: "+strings.Join(changed, ", "))
	}
	if s.version != p.seed.Version {
		reasons = append(reasons, VersionChanged)
		details = append(details, fmt.Sprintf("version %q → %q", s.version, p.seed.Version))
	}
	if s.code != p.code {
		reasons = append(reasons, CodeChanged)
		details = append(details, "Fingerprint function result changed")
	}
	if len(reasons) == 0 {
		return CodeChanged, nil, "fingerprint changed"
	}
	return reasons[0], changed, strings.Join(details, "; ")
}

// changedInputs returns the paths whose hash differs between cur and old, or that only one has.
func changedInputs(cur, old map[string]string) []string {
	set := map[string]bool{}
	for path, h := range cur {
		if old[path] != h {
			set[path] = true
		}
	}
	for path := range old {
		if _, ok := cur[path]; !ok {
			set[path] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}
