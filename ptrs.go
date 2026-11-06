package keel

import (
	"context"
	"slices"
	"unsafe"
)

// The functions in this file are the non-generic forms of One, List, Create and keel's other
// generic reads and writes: each takes and returns rows as unsafe.Pointer, the address of a model
// value of the table. Generated table and query types call them and convert the rows to the
// model's pointer type, so that the generated packages, of which a program has one per domain,
// compile no copy of keel's generic functions. Application code calls the generated methods, or
// the generic functions for a model keel gen does not cover.

// OnePtr is One returning the row's address.
func OnePtr(ctx context.Context, db *DB, q *QueryCore) (unsafe.Pointer, error) {
	var row unsafe.Pointer
	err := Batch(ctx, db, oneItem(&q.q, &row))
	return row, err
}

// FirstPtr is First returning the row's address.
func FirstPtr(ctx context.Context, db *DB, q *QueryCore) (unsafe.Pointer, error) {
	var row unsafe.Pointer
	err := Batch(ctx, db, firstItem(&q.q, &row))
	return row, err
}

// ListPtrs is List returning the rows' addresses.
func ListPtrs(ctx context.Context, db *DB, q *QueryCore) ([]unsafe.Pointer, error) {
	var rows []unsafe.Pointer
	err := Batch(ctx, db, listItem(&q.q, &rows))
	return rows, err
}

// AllPtrs is All returning the rows' addresses.
func AllPtrs(ctx context.Context, db *DB, q *QueryCore) ([]unsafe.Pointer, error) {
	var rows []unsafe.Pointer
	err := Batch(ctx, db, allItem(&q.q, &rows))
	return rows, err
}

// UpdateReturningPtrs is UpdateReturning returning the rows' addresses.
func UpdateReturningPtrs(ctx context.Context, db *DB, q *QueryCore, sets ...Assignment) ([]unsafe.Pointer, error) {
	var rows []unsafe.Pointer
	err := q.q.updateReturning(ctx, db, sets, func(row unsafe.Pointer) { rows = append(rows, row) })
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// PagePtrs is PageOf returning a page of the rows' addresses.
func PagePtrs(ctx context.Context, db *DB, q *QueryCore, p PageRequest) (Page[unsafe.Pointer], error) {
	var out Page[unsafe.Pointer]
	err := Batch(ctx, db, pageItem(&q.q, p, &out))
	return out, err
}

// CreatePtr is Create taking the row's address.
func CreatePtr(ctx context.Context, db *DB, t *TableBase, row unsafe.Pointer) error {
	return db.insert(ctx, t.Ref.Table, []unsafe.Pointer{row})
}

// CreateManyPtrs is CreateMany taking the rows' addresses.
func CreateManyPtrs(ctx context.Context, db *DB, t *TableBase, rows []unsafe.Pointer) error {
	if len(rows) == 0 {
		return nil
	}
	return db.insert(ctx, t.Ref.Table, slices.Clone(rows))
}

// SavePtr is Save taking the row's address.
func SavePtr(ctx context.Context, db *DB, t *TableBase, row unsafe.Pointer, cols ...Column) error {
	return db.save(ctx, t.Ref, row, cols, false)
}

// SaveExceptPtr is SaveExcept taking the row's address.
func SaveExceptPtr(ctx context.Context, db *DB, t *TableBase, row unsafe.Pointer, cols ...Column) error {
	return db.save(ctx, t.Ref, row, cols, true)
}

// DeleteRowPtr is DeleteRow taking the row's address.
func DeleteRowPtr(ctx context.Context, db *DB, t *TableBase, row unsafe.Pointer) error {
	return db.deleteRow(ctx, t.Ref, row)
}

// UpsertPtrs is Upsert taking the rows' addresses.
func UpsertPtrs(ctx context.Context, db *DB, t *TableBase, rows []unsafe.Pointer, c Conflict) ([]bool, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	return db.upsert(ctx, t.Ref, slices.Clone(rows), c)
}
