package keel

import (
	"context"
	"errors"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// CacheConfig configures the result cache (Config.Cache): query results kept by the statements
// that read them and served while the data versions of every table they read are unchanged, so a
// hit costs no round trip. It needs Config.DataVersions.Enabled.
type CacheConfig struct {
	// Local is the in-process store; nil keeps a MemoryCache of DefaultCacheBytes.
	Local LocalCache
	// Shared, when set, is a store every process shares, such as Redis (keel/cache/rediscache). A
	// local miss reads it, and an entry read while this process had no unflushed writes to its
	// tables is written to it.
	Shared SharedCache
	// Key returns key material for what the scope does not capture but the rows depend on, such
	// as the row-level security settings Config.OnBegin sets (Trenova returns its encoded read
	// scope). It is required when Config.OnBegin is set. It runs on the terminal's context without
	// its transaction and with its tenant slot: a statement it sends runs in a transaction of its
	// own (on a second pool connection when the caller holds one), never joins or begins the one
	// the terminal reads in, and shares the terminal's slot rather than waiting for another.
	Key func(ctx context.Context) string
	// MaxEntryBytes caps an encoded entry; a larger result is not cached. Default 1 MiB.
	MaxEntryBytes int
	// OnError receives the errors of the shared store, the listener and the decoders; the cache
	// treats each as a miss or a bypass.
	OnError func(err error)
	// Listen is the connection the listener LISTENs for data versions on; nil uses the primary
	// pool's settings (and its BeforeConnect). LISTEN needs a session, so behind a
	// transaction-mode pooler (PgBouncer) point it at Postgres directly. Build it with
	// pgx.ParseConfig; it is used as given, without the pool's BeforeConnect.
	Listen *pgx.ConnConfig
}

// defaultCacheTTL is the time to live of a query that asks for the cache without TTL, and
// defaultMaxEntryBytes the default CacheConfig.MaxEntryBytes.
const (
	defaultCacheTTL      = time.Minute
	defaultMaxEntryBytes = 1 << 20
)

// CacheOption configures Query.Cache.
type CacheOption func(*cacheSpec)

// cacheSpec is what a query asks of the result cache.
type cacheSpec struct {
	on     bool
	ttl    time.Duration
	tables []string
}

// TTL keeps a cached result for at most d. Validity never depends on it: an entry stops being
// served as soon as a table it read changes. It bounds how long an unused entry holds memory.
// The default is one minute.
func TTL(d time.Duration) CacheOption {
	return func(s *cacheSpec) {
		if d > 0 {
			s.ttl = d
		}
	}
}

// AlsoReads names tables the query reads that keel cannot see, such as a table only a keel.Raw
// fragment reads, so a change to them invalidates its cached results too.
func AlsoReads(tables ...string) CacheOption {
	return func(s *cacheSpec) { s.tables = append(s.tables, tables...) }
}

// Cache serves the query's One, First, List, All, Count, CountUpTo, CountEstimate, Exists and
// Page (and the Batch items of the same names) from the result cache (Config.Cache) while the
// data versions of every table they read (the base table, joins, relation loads, subqueries, Any
// and None, CTEs, and AlsoReads) are unchanged for the tenant. A Batch is served from the cache
// only when every item's query asks for it, for the shortest of their TTLs. The cache is bypassed
// inside a write transaction, a joined one and a REPEATABLE READ or SERIALIZABLE one that has
// already begun (sent a statement, so its snapshot is taken), under a system scope, for a query
// with Lock, and while the cache's listener is disconnected. A snapshot transaction that has not
// begun is served from the cache, and a hit leaves it unbegun: its snapshot is taken by its first
// statement that reads the database.
// Without Config.Cache it does nothing.
func (q *Query[T]) Cache(opts ...CacheOption) *Query[T] {
	spec := cacheSpec{on: true, ttl: defaultCacheTTL}
	for _, o := range opts {
		o(&spec)
	}
	q.q.cache = spec
	return q
}

// Cache starts a query served from the result cache; see Query.Cache.
func (h Handle[T]) Cache(opts ...CacheOption) *Query[T] { return h.Query().Cache(opts...) }

// cacheTTL returns how long the query's result may be cached: zero when it does not ask, -1 when
// it asks but cannot be (a Lock). The tables named with AlsoReads join ctx's read set.
func (q *query) cacheTTL(ctx context.Context) time.Duration {
	if !q.cache.on {
		return 0
	}
	if q.lock != "" {
		return -1
	}
	if rs := readSetFrom(ctx); rs != nil {
		for _, t := range q.cache.tables {
			rs.addName(t)
		}
	}
	return q.cache.ttl
}

// CacheStats counts the result cache's traffic and contents.
type CacheStats struct {
	// Hits are terminals served from the cache, Misses terminals that asked for it and read the
	// database, Bypasses terminals that asked for it where it does not apply.
	Hits, Misses, Bypasses int64
	// Errors counts the failures reported to CacheConfig.OnError.
	Errors int64
	// Bytes is what the local store holds.
	Bytes int64
	// Versions is the number of (tenant, table) pairs whose versions the process holds.
	Versions int
	// Listening reports that the listener is connected; while it is not, the cache is bypassed.
	Listening bool
}

// CacheStats returns the result cache's counters; all zero without Config.Cache.
func (db *DB) CacheStats() CacheStats {
	c := db.cache
	if c == nil {
		return CacheStats{}
	}
	return CacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Bypasses:  c.bypasses.Load(),
		Errors:    c.errors.Load(),
		Bytes:     c.local.Bytes(),
		Versions:  c.bus.pairs(),
		Listening: c.bus.up.Load(),
	}
}

// flightShards is the number of locked parts of the map of misses in flight.
const flightShards = 64

// flightShard is one part of the map of misses in flight: the key of each miss being read from
// the database, with a channel closed when it is stored or abandoned.
type flightShard struct {
	mu sync.Mutex
	m  map[string]chan struct{}
	_  [48]byte
}

// resultCache is a DB's result cache: its stores, the version bus, the decoders, the misses in
// flight and the counters.
type resultCache struct {
	db       *DB
	cfg      CacheConfig
	local    LocalCache
	ownLocal bool
	shared   SharedCache
	maxEntry int
	bus      *versionBus
	dec      *decoders

	seed    maphash.Seed
	flights [flightShards]flightShard

	hits, misses, bypasses, errors atomic.Int64
}

// openCache sets up Config.Cache and connects its listener. It refuses a cache without data
// versions, and one without Key when Config.OnBegin is set: the rows OnBegin's settings let a
// transaction see are invisible to the key otherwise.
func (db *DB) openCache(ctx context.Context) error {
	cc := db.cfg.Cache
	if cc == nil {
		return nil
	}
	if !db.cfg.DataVersions.Enabled {
		return errf("Config.Cache needs Config.DataVersions.Enabled")
	}
	if db.cfg.OnBegin != nil && cc.Key == nil {
		return errf("Config.Cache needs a Key when Config.OnBegin is set: the cache cannot see what OnBegin's statements change")
	}
	c := &resultCache{db: db, cfg: *cc, local: cc.Local, shared: cc.Shared, maxEntry: cc.MaxEntryBytes,
		seed: maphash.MakeSeed()}
	if c.local == nil {
		c.local = MemoryCache(DefaultCacheBytes)
		c.ownLocal = true
	}
	if c.maxEntry <= 0 {
		c.maxEntry = defaultMaxEntryBytes
	}
	for i := range c.flights {
		c.flights[i].m = map[string]chan struct{}{}
	}
	c.bus = newVersionBus(db)
	c.dec = newDecoders(db)
	db.cache = c
	c.bus.start(ctx)
	return nil
}

// closeCache stops the listener and the expiry goroutine of the local store keel made. The cache
// stays in place, bypassed, so writes that end after it still find it.
func (db *DB) closeCache() {
	if db.cache == nil {
		return
	}
	db.cache.bus.stop()
	if db.cache.ownLocal {
		db.cache.local.(*lru).m.Close()
	}
}

// report counts err and hands it to CacheConfig.OnError.
func (c *resultCache) report(err error) {
	if c == nil || err == nil {
		return
	}
	c.errors.Add(1)
	if c.cfg.OnError != nil {
		c.cfg.OnError(err)
	}
}

// collect returns ctx carrying a new read set when the cache is on, so the terminal's renders
// record the tables they read.
func (c *resultCache) collect(ctx context.Context) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, readSetKey{}, &readSet{})
}

// bumped records committed writes of this process to the pairs of set; see versionBus.bump.
func (c *resultCache) bumped(set versionSet) {
	if c != nil {
		c.bus.bump(set)
	}
}

// marks returns the epochs a flush of set covers; see versionBus.marks.
func (c *resultCache) marks(set versionSet) (map[versionKey]uint64, uint64) {
	if c == nil {
		return nil, 0
	}
	return c.bus.marks(set)
}

// flushed records a successful flush; see versionBus.flushed.
func (c *resultCache) flushed(marks map[versionKey]uint64, gen uint64, versions map[versionKey]int64) {
	if c != nil {
		c.bus.flushed(marks, gen, versions)
	}
}

// cacheRun is the result cache's part in one terminal: on a hit it feeds every round trip the
// terminal sends from the entry, on a miss it records every round trip's results and stamps the
// tables each reads before it is sent, and stores the entry when the terminal succeeds.
type cacheRun struct {
	c      *resultCache
	ctx    context.Context
	run    *runner
	key    string
	ttl    time.Duration
	tenant string
	reads  *readSet

	entry *cacheEntry
	next  int
	dec   *decoder

	batches  [][]*stmtRecord
	stamps   []pairStamp
	stamped  int
	prefixes int
	noStore  bool
	flight   chan struct{}
}

// start returns the cache's part in a terminal whose reads have been prepared, or nil when no
// read asks for the cache. It bypasses the cache where it does not apply, and otherwise looks the
// terminal's entry up (unless refill is set, after a hit that could not be replayed): a valid
// entry is a hit. A miss of a key another goroutine of the process is reading waits for it, with
// the connection the lookup took back in the pool and the tenant slot the runner took let go
// (the goroutine it waits for holds one of its own), then takes the slot again and looks again.
// It fails only when taking the slot again does.
func (c *resultCache) start(ctx context.Context, run *runner, reads []*read, stmts []*stmt,
	refill bool,
) (*cacheRun, error) {
	if c == nil {
		return nil, nil
	}
	ttl := cachedTTL(reads)
	if ttl == 0 {
		return nil, nil
	}
	rs := readSetFrom(ctx)
	if ttl < 0 || rs == nil || !c.applies(run) {
		c.bypasses.Add(1)
		return nil, nil
	}
	tenant := ""
	if run.sc.tenant != nil {
		tenant = TenantKeyOf(run.sc.tenant...)
	}
	extra := ""
	if c.cfg.Key != nil {
		extra = c.cfg.Key(run.db.callbackCtx(ctx, run.hold.slot))
	}
	key, ok := cacheKey(tenant, extra, readShape(reads), stmts)
	if !ok {
		c.bypasses.Add(1)
		return nil, nil
	}
	cr := &cacheRun{c: c, ctx: ctx, run: run, key: key, ttl: ttl, tenant: tenant, reads: rs,
		prefixes: len(tenantPrefixes(tenant)), noStore: run.replica() != nil}
	if !refill {
		if cr.hit() {
			return cr, nil
		}
		if wait := c.join(key); wait != nil {
			run.idle()
			yielded := run.yieldSlot()
			run.db.waitFill(ctx, wait, tenant)
			if yielded {
				if err := run.reclaimSlot(ctx); err != nil {
					return nil, err
				}
			}
			if cr.hit() {
				return cr, nil
			}
		} else {
			cr.flight = c.lead(key)
		}
	}
	c.misses.Add(1)
	return cr, nil
}

// readShape describes the reads of a terminal for its cache key: for each, whether it loads
// relation levels after the first round trip, so two terminals whose first round trips are equal
// but whose later ones differ never share an entry.
func readShape(reads []*read) string {
	b := make([]byte, len(reads))
	for i, rd := range reads {
		b[i] = '-'
		if rd.loads.deeper() {
			b[i] = 'd'
		}
	}
	return string(b)
}

// cachedTTL returns the time to live of a terminal's reads: the shortest of their TTLs when every
// read asks for the cache, zero when none does, -1 when some do and others do not or cannot.
func cachedTTL(reads []*read) time.Duration {
	var ttl time.Duration
	asked, other := false, false
	for _, rd := range reads {
		switch {
		case rd.cache > 0:
			asked = true
			if ttl == 0 || rd.cache < ttl {
				ttl = rd.cache
			}
		case rd.cache < 0:
			asked, other = true, true
		default:
			other = true
		}
	}
	switch {
	case !asked:
		return 0
	case other:
		return -1
	}
	return ttl
}

// applies reports whether the cache may serve a terminal on run: not under a system scope, only
// while the listener is connected, and inside a transaction the caller opened only when it is
// read-only, not joined from another data layer, and either READ COMMITTED (each statement takes
// its own snapshot, after the stamps) or a snapshot transaction that has not begun (its snapshot
// is taken by this terminal's round trip, after the stamps, or by a later one). An implicit
// transaction keel owns begins with the terminal, after the lookup.
func (c *resultCache) applies(run *runner) bool {
	if run.sc.system || !c.bus.up.Load() {
		return false
	}
	if tx := run.tx; tx != nil && !run.own {
		if !tx.readOnly || tx.joined || tx.begun && tx.snapshot() {
			return false
		}
	}
	return true
}

// via returns the connection the terminal's reads of versions run on: the runner's own (its
// transaction's, begun or about to begin with the terminal's round trip, or the pool connection
// its statements will use), so the cache never takes a second pool connection while the caller
// holds one. A runner on a replica reads them on the primary (nil), since a replica's versions lag.
func (cr *cacheRun) via(ctx context.Context) (pgConn, error) {
	if cr.run.replica() != nil {
		return nil, nil
	}
	return cr.run.acquire(ctx)
}

// hit looks the entry up, in the local store and then the shared one, and takes it when every
// table it read still has the stamps it was read under. A shared hit is copied into the local
// store. It reports whether the run is a hit, with a decoder to replay it.
func (cr *cacheRun) hit() bool {
	c := cr.c
	e := (*cacheEntry)(nil)
	if v, ok := c.local.Get(cr.key); ok {
		e = cr.valid(v)
	}
	if e == nil && c.shared != nil {
		v, ok, err := c.shared.Get(cr.ctx, cr.key)
		switch {
		case err != nil:
			c.report(err)
		case ok && len(v) <= c.maxEntry:
			if e = cr.valid(v); e != nil {
				c.local.Set(cr.key, v, cr.ttl)
			}
		}
	}
	if e == nil {
		return false
	}
	dec, err := c.dec.get(cr.ctx)
	if err != nil {
		c.report(err)
		return false
	}
	cr.entry, cr.dec = e, dec
	c.hits.Add(1)
	return true
}

// valid decodes raw and returns it when its tables' current stamps equal the stored ones.
func (cr *cacheRun) valid(raw []byte) *cacheEntry {
	e, err := decodeEntry(raw)
	if err != nil {
		return nil
	}
	if e.prefixes != cr.prefixes {
		return nil
	}
	cur, err := cr.c.bus.stamps(cr.ctx, cr.tenant, e.tables, cr.via)
	if err != nil {
		if !errors.Is(err, errVersionsReset) {
			cr.c.report(err)
		}
		return nil
	}
	if len(cur) != len(e.stamps) {
		return nil
	}
	for i := range cur {
		if cur[i] != e.stamps[i] {
			return nil
		}
	}
	return e
}

// batch is the runner's hook for each round trip of the terminal. On a hit it feeds the
// statements their stored results and reports done; on a miss it stamps the tables the
// statements read that are not stamped yet, before they are sent, and records their results. A
// later round trip of a snapshot transaction that has begun reads the snapshot taken before any
// stamp it could take now, so one that reads a table not stamped yet makes the result unstorable.
func (cr *cacheRun) batch(stmts []*stmt) (bool, error) {
	if cr.entry != nil {
		return true, cr.replay(stmts)
	}
	if cr.noStore {
		return false, nil
	}
	if tx := cr.run.tx; tx != nil && tx.begun && tx.snapshot() && cr.stamped < len(cr.reads.names) {
		cr.noStore = true
		return false, nil
	}
	if err := cr.stamp(); err != nil {
		cr.noStore = true
		if !errors.Is(err, errVersionsReset) {
			cr.c.report(err)
		}
		return false, nil
	}
	recs := make([]*stmtRecord, len(stmts))
	for i, s := range stmts {
		recs[i] = recordScan(s)
	}
	cr.batches = append(cr.batches, recs)
	return false, nil
}

// stamp reads the stamps of the tables the terminal's renders added since the last round trip.
func (cr *cacheRun) stamp() error {
	names := cr.reads.names
	if cr.stamped == len(names) {
		return nil
	}
	st, err := cr.c.bus.stamps(cr.ctx, cr.tenant, names[cr.stamped:], cr.via)
	if err != nil {
		return err
	}
	cr.stamps = append(cr.stamps, st...)
	cr.stamped = len(names)
	return nil
}

// replay feeds the next stored round trip to stmts. A round trip that does not match, or a
// result its scanner cannot decode, fails with errCacheReplay, and the terminal runs again.
func (cr *cacheRun) replay(stmts []*stmt) error {
	if cr.next >= len(cr.entry.batches) {
		return errCacheReplay
	}
	b := cr.entry.batches[cr.next]
	cr.next++
	if len(b) != len(stmts) {
		return errCacheReplay
	}
	for i, s := range stmts {
		st := &b[i]
		if st.sqlHash != sqlHash(s.sql) || st.scanned != (s.scan != nil) {
			return errCacheReplay
		}
		if s.scan != nil {
			rows := newReplayRows(cr.dec.conn, st)
			err := s.scan(rows)
			rows.Close()
			if err != nil {
				cr.c.report(errors.Join(errCacheReplay, err))
				return errCacheReplay
			}
		}
		s.tag = pgconn.NewCommandTag(st.tag)
	}
	return nil
}

// store writes the entry of a terminal that ran against the database and succeeded: to the local
// store, and to the shared one when no table it read had writes of this process its version did
// not show yet. A result read on a replica, or one whose versions could not be read, is not
// stored.
func (cr *cacheRun) store(ctx context.Context) {
	if cr == nil || cr.entry != nil || cr.noStore || len(cr.batches) == 0 {
		return
	}
	if cr.stamped != len(cr.reads.names) {
		return
	}
	buf := encodeEntry(cr.reads.names, cr.prefixes, cr.stamps, cr.batches)
	if len(buf) > cr.c.maxEntry {
		return
	}
	cr.c.local.Set(cr.key, buf, cr.ttl)
	if cr.c.shared == nil {
		return
	}
	for _, st := range cr.stamps {
		if st.epoch != 0 {
			return
		}
	}
	if err := cr.c.shared.Set(ctx, cr.key, buf, cr.ttl); err != nil {
		cr.c.report(err)
	}
}

// release returns the decoder of a hit and ends the miss in flight, waking the goroutines that
// wait for it.
func (cr *cacheRun) release() {
	if cr == nil {
		return
	}
	if cr.dec != nil {
		cr.c.dec.put(cr.dec)
		cr.dec = nil
	}
	if cr.flight != nil {
		cr.c.land(cr.key, cr.flight)
		cr.flight = nil
	}
}

// flightShard returns the shard of the misses in flight that holds key.
func (c *resultCache) flightShard(key string) *flightShard {
	return &c.flights[maphash.String(c.seed, key)%flightShards]
}

// join returns the channel of the miss of key in flight, or nil when there is none.
func (c *resultCache) join(key string) chan struct{} {
	s := c.flightShard(key)
	s.mu.Lock()
	ch := s.m[key]
	s.mu.Unlock()
	return ch
}

// lead registers a miss of key in flight, unless another goroutine registered one meanwhile, and
// returns its channel, or nil.
func (c *resultCache) lead(key string) chan struct{} {
	s := c.flightShard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; ok {
		return nil
	}
	ch := make(chan struct{})
	s.m[key] = ch
	return ch
}

// land ends the miss of key in flight.
func (c *resultCache) land(key string, ch chan struct{}) {
	s := c.flightShard(key)
	s.mu.Lock()
	if s.m[key] == ch {
		delete(s.m, key)
	}
	s.mu.Unlock()
	close(ch)
}
