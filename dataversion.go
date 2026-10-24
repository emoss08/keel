package keel

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
)

// Pending data versions are written every Config.DataVersions.Interval (defaultVersionInterval
// unless set), versionChunk pairs per statement, each write bounded by versionWriteTimeout, into
// versionTable.
const (
	defaultVersionInterval = time.Second
	versionWriteTimeout    = 30 * time.Second
	versionChunk           = 5000
	versionTable           = "keel_data_versions"
)

// versionTableSQL creates the data versions table. The fill factor leaves room on every page so
// that a version bump, which changes no indexed column, is a heap-only update.
const versionTableSQL = `CREATE TABLE IF NOT EXISTS keel_data_versions (
    tenant_key text COLLATE "C" NOT NULL,
    table_name text COLLATE "C" NOT NULL,
    version bigint NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_key, table_name)
) WITH (fillfactor = 70)`

// versionUpsertSQL bumps the versions of a chunk of pairs in one statement, locking the rows in
// key order so that concurrent flushes from many processes cannot deadlock.
const versionUpsertSQL = `INSERT INTO keel_data_versions AS v (tenant_key, table_name, version, changed_at)
SELECT k, t, 1, now() FROM unnest($1::text[], $2::text[]) AS u(k, t) ORDER BY k COLLATE "C", t COLLATE "C"
ON CONFLICT (tenant_key, table_name) DO UPDATE SET version = v.version + 1, changed_at = now()`

// DataVersionsConfig configures data versions: a counter per tenant and table that keel bumps
// after a write to the table commits, for caches keyed on the data they read. keel keeps them in
// keel_data_versions, which it creates on first use under an advisory lock.
type DataVersionsConfig struct {
	// Enabled turns data versions on. They are off by default: each changed (tenant, table) pair
	// costs a row upsert per Interval.
	Enabled bool
	// Interval is how often pending pairs are written; a pair changed many times within it is
	// bumped once. Defaults to 1s.
	Interval time.Duration
	// OnError receives the errors of background writes; the pairs of a failed write are retried
	// at the next interval.
	OnError func(err error)
}

// versionKey is one (tenant, table) pair.
type versionKey struct {
	tenant string
	table  string
}

// versionSet is a set of pairs.
type versionSet map[versionKey]struct{}

// versioner collects the pairs committed writes changed and writes them every interval from its
// own goroutine, outside every write's transaction.
type versioner struct {
	db *DB

	mu      sync.Mutex
	pending versionSet

	flushMu sync.Mutex

	readyMu sync.Mutex
	ready   bool

	stop chan struct{}
	done chan struct{}
}

// startVersioner starts db's versioner goroutine.
func startVersioner(db *DB) *versioner {
	v := &versioner{db: db, pending: versionSet{}, stop: make(chan struct{}), done: make(chan struct{})}
	go v.run()
	return v
}

// run writes the pending pairs every interval, and once more when stopped.
func (v *versioner) run() {
	defer close(v.done)
	t := time.NewTicker(v.db.cfg.DataVersions.Interval)
	defer t.Stop()
	for {
		select {
		case <-v.stop:
			v.flushInBackground()
			return
		case <-t.C:
			v.flushInBackground()
		}
	}
}

// flushInBackground writes the pending pairs within versionWriteTimeout and hands an error to
// Config.DataVersions.OnError.
func (v *versioner) flushInBackground() {
	ctx, cancel := context.WithTimeout(context.Background(), versionWriteTimeout)
	defer cancel()
	if err := v.flush(ctx); err != nil && v.db.cfg.DataVersions.OnError != nil {
		v.db.cfg.DataVersions.OnError(err)
	}
}

// close writes the pending pairs and stops the goroutine.
func (v *versioner) close() {
	close(v.stop)
	<-v.done
}

// add queues pairs for the next write.
func (v *versioner) add(set versionSet) {
	if v == nil {
		return
	}
	v.mu.Lock()
	for k := range set {
		v.pending[k] = struct{}{}
	}
	v.mu.Unlock()
}

// flush writes the pending pairs, one statement per chunk of pairs, every chunk in one round
// trip. Pairs it failed to write go back to the queue.
func (v *versioner) flush(ctx context.Context) error {
	v.flushMu.Lock()
	defer v.flushMu.Unlock()
	v.mu.Lock()
	set := v.pending
	v.pending = versionSet{}
	v.mu.Unlock()
	if len(set) == 0 {
		return nil
	}
	err := v.write(ctx, set)
	if err != nil {
		v.add(set)
		return fmt.Errorf("keel: write data versions: %w", err)
	}
	return nil
}

// write upserts set in key order, versionChunk pairs per statement, in one round trip.
func (v *versioner) write(ctx context.Context, set versionSet) error {
	if err := v.ensure(ctx); err != nil {
		return err
	}
	keys := make([]versionKey, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b versionKey) int {
		if c := strings.Compare(a.tenant, b.tenant); c != 0 {
			return c
		}
		return strings.Compare(a.table, b.table)
	})
	b := &pgx.Batch{}
	stmts := make([]*stmt, 0, (len(keys)+versionChunk-1)/versionChunk)
	for start := 0; start < len(keys); start += versionChunk {
		chunk := keys[start:min(start+versionChunk, len(keys))]
		tenants := make([]string, len(chunk))
		tables := make([]string, len(chunk))
		for i, k := range chunk {
			tenants[i], tables[i] = k.tenant, k.table
		}
		s := &stmt{sql: versionUpsertSQL, args: []any{tenants, tables}, op: "insert", table: versionTable}
		stmts = append(stmts, s)
		b.Queue(s.sql, s.args...)
	}
	db := v.db
	events, qctx := db.startEvents(ctx, nil, stmts)
	start := time.Now()
	br := db.pool.SendBatch(qctx, b)
	var err error
	for _, s := range stmts {
		if s.tag, err = br.Exec(); err != nil {
			break
		}
	}
	if closeErr := br.Close(); err == nil {
		err = closeErr
	}
	db.endEvents(events, stmts, time.Since(start), err)
	return err
}

// ensure creates keel_data_versions when it does not exist, under a transaction-scoped advisory
// lock, since concurrent CREATE TABLE IF NOT EXISTS statements can fail. A role without CREATE on
// the schema works once the table exists.
func (v *versioner) ensure(ctx context.Context) error {
	v.readyMu.Lock()
	defer v.readyMu.Unlock()
	if v.ready {
		return nil
	}
	pool := v.db.pool
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('"+versionTable+"') IS NOT NULL").Scan(&exists); err != nil {
		return fmt.Errorf("keel: find %s: %w", versionTable, err)
	}
	if !exists {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('"+versionTable+"', 0))"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, versionTableSQL)
			return err
		})
		if err != nil {
			return fmt.Errorf("keel: create %s: %w", versionTable, err)
		}
	}
	v.ready = true
	return nil
}

// FlushDataVersions writes the pending data versions now instead of at the next interval, for
// tests and before a planned shutdown (Close also flushes). It does nothing when data versions
// are off.
func (db *DB) FlushDataVersions(ctx context.Context) error {
	if db.versions == nil {
		return nil
	}
	return db.versions.flush(ctx)
}

// DataVersions returns the current version of each table for the tenant with key tenantKey (see
// TenantKey), in one query; a table never changed has version 0. A table's version also moves
// with the changes recorded for no tenant ("") and for each shorter prefix of the key, such as a
// table that holds only the first tenant column, so it changes whenever data the tenant can read
// in it changes. Versions are written at most Config.DataVersions.Interval after the write commits.
//
// It reads the primary, joining the transaction on ctx. On a context marked with OnReplica it also
// raises the context's commit position to the primary's current one, in the same round trip, so
// data read from a replica afterwards is at least as new as the versions.
func (db *DB) DataVersions(ctx context.Context, tenantKey string, tables ...string) (map[string]int64, error) {
	if db.versions == nil {
		return nil, errf("data versions are off; set Config.DataVersions.Enabled")
	}
	out := make(map[string]int64, len(tables))
	for _, t := range tables {
		out[t] = 0
	}
	if len(tables) == 0 {
		return out, nil
	}
	if err := db.versions.ensure(ctx); err != nil {
		return nil, err
	}
	run, err := db.runnerFor(ctx, modePrimaryRead)
	if err != nil {
		return nil, err
	}
	s := &stmt{
		sql: `SELECT table_name, sum(version)::int8 FROM keel_data_versions
		 WHERE tenant_key = ANY($1) AND table_name = ANY($2) GROUP BY table_name`,
		args:  []any{tenantPrefixes(tenantKey), tables},
		op:    "select",
		table: versionTable,
	}
	s.scan = func(rows pgx.Rows) error {
		for rows.Next() {
			var name string
			var version int64
			if err := rows.Scan(&name, &version); err != nil {
				return err
			}
			out[name] = version
		}
		return nil
	}
	stmts := []*stmt{s}
	var rt *route
	var lsn int64
	if db.replicas != nil && run.tx == nil {
		if rt = routeFrom(ctx); rt != nil {
			p := &stmt{sql: commitPositionSQL, op: "select"}
			p.scan = func(rows pgx.Rows) error {
				if rows.Next() {
					return rows.Scan(&lsn)
				}
				return nil
			}
			stmts = append(stmts, p)
		}
	}
	if err := run.close(ctx, run.batch(ctx, stmts, true)); err != nil {
		return nil, err
	}
	if rt != nil {
		db.recordCommit(rt, lsn, true)
	}
	return out, nil
}

// MarkChanged records that tables changed, for writes keel cannot see into: Exec, statements sent
// through Pool, triggers. Under a tenant scope the pairs are the tenant's; under a system scope or
// without a scope they are recorded for no tenant (""), which moves the tables' version for every
// tenant. Inside a transaction they are written after it commits. It does nothing when data
// versions are off.
func MarkChanged(ctx context.Context, db *DB, tables ...string) {
	if db.versions == nil || len(tables) == 0 {
		return
	}
	key := db.TenantKey(ctx)
	if tx := txFrom(ctx, db); tx != nil {
		for _, t := range tables {
			tx.touch(versionKey{tenant: key, table: t})
		}
		return
	}
	set := make(versionSet, len(tables))
	for _, t := range tables {
		set[versionKey{tenant: key, table: t}] = struct{}{}
	}
	db.versions.add(set)
}

// TenantKey returns the data-versions tenant key of ctx's scope: TenantKeyOf the tenant values, or
// "" under a system scope or without a scope.
func (db *DB) TenantKey(ctx context.Context) string {
	sc := db.resolveScope(ctx)
	if sc.tenant == nil {
		return ""
	}
	return TenantKeyOf(sc.tenant...)
}

// TenantKeyOf returns the data-versions tenant key of tenant values, given in the order of
// Config.TenantColumns: the values as text joined with "/", with "\" and "/" inside a value
// escaped with "\". Values of named types are keyed as their underlying type.
func TenantKeyOf(values ...any) string {
	var b strings.Builder
	for i, v := range values {
		if i > 0 {
			b.WriteByte('/')
		}
		s := tenantText(normalize(v))
		if !strings.ContainsAny(s, `\/`) {
			b.WriteString(s)
			continue
		}
		for j := 0; j < len(s); j++ {
			if s[j] == '\\' || s[j] == '/' {
				b.WriteByte('\\')
			}
			b.WriteByte(s[j])
		}
	}
	return b.String()
}

// tenantText formats a normalised tenant value for a tenant key.
func tenantText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int16:
		return strconv.FormatInt(int64(x), 10)
	case bool:
		return strconv.FormatBool(x)
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprint(v)
}

// tenantPrefixes returns key and every shorter prefix of it at an unescaped "/", down to "".
func tenantPrefixes(key string) []string {
	out := []string{key}
	if key == "" {
		return out
	}
	for i := len(key) - 1; i > 0; i-- {
		if key[i] != '/' {
			continue
		}
		slashes := 0
		for j := i - 1; j >= 0 && key[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			out = append(out, key[:i])
		}
	}
	return append(out, "")
}

// tenantPrefix returns the indexes in t of the longest prefix of Config.TenantColumns that t has
// as tenant columns: a table's data versions are keyed by those columns' values.
func (s *scopeState) tenantPrefix(t *schema.Table) []int32 {
	var cols []int32
	for _, name := range s.columns {
		found := int32(-1)
		for _, ci := range t.Tenant {
			if t.Columns[ci].Name == name {
				found = ci
				break
			}
		}
		if found < 0 {
			break
		}
		cols = append(cols, found)
	}
	return cols
}

// dataKey returns the tenant key a write to t under the scope changes: the scope tenant's values
// for t's tenant prefix, or "" for a table without tenant columns or under a system scope.
func (s *scopeState) dataKey(t *schema.Table) string {
	if !t.HasTenant() || s.tenant == nil {
		return ""
	}
	return TenantKeyOf(s.tenant[:len(s.tenantPrefix(t))]...)
}

// rowDataKey returns the tenant key of a row of t from its tenant prefix columns, or "" when one of
// them is empty.
func rowDataKey(t *schema.Table, row unsafe.Pointer, cols []int32) string {
	if len(cols) == 0 {
		return ""
	}
	vals := make([]any, len(cols))
	for i, ci := range cols {
		if zeroAt(t, row, ci) {
			return ""
		}
		vals[i] = t.Value(row, ci)
	}
	return TenantKeyOf(vals...)
}

// touchTable records that the runner's write changed t for its scope.
func (r *runner) touchTable(t *schema.Table) {
	if r.db.versions == nil {
		return
	}
	r.touch(versionKey{tenant: r.sc.dataKey(t), table: t.Name})
}

// touchRows records that the runner's write changed rows of t: for the scope's tenant, or, under
// a system scope, for each row's own tenant.
func (r *runner) touchRows(t *schema.Table, rows []unsafe.Pointer) {
	if r.db.versions == nil {
		return
	}
	if !r.sc.system || !t.HasTenant() {
		r.touchTable(t)
		return
	}
	cols := r.sc.tenantPrefix(t)
	prev := "\x00"
	for _, row := range rows {
		k := rowDataKey(t, row, cols)
		if k != prev {
			r.touch(versionKey{tenant: k, table: t.Name})
			prev = k
		}
	}
}

// touchRow is touchRows for one row.
func (r *runner) touchRow(t *schema.Table, row unsafe.Pointer) {
	if r.db.versions == nil {
		return
	}
	if !r.sc.system || !t.HasTenant() {
		r.touchTable(t)
		return
	}
	r.touch(versionKey{tenant: rowDataKey(t, row, r.sc.tenantPrefix(t)), table: t.Name})
}

// tenantsReturned reports whether a statement writing t under the runner's scope must return the
// tenants it wrote: a system-scope bulk write to a tenant table, when data versions are on.
func (r *runner) tenantsReturned(t *schema.Table) bool {
	return r.db.versions != nil && r.sc.system && t.HasTenant()
}

// returnTenants wraps a bulk write so that it returns, instead of rows, the distinct tenants it
// wrote with a row count each, and records them; *n receives the rows written. returning is the
// write's RETURNING target for a column, such as alias.col.
func (r *runner) returnTenants(s *stmt, t *schema.Table, returning func(col string) string, n *int64) {
	cols := r.sc.tenantPrefix(t)
	var b strings.Builder
	b.WriteString("WITH w AS (")
	b.WriteString(s.sql)
	b.WriteString(" RETURNING ")
	for i, ci := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(returning(t.Columns[ci].Quoted))
	}
	b.WriteString(") SELECT ")
	for _, ci := range cols {
		b.WriteString(t.Columns[ci].Quoted)
		b.WriteString(", ")
	}
	b.WriteString("count(*) FROM w")
	if len(cols) > 0 {
		b.WriteString(" GROUP BY ")
		for i, ci := range cols {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(t.Columns[ci].Quoted)
		}
	}
	s.sql = b.String()
	s.scan = func(rows pgx.Rows) error {
		row := t.New()
		dst := make([]any, len(cols)+1)
		for i, ci := range cols {
			dst[i] = t.Ptr(row, ci)
		}
		var count int64
		dst[len(cols)] = &count
		for rows.Next() {
			if err := rows.Scan(dst...); err != nil {
				return err
			}
			*n += count
			r.touch(versionKey{tenant: rowDataKey(t, row, cols), table: t.Name})
		}
		return nil
	}
}
