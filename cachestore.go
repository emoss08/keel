package keel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	analyticscache "github.com/emoss08/keel/analytics/cache"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// LocalCache is the in-process store of the result cache (CacheConfig.Local): encoded entries by
// key, each with a time to live. Values it returns are only read, never changed; every hit
// decodes them into new rows. keel.MemoryCache is the implementation keel provides.
type LocalCache interface {
	// Get returns the value stored under key and true, or false when there is none or it expired.
	Get(key string) ([]byte, bool)
	// Set stores value under key for ttl. A store may refuse a value, which is then a miss.
	Set(key string, value []byte, ttl time.Duration)
	// Bytes returns the bytes the store holds, for CacheStats.
	Bytes() int64
}

// SharedCache is a store every process shares (CacheConfig.Shared), such as Redis: encoded
// entries by key, each with a time to live. keel/cache/rediscache adapts a Redis client, and the
// stores of keel/analytics/cache fit as they are.
type SharedCache interface {
	// Get returns the value stored under key and true, or false when there is none or it expired.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value under key for ttl.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// DefaultCacheBytes is the size of the MemoryCache the result cache keeps when CacheConfig.Local
// is nil.
const DefaultCacheBytes = 64 << 20

// MemoryCache returns a LocalCache of at most maxBytes bytes of keys, values and bookkeeping,
// analytics' otter-backed memory store: a full store evicts the entries least likely to be read
// again, and a value larger than maxBytes is not kept. A maxBytes of zero or less is
// DefaultCacheBytes.
func MemoryCache(maxBytes int64) LocalCache {
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	return &lru{m: analyticscache.NewMemory(analyticscache.MemoryOptions{MaxBytes: maxBytes})}
}

// lru is MemoryCache's LocalCache: analytics' memory store.
type lru struct {
	m *analyticscache.Memory
}

// Get returns the value under key.
func (l *lru) Get(key string) ([]byte, bool) {
	v, ok, _ := l.m.Get(context.Background(), key)
	return v, ok
}

// Set stores value under key for ttl; a value larger than a shard is dropped.
func (l *lru) Set(key string, value []byte, ttl time.Duration) {
	_ = l.m.Set(context.Background(), key, value, ttl)
}

// Bytes returns the bytes the store holds.
func (l *lru) Bytes() int64 { return l.m.Stats().Bytes }

// errCacheReplay is the error of a cache hit whose stored result does not fit the statements the
// terminal sends: the terminal runs again against the database.
var errCacheReplay = errors.New("keel: the cached result does not fit the statements")

// errBadEntry is the error of an entry that does not decode.
var errBadEntry = errors.New("keel: malformed result cache entry")

// entryMagic starts every encoded entry; its last byte is the entry layout's version.
const entryMagic = "KC\x01"

// cacheEntry is a decoded entry: the tables its statements read with the stamps they were read
// under (len(prefixes) per table, as versionBus.stamps returns them), and the raw result of every
// statement of every round trip the terminal sent, in order.
type cacheEntry struct {
	tables   []string
	prefixes int
	stamps   []pairStamp
	batches  [][]cachedStmt
}

// cachedStmt is one statement's stored result: the hash of its text, its command tag, whether it
// returned rows to a scanner, its result columns and the raw values of its rows, row by row.
type cachedStmt struct {
	sqlHash uint64
	tag     string
	scanned bool
	fields  []pgconn.FieldDescription
	rows    int
	values  [][]byte
}

// stmtRecord is the result of one statement recorded as it is scanned on a miss: its columns and
// its rows' raw values, copied into one buffer (lens holds each value's length, -1 for NULL).
type stmtRecord struct {
	s       *stmt
	scanned bool
	fields  []pgconn.FieldDescription
	rows    int
	lens    []int32
	data    []byte
}

// setFields keeps the statement's result columns the first time they are known.
func (rec *stmtRecord) setFields(fds []pgconn.FieldDescription) {
	if rec.fields == nil && fds != nil {
		rec.fields = append(make([]pgconn.FieldDescription, 0, len(fds)), fds...)
	}
}

// addRow copies one row's raw values.
func (rec *stmtRecord) addRow(raw [][]byte) {
	rec.rows++
	for _, v := range raw {
		if v == nil {
			rec.lens = append(rec.lens, -1)
			continue
		}
		rec.lens = append(rec.lens, int32(len(v)))
		rec.data = append(rec.data, v...)
	}
}

// recordScan makes s's scanner record the rows it reads into a new record.
func recordScan(s *stmt) *stmtRecord {
	rec := &stmtRecord{s: s, scanned: s.scan != nil}
	if s.scan == nil {
		return rec
	}
	scan := s.scan
	s.scan = func(rows pgx.Rows) error {
		err := scan(&recordingRows{Rows: rows, rec: rec})
		rec.setFields(rows.FieldDescriptions())
		return err
	}
	return rec
}

// recordingRows passes a statement's rows to its scanner and records each row it reads.
type recordingRows struct {
	pgx.Rows
	rec *stmtRecord
}

// FieldDescriptions returns the result columns and records them.
func (r *recordingRows) FieldDescriptions() []pgconn.FieldDescription {
	fds := r.Rows.FieldDescriptions()
	r.rec.setFields(fds)
	return fds
}

// Next advances to the next row and records it.
func (r *recordingRows) Next() bool {
	if !r.Rows.Next() {
		return false
	}
	r.rec.setFields(r.Rows.FieldDescriptions())
	r.rec.addRow(r.RawValues())
	return true
}

// encodeEntry encodes the result of a terminal: the tables read with their stamps and the
// recorded statements of each round trip.
func encodeEntry(tables []string, prefixes int, stamps []pairStamp, batches [][]*stmtRecord) []byte {
	size := len(entryMagic) + 16
	for _, b := range batches {
		for _, rec := range b {
			size += 32 + len(rec.data) + 2*len(rec.lens) + 32*len(rec.fields)
		}
	}
	buf := make([]byte, 0, size)
	buf = append(buf, entryMagic...)
	buf = binary.AppendUvarint(buf, uint64(len(tables)))
	for _, t := range tables {
		buf = appendString(buf, t)
	}
	buf = binary.AppendUvarint(buf, uint64(prefixes))
	for _, st := range stamps {
		buf = binary.AppendVarint(buf, st.version)
		buf = binary.AppendUvarint(buf, st.epoch)
	}
	buf = binary.AppendUvarint(buf, uint64(len(batches)))
	for _, b := range batches {
		buf = binary.AppendUvarint(buf, uint64(len(b)))
		for _, rec := range b {
			buf = binary.LittleEndian.AppendUint64(buf, sqlHash(rec.s.sql))
			buf = appendString(buf, rec.s.tag.String())
			if rec.scanned {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
			buf = binary.AppendUvarint(buf, uint64(len(rec.fields)))
			for _, fd := range rec.fields {
				buf = appendString(buf, fd.Name)
				buf = binary.AppendUvarint(buf, uint64(fd.TableOID))
				buf = binary.AppendUvarint(buf, uint64(fd.TableAttributeNumber))
				buf = binary.AppendUvarint(buf, uint64(fd.DataTypeOID))
				buf = binary.AppendVarint(buf, int64(fd.DataTypeSize))
				buf = binary.AppendVarint(buf, int64(fd.TypeModifier))
				buf = binary.AppendVarint(buf, int64(fd.Format))
			}
			buf = binary.AppendUvarint(buf, uint64(rec.rows))
			off := 0
			for _, n := range rec.lens {
				buf = binary.AppendVarint(buf, int64(n))
				if n > 0 {
					buf = append(buf, rec.data[off:off+int(n)]...)
					off += int(n)
				}
			}
		}
	}
	return buf
}

// appendString appends s with its length.
func appendString(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

// entryReader decodes an entry, failing on anything out of bounds: an entry from a shared store
// is not trusted to be well formed.
type entryReader struct {
	b   []byte
	err error
}

// uvarint reads an unsigned integer.
func (r *entryReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errBadEntry
		return 0
	}
	r.b = r.b[n:]
	return v
}

// varint reads a signed integer.
func (r *entryReader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.err = errBadEntry
		return 0
	}
	r.b = r.b[n:]
	return v
}

// count reads a count of items each at least least bytes long, refusing one the rest of the
// entry cannot hold.
func (r *entryReader) count(least int) int {
	n := r.uvarint()
	if r.err == nil && n > uint64(len(r.b)/max(least, 1)+1) {
		r.err = errBadEntry
		return 0
	}
	return int(n)
}

// bytes reads n bytes, sharing the entry's memory.
func (r *entryReader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.err = errBadEntry
		return nil
	}
	out := r.b[:n:n]
	r.b = r.b[n:]
	return out
}

// str reads a string.
func (r *entryReader) str() string {
	return string(r.bytes(r.count(1)))
}

// u64 reads a little-endian unsigned integer.
func (r *entryReader) u64() uint64 {
	b := r.bytes(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

// decodeEntry decodes an encoded entry. Raw values share raw's memory, which the stores never
// change.
func decodeEntry(raw []byte) (*cacheEntry, error) {
	if len(raw) < len(entryMagic) || string(raw[:len(entryMagic)]) != entryMagic {
		return nil, errBadEntry
	}
	r := &entryReader{b: raw[len(entryMagic):]}
	e := &cacheEntry{}
	e.tables = make([]string, r.count(1))
	for i := range e.tables {
		e.tables[i] = r.str()
	}
	e.prefixes = r.count(0)
	if r.err == nil && (e.prefixes == 0 || e.prefixes > 64) {
		return nil, errBadEntry
	}
	e.stamps = make([]pairStamp, len(e.tables)*e.prefixes)
	for i := range e.stamps {
		e.stamps[i] = pairStamp{version: r.varint(), epoch: r.uvarint()}
	}
	e.batches = make([][]cachedStmt, r.count(1))
	for bi := range e.batches {
		b := make([]cachedStmt, r.count(10))
		for si := range b {
			st := &b[si]
			st.sqlHash = r.u64()
			st.tag = r.str()
			flag := r.bytes(1)
			st.scanned = len(flag) == 1 && flag[0] == 1
			st.fields = make([]pgconn.FieldDescription, r.count(7))
			for fi := range st.fields {
				st.fields[fi] = pgconn.FieldDescription{
					Name:                 r.str(),
					TableOID:             uint32(r.uvarint()),
					TableAttributeNumber: uint16(r.uvarint()),
					DataTypeOID:          uint32(r.uvarint()),
					DataTypeSize:         int16(r.varint()),
					TypeModifier:         int32(r.varint()),
					Format:               int16(r.varint()),
				}
			}
			st.rows = r.count(len(st.fields))
			n := st.rows * len(st.fields)
			if r.err != nil || n > len(r.b)+1 {
				return nil, errBadEntry
			}
			st.values = make([][]byte, n)
			for vi := range st.values {
				if l := r.varint(); l >= 0 {
					st.values[vi] = r.bytes(int(l))
					if st.values[vi] == nil && r.err == nil {
						st.values[vi] = []byte{}
					}
				}
			}
		}
		e.batches[bi] = b
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.b) != 0 {
		return nil, errBadEntry
	}
	return e, nil
}

// replayRows feeds a statement's scanner its stored result, decoded on a decoder connection.
type replayRows struct {
	conn   *pgx.Conn
	st     *cachedStmt
	row    int
	closed bool
	err    error
}

// newReplayRows returns the rows of st, positioned before the first.
func newReplayRows(conn *pgx.Conn, st *cachedStmt) *replayRows {
	return &replayRows{conn: conn, st: st, row: -1}
}

// Close ends the rows.
func (r *replayRows) Close() { r.closed = true }

// Err returns the error a Scan reported.
func (r *replayRows) Err() error { return r.err }

// CommandTag returns the statement's stored command tag.
func (r *replayRows) CommandTag() pgconn.CommandTag { return pgconn.NewCommandTag(r.st.tag) }

// FieldDescriptions returns the stored result columns.
func (r *replayRows) FieldDescriptions() []pgconn.FieldDescription { return r.st.fields }

// Next advances to the next stored row.
func (r *replayRows) Next() bool {
	if r.closed {
		return false
	}
	r.row++
	if r.row >= r.st.rows {
		r.closed = true
		return false
	}
	return true
}

// RawValues returns the current row's raw values.
func (r *replayRows) RawValues() [][]byte {
	if r.row < 0 || r.row >= r.st.rows {
		return nil
	}
	n := len(r.st.fields)
	return r.st.values[r.row*n : (r.row+1)*n : (r.row+1)*n]
}

// Scan decodes the current row into dest, as pgx's rows do: a nil destination skips its column.
func (r *replayRows) Scan(dest ...any) error {
	raw := r.RawValues()
	if raw == nil {
		return r.fail(errf("Scan called without a row"))
	}
	if len(dest) != len(r.st.fields) {
		return r.fail(errf("Scan has %d destinations for %d columns", len(dest), len(r.st.fields)))
	}
	m := r.conn.TypeMap()
	for i, d := range dest {
		if d == nil {
			continue
		}
		fd := r.st.fields[i]
		if err := m.Scan(fd.DataTypeOID, fd.Format, raw[i], d); err != nil {
			return r.fail(fmt.Errorf("keel: scan column %s: %w", fd.Name, err))
		}
	}
	return nil
}

// fail records err as the rows' error and returns it.
func (r *replayRows) fail(err error) error {
	r.err = err
	return err
}

// Values decodes the current row into Go values by each column's codec.
func (r *replayRows) Values() ([]any, error) {
	raw := r.RawValues()
	if raw == nil {
		return nil, r.fail(errf("Values called without a row"))
	}
	m := r.conn.TypeMap()
	out := make([]any, len(raw))
	for i, v := range raw {
		if v == nil {
			continue
		}
		fd := r.st.fields[i]
		typ, ok := m.TypeForOID(fd.DataTypeOID)
		if !ok {
			if fd.Format == pgtype.TextFormatCode {
				out[i] = string(v)
			} else {
				out[i] = append([]byte(nil), v...)
			}
			continue
		}
		val, err := typ.Codec.DecodeValue(m, fd.DataTypeOID, fd.Format, v)
		if err != nil {
			return nil, r.fail(err)
		}
		out[i] = val
	}
	return out, nil
}

// Conn returns the decoder connection, whose type map and plan cache decode the rows.
func (r *replayRows) Conn() *pgx.Conn { return r.conn }

// TypeMap returns the decoder connection's type map.
func (r *replayRows) TypeMap() *pgtype.Map { return r.conn.TypeMap() }

// decoder is a connection that decodes cache hits: opened with the primary pool's settings and
// AfterConnect, so it registers every type a pooled connection does (enums, LoadTypes, the JSON
// codec, an application's codecs), then closed. Its type map and scan plan cache stay usable,
// and a hit never takes a pooled connection or sends anything. One goroutine uses it at a time.
type decoder struct {
	conn   *pgx.Conn
	opened time.Time
}

// decoders hands out decoder connections: at most GOMAXPROCS, since decoding is CPU work, each
// replaced after the pool's MaxConnLifetime so types created since are registered.
type decoders struct {
	db   *DB
	free chan *decoder
	made atomic.Int32
	max  int32
	life time.Duration
}

// newDecoders returns an empty decoder set for db.
func newDecoders(db *DB) *decoders {
	n := max(runtime.GOMAXPROCS(0), 2)
	life := db.pool.Config().MaxConnLifetime
	if life <= 0 {
		life = time.Hour
	}
	return &decoders{db: db, free: make(chan *decoder, n), max: int32(n), life: life}
}

// get returns a free decoder, opens one while fewer than the maximum exist, or waits for one.
func (d *decoders) get(ctx context.Context) (*decoder, error) {
	select {
	case dec := <-d.free:
		return dec, nil
	default:
	}
	if d.made.Add(1) <= d.max {
		dec, err := d.open(ctx)
		if err != nil {
			d.made.Add(-1)
			return nil, err
		}
		return dec, nil
	}
	d.made.Add(-1)
	select {
	case dec := <-d.free:
		return dec, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// put returns dec, dropping it once it is older than the decoders' lifetime.
func (d *decoders) put(dec *decoder) {
	if time.Since(dec.opened) > d.life {
		d.made.Add(-1)
		return
	}
	select {
	case d.free <- dec:
	default:
		d.made.Add(-1)
	}
}

// open connects a decoder with the primary pool's settings, runs the pool's AfterConnect on it
// and closes it.
func (d *decoders) open(ctx context.Context) (*decoder, error) {
	pc := d.db.pool.Config()
	cc := pc.ConnConfig.Copy()
	if pc.BeforeConnect != nil {
		if err := pc.BeforeConnect(ctx, cc); err != nil {
			return nil, fmt.Errorf("keel: open a cache decoder: %w", err)
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("keel: open a cache decoder: %w", err)
	}
	if pc.AfterConnect != nil {
		if err := pc.AfterConnect(ctx, conn); err != nil {
			_ = conn.Close(context.Background())
			return nil, fmt.Errorf("keel: open a cache decoder: %w", err)
		}
	}
	_ = conn.Close(ctx)
	return &decoder{conn: conn, opened: time.Now()}, nil
}
