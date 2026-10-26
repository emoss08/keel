package cache

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/maypok86/otter/v2"
	"github.com/maypok86/otter/v2/stats"
)

// DefaultMemoryBytes is MemoryOptions.MaxBytes when it is not set.
const DefaultMemoryBytes = 64 << 20

// entryOverhead is the bytes an entry costs beyond its key and value: otter's node, its hash table
// slot and the entry's TTL.
const entryOverhead = 96

// MemoryOptions configure a Memory store.
type MemoryOptions struct {
	// MaxBytes bounds the keys, values and bookkeeping held; default DefaultMemoryBytes. A value
	// larger than it is ErrTooLarge.
	MaxBytes int64
	// Now is the clock entries expire by; default the wall clock, which also expires entries
	// nobody reads once a second.
	Now func() time.Time
}

// Memory is an in-process Store on otter (github.com/maypok86/otter): entries expire at their TTL,
// and a full store evicts the entries least likely to be read again (W-TinyLFU), so a scan of
// one-off keys does not flush the ones read often. Values returned by Get are shared and must not
// be modified. Close stops its expiry goroutine.
type Memory struct {
	c     *otter.Cache[string, memEntry]
	max   int64
	stats *stats.Counter
}

// memEntry is one cached value with the time to live it was stored for, zero for none.
type memEntry struct {
	value []byte
	ttl   time.Duration
}

// MemoryStats counts a Memory store's contents and traffic.
type MemoryStats struct {
	Entries   int
	Bytes     int64
	Hits      int64
	Misses    int64
	Evictions int64
}

// forever is the time to live of an entry stored without one.
const forever = time.Duration(math.MaxInt64 / 2)

// NewMemory returns an empty Memory store.
func NewMemory(o MemoryOptions) *Memory {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMemoryBytes
	}
	m := &Memory{max: o.MaxBytes, stats: stats.NewCounter()}
	opts := &otter.Options[string, memEntry]{
		MaximumWeight:    uint64(o.MaxBytes),
		Weigher:          weigh,
		ExpiryCalculator: expiry{},
		StatsRecorder:    m.stats,
	}
	if o.Now != nil {
		opts.Clock = manualClock{now: o.Now}
	}
	m.c = otter.Must(opts)
	return m
}

// weigh returns an entry's share of MaxBytes.
func weigh(key string, e memEntry) uint32 {
	return uint32(min(entrySize(key, e.value), math.MaxUint32))
}

// entrySize is the bytes an entry of key and value holds.
func entrySize(key string, value []byte) int64 {
	return int64(len(key)+len(value)) + entryOverhead
}

// expiry expires an entry at the TTL it was last stored with; reads leave it.
type expiry struct{}

func (expiry) ExpireAfterCreate(e otter.Entry[string, memEntry]) time.Duration {
	return lifetime(e.Value.ttl)
}

func (expiry) ExpireAfterUpdate(e otter.Entry[string, memEntry], _ memEntry) time.Duration {
	return lifetime(e.Value.ttl)
}

func (expiry) ExpireAfterRead(e otter.Entry[string, memEntry]) time.Duration {
	return e.ExpiresAfter()
}

// lifetime is otter's expiry of a TTL: forever for none.
func lifetime(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return forever
	}
	return ttl
}

// manualClock is MemoryOptions.Now as otter's clock. It never ticks: expired entries are removed
// when they are read or evicted.
type manualClock struct{ now func() time.Time }

func (c manualClock) NowNano() int64 { return c.now().UnixNano() }

func (manualClock) Tick(time.Duration) <-chan time.Time { return nil }

// Get returns the value under key; an expired one is missing.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	e, ok := m.c.GetIfPresent(key)
	if !ok {
		return nil, false, nil
	}
	return e.value, true, nil
}

// Set stores a copy of value under key for ttl, evicting entries beyond MaxBytes.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if size := entrySize(key, value); size > m.max {
		m.c.Invalidate(key)
		return fmt.Errorf("%w: %d bytes with its key, over the store's %d", ErrTooLarge, size, m.max)
	}
	m.c.Set(key, memEntry{value: append([]byte(nil), value...), ttl: ttl})
	return nil
}

// Delete removes the value under key.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.c.Invalidate(key)
	return nil
}

// Stats returns the store's contents and traffic, after the evictions and expiries otter has
// buffered.
func (m *Memory) Stats() MemoryStats {
	m.c.CleanUp()
	s := m.stats.Snapshot()
	return MemoryStats{
		Entries:   m.c.EstimatedSize(),
		Bytes:     int64(m.c.WeightedSize()),
		Hits:      int64(s.Hits),
		Misses:    int64(s.Misses),
		Evictions: int64(s.Evictions),
	}
}

// Close stops the goroutine that expires entries nobody reads. The store keeps working, expiring
// entries when they are read.
func (m *Memory) Close() { m.c.StopAllGoroutines() }
