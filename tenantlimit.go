package keel

import (
	"context"
	"fmt"
	"hash/maphash"
	"slices"
	"sync"
	"sync/atomic"
)

// tenantShards is the number of independently locked parts of a tenantLimiter's map, so tenants
// whose keys hash apart never contend on one mutex.
const tenantShards = 64

// tenantLimiter bounds the terminals and transactions one tenant runs at once on a DB
// (Config.TenantConcurrency). It holds a semaphore per tenant key with work running or waiting,
// in a map split into shards; an entry is removed when its last holder lets it go and nobody
// waits, so the map holds only the tenants active right now.
type tenantLimiter struct {
	limit  int
	seed   maphash.Seed
	waits  atomic.Int64
	shards [tenantShards]tenantShard
}

// tenantShard is one locked part of a tenantLimiter's map.
type tenantShard struct {
	mu    sync.Mutex
	slots map[string]*tenantSlot
}

// tenantSlot is the semaphore of one tenant key: active counts its holders, at most the
// limiter's limit, and waiters queues the work that found it full, first come first served. A
// holder that lets its slot go hands it to the first waiter directly. Its fields are guarded by
// its shard's mutex.
type tenantSlot struct {
	key     string
	shard   *tenantShard
	active  int
	waiters []chan struct{}
}

// slotKey holds, on a context, the tenant slot of the work the context runs inside (a
// transaction, a write calling its hooks), which work started under the context for the same
// tenant shares instead of taking a second.
type slotKey struct{}

func newTenantLimiter(limit int) *tenantLimiter {
	l := &tenantLimiter{limit: limit, seed: maphash.MakeSeed()}
	for i := range l.shards {
		l.shards[i].slots = map[string]*tenantSlot{}
	}
	return l
}

// enter returns the slot work under sc runs with. A system scope or a context without a tenant
// takes none (nil). When ctx carries a slot of the same tenant the work shares it (owned false);
// otherwise it takes one, waiting for it or for ctx (owned true), and must release it.
func (l *tenantLimiter) enter(ctx context.Context, sc *scopeState) (slot *tenantSlot, owned bool, err error) {
	if l == nil || sc.system || sc.tenant == nil {
		return nil, false, nil
	}
	key := TenantKeyOf(sc.tenant...)
	if held, ok := ctx.Value(slotKey{}).(*tenantSlot); ok && held != nil && held.key == key {
		return held, false, nil
	}
	slot, err = l.acquire(ctx, key)
	if err != nil {
		return nil, false, err
	}
	return slot, true, nil
}

// acquire takes a slot of key, waiting while the tenant runs the limit's worth of work.
func (l *tenantLimiter) acquire(ctx context.Context, key string) (*tenantSlot, error) {
	sh := &l.shards[maphash.String(l.seed, key)%tenantShards]
	sh.mu.Lock()
	s := sh.slots[key]
	if s == nil {
		s = &tenantSlot{key: key, shard: sh}
		sh.slots[key] = s
	}
	if s.active < l.limit {
		s.active++
		sh.mu.Unlock()
		return s, nil
	}
	ready := make(chan struct{}, 1)
	s.waiters = append(s.waiters, ready)
	sh.mu.Unlock()
	l.waits.Add(1)
	select {
	case <-ready:
		return s, nil
	case <-ctx.Done():
	}
	sh.mu.Lock()
	if i := slices.Index(s.waiters, ready); i >= 0 {
		s.waiters = slices.Delete(s.waiters, i, i+1)
		sh.mu.Unlock()
		return nil, fmt.Errorf("keel: wait for a tenant slot: %w", ctx.Err())
	}
	sh.mu.Unlock()
	// A holder handed the slot over as the context ended: pass it on.
	<-ready
	s.release()
	return nil, fmt.Errorf("keel: wait for a tenant slot: %w", ctx.Err())
}

// release lets a slot go: to the first waiter, or back to the tenant's semaphore, which leaves the
// map when nobody holds it.
func (s *tenantSlot) release() {
	sh := s.shard
	sh.mu.Lock()
	if len(s.waiters) > 0 {
		next := s.waiters[0]
		s.waiters[0] = nil
		s.waiters = s.waiters[1:]
		sh.mu.Unlock()
		next <- struct{}{}
		return
	}
	s.active--
	if s.active == 0 {
		delete(sh.slots, s.key)
		s.waiters = nil
	}
	sh.mu.Unlock()
}

// size returns the number of tenant keys with work running or waiting, for tests.
func (l *tenantLimiter) size() int {
	n := 0
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		n += len(sh.slots)
		sh.mu.Unlock()
	}
	return n
}
