package seed

import (
	"fmt"
	"reflect"
	"sync"
)

// Key names a value of type T that one seed puts and seeds after it read, within one Apply:
//
//	var DevOrg = seed.Key[*tenant.Organization]("dev-org")
//
//	seed.Put(r, DevOrg, org)    // in the seed that creates it
//	org := seed.Must(r, DevOrg) // in a seed that runs After it
//
// Values do not outlive the Apply. A seed can run without the seed that puts a value running too
// (that seed is up to date, or is a fixture already applied), so a seed that may run alone reads the
// value with Get and loads it from the database when it is absent.
type Key[T any] string

// values holds the values seeds put during one Apply.
type values struct {
	mu sync.Mutex
	m  map[string]any
}

// Put stores v under key for the seeds that run after r's seed.
func Put[T any](r *Run, key Key[T], v T) {
	r.values.mu.Lock()
	defer r.values.mu.Unlock()
	r.values.m[string(key)] = v
}

// Get returns the value stored under key, and whether a seed stored one in this Apply.
func Get[T any](r *Run, key Key[T]) (T, bool) {
	r.values.mu.Lock()
	v, ok := r.values.m[string(key)]
	r.values.mu.Unlock()
	if !ok {
		var zero T
		return zero, false
	}
	if v == nil {
		var zero T
		return zero, true
	}
	t, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("seed: %s reads key %q as %v, but it holds a %T", r.Seed.Name, string(key),
			reflect.TypeFor[T](), v))
	}
	return t, true
}

// Must returns the value stored under key. It panics, failing the seed, when no seed stored one in
// this Apply.
func Must[T any](r *Run, key Key[T]) T {
	v, ok := Get(r, key)
	if !ok {
		panic(fmt.Sprintf("seed: %s needs key %q, which no seed put in this run; run the seed that "+
			"puts it in the same Apply, or read it with seed.Get and load it when absent",
			r.Seed.Name, string(key)))
	}
	return v
}
