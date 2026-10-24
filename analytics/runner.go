package analytics

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/table"
)

// RunnerOptions configures a Runner. Zero fields take the defaults given.
type RunnerOptions struct {
	// Limits bound definitions.
	Limits ir.Limits
	// Complexity bounds what statements may do.
	Complexity ComplexityLimits
	// MaxCost refuses a statement whose EXPLAIN total cost exceeds it; zero skips the check.
	MaxCost float64
	// MaxPlanRows refuses a statement EXPLAIN expects to return more rows; zero skips the check.
	MaxPlanRows float64
	// ExplainTTL is how long a statement shape's estimates are reused for a tenant (default 30s).
	ExplainTTL time.Duration
	// PreviewRows is the rows a preview returns (default 100).
	PreviewRows int
	// MaxRows caps the rows Run streams (default 1,000,000).
	MaxRows int
	// MaxBytes caps the bytes of values a result holds; a result past it is cut and reported
	// truncated. Zero is no cap.
	MaxBytes int64
	// Slots is how many statements a tenant runs at once across every process, enforced with
	// Postgres advisory locks; zero is no limit.
	Slots int
	// SlotWait is how long a run waits for a free slot before it is refused; zero refuses at once.
	SlotWait time.Duration
	// Timeouts bound each entry point's statements.
	Timeouts Timeouts
	// Primary reads the primary instead of a replica.
	Primary bool
	// Cache stores results; nil runs every request. It needs data versions on the DB.
	Cache Cache
	// CacheTTL is how long a cached result lives (default 15m): the backstop for writes keel
	// cannot see.
	CacheTTL time.Duration
	// MaxCachedRows is the most rows of a streamed run that is cached (default 50,000).
	MaxCachedRows int
	// StreamBatch is the rows of each batch Run streams (default 1,024).
	StreamBatch int
	// Strategy chooses how to-many measures are pre-aggregated.
	Strategy Strategy
	// Rollups answers reports from rollup tables.
	Rollups RollupRouter
	// OnError receives the errors the runner does not return: a cache that fails to read or
	// store.
	OnError func(err error)
}

// Timeouts bound the statements of each entry point. Zero fields take the defaults.
type Timeouts struct {
	// Preview bounds Preview and Drill (default 30s).
	Preview time.Duration
	// Run bounds Run (default 10m).
	Run time.Duration
	// Count bounds Count (default 30s).
	Count time.Duration
	// Page bounds Page (default 30s).
	Page time.Duration
}

// withDefaults fills the zero options.
func (o RunnerOptions) withDefaults() RunnerOptions {
	pick := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	pick(&o.ExplainTTL, 30*time.Second)
	pick(&o.CacheTTL, 15*time.Minute)
	pick(&o.Timeouts.Preview, 30*time.Second)
	pick(&o.Timeouts.Run, 10*time.Minute)
	pick(&o.Timeouts.Count, 30*time.Second)
	pick(&o.Timeouts.Page, 30*time.Second)
	if o.PreviewRows <= 0 {
		o.PreviewRows = 100
	}
	if o.MaxRows <= 0 {
		o.MaxRows = 1_000_000
	}
	if o.MaxCachedRows <= 0 {
		o.MaxCachedRows = 50_000
	}
	if o.StreamBatch <= 0 {
		o.StreamBatch = 1024
	}
	return o
}

// Runner runs reports. Every entry point (Preview, Run, Count, Page, Drill) goes through the same
// guards: definition and complexity limits, EXPLAIN cost and rows, row and byte caps pushed into
// the statement, a per-tenant concurrency limit shared by every process, supersede by key across
// processes, the entry point's statement timeout, a read-only transaction on a replica, and the
// cache. A Runner is safe for concurrent use.
type Runner struct {
	db      *keel.DB
	cat     *Catalog
	opts    RunnerOptions
	plans   *planCache
	slotSQL string

	mu      sync.Mutex
	running map[string]*running
}

// Request is one report run: the definition, its parameter values, the caller's policy and time
// zone, and a supersede key.
type Request struct {
	// Definition is the report.
	Definition *ir.Definition
	// Params are the parameter values.
	Params map[string]any
	// Policy is the caller's policy; nil allows everything.
	Policy Policy
	// Location is the report's time zone; nil is UTC.
	Location *time.Location
	// Now is the instant relative date filters count from; zero is the time of the run.
	Now time.Time
	// Key supersedes: a run with the same key and tenant cancels the previous one, in any process
	// (a user's preview, a dashboard tile). Empty never supersedes.
	Key string
	// Format names the rendering the caller makes of the rows, for its cache entries.
	Format string
}

// NewRunner returns a runner over db for the catalog. A runner with a Cache needs data versions
// (Config.DataVersions.Enabled), which the cache keys on.
func NewRunner(db *keel.DB, cat *Catalog, opts RunnerOptions) (*Runner, error) {
	if db == nil || cat == nil {
		return nil, fail(CodeCatalog, "", "NewRunner needs a database and a catalog")
	}
	opts = opts.withDefaults()
	if opts.Cache != nil {
		if _, err := db.DataVersions(context.Background(), ""); err != nil {
			return nil, &Error{Code: CodeCatalog, Message: "a result cache needs data versions", Err: err}
		}
	}
	r := &Runner{db: db, cat: cat, opts: opts, running: map[string]*running{},
		plans: &planCache{ttl: opts.ExplainTTL, entries: map[string]planEntry{}}}
	if opts.Slots > 0 {
		r.slotSQL = slotSQL(opts.Slots)
	}
	return r, nil
}

// Compile compiles a request with the runner's options, for ToSQL and tools; the entry points
// compile for themselves.
func (r *Runner) Compile(ctx context.Context, req Request) (*Compiled, error) {
	return r.compile(ctx, req, CompileOptions{MaxRows: r.opts.MaxRows})
}

// compile compiles a request and checks its complexity.
func (r *Runner) compile(ctx context.Context, req Request, co CompileOptions) (*Compiled, error) {
	co.Location, co.Now, co.Limits = req.Location, req.Now, r.opts.Limits
	co.Strategy, co.Rollups = r.opts.Strategy, r.opts.Rollups
	comp, err := Compile(ctx, r.cat, req.Policy, req.Definition, req.Params, co)
	if err != nil {
		return nil, err
	}
	if err := checkComplexity(comp.Complexity, r.opts.Complexity); err != nil {
		return nil, err
	}
	return comp, nil
}

// Preview returns the first rows of a report, at most PreviewRows (or the definition's limit if
// lower), with one more asked of the database to report truncation.
func (r *Runner) Preview(ctx context.Context, req Request) (*Result, error) {
	return r.materialize(ctx, req, "preview", r.opts.Timeouts.Preview, r.opts.PreviewRows)
}

// Drill returns the record list behind one cell of a report's result (see Drill), previewed with
// the request's policy and parameters.
func (r *Runner) Drill(ctx context.Context, req Request, row []ir.DrillValue, column string, opts DrillOptions) (*Result, error) {
	if opts.Policy == nil {
		opts.Policy = req.Policy
	}
	if opts.Location == nil {
		opts.Location = req.Location
	}
	if opts.Params == nil {
		opts.Params = req.Params
	}
	def, err := Drill(ctx, r.cat, req.Definition, row, column, opts)
	if err != nil {
		return nil, err
	}
	req.Definition = def
	return r.materialize(ctx, req, "drill", r.opts.Timeouts.Preview, r.opts.PreviewRows)
}

// materialize runs a report to memory with a row cap and the probe row.
func (r *Runner) materialize(ctx context.Context, req Request, entry string, timeout time.Duration, rows int) (*Result, error) {
	comp, err := r.compile(ctx, req, CompileOptions{MaxRows: rows, Probe: true})
	if err != nil {
		return nil, err
	}
	key := r.cacheKeyOf(ctx, comp, entry, req.Format, strconv.Itoa(rows))
	if res := r.cachedResult(ctx, key); res != nil {
		return res, nil
	}
	var out *Result
	err = r.execute(ctx, req, comp, timeout, func(ctx context.Context, comp *Compiled) error {
		res, err := comp.Query.Run(ctx, r.db)
		if err != nil {
			return err
		}
		out, err = r.resultOf(comp, res)
		return err
	})
	if err != nil {
		return nil, err
	}
	r.storeResult(ctx, key, out)
	return out, nil
}

// resultOf turns a decoded statement result into a Result, cut at the limit and the byte cap.
func (r *Runner) resultOf(comp *Compiled, res *keel.DynResult) (*Result, error) {
	b, truncated, err := comp.batchOf(res, comp.Limit)
	if err != nil {
		return nil, err
	}
	if r.opts.MaxBytes > 0 {
		for b.Len > 0 && batchBytes(b) > r.opts.MaxBytes {
			b.Len /= 2
			truncated = true
		}
	}
	return &Result{columns: comp.Columns, batches: []*table.Batch{b}, truncated: truncated,
		cursor: res.Next, rollup: comp.Rollup}, nil
}

// Count returns the number of rows the report returns without a limit, subtotal and total rows
// included.
func (r *Runner) Count(ctx context.Context, req Request) (int64, error) {
	comp, err := r.compile(ctx, req, CompileOptions{Unlimited: true})
	if err != nil {
		return 0, err
	}
	key := r.cacheKeyOf(ctx, comp, "count", req.Format, "")
	if b, ok := r.cacheGet(ctx, key); ok {
		if n, err := decodeCount(b); err == nil {
			return n, nil
		}
	}
	var n int64
	err = r.execute(ctx, req, comp, r.opts.Timeouts.Count, func(ctx context.Context, comp *Compiled) error {
		var err error
		n, err = comp.Query.Count(ctx, r.db)
		return err
	})
	if err != nil {
		return 0, err
	}
	r.cacheSet(ctx, key, encodeCount(n))
	return n, nil
}

// Page returns the page of size rows after cursor (empty for the first page) in the report's
// order; the result's Cursor reads the next page. The order ends with the grouping columns (the
// record key for a record list), so every row has one place.
func (r *Runner) Page(ctx context.Context, req Request, cursor string, size int) (*Result, error) {
	def := *req.Definition
	def.Page = &ir.Page{Size: size, After: cursor}
	def.Limit = ir.Count{}
	req.Definition = &def
	comp, err := r.compile(ctx, req, CompileOptions{})
	if err != nil {
		return nil, err
	}
	key := r.cacheKeyOf(ctx, comp, "page", req.Format, cursor+"\x00"+strconv.Itoa(size))
	if res := r.cachedResult(ctx, key); res != nil {
		return res, nil
	}
	var out *Result
	err = r.execute(ctx, req, comp, r.opts.Timeouts.Page, func(ctx context.Context, comp *Compiled) error {
		res, err := comp.Query.Page(ctx, r.db, keel.After(cursor, size))
		if err != nil {
			return err
		}
		out, err = r.resultOf(comp, res)
		return err
	})
	if err != nil {
		return nil, err
	}
	r.storeResult(ctx, key, out)
	return out, nil
}

// execute runs fn in a read-only transaction with the run's guards, on a replica unless the
// runner reads the primary: the statement timeout and supersede tag set for the transaction, the
// previous run with the key cancelled, a dynamic pivot's values discovered, every statement's
// cost checked and a tenant slot taken before the statements run. A dynamic pivot runs in
// REPEATABLE READ so both its statements read one snapshot.
func (r *Runner) execute(ctx context.Context, req Request, comp *Compiled, timeout time.Duration,
	fn func(ctx context.Context, comp *Compiled) error,
) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	tag := ""
	if req.Key != "" {
		tag = supersedeTag(r.db.TenantKey(ctx), req.Key)
		var release func()
		ctx, release = r.supersede(ctx, tag)
		defer release()
	}
	run := comp.Context(ctx)
	var opts []keel.TxOption
	if !r.opts.Primary {
		run = keel.OnReplica(run)
		opts = append(opts, keel.ReplicaTx())
	}
	if comp.Discovery != nil {
		opts = append(opts, keel.RepeatableRead())
	}
	err := keel.Read(run, r.db, func(ctx context.Context) error {
		if err := r.prepare(ctx, timeout, tag); err != nil {
			return err
		}
		comp, err := r.discover(ctx, comp)
		if err != nil {
			return err
		}
		if comp, err = r.choosePreAgg(ctx, comp); err != nil {
			return err
		}
		if err := r.checkCost(ctx, comp.Query); err != nil {
			return err
		}
		if comp.Discovery == nil {
			if err := r.acquireSlot(ctx); err != nil {
				return err
			}
		}
		return fn(ctx, comp)
	}, opts...)
	return r.mapErr(ctx, err)
}

// prepare sets the transaction's statement timeout and supersede tag in one statement.
func (r *Runner) prepare(ctx context.Context, timeout time.Duration, tag string) error {
	ms := strconv.FormatInt(max(timeout.Milliseconds(), 1), 10) + "ms"
	if tag == "" {
		_, err := keel.Exec(ctx, r.db, "SELECT set_config('statement_timeout', $1, true)", ms)
		return err
	}
	_, err := keel.Exec(ctx, r.db,
		"SELECT set_config('statement_timeout', $1, true), set_config('application_name', $2, true)", ms, tag)
	return err
}

// discover runs a dynamic pivot's discovery statement, after its cost check and with the
// tenant's slot taken, and compiles the report statement for the values it finds.
func (r *Runner) discover(ctx context.Context, comp *Compiled) (*Compiled, error) {
	if comp.Discovery == nil {
		return comp, nil
	}
	if err := r.checkCost(ctx, comp.Discovery); err != nil {
		return nil, err
	}
	if err := r.acquireSlot(ctx); err != nil {
		return nil, err
	}
	if comp.Query != nil {
		return comp, nil
	}
	res, err := comp.Discovery.Run(ctx, r.db)
	if err != nil {
		return nil, err
	}
	return comp.WithPivotValues(ctx, pivotValues(res))
}

// mapErr turns the errors of a cancelled or timed-out run into analytics errors.
func (r *Runner) mapErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return err
	}
	if errors.Is(context.Cause(ctx), errSuperseded) {
		return &Error{Code: CodeSuperseded, Message: "a later run with the same key replaced this one", Err: err}
	}
	msg := err.Error()
	switch {
	case keel.Code(err) == "57014" && strings.Contains(msg, "statement timeout"),
		errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Message: "the report took too long to run", Err: err}
	case keel.Code(err) == "57014":
		return &Error{Code: CodeSuperseded, Message: "a later run with the same key replaced this one", Err: err}
	case strings.Contains(msg, "cursor"):
		return &Error{Code: CodeCursor, Message: "the cursor is not one this report produced", Err: err}
	}
	return err
}
