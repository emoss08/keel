package cache

import (
	"context"
	"fmt"
	"hash/maphash"
	"math/bits"
	"sync"
	"time"
)

// Defaults for MemoryOptions.
const (
	DefaultMemoryBytes  = 64 << 20
	DefaultMemoryShards = 64
)

// entryOverhead is the bytes an entry costs beyond its key and value: the entry, its map slot and
// its list links.
const entryOverhead = 96

// MemoryOptions configure a Memory store.
type MemoryOptions struct {
	// MaxBytes bounds the keys, values and bookkeeping held; default DefaultMemoryBytes. Each
	// shard holds an equal part, and a value larger than a part is ErrTooLarge.
	MaxBytes int64
	// Shards is the number of independently locked parts, rounded up to a power of two; default
	// DefaultMemoryShards.
	Shards int
	// Now is the clock entries expire by; default time.Now.
	Now func() time.Time
}

// Memory is an in-process Store: a fixed number of shards, each an LRU list under its own lock,
// so no lock is shared by every key. Entries expire at their TTL and the least recently used
// entry of a full shard is evicted. Values returned by Get are shared and must not be modified.
type Memory struct {
	shards []memShard
	mask   uint64
	seed   maphash.Seed
	now    func() time.Time
}

// memShard is one LRU part of a Memory store; head is the most recently used entry. The padding
// keeps neighbouring shards' locks off one cache line.
type memShard struct {
	mu        sync.Mutex
	items     map[string]*memEntry
	head      *memEntry
	tail      *memEntry
	bytes     int64
	max       int64
	hits      int64
	misses    int64
	evictions int64
	_         [64]byte
}

// memEntry is one cached value; expires is in Unix nanoseconds, zero for none.
type memEntry struct {
	key     string
	value   []byte
	expires int64
	size    int64
	prev    *memEntry
	next    *memEntry
}

// MemoryStats counts a Memory store's contents and traffic.
type MemoryStats struct {
	Entries   int
	Bytes     int64
	Hits      int64
	Misses    int64
	Evictions int64
}

// NewMemory returns an empty Memory store.
func NewMemory(o MemoryOptions) *Memory {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMemoryBytes
	}
	if o.Shards <= 0 {
		o.Shards = DefaultMemoryShards
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	n := 1 << bits.Len(uint(o.Shards-1))
	m := &Memory{shards: make([]memShard, n), mask: uint64(n - 1), seed: maphash.MakeSeed(), now: o.Now}
	per := max(o.MaxBytes/int64(n), 1)
	for i := range m.shards {
		m.shards[i].items = map[string]*memEntry{}
		m.shards[i].max = per
	}
	return m
}

// shard returns the shard that holds key.
func (m *Memory) shard(key string) *memShard {
	return &m.shards[maphash.String(m.seed, key)&m.mask]
}

// Get returns the value under key; an expired entry is removed and reported missing.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.items[key]
	if e == nil {
		s.misses++
		return nil, false, nil
	}
	if e.expires != 0 && m.now().UnixNano() >= e.expires {
		s.remove(e)
		s.misses++
		return nil, false, nil
	}
	s.toFront(e)
	s.hits++
	return e.value, true, nil
}

// Set stores a copy of value under key for ttl, evicting the shard's least recently used entries
// beyond its share of MaxBytes.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s := m.shard(key)
	size := int64(len(key)+len(value)) + entryOverhead
	var expires int64
	if ttl > 0 {
		expires = m.now().Add(ttl).UnixNano()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if size > s.max {
		if e := s.items[key]; e != nil {
			s.remove(e)
		}
		return fmt.Errorf("%w: %d bytes with its key, over a shard's %d", ErrTooLarge, size, s.max)
	}
	v := append([]byte(nil), value...)
	if e := s.items[key]; e != nil {
		s.bytes += size - e.size
		e.value, e.size, e.expires = v, size, expires
		s.toFront(e)
	} else {
		e = &memEntry{key: key, value: v, size: size, expires: expires}
		s.items[key] = e
		s.bytes += size
		s.pushFront(e)
	}
	for s.bytes > s.max && s.tail != nil {
		s.remove(s.tail)
		s.evictions++
	}
	return nil
}

// Delete removes the value under key.
func (m *Memory) Delete(_ context.Context, key string) error {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.items[key]; e != nil {
		s.remove(e)
	}
	return nil
}

// Stats sums the shards' counters, locking one shard at a time.
func (m *Memory) Stats() MemoryStats {
	var st MemoryStats
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		st.Entries += len(s.items)
		st.Bytes += s.bytes
		st.Hits += s.hits
		st.Misses += s.misses
		st.Evictions += s.evictions
		s.mu.Unlock()
	}
	return st
}

// pushFront links e as the most recently used entry.
func (s *memShard) pushFront(e *memEntry) {
	e.prev, e.next = nil, s.head
	if s.head != nil {
		s.head.prev = e
	}
	s.head = e
	if s.tail == nil {
		s.tail = e
	}
}

// unlink takes e out of the list.
func (s *memShard) unlink(e *memEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		s.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		s.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

// toFront marks e as the most recently used entry.
func (s *memShard) toFront(e *memEntry) {
	if s.head == e {
		return
	}
	s.unlink(e)
	s.pushFront(e)
}

// remove drops e from the shard.
func (s *memShard) remove(e *memEntry) {
	s.unlink(e)
	delete(s.items, e.key)
	s.bytes -= e.size
}
