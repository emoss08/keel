package keel

import (
	"context"
	"unsafe"
)

// BatchItem is one read of a Batch with its typed destination. ListInto, AllInto, OneInto,
// FirstInto, PageInto, CountInto, CountUpToInto, CountEstimateInto and ExistsInto read what the
// query terminal of the same name returns; IntoSlice and IntoValue read what Into and IntoOne
// return. An item holds no result, so it can run in more than one Batch.
type BatchItem struct {
	prepare func(ctx context.Context, run *runner) (*read, error)
}

// Batch runs independent reads in one round trip. The main statements of every item are sent
// together, in the transaction on the context, else in an implicit transaction when
// Config.OnBegin is set (BEGIN, the OnBegin statements, the items' statements and, when no
// relation loads follow, COMMIT in one round trip), else outside a transaction. The to-many
// relation loads of all items follow, one round trip per level.
//
// Each item keeps its terminal's checks: scope errors, interceptors, AfterLoad hooks,
// ErrUnbounded for a list without a Limit, ErrNotFound and ErrNotSingular for a single-row item.
// Batch returns the first error, and fills the destinations only when every item succeeded. The
// shortest Timeout among the items bounds the whole batch.
//
//	var rows []*shipment.Shipment
//	var total int64
//	var cust *customer.Customer
//	err := keel.Batch(ctx, db,
//	    keel.ListInto(s.Where(open).Order(s.CreatedAt.Desc()).Limit(50), &rows),
//	    keel.CountInto(s.Where(open), &total),
//	    keel.OneInto(c.Where(c.ID.Eq(id)), &cust),
//	)
func Batch(ctx context.Context, db *DB, items ...BatchItem) error {
	return runReads(ctx, db, items)
}

// ListInto reads the query's rows into dst, as List does. The query must have a Limit.
func ListInto[T any](q Query[T], dst *[]T) BatchItem {
	if dst == nil {
		return noDestination("ListInto")
	}
	mustBeRowPointer[T]()
	return listItem(&q.queryCore().q, rowsDst(dst))
}

// AllInto reads every matching row into dst, as All does.
func AllInto[T any](q Query[T], dst *[]T) BatchItem {
	if dst == nil {
		return noDestination("AllInto")
	}
	mustBeRowPointer[T]()
	return allItem(&q.queryCore().q, rowsDst(dst))
}

// OneInto reads the only matching row into dst, as One does: the batch fails with ErrNotFound
// when none matches and ErrNotSingular when more do.
func OneInto[T any](q Query[T], dst *T) BatchItem {
	if dst == nil {
		return noDestination("OneInto")
	}
	mustBeRowPointer[T]()
	return oneItem(&q.queryCore().q, rowDst(dst))
}

// FirstInto reads the first row in the query's order into dst, as First does: the batch fails
// with ErrNotFound when no row matches.
func FirstInto[T any](q Query[T], dst *T) BatchItem {
	if dst == nil {
		return noDestination("FirstInto")
	}
	mustBeRowPointer[T]()
	return firstItem(&q.queryCore().q, rowDst(dst))
}

// PageInto reads one keyset page into dst, as Page does; a requested total (and its estimate) is
// counted in the same round trip.
func PageInto[T any](q Query[T], p PageRequest, dst *Page[T]) BatchItem {
	if dst == nil {
		return noDestination("PageInto")
	}
	mustBeRowPointer[T]()
	return pageItem(&q.queryCore().q, p, pageDst(dst))
}

// CountInto stores the number of matching rows in dst, as Count does.
func CountInto[T any](q Query[T], dst *int64) BatchItem {
	if dst == nil {
		return noDestination("CountInto")
	}
	return countItem(&q.queryCore().q, 0, func(n int64) { *dst = n })
}

// CountUpToInto stores the number of matching rows, counted up to limit, in dst, as CountUpTo
// does.
func CountUpToInto[T any](q Query[T], limit int64, dst *int64) BatchItem {
	if dst == nil {
		return noDestination("CountUpToInto")
	}
	return countItem(&q.queryCore().q, limit, func(n int64) { *dst = n })
}

// CountEstimateInto stores the number of matching rows, counted exactly up to exactUpTo and
// estimated by the planner above it, in dst, as CountEstimate does. Its count and EXPLAIN join
// the batch's round trip.
func CountEstimateInto[T any](q Query[T], exactUpTo int64, dst *RowCount) BatchItem {
	if dst == nil {
		return noDestination("CountEstimateInto")
	}
	return estimateItem(&q.queryCore().q, exactUpTo, dst)
}

// listItem reads q's rows into dst; q must have a Limit.
func listItem(q *query, dst *[]unsafe.Pointer) BatchItem {
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		if q.limit <= 0 {
			return nil, ErrUnbounded
		}
		return fetchRows(ctx, run, q, fetchOpts{}, anyRows, func(rows []unsafe.Pointer) { *dst = rows })
	}}
}

// allItem reads every row q matches into dst.
func allItem(q *query, dst *[]unsafe.Pointer) BatchItem {
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		return fetchRows(ctx, run, q, fetchOpts{}, anyRows, func(rows []unsafe.Pointer) { *dst = rows })
	}}
}

// oneItem reads the only row q matches into dst.
func oneItem(q *query, dst *unsafe.Pointer) BatchItem {
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		return fetchRows(ctx, run, q, fetchOpts{limit: 2, sizeHint: 1}, oneRow,
			func(rows []unsafe.Pointer) { *dst = rows[0] })
	}}
}

// firstItem reads the first row in q's order into dst.
func firstItem(q *query, dst *unsafe.Pointer) BatchItem {
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		return fetchRows(ctx, run, q, fetchOpts{limit: 1, sizeHint: 1}, firstRow,
			func(rows []unsafe.Pointer) { *dst = rows[0] })
	}}
}

// pageItem reads one keyset page of q into dst.
func pageItem(q *query, p PageRequest, dst *Page[unsafe.Pointer]) BatchItem {
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		return pageRead(ctx, run, q, p, dst)
	}}
}

// estimateItem stores q's estimated count, exact up to exactUpTo, in dst.
func estimateItem(q *query, exactUpTo int64, dst *RowCount) BatchItem {
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		e, err := newEstimate(exactUpTo)
		if err != nil {
			return nil, err
		}
		stmts, err := q.estimateStmts(ctx, run, e)
		if err != nil {
			return nil, err
		}
		return &read{stmts: stmts, timeout: q.timeout, cache: q.cacheTTL(ctx), finish: func() { *dst = e.count() }}, nil
	}}
}

// DynCountEstimateInto stores the number of rows the dynamic query returns, counted exactly up
// to exactUpTo and estimated by the planner above it, in dst, as DynQuery.CountEstimate does.
func DynCountEstimateInto(q *DynQuery, exactUpTo int64, dst *RowCount) BatchItem {
	if dst == nil {
		return noDestination("DynCountEstimateInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		if q == nil {
			return nil, errf("DynCountEstimateInto: nil dynamic query")
		}
		e, err := newEstimate(exactUpTo)
		if err != nil {
			return nil, err
		}
		stmts, err := q.estimateStmts(ctx, run, e)
		if err != nil {
			return nil, err
		}
		return &read{stmts: stmts, timeout: q.q.timeout, finish: func() { *dst = e.count() }}, nil
	}}
}

// ExistsInto stores whether any row matches in dst, as Exists does.
func ExistsInto[T any](q Query[T], dst *bool) BatchItem {
	if dst == nil {
		return noDestination("ExistsInto")
	}
	return countItem(&q.queryCore().q, 1, func(n int64) { *dst = n > 0 })
}

// IntoSlice decodes the rows of a projection into dst, as Into does.
func IntoSlice[R any](p Projection, dst *[]R) BatchItem {
	if dst == nil {
		return noDestination("IntoSlice")
	}
	return intoItem(p, anyRows, func(rows []R) { *dst = rows })
}

// IntoValue decodes the only row of a projection into dst, as IntoOne does: the batch fails with
// ErrNotFound when the projection returns no row and ErrNotSingular when it returns more.
func IntoValue[R any](p Projection, dst *R) BatchItem {
	if dst == nil {
		return noDestination("IntoValue")
	}
	return intoItem(p, oneRow, func(rows []R) { *dst = rows[0] })
}

// noDestination is the item of an item function called with a nil destination: it fails the
// batch before anything is sent.
func noDestination(name string) BatchItem {
	return BatchItem{prepare: func(context.Context, *runner) (*read, error) {
		return nil, errf("%s needs a destination", name)
	}}
}
