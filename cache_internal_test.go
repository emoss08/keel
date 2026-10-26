package keel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestCacheEntryRoundTrip(t *testing.T) {
	main := &stmt{sql: "SELECT a, b FROM t", tag: pgconn.NewCommandTag("SELECT 2")}
	rec := &stmtRecord{s: main, scanned: true, fields: []pgconn.FieldDescription{
		{Name: "a", TableOID: 7, TableAttributeNumber: 1, DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1},
		{Name: "b", DataTypeOID: 20, DataTypeSize: 8, Format: 1},
	}}
	rec.addRow([][]byte{[]byte("x"), nil})
	rec.addRow([][]byte{{}, []byte("12345678")})
	load := &stmt{sql: "SELECT c FROM u", tag: pgconn.NewCommandTag("SELECT 0")}
	empty := &stmtRecord{s: load, scanned: true}
	stamps := []pairStamp{{version: 3}, {version: 0, epoch: 2}}
	buf := encodeEntry([]string{"t"}, 2, stamps, [][]*stmtRecord{{rec}, {empty}})

	e, err := decodeEntry(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.tables) != 1 || e.tables[0] != "t" || e.prefixes != 2 || len(e.stamps) != 2 || e.stamps[1] != stamps[1] {
		t.Fatalf("header %+v", e)
	}
	if len(e.batches) != 2 || len(e.batches[0]) != 1 || len(e.batches[1]) != 1 {
		t.Fatalf("batches %+v", e.batches)
	}
	st := e.batches[0][0]
	if st.sqlHash != sqlHash(main.sql) || st.tag != "SELECT 2" || !st.scanned || st.rows != 2 {
		t.Fatalf("statement %+v", st)
	}
	if st.fields[0] != rec.fields[0] || st.fields[1] != rec.fields[1] {
		t.Fatalf("fields %+v", st.fields)
	}
	rows := newReplayRows(nil, &st)
	var got []string
	for rows.Next() {
		for _, v := range rows.RawValues() {
			if v == nil {
				got = append(got, "NULL")
			} else {
				got = append(got, "'"+string(v)+"'")
			}
		}
	}
	if s := strings.Join(got, " "); s != "'x' NULL '' '12345678'" {
		t.Fatalf("values %s", s)
	}
	if rows.Next() || rows.CommandTag().String() != "SELECT 2" {
		t.Fatal("rows did not end")
	}
	if e.batches[1][0].rows != 0 || len(e.batches[1][0].fields) != 0 {
		t.Fatalf("empty statement %+v", e.batches[1][0])
	}

	for n := range len(buf) {
		if _, err := decodeEntry(buf[:n]); err == nil {
			t.Fatalf("a %d-byte prefix of a %d-byte entry decoded", n, len(buf))
		}
	}
	bad := append([]byte(nil), buf...)
	bad[2] = 9
	if _, err := decodeEntry(bad); err == nil {
		t.Fatal("an entry of another layout decoded")
	}
}

func TestCacheKeys(t *testing.T) {
	s := func(sql string, args ...any) []*stmt { return []*stmt{{sql: sql, args: args}} }
	key := func(tenant, extra string, stmts []*stmt) string {
		t.Helper()
		k, ok := cacheKey(tenant, extra, "-", stmts)
		if !ok {
			t.Fatal("not keyed")
		}
		return k
	}
	base := key("org/bu", "", s("SELECT $1", "a"))
	if key("org/bu", "", s("SELECT $1", "a")) != base {
		t.Fatal("equal statements keyed differently")
	}
	type named string
	text := "a"
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	others := map[string]string{
		"tenant":   key("org/bu2", "", s("SELECT $1", "a")),
		"extra":    key("org/bu", "x", s("SELECT $1", "a")),
		"text":     key("org/bu", "", s("SELECT $1 ", "a")),
		"value":    key("org/bu", "", s("SELECT $1", "b")),
		"int":      key("org/bu", "", s("SELECT $1", int64(1))),
		"named":    key("org/bu", "", s("SELECT $1", named("a"))),
		"nil":      key("org/bu", "", s("SELECT $1", nil)),
		"slice":    key("org/bu", "", s("SELECT $1", []string{"a"})),
		"pointer":  key("org/bu", "", s("SELECT $1", &text)),
		"split":    key("org/b", "u", s("SELECT $1", "a")),
		"time":     key("org/bu", "", s("SELECT $1", utc)),
		"timezone": key("org/bu", "", s("SELECT $1", utc.In(time.FixedZone("x", 3600)))),
	}
	seen := map[string]string{base: "base"}
	for name, k := range others {
		if prev, dup := seen[k]; dup {
			t.Fatalf("%s keys as %s", name, prev)
		}
		seen[k] = name
	}
	if _, ok := cacheKey("", "", "-", s("SELECT $1", func() {})); ok {
		t.Fatal("a function was keyed")
	}
}

func TestNotifyGroupsStayUnderThePayloadLimit(t *testing.T) {
	var keys []versionKey
	for i := range 2000 {
		keys = append(keys, versionKey{tenant: strings.Repeat("o", 40) + string(rune('a'+i%26)), table: "shipments"})
	}
	keys = append(keys, versionKey{tenant: strings.Repeat("\"", 2000), table: "big"})
	groups := notifyGroups(keys)
	sizes := map[int32]int{}
	for i, g := range groups {
		if i == len(keys)-1 {
			if g != -1 {
				t.Fatalf("an oversized pair is in group %d", g)
			}
			continue
		}
		row, _ := json.Marshal([]any{keys[i].tenant, keys[i].table, int64(1) << 62})
		sizes[g] += len(row) + 4
	}
	if len(sizes) < 2 {
		t.Fatal("pairs were not split")
	}
	for g, n := range sizes {
		if n >= 8000 {
			t.Fatalf("group %d holds %d bytes", g, n)
		}
	}
}

// testBus returns a version bus marked up, without a database.
func testBus() *versionBus {
	b := newVersionBus(&DB{})
	b.up.Store(true)
	return b
}

// know stores a known version in b.
func (b *versionBus) know(k versionKey, v int64) {
	s := b.shard(k)
	s.mu.Lock()
	b.state(s, k, true).observe(v)
	s.mu.Unlock()
}

func TestVersionBusEpochsAndNotifications(t *testing.T) {
	b := testBus()
	tables := []string{"customers"}
	pair := versionKey{tenant: "o/b", table: "customers"}
	for _, p := range tenantPrefixes("o/b") {
		b.know(versionKey{tenant: p, table: "customers"}, 0)
	}
	stamps := func() []pairStamp {
		t.Helper()
		st, err := b.stamps(t.Context(), "o/b", tables, nil)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	clean := stamps()
	if len(clean) != 3 || clean[0] != (pairStamp{}) {
		t.Fatalf("stamps %+v", clean)
	}

	b.bump(versionSet{pair: {}})
	dirty := stamps()
	if dirty[0].epoch != 1 || dirty[0].version != 0 {
		t.Fatalf("after a local write %+v", dirty[0])
	}
	marks, gen := b.marks(versionSet{pair: {}})
	b.bump(versionSet{pair: {}})
	b.flushed(marks, gen, map[versionKey]int64{pair: 1})
	if st := stamps()[0]; st.version != 1 || st.epoch != 2 {
		t.Fatalf("a write after the flush began must stay dirty: %+v", st)
	}
	marks, gen = b.marks(versionSet{pair: {}})
	b.flushed(marks, gen, map[versionKey]int64{pair: 2})
	if st := stamps()[0]; st != (pairStamp{version: 2}) {
		t.Fatalf("after the flush %+v", st)
	}

	b.apply(`[["o/b", "customers", 5], ["other", "customers", 9]]`)
	if st := stamps()[0]; st.version != 5 {
		t.Fatalf("after a notification %+v", st)
	}
	b.apply(`[["o/b", "customers", 4]]`)
	if st := stamps()[0]; st.version != 5 {
		t.Fatalf("an older notification moved the version back: %+v", st)
	}
	s := b.shard(versionKey{tenant: "other", table: "customers"})
	if s.m[versionKey{tenant: "other", table: "customers"}] != nil {
		t.Fatal("a notification for a pair nobody reads was kept")
	}

	b.bump(versionSet{pair: {}})
	b.apply(notifyAll)
	if _, err := b.stamps(t.Context(), "o/b", tables, nil); err == nil {
		t.Fatal("a reset bus still knew versions")
	}
	if st := b.shard(pair).m[pair]; st == nil || st.dirty() == 0 || st.known {
		t.Fatalf("a reset dropped a local write: %+v", st)
	}

	b.down()
	if _, err := b.stamps(t.Context(), "o/b", tables, nil); err == nil {
		t.Fatal("a bus that is down served versions")
	}
}

func TestCachedTTL(t *testing.T) {
	r := func(d time.Duration) *read { return &read{cache: d} }
	if got := cachedTTL([]*read{r(time.Minute), r(time.Second)}); got != time.Second {
		t.Fatalf("shortest %v", got)
	}
	if got := cachedTTL([]*read{r(0)}); got != 0 {
		t.Fatalf("none %v", got)
	}
	if got := cachedTTL([]*read{r(time.Minute), r(0)}); got >= 0 {
		t.Fatalf("mixed %v", got)
	}
	if got := cachedTTL([]*read{r(-1)}); got >= 0 {
		t.Fatalf("locked %v", got)
	}
}
