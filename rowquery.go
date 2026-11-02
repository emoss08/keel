package keel

import (
	"context"
	"time"
)

// rowQuery is the Query[*Row] of a CTE, CTETable.Query's result: QueryCore with the builders
// returning Query[*Row], written out as keel gen writes the query type of every model.
type rowQuery struct{ QueryCore }

var _ Query[*Row] = (*rowQuery)(nil)

// RowsOf starts a query over the table ref references that reads dynamic rows. Code that holds a
// table's columns but cannot import its generated package (a port of columns across module
// boundaries) queries it this way; keel.TableBase{Ref: ref} names the same table as a join target.
func RowsOf(ref *Ref) Query[*Row] { return &rowQuery{QueryCore: NewQueryCore(ref)} }

func (q *rowQuery) Where(ps ...Pred) Query[*Row] {
	q.QueryCore.Where(ps...)
	return q
}

func (q *rowQuery) Pick(es ...Expr) Query[*Row] {
	q.QueryCore.Pick(es...)
	return q
}

func (q *rowQuery) Omit(cols ...Column) Query[*Row] {
	q.QueryCore.Omit(cols...)
	return q
}

func (q *rowQuery) Load(ls ...Loader) Query[*Row] {
	q.QueryCore.Load(ls...)
	return q
}

func (q *rowQuery) Order(os ...Order) Query[*Row] {
	q.QueryCore.Order(os...)
	return q
}

func (q *rowQuery) Distinct() Query[*Row] {
	q.QueryCore.Distinct()
	return q
}

func (q *rowQuery) Group(es ...Expr) Query[*Row] {
	q.QueryCore.Group(es...)
	return q
}

func (q *rowQuery) Rollup(es ...Expr) Query[*Row] {
	q.QueryCore.Rollup(es...)
	return q
}

func (q *rowQuery) Cube(es ...Expr) Query[*Row] {
	q.QueryCore.Cube(es...)
	return q
}

func (q *rowQuery) Having(ps ...Pred) Query[*Row] {
	q.QueryCore.Having(ps...)
	return q
}

func (q *rowQuery) Limit(n int) Query[*Row] {
	q.QueryCore.Limit(n)
	return q
}

func (q *rowQuery) Offset(n int) Query[*Row] {
	q.QueryCore.Offset(n)
	return q
}

func (q *rowQuery) DistinctOn(es ...Expr) Query[*Row] {
	q.QueryCore.DistinctOn(es...)
	return q
}

func (q *rowQuery) AllTenants() Query[*Row] {
	q.QueryCore.AllTenants()
	return q
}

func (q *rowQuery) CustomPlan() Query[*Row] {
	q.QueryCore.CustomPlan()
	return q
}

func (q *rowQuery) GroupingSets(sets ...[]Expr) Query[*Row] {
	q.QueryCore.GroupingSets(sets...)
	return q
}

func (q *rowQuery) Window(name string, w Window) Query[*Row] {
	q.QueryCore.Window(name, w)
	return q
}

func (q *rowQuery) Timeout(d time.Duration) Query[*Row] {
	q.QueryCore.Timeout(d)
	return q
}

func (q *rowQuery) Lock(strength LockStrength, wait ...LockWait) Query[*Row] {
	q.QueryCore.Lock(strength, wait...)
	return q
}

func (q *rowQuery) Join(t interface{ TableRef() *Ref }, on ...Pred) Query[*Row] {
	q.QueryCore.Join(t, on...)
	return q
}

func (q *rowQuery) LeftJoin(t interface{ TableRef() *Ref }, on ...Pred) Query[*Row] {
	q.QueryCore.LeftJoin(t, on...)
	return q
}

func (q *rowQuery) LeftJoinLateral(sub LateralSource) Query[*Row] {
	q.QueryCore.LeftJoinLateral(sub)
	return q
}

func (q *rowQuery) JoinLateral(sub LateralSource) Query[*Row] {
	q.QueryCore.JoinLateral(sub)
	return q
}

func (q *rowQuery) Clone() Query[*Row] { return &rowQuery{QueryCore: *q.QueryCore.Clone()} }

func (q *rowQuery) Cache(opts ...CacheOption) Query[*Row] {
	q.QueryCore.Cache(opts...)
	return q
}

func (q *rowQuery) With(ctes ...*CTETable) Query[*Row] {
	q.QueryCore.With(ctes...)
	return q
}

func (q *rowQuery) Compute(col Column, e Expr) Query[*Row] {
	q.QueryCore.Compute(col, e)
	return q
}

func (q *rowQuery) LoadStrategy(s LoadStrategy) Query[*Row] {
	q.QueryCore.LoadStrategy(s)
	return q
}

func (q *rowQuery) One(ctx context.Context, db *DB) (*Row, error) {
	return One[*Row](ctx, db, &q.QueryCore)
}

func (q *rowQuery) First(ctx context.Context, db *DB) (*Row, error) {
	return First[*Row](ctx, db, &q.QueryCore)
}

func (q *rowQuery) List(ctx context.Context, db *DB) ([]*Row, error) {
	return List[*Row](ctx, db, &q.QueryCore)
}

func (q *rowQuery) All(ctx context.Context, db *DB) ([]*Row, error) {
	return All[*Row](ctx, db, &q.QueryCore)
}

func (q *rowQuery) UpdateReturning(ctx context.Context, db *DB, sets ...Assignment) ([]*Row, error) {
	return UpdateReturning[*Row](ctx, db, &q.QueryCore, sets...)
}

func (q *rowQuery) Page(ctx context.Context, db *DB, p PageRequest) (Page[*Row], error) {
	return PageOf[*Row](ctx, db, &q.QueryCore, p)
}

func (q *rowQuery) Each(ctx context.Context, db *DB, size int,
	fn func(ctx context.Context, batch []*Row) error,
) error {
	return Each(ctx, db, &q.QueryCore, size, fn)
}
