package keel

import (
	"context"
	"fmt"
	"time"
	"unsafe"
)

// Handle is embedded in every generated table type. It starts queries and runs writes for model T.
type Handle[T any] struct {
	Ref *Ref
}

// Query starts a query over the table.
func (h Handle[T]) Query() *Query[T] { return &Query[T]{q: query{ref: h.Ref}} }

// Where starts a query filtered by ps.
func (h Handle[T]) Where(ps ...Pred) *Query[T] { return h.Query().Where(ps...) }

// Pick starts a query that selects only es.
func (h Handle[T]) Pick(es ...Expr) *Query[T] { return h.Query().Pick(es...) }

// Load starts a query that loads relations.
func (h Handle[T]) Load(ls ...Loader) *Query[T] { return h.Query().Load(ls...) }

// Order starts an ordered query.
func (h Handle[T]) Order(os ...Order) *Query[T] { return h.Query().Order(os...) }

// Limit starts a query limited to n rows.
func (h Handle[T]) Limit(n int) *Query[T] { return h.Query().Limit(n) }

// Group starts a grouped query.
func (h Handle[T]) Group(es ...Expr) *Query[T] { return h.Query().Group(es...) }

// AllTenants starts a query that crosses tenants. It needs a system scope.
func (h Handle[T]) AllTenants() *Query[T] { return h.Query().AllTenants() }

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
	picks      []Col
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

// Group adds GROUP BY expressions.
func (q *Query[T]) Group(es ...Expr) *Query[T] {
	q.q.group = append(q.q.group, es...)
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

// AllTenants lets the query cross tenants. It needs a system scope.
func (q *Query[T]) AllTenants() *Query[T] {
	q.q.allTenants = true
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
func (q *Query[T]) One(ctx context.Context, db *DB) (*T, error) {
	saved := q.q.limit
	q.q.limit = 2
	rows, err := fetchAll[T](ctx, db, &q.q)
	q.q.limit = saved
	if err != nil {
		return nil, err
	}
	switch len(rows) {
	case 0:
		return nil, ErrNotFound
	case 1:
		return rows[0], nil
	}
	return nil, ErrNotSingular
}

// First returns the first row in the query's order, or ErrNotFound.
func (q *Query[T]) First(ctx context.Context, db *DB) (*T, error) {
	saved := q.q.limit
	q.q.limit = 1
	rows, err := fetchAll[T](ctx, db, &q.q)
	q.q.limit = saved
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return rows[0], nil
}

// List returns the matching rows. The query must have a Limit.
func (q *Query[T]) List(ctx context.Context, db *DB) ([]*T, error) {
	if q.q.limit <= 0 {
		return nil, ErrUnbounded
	}
	return fetchAll[T](ctx, db, &q.q)
}

// All returns every matching row. Use it only for sets known to be small.
func (q *Query[T]) All(ctx context.Context, db *DB) ([]*T, error) {
	return fetchAll[T](ctx, db, &q.q)
}

// Count returns the number of matching rows.
func (q *Query[T]) Count(ctx context.Context, db *DB) (int64, error) {
	return q.q.count(ctx, db, 0)
}

// CountUpTo counts matching rows, stopping at limit.
func (q *Query[T]) CountUpTo(ctx context.Context, db *DB, limit int64) (int64, error) {
	return q.q.count(ctx, db, limit)
}

// Exists reports whether any row matches.
func (q *Query[T]) Exists(ctx context.Context, db *DB) (bool, error) {
	n, err := q.q.count(ctx, db, 1)
	return n > 0, err
}

// Update sets columns on every matching row and returns the number of rows changed. The
// autoupdate column is stamped and the version column incremented unless sets name them.
func (q *Query[T]) Update(ctx context.Context, db *DB, sets ...Assignment) (int64, error) {
	return q.q.update(ctx, db, sets)
}

// Delete deletes every matching row and returns the number of rows deleted.
func (q *Query[T]) Delete(ctx context.Context, db *DB) (int64, error) {
	return q.q.delete(ctx, db)
}

func fetchAll[T any](ctx context.Context, db *DB, q *query) ([]*T, error) {
	var out []*T
	if q.limit > 0 && q.limit <= 1024 {
		out = make([]*T, 0, q.limit)
	}
	err := q.fetch(ctx, db, nil, func(p unsafe.Pointer) {
		out = append(out, (*T)(p))
	})
	return out, err
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
	fn func(ctx context.Context, batch []*T) error,
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
