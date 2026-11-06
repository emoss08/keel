package keel

import (
	"context"
	"fmt"
	"reflect"
	"time"
	"unsafe"
)

// Loader is a relation path to load, optionally narrowed: a relation handle (s.Customer()) or a
// query started from one (m.Order(m.Sequence.Asc())).
type Loader interface {
	loadSpec() *loadSpec
}

type loadSpec struct {
	ref *Ref
	q   *query
}

// LateralSource is what LeftJoinLateral and JoinLateral join: a subquery with a table reference,
// such as a query or keel.Rows. It is an alias, so generated code can name it.
type LateralSource = interface {
	subquery
	TableRef() *Ref
}

// Query is a query over model T, the model's pointer type: Query[*fleet.Shipment]. Builder methods
// change the query and return it; terminals run it. keel gen emits one query type per model that
// implements it by embedding QueryCore, so a Query[T] holds a pointer and copying it shares the
// query: use Clone for an independent copy.
//
// Query is an interface rather than a generic struct so that a program pays for keel's query
// engine once: its methods live on QueryCore, which is not generic, and each generated query type
// adds only thin typed wrappers, compiled once in its own package. See the guide's "Why queries
// are interfaces".
type Query[T any] interface {
	Loader
	Projection
	subquery

	// Where adds predicates, joined by AND. Nil predicates are skipped.
	Where(ps ...Pred) Query[T]
	// Pick selects only es; see QueryCore.Pick.
	Pick(es ...Expr) Query[T]
	// Omit leaves cols out of the columns the query reads; see QueryCore.Omit.
	Omit(cols ...Column) Query[T]
	// Load loads relations into the rows.
	Load(ls ...Loader) Query[T]
	// Order sets the sort order.
	Order(os ...Order) Query[T]
	// Distinct returns each distinct row once: SELECT DISTINCT.
	Distinct() Query[T]
	// Group adds GROUP BY expressions.
	Group(es ...Expr) Query[T]
	// GroupingSets groups by each of sets in turn; see QueryCore.GroupingSets.
	GroupingSets(sets ...[]Expr) Query[T]
	// Rollup groups by es and by each prefix of es down to the grand total.
	Rollup(es ...Expr) Query[T]
	// Cube groups by every subset of es.
	Cube(es ...Expr) Query[T]
	// Window declares a named window; see QueryCore.Window.
	Window(name string, w Window) Query[T]
	// Having adds HAVING predicates.
	Having(ps ...Pred) Query[T]
	// Limit caps the number of rows.
	Limit(n int) Query[T]
	// Offset skips n rows. Prefer Page for anything a person scrolls.
	Offset(n int) Query[T]
	// DistinctOn keeps the first row of each group of es (DISTINCT ON).
	DistinctOn(es ...Expr) Query[T]
	// AllTenants lets the query cross tenants; see QueryCore.AllTenants.
	AllTenants() Query[T]
	// CustomPlan plans the query for its parameter values on every execution; see
	// QueryCore.CustomPlan.
	CustomPlan() Query[T]
	// Timeout bounds the query's execution.
	Timeout(d time.Duration) Query[T]
	// Lock locks the table's selected rows. Joined relations are not locked.
	Lock(strength LockStrength, wait ...LockWait) Query[T]
	// Join adds an INNER JOIN of another table on the given predicates.
	Join(t interface{ TableRef() *Ref }, on ...Pred) Query[T]
	// LeftJoin adds a LEFT JOIN of another table on the given predicates.
	LeftJoin(t interface{ TableRef() *Ref }, on ...Pred) Query[T]
	// LeftJoinLateral adds LEFT JOIN LATERAL (sub) ON TRUE.
	LeftJoinLateral(sub LateralSource) Query[T]
	// JoinLateral adds CROSS JOIN LATERAL (sub).
	JoinLateral(sub LateralSource) Query[T]
	// Clone returns an independent copy of the query.
	Clone() Query[T]
	// Cache serves the query from the result cache; see QueryCore.Cache.
	Cache(opts ...CacheOption) Query[T]
	// With declares CTEs the query uses.
	With(ctes ...*CTETable) Query[T]
	// Compute selects e into the computed field col; see QueryCore.Compute.
	Compute(col Column, e Expr) Query[T]
	// LoadStrategy chooses how the query's relations load; see QueryCore.LoadStrategy.
	LoadStrategy(s LoadStrategy) Query[T]

	// One returns the only matching row: ErrNotFound when none matches, ErrNotSingular when more
	// do.
	One(ctx context.Context, db *DB) (T, error)
	// First returns the first row in the query's order, or ErrNotFound.
	First(ctx context.Context, db *DB) (T, error)
	// List returns the matching rows. The query must have a Limit.
	List(ctx context.Context, db *DB) ([]T, error)
	// All returns every matching row. Use it only for sets known to be small.
	All(ctx context.Context, db *DB) ([]T, error)
	// UpdateReturning sets columns on every matching row and returns the updated rows; see the
	// function UpdateReturning.
	UpdateReturning(ctx context.Context, db *DB, sets ...Assignment) ([]T, error)
	// Page returns the page of rows after the request's cursor; see PageOf.
	Page(ctx context.Context, db *DB, p PageRequest) (Page[T], error)
	// Each runs the query page by page and calls fn with each batch; see the function Each.
	Each(ctx context.Context, db *DB, size int, fn func(ctx context.Context, batch []T) error) error

	// TableRef returns the query's table reference.
	TableRef() *Ref
	// Any is true when at least one row of the query matches ps.
	Any(ps ...Pred) Pred
	// None is the negation of Any.
	None(ps ...Pred) Pred
	// Count returns the number of matching rows.
	Count(ctx context.Context, db *DB) (int64, error)
	// CountUpTo counts matching rows, stopping at limit.
	CountUpTo(ctx context.Context, db *DB, limit int64) (int64, error)
	// CountEstimate returns the number of matching rows, exact up to exactUpTo and estimated
	// above it; see QueryCore.CountEstimate.
	CountEstimate(ctx context.Context, db *DB, exactUpTo int64) (RowCount, error)
	// Exists reports whether any row matches.
	Exists(ctx context.Context, db *DB) (bool, error)
	// Update sets columns on every matching row and returns the number of rows changed.
	Update(ctx context.Context, db *DB, sets ...Assignment) (int64, error)
	// Delete deletes every matching row and returns the number of rows deleted.
	Delete(ctx context.Context, db *DB) (int64, error)
	// ToSQL renders the query's main statement as it would run under ctx's scope.
	ToSQL(ctx context.Context, db *DB) (string, []any, error)
	// Explain returns the plan of the query's main statement.
	Explain(ctx context.Context, db *DB, opts ExplainOptions) (*Plan, error)

	queryCore() *QueryCore
}

// QueryCore is the query engine: the state of one query, its builders and every terminal that
// does not return rows. It is not generic, so its methods are compiled once, in keel. Every query
// type keel gen emits embeds it by value and implements Query[T] by shadowing each builder with
// one that returns Query[T] and adding the row terminals, which call keel's generic functions
// (One, First, List, All, UpdateReturning, PageOf, Each). Builder methods change the query and
// return it.
type QueryCore struct {
	q query
}

// NewQueryCore returns the engine of a query over the table ref references. Generated table types
// call it to start a query.
func NewQueryCore(ref *Ref) QueryCore {
	return QueryCore{q: query{ref: ref}}
}

// queryCore returns q itself, so keel reaches the engine of any Query[T].
func (q *QueryCore) queryCore() *QueryCore { return q }

type query struct {
	ref        *Ref
	where      []Pred
	exprs      []Expr
	loads      []*loadSpec
	order      []Order
	group      []Expr
	having     []Pred
	joins      []joinClause
	limit      int64
	offset     int64
	lock       string
	allTenants bool
	distinctOn []Expr
	timeout    time.Duration
	customPlan bool
	distinct   bool
	ctes       []*CTETable
	windows    []namedWindow
	computes   []compute
	omit       []Col
	cache      cacheSpec

	loadStrategy LoadStrategy
}

type joinClause struct {
	kind    string
	ref     *Ref
	on      []Pred
	lateral subquery
}

// Where adds predicates, joined by AND. Nil predicates are skipped.
func (q *QueryCore) Where(ps ...Pred) *QueryCore {
	q.q.where = append(q.q.where, ps...)
	return q
}

// Pick selects only es. When every expression is a column of the table, the rows still decode
// into T with the other fields left zero; otherwise the query is a projection for Into.
func (q *QueryCore) Pick(es ...Expr) *QueryCore {
	q.q.exprs = append(q.q.exprs, es...)
	return q
}

// Omit leaves cols out of the columns the query reads, so a heavy or secret column (a transcript,
// raw file contents, a token) is not read; the fields stay zero. The primary key and the columns
// relation loads need are read anyway. It works on loads too: Load(r.Omit(r.Payload)).
func (q *QueryCore) Omit(cols ...Column) *QueryCore {
	for _, c := range cols {
		q.q.omit = append(q.q.omit, c.column())
	}
	return q
}

// Load loads relations into the rows.
func (q *QueryCore) Load(ls ...Loader) *QueryCore {
	for _, l := range ls {
		q.q.loads = append(q.q.loads, l.loadSpec())
	}
	return q
}

// Order sets the sort order.
func (q *QueryCore) Order(os ...Order) *QueryCore {
	q.q.order = append(q.q.order, os...)
	return q
}

// Distinct returns each distinct row once: SELECT DISTINCT.
func (q *QueryCore) Distinct() *QueryCore {
	q.q.distinct = true
	return q
}

// Group adds GROUP BY expressions.
func (q *QueryCore) Group(es ...Expr) *QueryCore {
	q.q.group = append(q.q.group, es...)
	return q
}

// GroupingSets groups by each of sets in turn, in one statement: GROUP BY GROUPING SETS ((a, b),
// (a), ()). Each set yields its own rows, with the expressions it does not group by NULL; an empty
// set is the grand total. Grouping tells the rows of each set apart. Group expressions added
// before or after are grouped in every set.
func (q *QueryCore) GroupingSets(sets ...[]Expr) *QueryCore {
	q.q.group = append(q.q.group, groupingSets(sets))
	return q
}

// Rollup groups by es and by each prefix of es down to the grand total: ROLLUP (a, b) is
// GROUPING SETS ((a, b), (a), ()), the rows with their subtotals and total.
func (q *QueryCore) Rollup(es ...Expr) *QueryCore {
	q.q.group = append(q.q.group, rollup(es))
	return q
}

// Cube groups by every subset of es: CUBE (a, b) is GROUPING SETS ((a, b), (a), (b), ()).
func (q *QueryCore) Cube(es ...Expr) *QueryCore {
	q.q.group = append(q.q.group, cube(es))
	return q
}

// Window declares a named window (WINDOW name AS (…)) that window functions of the query use
// through NamedWindow. name must match [a-z_][a-z0-9_]*.
func (q *QueryCore) Window(name string, w Window) *QueryCore {
	q.q.windows = append(q.q.windows, namedWindow{name: name, w: w})
	return q
}

// Having adds HAVING predicates.
func (q *QueryCore) Having(ps ...Pred) *QueryCore {
	q.q.having = append(q.q.having, ps...)
	return q
}

// Limit caps the number of rows.
func (q *QueryCore) Limit(n int) *QueryCore {
	q.q.limit = int64(n)
	return q
}

// Offset skips n rows. Prefer Page for anything a person scrolls.
func (q *QueryCore) Offset(n int) *QueryCore {
	q.q.offset = int64(n)
	return q
}

// DistinctOn keeps the first row of each group of es (DISTINCT ON).
func (q *QueryCore) DistinctOn(es ...Expr) *QueryCore {
	q.q.distinctOn = append(q.q.distinctOn, es...)
	return q
}

// AllTenants lets the query cross tenants. It needs a system scope. The query's relation loads
// and its Any and None predicates cross tenants with it; a query used as a subquery (Exists,
// InQuery, a lateral join, a CTE, a set operation) declares AllTenants itself.
func (q *QueryCore) AllTenants() *QueryCore {
	q.q.allTenants = true
	return q
}

// CustomPlan plans this query for its parameter values on every execution, even when the
// connection would reuse a generic plan. Use it for queries whose best plan depends on the values,
// such as a filter whose selectivity differs widely between tenants. It costs no round trip.
func (q *QueryCore) CustomPlan() *QueryCore {
	q.q.customPlan = true
	return q
}

// Timeout bounds the query's execution.
func (q *QueryCore) Timeout(d time.Duration) *QueryCore {
	q.q.timeout = d
	return q
}

// LockStrength is the row lock a query takes.
type LockStrength string

// LockWait is what a locking query does when a row is already locked.
type LockWait string

const (
	ForUpdate      LockStrength = "FOR UPDATE"
	ForNoKeyUpdate LockStrength = "FOR NO KEY UPDATE"
	ForShare       LockStrength = "FOR SHARE"
	ForKeyShare    LockStrength = "FOR KEY SHARE"

	Wait       LockWait = ""
	NoWait     LockWait = " NOWAIT"
	SkipLocked LockWait = " SKIP LOCKED"
)

// Lock locks the table's selected rows. Joined relations are not locked.
func (q *QueryCore) Lock(strength LockStrength, wait ...LockWait) *QueryCore {
	q.q.lock = string(strength)
	if len(wait) > 0 {
		q.q.lock += "\x00" + string(wait[0])
	}
	return q
}

// Join adds an INNER JOIN of another table on the given predicates.
func (q *QueryCore) Join(t interface{ TableRef() *Ref }, on ...Pred) *QueryCore {
	q.q.joins = append(q.q.joins, joinClause{kind: " JOIN ", ref: t.TableRef(), on: on})
	return q
}

// LeftJoin adds a LEFT JOIN of another table on the given predicates.
func (q *QueryCore) LeftJoin(t interface{ TableRef() *Ref }, on ...Pred) *QueryCore {
	q.q.joins = append(q.q.joins, joinClause{kind: " LEFT JOIN ", ref: t.TableRef(), on: on})
	return q
}

// LeftJoinLateral adds LEFT JOIN LATERAL (sub) ON TRUE. Columns of sub's table, referenced
// outside sub, read sub's output.
func (q *QueryCore) LeftJoinLateral(sub LateralSource) *QueryCore {
	q.q.joins = append(q.q.joins, joinClause{kind: " LEFT JOIN LATERAL ", ref: sub.TableRef(),
		lateral: sub})
	return q
}

// JoinLateral adds CROSS JOIN LATERAL (sub).
func (q *QueryCore) JoinLateral(sub LateralSource) *QueryCore {
	q.q.joins = append(q.q.joins, joinClause{kind: " CROSS JOIN LATERAL ", ref: sub.TableRef(),
		lateral: sub})
	return q
}

// Clone returns an independent copy of the query.
func (q *QueryCore) Clone() *QueryCore {
	c := &QueryCore{q: q.q}
	c.q.where = append([]Pred(nil), q.q.where...)
	c.q.exprs = append([]Expr(nil), q.q.exprs...)
	c.q.loads = append([]*loadSpec(nil), q.q.loads...)
	c.q.order = append([]Order(nil), q.q.order...)
	c.q.group = append([]Expr(nil), q.q.group...)
	c.q.having = append([]Pred(nil), q.q.having...)
	c.q.joins = append([]joinClause(nil), q.q.joins...)
	c.q.distinctOn = append([]Expr(nil), q.q.distinctOn...)
	c.q.ctes = append([]*CTETable(nil), q.q.ctes...)
	c.q.windows = append([]namedWindow(nil), q.q.windows...)
	c.q.computes = append([]compute(nil), q.q.computes...)
	c.q.omit = append([]Col(nil), q.q.omit...)
	return c
}

// TableRef returns the query's table reference.
func (q *QueryCore) TableRef() *Ref { return q.q.ref }

func (q *QueryCore) loadSpec() *loadSpec { return &loadSpec{ref: q.q.ref, q: &q.q} }

func (q *QueryCore) writeSubquery(r *render, existsOnly bool) { q.q.writeSubquery(r, existsOnly) }

func (q *QueryCore) any(ps []Pred, not bool) Pred {
	return relExistsQuery(&q.q, ps, not)
}

// Any is true when at least one row of the query matches ps. On a query started from a to-many
// relation path it is correlated with the parent row.
func (q *QueryCore) Any(ps ...Pred) Pred { return q.any(ps, false) }

// None is the negation of Any.
func (q *QueryCore) None(ps ...Pred) Pred { return q.any(ps, true) }

// One returns the only row q matches, decoded as T, the model's pointer type: ErrNotFound when
// none matches, ErrNotSingular when more do. Generated query types call it for their One method.
func One[T any](ctx context.Context, db *DB, q *QueryCore) (T, error) {
	mustBeRowPointer[T]()
	row, err := OnePtr(ctx, db, q)
	return fromPtr[T](row), err
}

// First returns the first row in q's order, or ErrNotFound. Generated query types call it for
// their First method.
func First[T any](ctx context.Context, db *DB, q *QueryCore) (T, error) {
	mustBeRowPointer[T]()
	row, err := FirstPtr(ctx, db, q)
	return fromPtr[T](row), err
}

// List returns the rows q matches. The query must have a Limit. Generated query types call it for
// their List method.
func List[T any](ctx context.Context, db *DB, q *QueryCore) ([]T, error) {
	mustBeRowPointer[T]()
	rows, err := ListPtrs(ctx, db, q)
	return fromPtrs[T](rows), err
}

// All returns every row q matches. Use it only for sets known to be small. Generated query types
// call it for their All method.
func All[T any](ctx context.Context, db *DB, q *QueryCore) ([]T, error) {
	mustBeRowPointer[T]()
	rows, err := AllPtrs(ctx, db, q)
	return fromPtrs[T](rows), err
}

// Count returns the number of matching rows.
func (q *QueryCore) Count(ctx context.Context, db *DB) (int64, error) {
	var n int64
	err := Batch(ctx, db, countItem(&q.q, 0, func(c int64) { n = c }))
	return n, err
}

// CountUpTo counts matching rows, stopping at limit.
func (q *QueryCore) CountUpTo(ctx context.Context, db *DB, limit int64) (int64, error) {
	var n int64
	err := Batch(ctx, db, countItem(&q.q, limit, func(c int64) { n = c }))
	return n, err
}

// Exists reports whether any row matches.
func (q *QueryCore) Exists(ctx context.Context, db *DB) (bool, error) {
	var ok bool
	err := Batch(ctx, db, countItem(&q.q, 1, func(c int64) { ok = c > 0 }))
	return ok, err
}

// Update sets columns on every matching row and returns the number of rows changed. The
// autoupdate column is stamped and the version column incremented unless sets name them.
func (q *QueryCore) Update(ctx context.Context, db *DB, sets ...Assignment) (int64, error) {
	return q.q.update(ctx, db, sets)
}

// UpdateReturning sets columns on every row q matches, as Update does, and returns the updated
// rows, decoded as T, read in the same statement (WITH keel_upd AS (UPDATE … RETURNING …)
// SELECT …): their values after the update, readonly columns included, with the autoupdate stamp
// and the incremented version. The query's Load paths are read too, to-one relations joined into
// the statement and to-many relations loaded after it, both as of the statement's snapshot (before
// the update); Order orders the rows; Pick and Omit narrow the columns read back (the primary key
// and tenant columns are always read, every default column when the table has update hooks, which
// receive the same rows). The update's tenant predicate and interceptors choose the rows; read
// interceptors do not apply to them. Limit, Offset, Lock, Group and DistinctOn are refused.
// Generated query types call it for their UpdateReturning method.
func UpdateReturning[T any](ctx context.Context, db *DB, q *QueryCore, sets ...Assignment) ([]T, error) {
	mustBeRowPointer[T]()
	rows, err := UpdateReturningPtrs(ctx, db, q, sets...)
	return fromPtrs[T](rows), err
}

// Delete deletes every matching row and returns the number of rows deleted.
func (q *QueryCore) Delete(ctx context.Context, db *DB) (int64, error) {
	return q.q.delete(ctx, db)
}

func relExists(ref *Ref, ps []Pred, not bool) Pred {
	return relExistsQuery(&query{ref: ref}, ps, not)
}

func relExistsQuery(q *query, ps []Pred, not bool) Pred {
	return predExpr{e: func(r *render) {
		if not {
			r.s("NOT ")
		}
		r.s("EXISTS (")
		sub := *q
		sub.where = append(append([]Pred(nil), q.where...), ps...)
		sub.allTenants = sub.allTenants || r.sc != nil && r.sc.allTenants
		sub.writeSubquery(r, true)
		r.b(')')
	}}
}

func (q *query) projection() bool {
	for _, e := range q.exprs {
		c, ok := e.(columnar)
		if !ok || c.column().Ref != q.ref {
			return true
		}
	}
	return false
}

func (q *query) checkTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if q.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, q.timeout)
}

func errf(format string, args ...any) error { return fmt.Errorf("keel: "+format, args...) }

// Each runs q page by page in keyset order and calls fn with each batch of at most size rows,
// decoded as T, holding one batch in memory at a time. Generated query types call it for their
// Each method.
func Each[T any](ctx context.Context, db *DB, q *QueryCore, size int,
	fn func(ctx context.Context, batch []T) error,
) error {
	mustBeRowPointer[T]()
	return EachPtrs(ctx, db, q, size, func(ctx context.Context, rows []unsafe.Pointer) error {
		return fn(ctx, fromPtrs[T](rows))
	})
}

// EachPtrs is Each with rows as the addresses of their models; generated query types call it.
func EachPtrs(ctx context.Context, db *DB, q *QueryCore, size int,
	fn func(ctx context.Context, rows []unsafe.Pointer) error,
) error {
	cursor := ""
	for {
		page, err := PagePtrs(ctx, db, q, After(cursor, size))
		if err != nil {
			return err
		}
		if len(page.Items) > 0 {
			if err := fn(ctx, page.Items); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		cursor = page.Next
	}
}

// ToSQL renders the query's main statement as it would run under ctx's scope, for logging, tests
// and EXPLAIN. Relation loads that run as separate statements are not included.
func (q *QueryCore) ToSQL(ctx context.Context, db *DB) (string, []any, error) {
	r := newRender(db.resolveScope(ctx), ctx, db)
	defer r.recycle()
	root := newRoot(&q.q)
	if err := buildTree(root, q.q.loads); err != nil {
		return "", nil, err
	}
	q.q.renderSelect(r, root, selectOpts{exact: q.q.projection()})
	if r.err != nil {
		return "", nil, r.err
	}
	return string(r.buf), r.argsCopy(), nil
}

// fromPtr converts the address of a model row to T, the row's pointer type.
func fromPtr[T any](p unsafe.Pointer) T { return *(*T)(unsafe.Pointer(&p)) }

// fromPtrs reads a slice of row addresses as rows of pointer type T. Every pointer type has the
// layout of unsafe.Pointer, so the slices share their array.
func fromPtrs[T any](rows []unsafe.Pointer) []T { return *(*[]T)(unsafe.Pointer(&rows)) }

// rowsDst, rowDst and pageDst read a destination of rows of pointer type T as one of row
// addresses, which keel's non-generic reads fill.
func rowsDst[T any](dst *[]T) *[]unsafe.Pointer { return (*[]unsafe.Pointer)(unsafe.Pointer(dst)) }

func rowDst[T any](dst *T) *unsafe.Pointer { return (*unsafe.Pointer)(unsafe.Pointer(dst)) }

func pageDst[T any](dst *Page[T]) *Page[unsafe.Pointer] {
	return (*Page[unsafe.Pointer])(unsafe.Pointer(dst))
}

// toPtr returns the address a row of pointer type T holds.
func toPtr[T any](row T) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&row)) }

// mustBeRowPointer panics unless T is a pointer type, the only type argument keel's generic
// functions take: they convert rows to T through unsafe.Pointer. Every generic function that
// decodes or writes rows of T calls it before it touches a row.
func mustBeRowPointer[T any]() {
	if t := reflect.TypeFor[T](); t.Kind() != reflect.Pointer {
		panic("keel: the type argument " + t.String() + " must be the model's pointer type (*" + t.String() + ")")
	}
}
