package keel

import (
	"context"
	"time"
)

// typedQuery is Query[T] for a model without generated code: QueryOf's result. Generated tables
// write a concrete query type per model instead, compiled once in their own package; every
// package that calls QueryOf compiles these methods for its T.
type typedQuery[T any] struct{ QueryCore }

// QueryOf starts a query over the table ref references, reading rows of T, the model's pointer
// type. It is for models keel gen does not cover, such as test models with a hand-built schema;
// generated tables start queries with their own Query method.
func QueryOf[T any](ref *Ref) Query[T] {
	mustBeRowPointer[T]()
	return &typedQuery[T]{QueryCore: NewQueryCore(ref)}
}

func (q *typedQuery[T]) Where(ps ...Pred) Query[T] {
	q.QueryCore.Where(ps...)
	return q
}

func (q *typedQuery[T]) Pick(es ...Expr) Query[T] {
	q.QueryCore.Pick(es...)
	return q
}

func (q *typedQuery[T]) Omit(cols ...Column) Query[T] {
	q.QueryCore.Omit(cols...)
	return q
}

func (q *typedQuery[T]) Load(ls ...Loader) Query[T] {
	q.QueryCore.Load(ls...)
	return q
}

func (q *typedQuery[T]) Order(os ...Order) Query[T] {
	q.QueryCore.Order(os...)
	return q
}

func (q *typedQuery[T]) Distinct() Query[T] {
	q.QueryCore.Distinct()
	return q
}

func (q *typedQuery[T]) Group(es ...Expr) Query[T] {
	q.QueryCore.Group(es...)
	return q
}

func (q *typedQuery[T]) Rollup(es ...Expr) Query[T] {
	q.QueryCore.Rollup(es...)
	return q
}

func (q *typedQuery[T]) Cube(es ...Expr) Query[T] {
	q.QueryCore.Cube(es...)
	return q
}

func (q *typedQuery[T]) Having(ps ...Pred) Query[T] {
	q.QueryCore.Having(ps...)
	return q
}

func (q *typedQuery[T]) Limit(n int) Query[T] {
	q.QueryCore.Limit(n)
	return q
}

func (q *typedQuery[T]) Offset(n int) Query[T] {
	q.QueryCore.Offset(n)
	return q
}

func (q *typedQuery[T]) DistinctOn(es ...Expr) Query[T] {
	q.QueryCore.DistinctOn(es...)
	return q
}

func (q *typedQuery[T]) AllTenants() Query[T] {
	q.QueryCore.AllTenants()
	return q
}

func (q *typedQuery[T]) CustomPlan() Query[T] {
	q.QueryCore.CustomPlan()
	return q
}

func (q *typedQuery[T]) GroupingSets(sets ...[]Expr) Query[T] {
	q.QueryCore.GroupingSets(sets...)
	return q
}

func (q *typedQuery[T]) Window(name string, w Window) Query[T] {
	q.QueryCore.Window(name, w)
	return q
}

func (q *typedQuery[T]) Timeout(d time.Duration) Query[T] {
	q.QueryCore.Timeout(d)
	return q
}

func (q *typedQuery[T]) Lock(strength LockStrength, wait ...LockWait) Query[T] {
	q.QueryCore.Lock(strength, wait...)
	return q
}

func (q *typedQuery[T]) Join(t interface{ TableRef() *Ref }, on ...Pred) Query[T] {
	q.QueryCore.Join(t, on...)
	return q
}

func (q *typedQuery[T]) LeftJoin(t interface{ TableRef() *Ref }, on ...Pred) Query[T] {
	q.QueryCore.LeftJoin(t, on...)
	return q
}

func (q *typedQuery[T]) LeftJoinLateral(sub LateralSource) Query[T] {
	q.QueryCore.LeftJoinLateral(sub)
	return q
}

func (q *typedQuery[T]) JoinLateral(sub LateralSource) Query[T] {
	q.QueryCore.JoinLateral(sub)
	return q
}

func (q *typedQuery[T]) Clone() Query[T] { return &typedQuery[T]{QueryCore: *q.QueryCore.Clone()} }

func (q *typedQuery[T]) Cache(opts ...CacheOption) Query[T] {
	q.QueryCore.Cache(opts...)
	return q
}

func (q *typedQuery[T]) With(ctes ...*CTETable) Query[T] {
	q.QueryCore.With(ctes...)
	return q
}

func (q *typedQuery[T]) Compute(col Column, e Expr) Query[T] {
	q.QueryCore.Compute(col, e)
	return q
}

func (q *typedQuery[T]) LoadStrategy(s LoadStrategy) Query[T] {
	q.QueryCore.LoadStrategy(s)
	return q
}

func (q *typedQuery[T]) One(ctx context.Context, db *DB) (T, error) {
	return One[T](ctx, db, &q.QueryCore)
}

func (q *typedQuery[T]) First(ctx context.Context, db *DB) (T, error) {
	return First[T](ctx, db, &q.QueryCore)
}

func (q *typedQuery[T]) List(ctx context.Context, db *DB) ([]T, error) {
	return List[T](ctx, db, &q.QueryCore)
}

func (q *typedQuery[T]) All(ctx context.Context, db *DB) ([]T, error) {
	return All[T](ctx, db, &q.QueryCore)
}

func (q *typedQuery[T]) UpdateReturning(ctx context.Context, db *DB, sets ...Assignment) ([]T, error) {
	return UpdateReturning[T](ctx, db, &q.QueryCore, sets...)
}

func (q *typedQuery[T]) Page(ctx context.Context, db *DB, p PageRequest) (Page[T], error) {
	return PageOf[T](ctx, db, &q.QueryCore, p)
}

func (q *typedQuery[T]) Each(ctx context.Context, db *DB, size int,
	fn func(ctx context.Context, batch []T) error,
) error {
	return Each(ctx, db, &q.QueryCore, size, fn)
}
