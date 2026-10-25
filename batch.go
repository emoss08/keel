package keel

import "context"

// BatchItem is one read of a Batch with its typed destination. ListInto, AllInto, OneInto,
// FirstInto, PageInto, CountInto, CountUpToInto and ExistsInto read what the query terminal of the
// same name returns; IntoSlice and IntoValue read what Into and IntoOne return. An item holds no
// result, so it can run in more than one Batch.
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
func ListInto[T any](q *Query[T], dst *[]T) BatchItem {
	if dst == nil {
		return noDestination("ListInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		if q.q.limit <= 0 {
			return nil, ErrUnbounded
		}
		return fetchInto(ctx, run, &q.q, fetchOpts{}, anyRows, func(rows []T) { *dst = rows })
	}}
}

// AllInto reads every matching row into dst, as All does.
func AllInto[T any](q *Query[T], dst *[]T) BatchItem {
	if dst == nil {
		return noDestination("AllInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		return fetchInto(ctx, run, &q.q, fetchOpts{}, anyRows, func(rows []T) { *dst = rows })
	}}
}

// OneInto reads the only matching row into dst, as One does: the batch fails with ErrNotFound
// when none matches and ErrNotSingular when more do.
func OneInto[T any](q *Query[T], dst *T) BatchItem {
	if dst == nil {
		return noDestination("OneInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		return fetchInto(ctx, run, &q.q, fetchOpts{limit: 2, sizeHint: 1}, oneRow,
			func(rows []T) { *dst = rows[0] })
	}}
}

// FirstInto reads the first row in the query's order into dst, as First does: the batch fails
// with ErrNotFound when no row matches.
func FirstInto[T any](q *Query[T], dst *T) BatchItem {
	if dst == nil {
		return noDestination("FirstInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		return fetchInto(ctx, run, &q.q, fetchOpts{limit: 1, sizeHint: 1}, firstRow,
			func(rows []T) { *dst = rows[0] })
	}}
}

// PageInto reads one keyset page into dst, as Page does; a requested total is counted in the
// same round trip.
func PageInto[T any](q *Query[T], p PageRequest, dst *Page[T]) BatchItem {
	if dst == nil {
		return noDestination("PageInto")
	}
	return BatchItem{prepare: func(ctx context.Context, run *runner) (*read, error) {
		return pageRead(ctx, run, &q.q, p, dst)
	}}
}

// CountInto stores the number of matching rows in dst, as Count does.
func CountInto[T any](q *Query[T], dst *int64) BatchItem {
	if dst == nil {
		return noDestination("CountInto")
	}
	return countItem(&q.q, 0, func(n int64) { *dst = n })
}

// CountUpToInto stores the number of matching rows, counted up to limit, in dst, as CountUpTo
// does.
func CountUpToInto[T any](q *Query[T], limit int64, dst *int64) BatchItem {
	if dst == nil {
		return noDestination("CountUpToInto")
	}
	return countItem(&q.q, limit, func(n int64) { *dst = n })
}

// ExistsInto stores whether any row matches in dst, as Exists does.
func ExistsInto[T any](q *Query[T], dst *bool) BatchItem {
	if dst == nil {
		return noDestination("ExistsInto")
	}
	return countItem(&q.q, 1, func(n int64) { *dst = n > 0 })
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
