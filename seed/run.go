package seed

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"slices"
	"sync/atomic"

	"github.com/emoss08/keel"
)

// ErrUndeclaredInput is returned when a seed reads a file it did not declare in Inputs. Reading only
// declared files keeps the fingerprint honest: every file that shapes the data is hashed.
var ErrUndeclaredInput = errors.New("seed: file is not a declared input")

// Run is what a seed's Run function works with: the database, the tenant, the environment, its
// inputs, a deterministic random source and a row counter. A Run belongs to one run of one seed (one
// tenant of a PerTenant seed); its Rand is not safe for concurrent use.
type Run struct {
	// DB is the database being seeded. Use it with the context Run receives, which carries the
	// seed's transaction.
	DB *keel.DB
	// Seed is the seed being run.
	Seed *Seed
	// Tenant is the tenant being seeded by a PerTenant seed; zero otherwise.
	Tenant Tenant
	// Environment is the environment of the run: Options.Environment, or the one recorded on the
	// database when the options name none.
	Environment Env

	prep   *prepared
	values *values
	rows   atomic.Int64
	rnd    *rand.Rand
}

// Rand returns the run's random source: a PCG generator seeded from the seed's name and fingerprint,
// and the tenant's key for a PerTenant seed, so generated data is the same on every machine until
// the seed changes.
func (r *Run) Rand() *rand.Rand {
	if r.rnd == nil {
		h := sha256.New()
		writeField(h, r.Seed.Name)
		writeField(h, r.prep.fingerprint)
		writeField(h, r.Tenant.Key)
		sum := h.Sum(nil)
		r.rnd = rand.New(rand.NewPCG(binary.LittleEndian.Uint64(sum[:8]), binary.LittleEndian.Uint64(sum[8:16])))
	}
	return r.rnd
}

// Count adds n to the rows the seed reports. Feed it the results of Copy, CreateOrSkip and the like.
// It is safe for concurrent use.
func (r *Run) Count(n int64) { r.rows.Add(n) }

// Rows returns the rows counted so far.
func (r *Run) Rows() int64 { return r.rows.Load() }

// Inputs returns the paths of the seed's input files, with glob patterns expanded, sorted.
func (r *Run) Inputs() []string { return slices.Sorted(maps.Keys(r.prep.files)) }

// Open opens one of the seed's input files, for a format the readers do not cover.
func (r *Run) Open(path string) (fs.File, error) {
	fsys, err := r.input(path)
	if err != nil {
		return nil, err
	}
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("seed: %s: %w", r.Seed.Name, err)
	}
	return f, nil
}

// ReadFile reads one of the seed's input files.
func (r *Run) ReadFile(path string) ([]byte, error) {
	fsys, err := r.input(path)
	if err != nil {
		return nil, err
	}
	b, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("seed: %s: %w", r.Seed.Name, err)
	}
	return b, nil
}

func (r *Run) input(path string) (fs.FS, error) {
	fsys, ok := r.prep.files[path]
	if !ok {
		return nil, fmt.Errorf("%w: seed %q reads %q; add it to the seed's Inputs", ErrUndeclaredInput,
			r.Seed.Name, path)
	}
	return fsys, nil
}
