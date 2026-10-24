package adapt

import (
	"context"
	"fmt"
	"slices"

	"github.com/emoss08/keel/keelcli/seedport"
	"github.com/emoss08/keel/seed"
)

// Environments reads and records the environment mark in keel_meta, for applications with seeds
// and without.
type Environments struct {
	DB *Database
}

// Environments implements the environment port.
var _ seedport.Environments = Environments{}

// Environment reads the database's environment mark; a database without keel_meta is unmarked.
func (x Environments) Environment(ctx context.Context) (seedport.Environment, error) {
	db, err := x.DB.DB(ctx)
	if err != nil {
		return "", err
	}
	env, err := seed.Environment(ctx, db)
	return seedport.Environment(env), err
}

// MarkEnvironment records env, one of production, staging, development and test. A database
// marked otherwise is refused.
func (x Environments) MarkEnvironment(ctx context.Context, env seedport.Environment) error {
	db, err := x.DB.DB(ctx)
	if err != nil {
		return err
	}
	return seed.MarkEnvironment(ctx, db, seed.Env(env), false)
}

// Seeds implements seedport.Engine over a keel/seed registry. Options carries the tenants, the
// system scope and the parallelism every run uses.
type Seeds struct {
	Environments
	Registry *seed.Registry
	Options  seed.Options
}

// Seeds implements every seed port.
var _ seedport.Engine = (*Seeds)(nil)

// Graph lists the registered seeds in dependency order.
func (x *Seeds) Graph() []seedport.Seed {
	seeds := x.Registry.Seeds()
	out := make([]seedport.Seed, len(seeds))
	for i, s := range seeds {
		out[i] = seedDef(s)
	}
	return out
}

// Status lists the registered seeds with their state, then the recorded ones no longer
// registered.
func (x *Seeds) Status(ctx context.Context) ([]seedport.Status, error) {
	db, err := x.DB.DB(ctx)
	if err != nil {
		return nil, err
	}
	st, err := x.Registry.Status(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]seedport.Status, len(st))
	for i, s := range st {
		out[i] = seedStatus(s)
	}
	return out, nil
}

// options are the run options with a selection, force and observer.
func (x *Seeds) options(only []string, force bool, observe func(seed.Event)) seed.Options {
	o := x.Options
	o.Only, o.Force, o.Observer, o.DryRun = only, force, observe, false
	return o
}

// Plan reports which seeds a run would apply and why.
func (x *Seeds) Plan(ctx context.Context, opts seedport.PlanOptions) ([]seedport.PlanItem, error) {
	db, err := x.DB.DB(ctx)
	if err != nil {
		return nil, err
	}
	p, err := x.Registry.Plan(ctx, db, x.options(opts.Only, opts.Force, nil))
	if err != nil {
		return nil, err
	}
	return planItems(p), nil
}

// Apply runs the seeds the plan selects, translating the engine's events.
func (x *Seeds) Apply(ctx context.Context, opts seedport.ApplyOptions, observe seedport.Observer) error {
	t := newSeedEvents(observe)
	db, err := x.DB.DB(ctx)
	if err != nil {
		t.observe(seedport.Event{Kind: seedport.RunEnd, Err: err})
		return err
	}
	_, err = x.Registry.Apply(ctx, db, x.options(opts.Only, opts.Force, t.event))
	return err
}

// Prune deletes the records of seeds no longer registered and returns what they recorded.
func (x *Seeds) Prune(ctx context.Context) ([]seedport.Status, error) {
	before, err := x.Status(ctx)
	if err != nil {
		return nil, err
	}
	db, err := x.DB.DB(ctx)
	if err != nil {
		return nil, err
	}
	names, err := x.Registry.Prune(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("keelcli: prune seeds: %w", err)
	}
	var out []seedport.Status
	for _, name := range names {
		i := slices.IndexFunc(before, func(s seedport.Status) bool { return s.Seed.Name == name && s.State == seedport.Orphaned })
		if i < 0 {
			out = append(out, seedport.Status{Seed: seedport.Seed{Name: name}, State: seedport.Orphaned})
			continue
		}
		out = append(out, before[i])
	}
	return out, nil
}
