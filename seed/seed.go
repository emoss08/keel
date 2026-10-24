// Package seed applies the data a database needs, reference data and development fixtures, as
// declared seeds that re-run exactly when what they are made from changes.
//
// A Registry holds Seeds. Apply runs each one in its own transaction (a keel.Write, or the
// application's through Options.Transact), in After order, together with its bookkeeping row, so
// a seed either applies and is recorded or leaves no trace. A Reference seed runs again when its
// fingerprint (its name, Version, Inputs and Fingerprint function) changes; a Fixture seed runs
// once and refuses to run against a database marked production.
package seed

import (
	"context"
	"io/fs"
	"slices"
	"sync"
	"time"
)

// Kind says what a seed's data is for.
type Kind uint8

const (
	// Reference data is data production needs: states, hazmat references, roles. It re-runs when
	// its fingerprint changes and must therefore be idempotent (upserts).
	Reference Kind = iota + 1
	// Fixture data is development data: test organizations, workers, shipments. It runs once per
	// database and never against a database marked production.
	Fixture
)

// String returns "reference" or "fixture".
func (k Kind) String() string {
	switch k {
	case Reference:
		return "reference"
	case Fixture:
		return "fixture"
	}
	return "unknown"
}

// Seed declares one seed. Register it with Registry.Add and refer to it by the pointer Add returns.
type Seed struct {
	// Name is the seed's stable identity, the key of its bookkeeping row.
	Name string
	// Kind is Reference or Fixture.
	Kind Kind
	// Description says what the seed writes, for status output.
	Description string
	// After lists seeds that must have run first. They must be in the same (merged) registry.
	After []*Seed
	// Inputs are the files the seed reads. Their contents join the fingerprint, and they are the
	// only files YAML, JSON, CSV, CSVEach and Run.Open read.
	Inputs []Input
	// Fingerprint returns a value that joins the fingerprint, for data built from code (a
	// permission registry, template starters).
	Fingerprint func() string
	// Version joins the fingerprint; change it when a change to Run changes what it writes.
	Version string
	// PerTenant runs Run once per tenant, each in its own transaction with the tenant's scope.
	PerTenant bool
	// Timeout bounds one run of the seed, or of one tenant for a PerTenant seed. Zero means none.
	Timeout time.Duration
	// Run does the work. Its context carries the seed's transaction and scope, so every keel call
	// made with it joins the transaction.
	Run func(ctx context.Context, r *Run) error
}

// Input is one file a seed reads.
type Input struct {
	// FS holds the file, typically an embed.FS.
	FS fs.FS
	// Path is the file's path in FS, or a fs.Glob pattern that must match at least one file.
	Path string
}

// Files declares input files of fsys. A path may be a glob pattern (data/*.csv).
func Files(fsys fs.FS, paths ...string) []Input {
	in := make([]Input, len(paths))
	for i, p := range paths {
		in[i] = Input{FS: fsys, Path: p}
	}
	return in
}

// Registry is a set of seeds. Registries are usually package variables filled by Add at package
// initialisation; they are safe for concurrent use.
type Registry struct {
	mu    sync.Mutex
	seeds []*Seed
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Add registers a copy of s and returns it, so other seeds can name it in After. Problems with the
// seed are reported by Validate, Plan and Apply, all at once.
func (g *Registry) Add(s Seed) *Seed {
	p := &s
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seeds = append(g.seeds, p)
	return p
}

// Merge returns a registry holding the seeds of every registry given, for a binary that applies
// reference seeds and fixtures together. A seed in several of them is added once. It takes the seeds
// registered when it is called.
func Merge(regs ...*Registry) *Registry {
	out := NewRegistry()
	for _, g := range regs {
		for _, s := range g.list() {
			if !slices.Contains(out.seeds, s) {
				out.seeds = append(out.seeds, s)
			}
		}
	}
	return out
}

// Seeds returns the registered seeds in dependency order: every seed after the seeds it names in
// After, otherwise in registration order. Seeds caught in a cycle come last, in registration order.
func (g *Registry) Seeds() []*Seed { return order(g.list()) }

// Validate reports every problem with the registry at once: missing names, duplicate names,
// unknown or cyclic After seeds, Reference seeds after Fixture seeds, and unreadable inputs.
func (g *Registry) Validate() error {
	_, err := prepare(g.list())
	return err
}

func (g *Registry) list() []*Seed {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.seeds)
}
