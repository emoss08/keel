package keel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The lag tracker checks every replica each replicaPollInterval and stops using one whose last
// check is older than replicaStaleAfter. WAL positions are normalised with the server's page and
// segment sizes (the defaults until the first check reads them) and the sizes of a page header
// and of a segment's first page header on a 64-bit server.
const (
	defaultMaxReplicaLag = 2 * time.Second
	replicaPollInterval  = time.Second
	replicaStaleAfter    = 3 * replicaPollInterval
	defaultWALBlock      = 8192
	defaultWALSegment    = 16 << 20
	shortPageHeader      = 24
	longPageHeader       = 40
)

// ReplicaConfig is one streaming replica of the primary.
type ReplicaConfig struct {
	// DSN is the replica's connection string. Pool, when set, is used instead; keel applies the
	// same query mode, statement cache and AfterConnect setup as on the primary.
	DSN  string
	Pool *pgxpool.Config
	// Weight is the replica's capacity relative to the others: at equal load a replica of weight
	// 2 takes twice the reads of one of weight 1. Defaults to 1.
	Weight int
}

// ReplicaStats is the state of one replica as the lag tracker last saw it, with its routing
// counters.
type ReplicaStats struct {
	// Name is host:port/database.
	Name   string
	Weight int
	// Healthy reports that the last check reached the replica and found it in recovery.
	Healthy bool
	// Lag bounds how far the replica was behind the primary at the last check: it had replayed
	// everything the primary had written Lag before the check.
	Lag time.Duration
	// Position is the WAL position the replica had replayed at the last check.
	Position  Position
	CheckedAt time.Time
	// InFlight counts the terminals and transactions running on the replica now.
	InFlight int64
	// Reads counts the terminals and transactions routed to the replica.
	Reads uint64
	// Err is the error of the last check, if it failed or found the server not in recovery.
	Err error
}

// Position is a WAL position (an LSN) of the primary, as Postgres prints it: 16/B374D848.
type Position uint64

// String formats p as Postgres prints an LSN.
func (p Position) String() string {
	return strings.ToUpper(strconv.FormatUint(uint64(p)>>32, 16) + "/" +
		strconv.FormatUint(uint64(p)&0xFFFFFFFF, 16))
}

// ParsePosition parses an LSN as Postgres and Position.String print it.
func ParsePosition(s string) (Position, error) {
	hi, lo, ok := strings.Cut(s, "/")
	if !ok {
		return 0, errf("invalid WAL position %q", s)
	}
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, errf("invalid WAL position %q", s)
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, errf("invalid WAL position %q", s)
	}
	return Position(h<<32 | l), nil
}

// routeKey is the context key of a route.
type routeKey struct{}

// route is the replica routing of a context: OnReplica puts one on it, and writes made with the
// context raise its floor per DB to their commit position, so later replica reads on it only use
// replicas that have replayed them.
type route struct {
	mu    sync.Mutex
	marks []routeMark
}

// routeMark is a route's floor for one DB: the position replica reads must have replayed, and when
// it was committed (zero when it came from OnReplicaAfter).
type routeMark struct {
	db  *DB
	pos Position
	at  time.Time
}

// unreachable is the floor of a context whose last commit position could not be read: no replica
// can serve it.
const unreachable = ^Position(0)

// OnReplica marks ctx so that read terminals (One, First, List, All, Page, Count, CountUpTo,
// Exists, Each, Into, IntoOne, Batch) called with it outside a transaction run on a replica
// within Config.MaxReplicaLag, the least loaded relative to its weight, and on the primary when
// none qualifies. Explicit transactions keep using the primary unless started with ReplicaTx.
//
// Reads on the context see its own writes: a write made with ctx, or a context derived from it,
// records the primary's WAL position after its commit, in the commit's round trip, and later
// replica reads on ctx run only where that position has been replayed. A replica the lag tracker
// has seen replay it is used as is; one it has not yet seen is used when the write is older than
// the replica's lag, with a check of its replay position sent first in the read's own round trip,
// and the read moves to the primary when the check fails. A context that is already marked is
// returned unchanged, so the positions are shared with it.
func OnReplica(ctx context.Context) context.Context {
	if routeFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, routeKey{}, &route{})
}

// OnReplicaAfter is OnReplica for reads that must see everything db committed up to pos, such as
// a position an earlier request returned from CommitPosition and the client sent back.
func OnReplicaAfter(ctx context.Context, db *DB, pos Position) context.Context {
	ctx = OnReplica(ctx)
	routeFrom(ctx).raise(db, pos, time.Time{})
	return ctx
}

// CommitPosition returns the newest position of db that replica reads on ctx must have replayed:
// the commit position of the last write made with ctx after OnReplica, or the position given to
// OnReplicaAfter. It is zero when there is none or db has no replicas, and the largest Position
// when keel could not read a commit's position, which keeps the context's reads on the primary.
func CommitPosition(ctx context.Context, db *DB) Position {
	return routeFrom(ctx).mark(db).pos
}

// ReplicaTx runs a Read on a replica, for long read-only work such as a report or an export: the
// transaction holds a replica connection from its first statement to its end, and every terminal
// in it runs there. It picks the replica as OnReplica does, among those the lag tracker has seen
// replay the context's commit position, falls back to the primary when none qualifies, and is
// refused on a Write and with Serializable, which a hot standby does not run.
func ReplicaTx() TxOption { return txOptionFunc(func(o *txOptions) { o.replica = true }) }

// Replicas returns the state of db's replicas, in Config.Replicas order.
func (db *DB) Replicas() []ReplicaStats {
	if db.replicas == nil {
		return nil
	}
	out := make([]ReplicaStats, len(db.replicas.list))
	for i, rep := range db.replicas.list {
		out[i] = rep.stats()
	}
	return out
}

// ReplicaFallbacks counts the reads that asked for a replica and ran on the primary because no
// replica was healthy, within the lag bound and past the context's commit position.
func (db *DB) ReplicaFallbacks() uint64 {
	if db.replicas == nil {
		return 0
	}
	return db.replicas.fallbacks.Load()
}

// routeFrom returns the route on ctx, or nil.
func routeFrom(ctx context.Context) *route {
	rt, _ := ctx.Value(routeKey{}).(*route)
	return rt
}

// mark returns the route's floor for db; a nil route has none.
func (rt *route) mark(db *DB) routeMark {
	if rt == nil {
		return routeMark{}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, m := range rt.marks {
		if m.db == db {
			return m
		}
	}
	return routeMark{}
}

// raise moves the route's floor for db up to pos, committed at at.
func (rt *route) raise(db *DB, pos Position, at time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i := range rt.marks {
		if m := &rt.marks[i]; m.db == db {
			if pos > m.pos {
				m.pos, m.at = pos, at
			}
			return
		}
	}
	rt.marks = append(rt.marks, routeMark{db: db, pos: pos, at: at})
}

// replicaSet is a DB's replicas and the goroutine that tracks their lag.
type replicaSet struct {
	list      []*replica
	maxLag    time.Duration
	fallbacks atomic.Uint64

	block   atomic.Int64
	segment atomic.Int64

	samples []walSample

	cancel context.CancelFunc
	done   chan struct{}
}

// walSample is the primary's WAL insert position read at a time on keel's clock.
type walSample struct {
	at  time.Time
	pos Position
}

// replica is one replica pool with its last check and routing counters.
type replica struct {
	pool   *pgxpool.Pool
	name   string
	weight int64

	inflight atomic.Int64
	reads    atomic.Uint64

	healthy atomic.Bool
	lag     atomic.Int64
	replay  atomic.Uint64
	checked atomic.Int64

	mu  sync.Mutex
	err error
}

// openReplicas opens a pool per configured replica, each set up as the primary is.
func openReplicas(ctx context.Context, cfg *Config) (*replicaSet, error) {
	rs := &replicaSet{maxLag: cfg.MaxReplicaLag, done: make(chan struct{})}
	for i, rc := range cfg.Replicas {
		if rc.Weight < 0 {
			rs.close()
			return nil, errf("replica %d has a negative weight", i)
		}
		pc, err := poolConfig("replica "+strconv.Itoa(i), rc.DSN, rc.Pool)
		if err != nil {
			rs.close()
			return nil, err
		}
		cfg.configure(pc)
		pool, err := pgxpool.NewWithConfig(ctx, pc)
		if err != nil {
			rs.close()
			return nil, fmt.Errorf("keel: open replica %d pool: %w", i, err)
		}
		weight := int64(rc.Weight)
		if weight == 0 {
			weight = 1
		}
		cc := pc.ConnConfig
		rs.list = append(rs.list, &replica{pool: pool, weight: weight,
			name: cc.Host + ":" + strconv.Itoa(int(cc.Port)) + "/" + cc.Database})
	}
	return rs, nil
}

// start starts the lag tracker.
func (rs *replicaSet) start(db *DB) {
	ctx, cancel := context.WithCancel(context.Background())
	rs.cancel = cancel
	go rs.run(ctx, db)
}

// run checks the replicas at once and then every replicaPollInterval until stop.
func (rs *replicaSet) run(ctx context.Context, db *DB) {
	defer close(rs.done)
	rs.poll(ctx, db)
	t := time.NewTicker(replicaPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rs.poll(ctx, db)
		}
	}
}

// stop stops the lag tracker and waits for it.
func (rs *replicaSet) stop() {
	rs.cancel()
	<-rs.done
}

// close closes the replica pools.
func (rs *replicaSet) close() {
	for _, rep := range rs.list {
		rep.pool.Close()
	}
}

// poll samples the primary's WAL insert position and then checks every replica concurrently
// against the samples kept.
func (rs *replicaSet) poll(ctx context.Context, db *DB) {
	ctx, cancel := context.WithTimeout(ctx, replicaPollInterval)
	defer cancel()
	if rs.block.Load() == 0 {
		var block, segment int64
		err := db.pool.QueryRow(ctx, `SELECT current_setting('wal_block_size')::int8,
		       pg_size_bytes(current_setting('wal_segment_size'))`).Scan(&block, &segment)
		if err == nil && block > 0 && segment > 0 {
			rs.block.Store(block)
			rs.segment.Store(segment)
		}
	}
	at := time.Now()
	var lsn int64
	sampled := db.pool.QueryRow(ctx, `SELECT (pg_current_wal_insert_lsn() - '0/0')::int8`).Scan(&lsn) == nil
	if sampled {
		rs.addSample(walSample{at: at, pos: rs.normalize(Position(lsn))})
	}
	var wg sync.WaitGroup
	for _, rep := range rs.list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs.check(ctx, rep, sampled)
		}()
	}
	wg.Wait()
}

// addSample keeps the samples that can show a lag up to the bound and a little beyond.
func (rs *replicaSet) addSample(s walSample) {
	keep := int(rs.maxLag/replicaPollInterval) + 3
	rs.samples = append(rs.samples, s)
	if n := len(rs.samples); n > keep {
		rs.samples = append(rs.samples[:0], rs.samples[n-keep:]...)
	}
}

// check reads a replica's recovery state and replay position and stores its lag. With a primary
// sample the lag is the age of the newest sample the replica has replayed, which needs no clock
// agreement between servers and stays small on an idle primary. Without one (the primary is
// unreachable) it falls back to the age of the last replayed commit, or zero when the replica has
// replayed all it received.
func (rs *replicaSet) check(ctx context.Context, rep *replica, sampled bool) {
	var inRecovery bool
	var replay, received int64
	var since float64
	err := rep.pool.QueryRow(ctx, `SELECT pg_is_in_recovery(),
	       COALESCE((pg_last_wal_replay_lsn() - '0/0')::int8, 0),
	       COALESCE((pg_last_wal_receive_lsn() - '0/0')::int8, 0),
	       COALESCE(EXTRACT(epoch FROM now() - pg_last_xact_replay_timestamp())::float8, 0)`).
		Scan(&inRecovery, &replay, &received, &since)
	at := time.Now()
	if err == nil && !inRecovery {
		err = errf("replica %s is not in recovery", rep.name)
	}
	if err != nil {
		rep.healthy.Store(false)
		rep.setErr(err)
		rep.checked.Store(at.UnixNano())
		return
	}
	var lag time.Duration
	switch {
	case sampled:
		lag = rs.lagOf(Position(replay), at, time.Duration(since*float64(time.Second)))
	case replay < received:
		lag = time.Duration(since * float64(time.Second))
	}
	rep.replay.Store(uint64(replay))
	rep.lag.Store(int64(lag))
	rep.setErr(nil)
	rep.checked.Store(at.UnixNano())
	rep.healthy.Store(true)
}

// lagOf returns the age at time at of the newest primary sample the replay position covers. A
// position older than every sample kept is at least as old as the oldest one, and at least as old
// as its last replayed commit.
func (rs *replicaSet) lagOf(replay Position, at time.Time, since time.Duration) time.Duration {
	for i := len(rs.samples) - 1; i >= 0; i-- {
		if replay >= rs.samples[i].pos {
			return max(at.Sub(rs.samples[i].at), 0)
		}
	}
	return max(at.Sub(rs.samples[0].at), since)
}

// normalize moves a WAL insert position that sits just past a page header back to the page
// boundary, which is where a replica's replay position stops when the last record ends there.
func (rs *replicaSet) normalize(p Position) Position {
	block, segment := rs.block.Load(), rs.segment.Load()
	if block == 0 {
		block, segment = defaultWALBlock, defaultWALSegment
	}
	switch {
	case int64(p)%segment == longPageHeader:
		return p - longPageHeader
	case int64(p)%block == shortPageHeader:
		return p - shortPageHeader
	}
	return p
}

// pick returns the replica a read runs on, counted in flight until release. A replica qualifies
// when it was checked recently, is healthy and within the lag bound, and the tracker saw it replay
// the floor; with guarded set, one the tracker has not seen replay the floor also qualifies, after
// those that did, when the floor was committed longer ago than the replica's lag (the read then
// checks the position in its round trip). Among the qualifying replicas of the best kind it takes
// the one with the fewest reads in flight for its weight, then the fewest reads routed for its
// weight. It returns nil, and counts a fallback, when none qualifies.
func (rs *replicaSet) pick(floor routeMark, guarded bool) *replica {
	now := time.Now()
	var best *replica
	var bestUnseen bool
	var bestLoad, bestShare float64
	for _, rep := range rs.list {
		lag := rep.lag.Load()
		if !rep.healthy.Load() || lag > int64(rs.maxLag) ||
			now.UnixNano()-rep.checked.Load() > int64(replicaStaleAfter) {
			continue
		}
		unseen := Position(rep.replay.Load()) < floor.pos
		if unseen && (!guarded || floor.pos == unreachable ||
			!floor.at.IsZero() && now.Sub(floor.at) < time.Duration(lag)) {
			continue
		}
		load := float64(rep.inflight.Load()) / float64(rep.weight)
		share := float64(rep.reads.Load()) / float64(rep.weight)
		better := best == nil || bestUnseen && !unseen ||
			bestUnseen == unseen && (load < bestLoad || load == bestLoad && share < bestShare)
		if better {
			best, bestUnseen, bestLoad, bestShare = rep, unseen, load, share
		}
	}
	if best == nil {
		rs.fallbacks.Add(1)
		return nil
	}
	best.inflight.Add(1)
	best.reads.Add(1)
	return best
}

// release ends a read picked on the replica.
func (rep *replica) release() { rep.inflight.Add(-1) }

// setErr stores the error of the last check.
func (rep *replica) setErr(err error) {
	rep.mu.Lock()
	rep.err = err
	rep.mu.Unlock()
}

// stats returns the replica's state and counters.
func (rep *replica) stats() ReplicaStats {
	rep.mu.Lock()
	err := rep.err
	rep.mu.Unlock()
	st := ReplicaStats{
		Name:     rep.name,
		Weight:   int(rep.weight),
		Healthy:  rep.healthy.Load(),
		Lag:      time.Duration(rep.lag.Load()),
		Position: Position(rep.replay.Load()),
		InFlight: rep.inflight.Load(),
		Reads:    rep.reads.Load(),
		Err:      err,
	}
	if c := rep.checked.Load(); c > 0 {
		st.CheckedAt = time.Unix(0, c)
	}
	return st
}

// recordCommit raises the route's floor to the primary position read after a commit. When the
// position could not be read, the floor becomes unreachable, keeping the context's reads on the
// primary.
func (db *DB) recordCommit(rt *route, lsn int64, ok bool) {
	if !ok {
		rt.raise(db, unreachable, time.Now())
		return
	}
	rt.raise(db, db.replicas.normalize(Position(lsn)), time.Now())
}

// commitPositionSQL reads the primary's WAL insert position. Sent after COMMIT in the same round
// trip, it is at or past the end of the commit record.
const commitPositionSQL = "SELECT (pg_current_wal_insert_lsn() - '0/0')::int8"
