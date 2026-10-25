package keel

import (
	"context"
	"fmt"
	"reflect"
	"time"
	"unsafe"
)

// Handle is embedded in every generated table type. It starts queries and runs writes for rows of
// type T, the model's pointer type: Handle[*fleet.Shipment]. Every pointer type shares one shape, so
// the compiler builds one copy of keel's generic code for all models instead of one per model.
type Handle[T any] struct {
	Ref *Ref
}

// Query starts a query over the table.
func (h Handle[T]) Query() *Query[T] {
	mustBeRowPointer[T]()
	return &Query[T]{q: query{ref: h.Ref}}
}

// Where starts a query filtered by ps.
func (h Handle[T]) Where(ps ...Pred) *Query[T] { return h.Query().Where(ps...) }

// Pick starts a query that selects only es.
func (h Handle[T]) Pick(es ...Expr) *Query[T] { return h.Query().Pick(es...) }

// Load starts a query that loads relations.
func (h Handle[T]) Load(ls ...Loader) *Query[T] { return h.Query().Load(ls...) }

// Omit starts a query that leaves cols out of the columns it reads.
func (h Handle[T]) Omit(cols ...Column) *Query[T] { return h.Query().Omit(cols...) }

// Order starts an ordered query.
func (h Handle[T]) Order(os ...Order) *Query[T] { return h.Query().Order(os...) }

// Limit starts a query limited to n rows.
func (h Handle[T]) Limit(n int) *Query[T] { return h.Query().Limit(n) }

// Group starts a grouped query.
func (h Handle[T]) Group(es ...Expr) *Query[T] { return h.Query().Group(es...) }

// GroupingSets starts a query grouped by GROUPING SETS.
func (h Handle[T]) GroupingSets(sets ...[]Expr) *Query[T] { return h.Query().GroupingSets(sets...) }

// Rollup starts a query grouped by ROLLUP.
func (h Handle[T]) Rollup(es ...Expr) *Query[T] { return h.Query().Rollup(es...) }

// Cube starts a query grouped by CUBE.
func (h Handle[T]) Cube(es ...Expr) *Query[T] { return h.Query().Cube(es...) }

// AllTenants starts a query that crosses tenants. It needs a system scope.
func (h Handle[T]) AllTenants() *Query[T] { return h.Query().AllTenants() }

// Join starts a query with an INNER JOIN of another table.
func (h Handle[T]) Join(t interface{ TableRef() *Ref }, on ...Pred) *Query[T] {
	return h.Query().Join(t, on...)
}

// LeftJoin starts a query with a LEFT JOIN of another table.
func (h Handle[T]) LeftJoin(t interface{ TableRef() *Ref }, on ...Pred) *Query[T] {
	return h.Query().LeftJoin(t, on...)
}

// LeftJoinLateral starts a query with LEFT JOIN LATERAL (sub) ON TRUE.
func (h Handle[T]) LeftJoinLateral(sub interface {
	subquery
	TableRef() *Ref
},
) *Query[T] {
	return h.Query().LeftJoinLateral(sub)
}

// JoinLateral starts a query with CROSS JOIN LATERAL (sub).
func (h Handle[T]) JoinLateral(sub interface {
	subquery
	TableRef() *Ref
},
) *Query[T] {
	return h.Query().JoinLateral(sub)
}

// Lock starts a query that locks the rows it selects.
func (h Handle[T]) Lock(strength LockStrength, wait ...LockWait) *Query[T] {
	return h.Query().Lock(strength, wait...)
}

// DistinctOn starts a DISTINCT ON query.
func (h Handle[T]) DistinctOn(es ...Expr) *Query[T] { return h.Query().DistinctOn(es...) }

// Any is true when at least one row of the relation path matches ps: EXISTS (…).
func (h Handle[T]) Any(ps ...Pred) Pred { return relExists(h.Ref, ps, false) }

// None is true when no row of the relation path matches ps: NOT EXISTS (…).
func (h Handle[T]) None(ps ...Pred) Pred { return relExists(h.Ref, ps, true) }

func (h Handle[T]) loadSpec() *loadSpec { return &loadSpec{ref: h.Ref} }

// TableRef returns the handle's table reference.
func (h Handle[T]) TableRef() *Ref { return h.Ref }

// Loader is a relation path to load, optionally narrowed: a relation handle (s.Customer()) or a
// query started from one (m.Order(m.Sequence.Asc())).
type Loader interface {
	loadSpec() *loadSpec
}

type loadSpec struct {
	ref *Ref
	q   *query
}

// Query is a query over model T. Builder methods change the query and return it.
type Query[T any] struct {
	q query
}

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
}

type joinClause struct {
	kind    string
	ref     *Ref
	on      []Pred
	lateral subquery
}

// Where adds predicates, joined by AND. Nil predicates are skipped.
func (q *Query[T]) Where(ps ...Pred) *Query[T] {
	q.q.where = append(q.q.where, ps...)
	return q
}

// Pick selects only es. When every expression is a column of the table, the rows still decode
// into T with the other fields left zero; otherwise the query is a projection for Into.
func (q *Query[T]) Pick(es ...Expr) *Query[T] {
	q.q.exprs = append(q.q.exprs, es...)
	return q
}

// Omit leaves cols out of the columns the query reads, so a heavy or secret column (a transcript,
// raw file contents, a token) is not read; the fields stay zero. The primary key and the columns
// relation loads need are read anyway. It works on loads too: Load(r.Omit(r.Payload)).
func (q *Query[T]) Omit(cols ...Column) *Query[T] {
	for _, c := range cols {
		q.q.omit = append(q.q.omit, c.column())
	}
	return q
}

// Load loads relations into the rows.
func (q *Query[T]) Load(ls ...Loader) *Query[T] {
	for _, l := range ls {
		q.q.loads = append(q.q.loads, l.loadSpec())
	}
	return q
}

// Order sets the sort order.
func (q *Query[T]) Order(os ...Order) *Query[T] {
	q.q.order = append(q.q.order, os...)
	return q
}

// Distinct returns each distinct row once: SELECT DISTINCT.
func (q *Query[T]) Distinct() *Query[T] {
	q.q.distinct = true
	return q
}

// Group adds GROUP BY expressions.
func (q *Query[T]) Group(es ...Expr) *Query[T] {
	q.q.group = append(q.q.group, es...)
	return q
}

// GroupingSets groups by each of sets in turn, in one statement: GROUP BY GROUPING SETS ((a, b),
// (a), ()). Each set yields its own rows, with the expressions it does not group by NULL; an empty
// set is the grand total. Grouping tells the rows of each set apart. Group expressions added
// before or after are grouped in every set.
func (q *Query[T]) GroupingSets(sets ...[]Expr) *Query[T] {
	q.q.group = append(q.q.group, groupingSets(sets))
	return q
}

// Rollup groups by es and by each prefix of es down to the grand total: ROLLUP (a, b) is
// GROUPING SETS ((a, b), (a), ()), the rows with their subtotals and total.
func (q *Query[T]) Rollup(es ...Expr) *Query[T] {
	q.q.group = append(q.q.group, rollup(es))
	return q
}

// Cube groups by every subset of es: CUBE (a, b) is GROUPING SETS ((a, b), (a), (b), ()).
func (q *Query[T]) Cube(es ...Expr) *Query[T] {
	q.q.group = append(q.q.group, cube(es))
	return q
}

// Window declares a named window (WINDOW name AS (…)) that window functions of the query use
// through NamedWindow. name must match [a-z_][a-z0-9_]*.
func (q *Query[T]) Window(name string, w Window) *Query[T] {
	q.q.windows = append(q.q.windows, namedWindow{name: name, w: w})
	return q
}

// Having adds HAVING predicates.
func (q *Query[T]) Having(ps ...Pred) *Query[T] {
	q.q.having = append(q.q.having, ps...)
	return q
}

// Limit caps the number of rows.
func (q *Query[T]) Limit(n int) *Query[T] {
	q.q.limit = int64(n)
	return q
}

// Offset skips n rows. Prefer Page for anything a person scrolls.
func (q *Query[T]) Offset(n int) *Query[T] {
	q.q.offset = int64(n)
	return q
}

// DistinctOn keeps the first row of each group of es (DISTINCT ON).
func (q *Query[T]) DistinctOn(es ...Expr) *Query[T] {
	q.q.distinctOn = append(q.q.distinctOn, es...)
	return q
}

// AllTenants lets the query cross tenants. It needs a system scope. The query's relation loads
// and its Any and None predicates cross tenants with it; a query used as a subquery (Exists,
// InQuery, a lateral join, a CTE, a set operation) declares AllTenants itself.
func (q *Query[T]) AllTenants() *Query[T] {
	q.q.allTenants = true
	return q
}

// CustomPlan plans this query for its parameter values on every execution, even when the
// connection would reuse a generic plan. Use it for queries whose best plan depends on the values,
// such as a filter whose selectivity differs widely between tenants. It costs no round trip.
func (q *Query[T]) CustomPlan() *Query[T] {
	q.q.customPlan = true
	return q
}

// Timeout bounds the query's execution.
func (q *Query[T]) Timeout(d time.Duration) *Query[T] {
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
func (q *Query[T]) Lock(strength LockStrength, wait ...LockWait) *Query[T] {
	q.q.lock = string(strength)
	if len(wait) > 0 {
		q.q.lock += "\x00" + string(wait[0])
	}
	return q
}

// Join adds an INNER JOIN of another table on the given predicates.
func (q *Query[T]) Join(t interface{ TableRef() *Ref }, on ...Pred) *Query[T] {
	q.q.joins = append(q.q.joins, joinClause{kind: " JOIN ", ref: t.TableRef(), on: on})
	return q
}

// LeftJoin adds a LEFT JOIN of another table on the given predicates.
func (q *Query[T]) LeftJoin(t interface{ TableRef() *Ref }, on ...Pred) *Query[T] {
	q.q.joins = append(q.q.joins, joinClause{kind: " LEFT JOIN ", ref: t.TableRef(), on: on})
	return q
}

// LeftJoinLateral adds LEFT JOIN LATERAL (sub) ON TRUE. Columns of sub's table, referenced
// outside sub, read sub's output.
func (q *Query[T]) LeftJoinLateral(sub interface {
	subquery
	TableRef() *Ref
},
) *Query[T] {
	q.q.joins = append(q.q.joins, joinClause{kind: " LEFT JOIN LATERAL ", ref: sub.TableRef(),
		lateral: sub})
	return q
}

// JoinLateral adds CROSS JOIN LATERAL (sub).
func (q *Query[T]) JoinLateral(sub interface {
	subquery
	TableRef() *Ref
},
) *Query[T] {
	q.q.joins = append(q.q.joins, joinClause{kind: " CROSS JOIN LATERAL ", ref: sub.TableRef(),
		lateral: sub})
	return q
}

// Clone returns an independent copy of the query.
func (q *Query[T]) Clone() *Query[T] {
	c := &Query[T]{q: q.q}
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
func (q *Query[T]) TableRef() *Ref { return q.q.ref }

func (q *Query[T]) loadSpec() *loadSpec { return &loadSpec{ref: q.q.ref, q: &q.q} }

func (q *Query[T]) writeSubquery(r *render, existsOnly bool) { q.q.writeSubquery(r, existsOnly) }

func (q *Query[T]) any(ps []Pred, not bool) Pred {
	return relExistsQuery(&q.q, ps, not)
}

// Any is true when at least one row of the query matches ps. On a query started from a to-many
// relation path it is correlated with the parent row.
func (q *Query[T]) Any(ps ...Pred) Pred { return q.any(ps, false) }

// None is the negation of Any.
func (q *Query[T]) None(ps ...Pred) Pred { return q.any(ps, true) }

// One returns the only matching row: ErrNotFound when none matches, ErrNotSingular when more do.
func (q *Query[T]) One(ctx context.Context, db *DB) (T, error) {
	var row T
	err := Batch(ctx, db, OneInto(q, &row))
	return row, err
}

// First returns the first row in the query's order, or ErrNotFound.
func (q *Query[T]) First(ctx context.Context, db *DB) (T, error) {
	var row T
	err := Batch(ctx, db, FirstInto(q, &row))
	return row, err
}

// List returns the matching rows. The query must have a Limit.
func (q *Query[T]) List(ctx context.Context, db *DB) ([]T, error) {
	var rows []T
	err := Batch(ctx, db, ListInto(q, &rows))
	return rows, err
}

// All returns every matching row. Use it only for sets known to be small.
func (q *Query[T]) All(ctx context.Context, db *DB) ([]T, error) {
	var rows []T
	err := Batch(ctx, db, AllInto(q, &rows))
	return rows, err
}

// Count returns the number of matching rows.
func (q *Query[T]) Count(ctx context.Context, db *DB) (int64, error) {
	var n int64
	err := Batch(ctx, db, CountInto(q, &n))
	return n, err
}

// CountUpTo counts matching rows, stopping at limit.
func (q *Query[T]) CountUpTo(ctx context.Context, db *DB, limit int64) (int64, error) {
	var n int64
	err := Batch(ctx, db, CountUpToInto(q, limit, &n))
	return n, err
}

// Exists reports whether any row matches.
func (q *Query[T]) Exists(ctx context.Context, db *DB) (bool, error) {
	var ok bool
	err := Batch(ctx, db, ExistsInto(q, &ok))
	return ok, err
}

// Update sets columns on every matching row and returns the number of rows changed. The
// autoupdate column is stamped and the version column incremented unless sets name them.
func (q *Query[T]) Update(ctx context.Context, db *DB, sets ...Assignment) (int64, error) {
	return q.q.update(ctx, db, sets)
}

// UpdateReturning sets columns on every matching row, as Update does, and returns the updated rows
// read in the same statement (WITH keel_upd AS (UPDATE … RETURNING …) SELECT …): their values
// after the update, readonly columns included, with the autoupdate stamp and the incremented
// version. The query's Load paths are read too, to-one relations joined into the statement and
// to-many relations loaded after it, both as of the statement's snapshot (before the update);
// Order orders the rows; Pick and Omit narrow the columns read back (the primary key and tenant
// columns are always read, every default column when the table has update hooks, which receive
// the same rows). The update's tenant predicate and interceptors choose the rows; read
// interceptors do not apply to them. Limit, Offset, Lock, Group and DistinctOn are refused.
func (q *Query[T]) UpdateReturning(ctx context.Context, db *DB, sets ...Assignment) ([]T, error) {
	var rows []T
	err := q.q.updateReturning(ctx, db, sets, func(row unsafe.Pointer) { rows = append(rows, fromPtr[T](row)) })
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Delete deletes every matching row and returns the number of rows deleted.
func (q *Query[T]) Delete(ctx context.Context, db *DB) (int64, error) {
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

// Each runs the query page by page in keyset order and calls fn with each batch of at most size
// rows, holding one batch in memory at a time.
func (q *Query[T]) Each(ctx context.Context, db *DB, size int,
	fn func(ctx context.Context, batch []T) error,
) error {
	cursor := ""
	for {
		page, err := q.Page(ctx, db, After(cursor, size))
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
func (q *Query[T]) ToSQL(ctx context.Context, db *DB) (string, []any, error) {
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

// toPtr returns the address a row of pointer type T holds.
func toPtr[T any](row T) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&row)) }

// mustBeRowPointer panics unless T is a pointer type, the only type argument keel's generic types
// take: they convert rows to T through unsafe.Pointer.
func mustBeRowPointer[T any]() {
	if t := reflect.TypeFor[T](); t.Kind() != reflect.Pointer {
		panic("keel: the type argument " + t.String() + " must be the model's pointer type (*" + t.String() + ")")
	}
}
