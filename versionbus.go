package keel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// versionChannel is the channel the versioner notifies with the versions it wrote, and the result
// cache's listener listens on.
const versionChannel = "keel_data_versions"

// The version bus keeps busShards independently locked maps of at most maxVersionPairs pairs in
// all. Its listener waits listenIdle for a notification before it pings its connection, within
// listenPing, and reconnects after a loss with a backoff from listenRetryMin to listenRetryMax.
const (
	busShards       = 64
	maxVersionPairs = 1 << 20
	listenIdle      = 30 * time.Second
	listenPing      = 10 * time.Second
	listenRetryMin  = 100 * time.Millisecond
	listenRetryMax  = 5 * time.Second
)

// versionsFillSQL reads the stored versions of every pair of a set of tenant keys and tables.
const versionsFillSQL = `SELECT tenant_key, table_name, version FROM keel_data_versions
 WHERE tenant_key = ANY($1) AND table_name = ANY($2)`

// errVersionsReset is the error of a version read that a listener loss or reset overtook: its
// values cannot be trusted, so the read bypasses the cache.
var errVersionsReset = errors.New("keel: the data versions were reset while they were read")

// pairState is what a process knows of one (tenant key, table) pair of keel_data_versions:
// version, the stored version, once known (read from the table or received from a notification
// while the listener is up); epoch, the number of this process's committed writes to the pair;
// and clean, the epoch the last flush that wrote the pair covered. A pair whose epoch is past
// clean has local writes its stored version does not show yet.
type pairState struct {
	version int64
	known   bool
	epoch   uint64
	clean   uint64
}

// dirty returns the pair's epoch when it has writes its version does not show, else zero.
func (p *pairState) dirty() uint64 {
	if p.epoch > p.clean {
		return p.epoch
	}
	return 0
}

// pairStamp is the state of a pair a cached result was read under: its version and, while this
// process has writes the version does not show, its epoch. A result is valid while every stamp
// it was read under is unchanged.
type pairStamp struct {
	version int64
	epoch   uint64
}

// busShard is one locked part of the version map; the padding keeps neighbouring locks off one
// cache line.
type busShard struct {
	mu sync.Mutex
	m  map[versionKey]*pairState
	_  [48]byte
}

// versionBus is the result cache's view of keel_data_versions: an in-memory map of the pairs the
// process has read, filled on demand in one round trip and kept current by the notifications the
// versioner of every process sends, and the local epochs of the pairs this process wrote. While
// its listener is down the map holds no versions and the cache is bypassed.
type versionBus struct {
	db     *DB
	seed   maphash.Seed
	shards [busShards]busShard
	limit  int

	up      atomic.Bool
	gen     atomic.Uint64
	filling atomic.Int32

	cancel context.CancelFunc
	done   chan struct{}
}

// newVersionBus returns an empty bus for db; start runs its listener.
func newVersionBus(db *DB) *versionBus {
	b := &versionBus{db: db, seed: maphash.MakeSeed(), limit: maxVersionPairs / busShards}
	for i := range b.shards {
		b.shards[i].m = map[versionKey]*pairState{}
	}
	return b
}

// shard returns the shard that holds k.
func (b *versionBus) shard(k versionKey) *busShard {
	return &b.shards[maphash.Comparable(b.seed, k)%busShards]
}

// state returns the state of k in s, adding it when add is set, or nil. A full shard first drops
// a pair without local writes; such a pair is read again when it is needed.
func (b *versionBus) state(s *busShard, k versionKey, add bool) *pairState {
	if st := s.m[k]; st != nil || !add {
		return st
	}
	if len(s.m) >= b.limit {
		for old, st := range s.m {
			if st.dirty() == 0 {
				delete(s.m, old)
				break
			}
		}
	}
	st := &pairState{}
	s.m[k] = st
	return st
}

// start connects the listener, waiting for the first connection within ctx, and runs it in its
// own goroutine. A failed first connection is reported and retried in the background.
func (b *versionBus) start(ctx context.Context) {
	conn, err := b.connect(ctx)
	if err != nil {
		b.db.cache.report(fmt.Errorf("keel: listen for data versions: %w", err))
	}
	lctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.done = make(chan struct{})
	go b.run(lctx, conn)
}

// stop ends the listener and waits for it.
func (b *versionBus) stop() {
	if b.cancel == nil {
		return
	}
	b.cancel()
	<-b.done
}

// connect opens the listener's connection, with CacheConfig.Listen or else the primary pool's
// settings, and LISTENs on it, then clears the map and marks the bus up: notifications from here
// on reach it.
func (b *versionBus) connect(ctx context.Context) (*pgx.Conn, error) {
	cc, err := b.listenConfig(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+versionChannel); err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	b.reset()
	b.up.Store(true)
	return conn, nil
}

// listenConfig returns a copy of the listener's connection settings: CacheConfig.Listen as given,
// or the primary pool's after its BeforeConnect.
func (b *versionBus) listenConfig(ctx context.Context) (*pgx.ConnConfig, error) {
	if cc := b.db.cfg.Cache; cc != nil && cc.Listen != nil {
		return cc.Listen.Copy(), nil
	}
	pc := b.db.pool.Config()
	cc := pc.ConnConfig.Copy()
	if pc.BeforeConnect != nil {
		if err := pc.BeforeConnect(ctx, cc); err != nil {
			return nil, err
		}
	}
	return cc, nil
}

// run applies notifications until ctx ends, reconnecting after a loss. While the listener is down
// the bus is marked down and its versions are cleared, since notifications sent meanwhile are
// lost.
func (b *versionBus) run(ctx context.Context, conn *pgx.Conn) {
	defer close(b.done)
	wait := listenRetryMin
	for {
		if conn != nil {
			err := b.listen(ctx, conn)
			b.down()
			_ = conn.Close(context.Background())
			conn = nil
			if ctx.Err() != nil {
				return
			}
			b.db.cache.report(fmt.Errorf("keel: data versions listener lost: %w", err))
			wait = listenRetryMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		var err error
		if conn, err = b.connect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			b.db.cache.report(fmt.Errorf("keel: listen for data versions: %w", err))
			wait = min(wait*2, listenRetryMax)
		}
	}
}

// listen applies the notifications conn receives until it fails or ctx ends. After listenIdle
// without one it pings the connection, so a connection the network dropped is noticed.
func (b *versionBus) listen(ctx context.Context, conn *pgx.Conn) error {
	for {
		wctx, cancel := context.WithTimeout(ctx, listenIdle)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		if err == nil {
			b.apply(n.Payload)
			continue
		}
		if ctx.Err() != nil || !pgconn.Timeout(err) {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, listenPing)
		_, err = conn.Exec(pctx, "SELECT 1")
		cancel()
		if err != nil {
			return err
		}
	}
}

// down marks the bus down and clears its versions.
func (b *versionBus) down() {
	b.up.Store(false)
	b.reset()
}

// reset forgets every version, keeping the epochs of pairs with local writes their versions do
// not show yet, so this process never takes its own stale results for fresh ones. Reads of
// versions begun before it fail with errVersionsReset.
func (b *versionBus) reset() {
	b.gen.Add(1)
	for i := range b.shards {
		s := &b.shards[i]
		s.mu.Lock()
		for k, st := range s.m {
			if st.dirty() == 0 {
				delete(s.m, k)
				continue
			}
			st.known, st.version = false, 0
		}
		s.mu.Unlock()
	}
}

// notifyAll is the payload of a notification that names no pair: a bumped pair too large for a
// payload. It resets the bus.
const notifyAll = "*"

// apply takes in a notification's versions: a JSON array of [tenant key, table, version]. A pair
// the bus holds, or any pair while a read of versions is in flight, takes the version when it is
// newer than the one it has.
func (b *versionBus) apply(payload string) {
	if payload == notifyAll {
		b.reset()
		return
	}
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	var rows [][]any
	if err := dec.Decode(&rows); err != nil {
		b.db.cache.report(fmt.Errorf("keel: data versions notification: %w", err))
		b.reset()
		return
	}
	for _, row := range rows {
		if len(row) != 3 {
			continue
		}
		tenant, ok1 := row[0].(string)
		table, ok2 := row[1].(string)
		num, ok3 := row[2].(json.Number)
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		version, err := num.Int64()
		if err != nil {
			continue
		}
		b.learn(versionKey{tenant: tenant, table: table}, version)
	}
}

// learn records that pair k has the stored version version.
func (b *versionBus) learn(k versionKey, version int64) {
	s := b.shard(k)
	s.mu.Lock()
	st := b.state(s, k, b.filling.Load() > 0)
	if st != nil {
		st.observe(version)
	}
	s.mu.Unlock()
}

// observe takes in a stored version of the pair, keeping the newest.
func (p *pairState) observe(version int64) {
	if !p.known || version > p.version {
		p.version = version
	}
	p.known = true
}

// bump records that a write of this process committed changes to the pairs of set. The versioner
// calls it under its lock, as it queues them.
func (b *versionBus) bump(set versionSet) {
	for k := range set {
		s := b.shard(k)
		s.mu.Lock()
		b.state(s, k, true).epoch++
		s.mu.Unlock()
	}
}

// marks returns the epochs of the pairs of set and the bus generation, as a flush that writes
// them takes them from the queue.
func (b *versionBus) marks(set versionSet) (map[versionKey]uint64, uint64) {
	out := make(map[versionKey]uint64, len(set))
	for k := range set {
		s := b.shard(k)
		s.mu.Lock()
		if st := s.m[k]; st != nil {
			out[k] = st.epoch
		}
		s.mu.Unlock()
	}
	return out, b.gen.Load()
}

// flushed records a flush that wrote the pairs of marks: their writes up to the marked epochs now
// show in their versions, which the flush returned. The versions are taken only while the bus has
// not been reset since the flush began.
func (b *versionBus) flushed(marks map[versionKey]uint64, gen uint64, versions map[versionKey]int64) {
	trusted := b.up.Load() && b.gen.Load() == gen
	for k, epoch := range marks {
		s := b.shard(k)
		s.mu.Lock()
		if st := s.m[k]; st != nil {
			st.clean = max(st.clean, epoch)
			if v, ok := versions[k]; ok && trusted {
				st.observe(v)
			}
		}
		s.mu.Unlock()
	}
}

// connSource returns the connection a read of versions runs on: the connection of the terminal
// that needs them, or nil for one of the primary pool's. A terminal that holds a connection, or
// whose transaction is about to take one, hands over that one, so a read of versions never waits
// for a second pool connection while its caller holds the first.
type connSource func(ctx context.Context) (pgConn, error)

// stamps returns the stamps of tables for tenant key tenant: for each table, in order, one per
// prefix of the key (tenantPrefixes), the order DataVersions sums them in. Pairs the bus does not
// know are read from keel_data_versions in one round trip, on the connection via returns. It fails
// with errVersionsReset while the listener is down or when it was lost during the read.
func (b *versionBus) stamps(ctx context.Context, tenant string, tables []string, via connSource) ([]pairStamp, error) {
	if !b.up.Load() {
		return nil, errVersionsReset
	}
	prefixes := tenantPrefixes(tenant)
	out := make([]pairStamp, len(tables)*len(prefixes))
	missing := b.read(prefixes, tables, out)
	if len(missing) == 0 {
		return out, nil
	}
	if err := b.fill(ctx, missing, via); err != nil {
		return nil, err
	}
	if missing = b.read(prefixes, tables, out); len(missing) > 0 {
		return nil, errVersionsReset
	}
	return out, nil
}

// read fills out with the stamps of the known pairs and returns the pairs whose versions it does
// not know.
func (b *versionBus) read(prefixes, tables []string, out []pairStamp) []versionKey {
	var missing []versionKey
	for ti, t := range tables {
		for pi, p := range prefixes {
			k := versionKey{tenant: p, table: t}
			s := b.shard(k)
			s.mu.Lock()
			st := s.m[k]
			if st == nil || !st.known {
				missing = append(missing, k)
			} else {
				out[ti*len(prefixes)+pi] = pairStamp{version: st.version, epoch: st.dirty()}
			}
			s.mu.Unlock()
		}
	}
	return missing
}

// fill reads the stored versions of keys from keel_data_versions, a pair without a row being at
// version 0, on the connection via returns (inside the caller's transaction when it has one: a
// plain SELECT, and under REPEATABLE READ a snapshot no newer than the data the terminal reads),
// or on one of the primary pool's when via is nil or returns none. When the table does not exist
// yet every version is 0; it is never created here, so the read cannot fail the caller's
// transaction. Notifications that arrive during the read are kept even for pairs the bus did not
// hold, so a version bumped while the read was in flight is not lost.
func (b *versionBus) fill(ctx context.Context, keys []versionKey, via connSource) error {
	db := b.db
	if db.versions == nil || db.pool == nil {
		return errVersionsReset
	}
	var conn pgConn
	if via != nil {
		c, err := via(ctx)
		if err != nil {
			return err
		}
		conn = c
	}
	if conn == nil {
		pc, err := db.acquire(ctx, db.pool, nil)
		if err != nil {
			return fmt.Errorf("keel: read data versions: %w", err)
		}
		defer pc.Release()
		conn = pc
	}
	gen := b.gen.Load()
	b.filling.Add(1)
	defer b.filling.Add(-1)
	var tenants, tables []string
	for _, k := range keys {
		if !containsString(tenants, k.tenant) {
			tenants = append(tenants, k.tenant)
		}
		if !containsString(tables, k.table) {
			tables = append(tables, k.table)
		}
	}
	found := make(map[versionKey]int64, len(keys))
	s := &stmt{sql: versionsFillSQL, args: []any{tenants, tables}, op: "select", table: versionTable}
	s.scan = func(rows pgx.Rows) error {
		for rows.Next() {
			var k versionKey
			var v int64
			if err := rows.Scan(&k.tenant, &k.table, &v); err != nil {
				return err
			}
			found[k] = v
		}
		return nil
	}
	exists, err := db.versions.exists(ctx, conn)
	if err != nil {
		return fmt.Errorf("keel: find %s: %w", versionTable, err)
	}
	if exists {
		stmts := []*stmt{s}
		events, qctx := db.startEvents(ctx, nil, false, stmts)
		start := time.Now()
		rows, err := conn.Query(qctx, s.sql, s.args...)
		if err == nil {
			err = scanRows(rows, s)
		}
		if err = db.endEvents(events, stmts, time.Since(start), err); err != nil {
			return fmt.Errorf("keel: read data versions: %w", err)
		}
	}
	if !b.up.Load() || b.gen.Load() != gen {
		return errVersionsReset
	}
	for _, k := range keys {
		sh := b.shard(k)
		sh.mu.Lock()
		b.state(sh, k, true).observe(found[k])
		sh.mu.Unlock()
	}
	return nil
}

// pairs returns the number of pairs the bus holds.
func (b *versionBus) pairs() int {
	n := 0
	for i := range b.shards {
		s := &b.shards[i]
		s.mu.Lock()
		n += len(s.m)
		s.mu.Unlock()
	}
	return n
}

// containsString reports whether ss holds s.
func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
