package analytics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/analytics/table"
)

// Cache stores run results under a key for a time. Implementations live in analytics/cache (an
// in-memory LRU, a Postgres unlogged table, Redis); any key-value store with expiry fits. Get
// reports a miss with false; errors are reported and treated as a miss by the runner. Values are
// opaque bytes the runner encodes.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// CacheKey is everything a cached result depends on: the statement and its arguments (the
// tenant's values and the policy's row rules are rendered into them), the entry point and the
// output format, the output schema (labels are translated per request), and the data version of
// every table the statement reads for the request's tenant. A write to any of those tables for the
// tenant moves its version, so the next run misses. Writes keel cannot see (manual SQL, other
// services) do not move versions unless the application calls keel.MarkChanged; the TTL is the
// backstop for those.
type CacheKey struct {
	// Entry is the entry point ("preview", "run", "count", "page").
	Entry string
	// Format is the output format the caller asked for, for callers that cache different
	// renderings of the same rows.
	Format string
	// SQL and Args are the statement as it runs.
	SQL  string
	Args []any
	// Schema is the output schema.
	Schema []table.Column
	// Tenant is the request's data-versions tenant key.
	Tenant string
	// Versions are the data versions of the tables the statement reads, by table.
	Versions map[string]int64
	// Extra holds what else the entry point depends on: a page cursor, a limit.
	Extra string
}

// String returns the key's hash, the string a Cache stores the result under.
func (k CacheKey) String() string {
	h := sha256.New()
	fmt.Fprintf(h, "keel-analytics/1\x00%s\x00%s\x00%s\x00%s\x00", k.Entry, k.Format, k.Tenant, k.Extra)
	h.Write([]byte(k.SQL))
	for _, a := range k.Args {
		fmt.Fprintf(h, "\x00%T:%v", a, a)
	}
	schema, _ := json.Marshal(k.Schema)
	h.Write(schema)
	tables := make([]string, 0, len(k.Versions))
	for t := range k.Versions {
		tables = append(tables, t)
	}
	slices.Sort(tables)
	for _, t := range tables {
		fmt.Fprintf(h, "\x00%s=%d", t, k.Versions[t])
	}
	return "keel-analytics:" + hex.EncodeToString(h.Sum(nil))
}

// cacheKey builds the cache key of a compiled statement for the request's tenant.
func (r *Runner) cacheKey(ctx context.Context, comp *Compiled, q *keel.DynQuery, entry, format, extra string) (string, error) {
	sql, args, err := q.ToSQL(comp.Context(ctx), r.db)
	if err != nil {
		return "", err
	}
	tenant := r.db.TenantKey(ctx)
	versions, err := r.db.DataVersions(ctx, tenant, comp.Tables...)
	if err != nil {
		return "", err
	}
	return CacheKey{Entry: entry, Format: format, SQL: sql, Args: args, Schema: comp.Columns, Tenant: tenant,
		Versions: versions, Extra: extra}.String(), nil
}

// cachedResult is a cache entry: the result as a rows envelope with its schema, its page cursor
// and its count.
type cachedResult struct {
	// Columns is the output schema.
	Columns []table.Column `json:"columns,omitempty"`
	// Rows is the rows envelope.
	Rows json.RawMessage `json:"rows,omitempty"`
	// Cursor is the next page's cursor.
	Cursor string `json:"cursor,omitempty"`
	// Count is a count's result.
	Count int64 `json:"count,omitempty"`
	// Truncated and Rollup are the result's truncation and rollup.
	Truncated bool   `json:"truncated,omitempty"`
	Rollup    string `json:"rollup,omitempty"`
}

// encodeResult encodes a result for the cache.
func encodeResult(ctx context.Context, res *Result) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := export.JSON(ctx, &buf, res.Reader(), export.Options{}); err != nil {
		return nil, err
	}
	return json.Marshal(cachedResult{Columns: res.columns, Rows: buf.Bytes(), Cursor: res.cursor,
		Truncated: res.truncated, Rollup: res.rollup})
}

// decodeResult reads a cached result back: the envelope keeps the values, the stored schema the
// displays and sensitivities.
func decodeResult(b []byte) (*Result, error) {
	var c cachedResult
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	env, err := export.ReadJSON(bytes.NewReader(c.Rows))
	if err != nil {
		return nil, err
	}
	if len(env.Table.Columns) != len(c.Columns) {
		return nil, fmt.Errorf("analytics: cached result has %d columns and a schema of %d", len(env.Table.Columns), len(c.Columns))
	}
	return &Result{columns: c.Columns, batches: env.Table.Batches, truncated: c.Truncated, cursor: c.Cursor,
		rollup: c.Rollup, cached: true}, nil
}

// cacheKeyOf returns the cache key of a compiled report, or "" without a cache or when the key
// cannot be built (the error goes to OnError).
func (r *Runner) cacheKeyOf(ctx context.Context, comp *Compiled, entry, format, extra string) string {
	if r.opts.Cache == nil {
		return ""
	}
	q := comp.Query
	if q == nil {
		q = comp.Discovery
	}
	key, err := r.cacheKey(ctx, comp, q, entry, format, extra)
	if err != nil {
		r.report(err)
		return ""
	}
	return key
}

// cacheGet reads a cache entry; an error is reported and read as a miss.
func (r *Runner) cacheGet(ctx context.Context, key string) ([]byte, bool) {
	if key == "" {
		return nil, false
	}
	b, ok, err := r.opts.Cache.Get(ctx, key)
	if err != nil {
		r.report(err)
		return nil, false
	}
	return b, ok
}

// cacheSet stores a cache entry; an error is reported.
func (r *Runner) cacheSet(ctx context.Context, key string, value []byte) {
	if key == "" {
		return
	}
	if err := r.opts.Cache.Set(context.WithoutCancel(ctx), key, value, r.opts.CacheTTL); err != nil {
		r.report(err)
	}
}

// cachedResult returns the cached result under key, or nil.
func (r *Runner) cachedResult(ctx context.Context, key string) *Result {
	b, ok := r.cacheGet(ctx, key)
	if !ok {
		return nil
	}
	res, err := decodeResult(b)
	if err != nil {
		r.report(err)
		return nil
	}
	return res
}

// storeResult caches a result under key.
func (r *Runner) storeResult(ctx context.Context, key string, res *Result) {
	if key == "" {
		return
	}
	b, err := encodeResult(ctx, res)
	if err != nil {
		r.report(err)
		return
	}
	r.cacheSet(ctx, key, b)
}

// report hands an error the runner does not return to OnError.
func (r *Runner) report(err error) {
	if r.opts.OnError != nil {
		r.opts.OnError(err)
	}
}

// encodeCount encodes a count for the cache.
func encodeCount(n int64) []byte { return []byte(`{"count":` + strconv.FormatInt(n, 10) + `}`) }

// decodeCount reads a cached count back.
func decodeCount(b []byte) (int64, error) {
	var c cachedResult
	err := json.Unmarshal(b, &c)
	return c.Count, err
}
