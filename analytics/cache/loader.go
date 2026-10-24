package cache

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"sync"
	"time"
)

// defaultFlightShards is the number of locks the in-flight loads of a Loader are spread over.
const defaultFlightShards = 32

// LoaderOptions configure a Loader.
type LoaderOptions struct {
	// OnError receives the store's errors. A failing Get is treated as a miss and a failing Set
	// still returns the loaded value, so a cache outage slows reports down without failing them.
	OnError func(err error)
}

// Loader reads values through a Store and loads the missing ones, once per key at a time in the
// process: concurrent misses of a key wait for a single load and share its value. In-flight loads
// are spread over sharded locks, so there is no lock every key shares.
type Loader struct {
	store   Store
	onError func(error)
	shards  [defaultFlightShards]flightShard
	seed    maphash.Seed
}

// flightShard holds the in-flight loads of a share of the keys.
type flightShard struct {
	mu    sync.Mutex
	calls map[string]*flight
}

// flight is one in-flight load: done closes when value and err are set. waiters counts the callers
// still waiting; the load is cancelled, and forgotten, when the last one gives up.
type flight struct {
	done    chan struct{}
	value   []byte
	err     error
	waiters int
	cancel  context.CancelFunc
}

// NewLoader returns a Loader over store.
func NewLoader(store Store, o LoaderOptions) *Loader {
	l := &Loader{store: store, onError: o.OnError, seed: maphash.MakeSeed()}
	for i := range l.shards {
		l.shards[i].calls = map[string]*flight{}
	}
	return l
}

// Load returns the value under key and true when the store had it. Otherwise it calls load, stores
// the result for ttl and returns it with false; callers that miss the same key meanwhile share
// that call. load runs with the first caller's context values but without its cancellation, and
// is cancelled only when every caller waiting for it has given up. A load error is returned to
// every waiter and nothing is stored.
func (l *Loader) Load(ctx context.Context, key string, ttl time.Duration,
	load func(ctx context.Context) ([]byte, error),
) ([]byte, bool, error) {
	v, ok, err := l.store.Get(ctx, key)
	if err != nil {
		l.report(fmt.Errorf("cache: get: %w", err))
	} else if ok {
		return v, true, nil
	}
	s := &l.shards[maphash.String(l.seed, key)%defaultFlightShards]
	s.mu.Lock()
	f := s.calls[key]
	if f == nil {
		lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &flight{done: make(chan struct{}), cancel: cancel}
		s.calls[key] = f
		go l.run(lctx, s, f, key, ttl, load)
	}
	f.waiters++
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.value, false, f.err
	case <-ctx.Done():
		s.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
			s.drop(key, f)
		}
		s.mu.Unlock()
		return nil, false, ctx.Err()
	}
}

// run performs a flight's load, stores its value and wakes the waiters.
func (l *Loader) run(ctx context.Context, s *flightShard, f *flight, key string, ttl time.Duration,
	load func(ctx context.Context) ([]byte, error),
) {
	defer f.cancel()
	value, err := guarded(ctx, load)
	if err == nil {
		if serr := l.store.Set(ctx, key, value, ttl); serr != nil && !errors.Is(serr, ErrTooLarge) {
			l.report(fmt.Errorf("cache: set: %w", serr))
		}
	}
	s.mu.Lock()
	s.drop(key, f)
	s.mu.Unlock()
	f.value, f.err = value, err
	close(f.done)
}

// drop removes f from the in-flight loads unless another load of key replaced it, so a caller
// arriving after every waiter gave up starts a fresh load instead of joining a cancelled one.
func (s *flightShard) drop(key string, f *flight) {
	if s.calls[key] == f {
		delete(s.calls, key)
	}
}

// guarded calls load, turning a panic into an error so that waiters are always woken.
func guarded(ctx context.Context, load func(ctx context.Context) ([]byte, error)) (v []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, err = nil, fmt.Errorf("cache: load panicked: %v", r)
		}
	}()
	return load(ctx)
}

// report hands err to OnError.
func (l *Loader) report(err error) {
	if l.onError != nil {
		l.onError(err)
	}
}
