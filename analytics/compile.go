package analytics

import (
	"context"
	"slices"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
	"github.com/emoss08/keel/schema"
)

// Strategy chooses how measures over a to-many relation read it.
type Strategy uint8

// The strategies.
const (
	// StrategyAuto pre-aggregates in a lateral subquery per parent when the report's filters pin
	// the entity's key to at most CompileOptions.LateralParents values, and in a CTE otherwise;
	// a Runner then picks, by the planner's estimates, between restricting the CTE to the parents
	// the filters select (StrategyCTE) and aggregating every parent (StrategyCTEAll).
	StrategyAuto Strategy = iota
	// StrategyCTE pre-aggregates the related rows in one grouped pass, keyed by the parent and
	// restricted to the parents the report's filters select: best when the filters keep few.
	StrategyCTE
	// StrategyCTEAll pre-aggregates the related rows of every parent in one grouped pass, without
	// the semi-join that restricts them: best when the filters keep most parents.
	StrategyCTEAll
	// StrategyLateral pre-aggregates each parent's related rows in a lateral subquery.
	StrategyLateral
)

// defaultLateralParents is the most parents StrategyAuto reads with lateral subqueries.
const defaultLateralParents = 100

// CompileOptions configures Compile.
type CompileOptions struct {
	// Location is the report's time zone: buckets are periods in it and relative date filters
	// (today, this month) are its days. Nil is UTC. It must be a named zone Postgres knows.
	Location *time.Location
	// Now is the instant relative date filters count from; zero is time.Now.
	Now time.Time
	// Limits bound the definition; zero fields take their defaults.
	Limits ir.Limits
	// MaxRows caps the rows a statement returns, the definition's limit included; zero leaves only
	// the definition's limit.
	MaxRows int
	// Probe asks for one row past the limit, so a reader can report that the result was cut.
	Probe bool
	// Unlimited leaves the limit out, for a count of the report's rows or a page.
	Unlimited bool
	// Strategy chooses how to-many measures are pre-aggregated.
	Strategy Strategy
	// LateralParents is the most parents StrategyAuto reads with lateral subqueries (default 100).
	LateralParents int
	// Rollups answers a report from a rollup table when one covers it.
	Rollups RollupRouter
	// PivotValues are a dynamic pivot's discovered values; the runner sets them after running the
	// discovery statement.
	PivotValues []string
}

// Compiled is a report compiled to statements: the main query (nil until a dynamic pivot's values
// are known), the discovery query of a dynamic pivot, the output schema and what the guards and
// the cache need to know.
type Compiled struct {
	// Definition is the definition as given.
	Definition *ir.Definition
	// Resolved is the definition validated against the catalog.
	Resolved *ir.Resolved
	// Columns is the output schema, pivot cells expanded; for a dynamic pivot it is known once the
	// values are.
	Columns []table.Column
	// Query is the report's statement, or nil while a dynamic pivot's values are pending.
	Query *keel.DynQuery
	// Discovery is the statement that finds a dynamic pivot's values, or nil.
	Discovery *keel.DynQuery
	// Tables are the tables the statements read, for data versions.
	Tables []string
	// Complexity counts what the statements do, for the complexity guard.
	Complexity Complexity
	// Rollup names the rollup table that answers the report, or "" for the base tables.
	Rollup string
	// Limit is the row limit applied, without the probe row; zero for none.
	Limit int
	// Paged reports that the definition asks for a keyset page instead of a limit.
	Paged bool
	// Params are the bound parameter values.
	Params map[string]any

	dec       *decisions
	levels    map[int64]table.Level
	slots     []int
	level     int
	opts      CompileOptions
	cat       *Catalog
	choosable bool
}

// Complexity counts what a report's statements do.
type Complexity struct {
	// Joins counts the to-one relation paths joined, picks included.
	Joins int `json:"joins"`
	// ToMany counts the to-many relation paths read: pre-aggregations, EXISTS filters and
	// fan-out distinct counts.
	ToMany int `json:"toMany"`
	// PreAggs counts the pre-aggregations.
	PreAggs int `json:"preAggs"`
	// Windows counts the window columns.
	Windows int `json:"windows"`
	// Dimensions counts the grouping columns.
	Dimensions int `json:"dimensions"`
	// PivotColumns counts the columns a pivot produces.
	PivotColumns int `json:"pivotColumns"`
	// Statements counts the statements a run sends.
	Statements int `json:"statements"`
}

// Score weighs the counts into one number: to-many paths and pre-aggregations cost more than
// joins and windows.
func (c Complexity) Score() int {
	return c.Joins + 3*c.ToMany + 2*c.PreAggs + 2*c.Windows + c.Dimensions + c.PivotColumns/4 + 2*(c.Statements-1)
}

// Compile validates a definition against the catalog and the policy (nil allows everything),
// binds its parameters and compiles it to tenant-scoped statements over the dynamic query layer:
// every table reference carries the scope's tenant predicate and the policy's row rules, and every
// value is a bound parameter. The error is an *Error: CodeInvalid with the definition errors,
// CodeDenied with the policy's reason, CodeParameter or CodeNotAllowed.
func Compile(ctx context.Context, cat *Catalog, policy Policy, def *ir.Definition, params map[string]any,
	opts CompileOptions,
) (*Compiled, error) {
	res, err := ir.Resolve(def, cat, opts.Limits)
	if err != nil {
		return nil, invalid(err)
	}
	dec := newDecisions(cat, policy)
	if problems := accessProblems(ctx, cat, dec, res); len(problems) > 0 {
		return nil, problems[0]
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	bound, err := bindParams(def, params, opts.Location)
	if err != nil {
		return nil, err
	}
	return compileWith(ctx, cat, dec, res, bound, opts)
}

// compileWith compiles a resolved definition: from a rollup when the router covers it, else from
// the base tables.
func compileWith(ctx context.Context, cat *Catalog, dec *decisions, res *ir.Resolved, params map[string]any,
	opts CompileOptions,
) (*Compiled, error) {
	out, err := tryRollup(ctx, cat, dec, res, params, opts)
	if err != nil || out != nil {
		return out, err
	}
	c, err := newCompiler(ctx, cat, dec, res, params, opts)
	if err != nil {
		return nil, err
	}
	return c.compile()
}

// WithPivotValues compiles the report statement of a dynamic pivot for the values its discovery
// statement found. A report without a dynamic pivot is returned as it is.
func (c *Compiled) WithPivotValues(ctx context.Context, values []string) (*Compiled, error) {
	if c.Discovery == nil {
		return c, nil
	}
	opts := c.opts
	opts.PivotValues = slices.Clone(values)
	if opts.PivotValues == nil {
		opts.PivotValues = []string{}
	}
	out, err := compileWith(ctx, c.cat, c.dec, c.Resolved, c.Params, opts)
	if err != nil {
		return nil, err
	}
	out.Discovery = c.Discovery
	return out, nil
}

// withStrategy compiles the report again with another pre-aggregation strategy.
func (c *Compiled) withStrategy(ctx context.Context, s Strategy) (*Compiled, error) {
	opts := c.opts
	opts.Strategy = s
	out, err := compileWith(ctx, c.cat, c.dec, c.Resolved, c.Params, opts)
	if err != nil {
		return nil, err
	}
	out.Discovery = c.Discovery
	return out, nil
}

// Context returns ctx with the policy's row rules, for running or rendering the statements
// outside a Runner.
func (c *Compiled) Context(ctx context.Context) context.Context {
	return keel.WithReadFilter(ctx, c.dec.filter())
}

// ToSQL renders the report's statement as it runs under ctx's scope, row rules included, for
// debugging; while a dynamic pivot's values are pending it renders the discovery statement.
func (c *Compiled) ToSQL(ctx context.Context, db *keel.DB) (string, []any, error) {
	q := c.Query
	if q == nil {
		q = c.Discovery
	}
	return q.ToSQL(c.Context(ctx), db)
}

// Level returns the grouping level of a row from the value of the hidden level column.
func (c *Compiled) levelOf(mask int64) table.Level {
	if l, ok := c.levels[mask]; ok {
		return l
	}
	return table.Detail
}

// compiler is one compilation: the resolved definition, its bound parameters, and the plan of
// every column.
type compiler struct {
	ctx    context.Context
	cat    *Catalog
	dec    *decisions
	res    *ir.Resolved
	def    *ir.Definition
	base   *Entity
	params map[string]any
	opts   CompileOptions
	loc    *time.Location
	now    time.Time
	tz     keel.DynExpr

	cols    []*colPlan
	byID    map[string]*colPlan
	grouped []*colPlan
	sets    [][]int
	pivot   *pivotPlan
	tables  map[*schema.Table]bool
	cx      Complexity
	rollup  *RollupPlan

	hoistLabels map[string]string
	hoistList   []hoist
	joinKeys    map[string]bool
	manyKeys    map[string]bool
	preaggKeys  map[string]bool
}

// colPlan is how one column of the definition is computed.
type colPlan struct {
	def   *ir.Column
	out   ir.Output
	vtype value.Type
	// outer is set for a column computed over the grouped result: windows, fan-out distinct
	// counts and calculations over them.
	outer bool
	// fanout is set for a distinct count across a to-many path that pre-aggregation cannot
	// answer exactly.
	fanout  bool
	pivoted bool
	sens    table.Sensitivity
}

// newCompiler plans a compilation.
func newCompiler(ctx context.Context, cat *Catalog, dec *decisions, res *ir.Resolved, params map[string]any,
	opts CompileOptions,
) (*compiler, error) {
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.LateralParents <= 0 {
		opts.LateralParents = defaultLateralParents
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	c := &compiler{ctx: ctx, cat: cat, dec: dec, res: res, def: res.Definition, params: params, opts: opts,
		loc: opts.Location, now: now, byID: map[string]*colPlan{}, hoistLabels: map[string]string{},
		tables: map[*schema.Table]bool{}, joinKeys: map[string]bool{}, manyKeys: map[string]bool{},
		preaggKeys: map[string]bool{}}
	c.base = cat.byKey[c.def.Entity]
	c.tz = keel.DynLit(opts.Location.String()).Cast(keel.TypeString)
	c.tables[c.base.Table] = true
	if err := c.planColumns(); err != nil {
		return nil, err
	}
	if err := c.checkDisplays(); err != nil {
		return nil, err
	}
	if err := c.planPivot(); err != nil {
		return nil, err
	}
	c.planGroupings()
	return c, nil
}

// compile builds the statements and the output schema.
func (c *compiler) compile() (*Compiled, error) {
	out := &Compiled{Definition: c.def, Resolved: c.res, Params: c.params, dec: c.dec, opts: c.opts,
		cat: c.cat, Paged: c.def.Page != nil}
	if c.pivot != nil && c.pivot.dynamic() {
		d, err := c.discovery()
		if err != nil {
			return nil, err
		}
		out.Discovery = d
		c.cx.Statements++
		if c.opts.PivotValues == nil {
			out.Complexity = c.complexity()
			out.Tables = c.tableNames()
			return out, nil
		}
	}
	q, err := c.build(out)
	if err != nil {
		return nil, err
	}
	out.Query = q
	out.choosable = c.opts.Strategy == StrategyAuto && c.strategy() == keel.PreAggCTE &&
		len(c.preaggKeys) > 0 && !c.def.Filters.Empty()
	out.Columns = c.outputColumns()
	out.levels = c.levelMasks()
	out.slots, out.level = c.slots()
	c.cx.Statements++
	out.Complexity = c.complexity()
	out.Tables = c.tableNames()
	if c.rollup != nil {
		out.Rollup = c.rollup.Name
	}
	return out, nil
}

// tableNames returns the names of the tables the statements read, sorted.
func (c *compiler) tableNames() []string {
	out := make([]string, 0, len(c.tables))
	for t := range c.tables {
		out = append(out, t.Name)
	}
	slices.Sort(out)
	return out
}

// complexity returns the counts of the statements built.
func (c *compiler) complexity() Complexity {
	cx := c.cx
	cx.Joins, cx.ToMany, cx.PreAggs = len(c.joinKeys), len(c.manyKeys), len(c.preaggKeys)
	for _, col := range c.cols {
		if col.def.Window != nil {
			cx.Windows++
		}
	}
	cx.Dimensions = len(c.grouped)
	if c.pivot != nil {
		cx.PivotColumns = len(c.pivot.cells()) * len(c.def.Pivot.Columns)
	}
	return cx
}
