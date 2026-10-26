package keel

import (
	"context"
	"fmt"
	"hash/maphash"
	"slices"
	"sync"
	"sync/atomic"
	"time"
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
	slow   *slowWaits
	shards [tenantShards]tenantShard
}

// tenantShard is one locked part of a tenantLimiter's map.
type tenantShard struct {
	mu    sync.Mutex
	slots map[string]*tenantSlot
}

// tenantSlot is the semaphore of one tenant key: active counts its holders, at most the
// limiter's limit, and waiters queues the work that found it full, first come first served. A
// holder that lets its slot go hands it to the first waiter directly. With SlowWaitConfig set,
// holds records each holder, for the report of a wait. Its fields are guarded by its shard's
// mutex.
type tenantSlot struct {
	key     string
	shard   *tenantShard
	active  int
	waiters []chan struct{}
	holds   map[*slotHold]struct{}
}

// slotHold is a terminal's or transaction's part in a tenant slot: the slot it runs under, whether
// it took the slot itself (and must let it go) or shares one its context carries, and, with
// SlowWaitConfig set, when it took the slot and the stack that took it.
type slotHold struct {
	slot  *tenantSlot
	owned bool
	since time.Time
	stack []uintptr
}

// release lets the hold's slot go when the hold took it, once.
func (h *slotHold) release() {
	if h.owned {
		h.owned = false
		h.slot.release(h)
	}
	h.slot = nil
}

// slotKey holds, on a context, the tenant slot of the work the context runs inside (a
// transaction, a write calling its hooks), which work started under the context for the same
// tenant shares instead of taking a second.
type slotKey struct{}

func newTenantLimiter(limit int, slow *slowWaits) *tenantLimiter {
	l := &tenantLimiter{limit: limit, seed: maphash.MakeSeed(), slow: slow}
	for i := range l.shards {
		l.shards[i].slots = map[string]*tenantSlot{}
	}
	return l
}

// enter fills h with the slot work under sc runs with. A system scope or a context without a tenant
// takes none. When ctx carries a slot of the same tenant the work shares it; otherwise it takes
// one, waiting for it or for ctx, and must release h.
func (l *tenantLimiter) enter(ctx context.Context, sc *scopeState, h *slotHold) error {
	if l == nil || sc.system || sc.tenant == nil {
		return nil
	}
	key := TenantKeyOf(sc.tenant...)
	if held, ok := ctx.Value(slotKey{}).(*tenantSlot); ok && held != nil && held.key == key {
		h.slot = held
		return nil
	}
	if l.slow != nil {
		h.stack = callers()
	}
	slot, err := l.acquire(ctx, key, h)
	if err != nil {
		return err
	}
	h.slot, h.owned = slot, true
	return nil
}

// acquire takes a slot of key for h, waiting while the tenant runs the limit's worth of work. A wait
// that crosses SlowWaitConfig.After is reported once, with the work holding the tenant's slots.
func (l *tenantLimiter) acquire(ctx context.Context, key string, h *slotHold) (*tenantSlot, error) {
	sh := &l.shards[maphash.String(l.seed, key)%tenantShards]
	sh.mu.Lock()
	s := sh.slots[key]
	if s == nil {
		s = &tenantSlot{key: key, shard: sh}
		sh.slots[key] = s
	}
	if s.active < l.limit {
		s.active++
		l.hold(s, h)
		sh.mu.Unlock()
		return s, nil
	}
	ready := make(chan struct{}, 1)
	s.waiters = append(s.waiters, ready)
	sh.mu.Unlock()
	l.waits.Add(1)
	var slow <-chan time.Time
	start := time.Now()
	if l.slow != nil {
		timer := time.NewTimer(l.slow.after)
		defer timer.Stop()
		slow = timer.C
	}
	for {
		select {
		case <-ready:
			sh.mu.Lock()
			l.hold(s, h)
			sh.mu.Unlock()
			return s, nil
		case <-slow:
			slow = nil
			w := l.slotWait(s, h, time.Since(start))
			l.slow.report(ctx, &w)
			continue
		case <-ctx.Done():
		}
		break
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
	s.release(nil)
	return nil, fmt.Errorf("keel: wait for a tenant slot: %w", ctx.Err())
}

// hold records h as a holder of s when slow waits are reported. The caller holds s's shard lock.
func (l *tenantLimiter) hold(s *tenantSlot, h *slotHold) {
	if l.slow == nil || h == nil {
		return
	}
	h.since = time.Now()
	if s.holds == nil {
		s.holds = map[*slotHold]struct{}{}
	}
	s.holds[h] = struct{}{}
}

// slotWait describes h's wait for a slot of s: the tenant's limit, the work waiting and the work
// holding its slots, oldest first.
func (l *tenantLimiter) slotWait(s *tenantSlot, h *slotHold, waited time.Duration) SlowWait {
	type held struct {
		since time.Time
		stack []uintptr
	}
	s.shard.mu.Lock()
	waiters := len(s.waiters)
	holders := make([]held, 0, len(s.holds))
	for hh := range s.holds {
		holders = append(holders, held{since: hh.since, stack: hh.stack})
	}
	s.shard.mu.Unlock()
	slices.SortFunc(holders, func(a, b held) int { return a.since.Compare(b.since) })
	now := time.Now()
	w := SlowWait{
		Kind:    WaitTenantSlot,
		Tenant:  s.key,
		Waited:  waited,
		Stack:   frames(h.stack),
		Limit:   l.limit,
		Waiters: waiters,
		Holders: make([]SlotHolder, len(holders)),
	}
	for i, hh := range holders {
		w.Holders[i] = SlotHolder{Held: now.Sub(hh.since), Stack: frames(hh.stack)}
	}
	return w
}

// release lets a slot go on behalf of h (nil for a slot handed over to work that no longer wants
// it): to the first waiter, or back to the tenant's semaphore, which leaves the map when nobody
// holds it.
func (s *tenantSlot) release(h *slotHold) {
	sh := s.shard
	sh.mu.Lock()
	if h != nil && s.holds != nil {
		delete(s.holds, h)
	}
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
