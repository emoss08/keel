package keel

import "context"

// TableBase is embedded in every generated table type. It holds the table's reference and the
// table methods that do not involve the model type: Any, None, TableRef, InsertFrom, UpsertFrom
// and their SQL renderers. It is not generic, so its methods are compiled once, in keel.
type TableBase struct {
	Ref *Ref
}

// Any is true when at least one row of the relation path matches ps: EXISTS (…).
func (t TableBase) Any(ps ...Pred) Pred { return relExists(t.Ref, ps, false) }

// None is true when no row of the relation path matches ps: NOT EXISTS (…).
func (t TableBase) None(ps ...Pred) Pred { return relExists(t.Ref, ps, true) }

func (t TableBase) loadSpec() *loadSpec { return &loadSpec{ref: t.Ref} }

// TableRef returns the table's reference.
func (t TableBase) TableRef() *Ref { return t.Ref }

// Handle is the typed API of a table of model T, the model's pointer type:
// Handle[*fleet.Shipment]. Every table type keel gen emits implements it: it embeds TableBase and
// adds the query starters, which return Query[T], and the typed writes, which call keel's generic
// functions (Create, CreateMany, Save, SaveExcept, DeleteRow, Upsert, CreateOrSkip, Copy). Hook
// registration (OnCreate, OnUpdate, OnDelete, OnWrite) takes a Handle[T].
//
// Handle is an interface rather than a generic struct so that a program pays for keel once: see
// the guide's "Why queries are interfaces".
type Handle[T any] interface {
	Loader

	// Query starts a query over the table.
	Query() Query[T]
	// Where starts a query filtered by ps.
	Where(ps ...Pred) Query[T]
	// Pick starts a query that selects only es.
	Pick(es ...Expr) Query[T]
	// Load starts a query that loads relations.
	Load(ls ...Loader) Query[T]
	// Omit starts a query that leaves cols out of the columns it reads.
	Omit(cols ...Column) Query[T]
	// Order starts an ordered query.
	Order(os ...Order) Query[T]
	// Limit starts a query limited to n rows.
	Limit(n int) Query[T]
	// Group starts a grouped query.
	Group(es ...Expr) Query[T]
	// GroupingSets starts a query grouped by GROUPING SETS.
	GroupingSets(sets ...[]Expr) Query[T]
	// Rollup starts a query grouped by ROLLUP.
	Rollup(es ...Expr) Query[T]
	// Cube starts a query grouped by CUBE.
	Cube(es ...Expr) Query[T]
	// AllTenants starts a query that crosses tenants. It needs a system scope.
	AllTenants() Query[T]
	// Join starts a query with an INNER JOIN of another table.
	Join(t interface{ TableRef() *Ref }, on ...Pred) Query[T]
	// LeftJoin starts a query with a LEFT JOIN of another table.
	LeftJoin(t interface{ TableRef() *Ref }, on ...Pred) Query[T]
	// LeftJoinLateral starts a query with LEFT JOIN LATERAL (sub) ON TRUE.
	LeftJoinLateral(sub LateralSource) Query[T]
	// JoinLateral starts a query with CROSS JOIN LATERAL (sub).
	JoinLateral(sub LateralSource) Query[T]
	// Lock starts a query that locks the rows it selects.
	Lock(strength LockStrength, wait ...LockWait) Query[T]
	// DistinctOn starts a DISTINCT ON query.
	DistinctOn(es ...Expr) Query[T]
	// Cache starts a query served from the result cache; see QueryCore.Cache.
	Cache(opts ...CacheOption) Query[T]
	// LoadStrategy starts a query that loads its relations with s.
	LoadStrategy(s LoadStrategy) Query[T]
	// With starts a query that declares CTEs.
	With(ctes ...*CTETable) Query[T]
	// Compute starts a query that fills the computed field col with e.
	Compute(col Column, e Expr) Query[T]

	// Create inserts row; see the function Create.
	Create(ctx context.Context, db *DB, row T) error
	// CreateMany inserts rows; see the function CreateMany.
	CreateMany(ctx context.Context, db *DB, rows []T) error
	// Save updates row by its primary key; see the function Save.
	Save(ctx context.Context, db *DB, row T, cols ...Column) error
	// SaveExcept is Save writing every updatable column except cols.
	SaveExcept(ctx context.Context, db *DB, row T, cols ...Column) error
	// DeleteRow deletes row by its primary key; see the function DeleteRow.
	DeleteRow(ctx context.Context, db *DB, row T) error
	// Upsert inserts rows, updating the existing row on conflict; see the function Upsert.
	Upsert(ctx context.Context, db *DB, rows []T, c Conflict) ([]bool, error)
	// CreateOrSkip inserts rows, skipping those that conflict; see the function CreateOrSkip.
	CreateOrSkip(ctx context.Context, db *DB, rows []T, targets ...Expr) (int64, error)
	// Copy inserts rows through the binary COPY protocol; see the function Copy.
	Copy(ctx context.Context, db *DB, rows []T) (int64, error)

	// Any is true when at least one row of the relation path matches ps: EXISTS (…).
	Any(ps ...Pred) Pred
	// None is true when no row of the relation path matches ps: NOT EXISTS (…).
	None(ps ...Pred) Pred
	// TableRef returns the table's reference.
	TableRef() *Ref
	// InsertFrom inserts the rows of src into cols; see TableBase.InsertFrom.
	InsertFrom(ctx context.Context, db *DB, src subquery, cols ...Column) (int64, error)
	// InsertFromSQL renders the statement InsertFrom sends, without running it.
	InsertFromSQL(ctx context.Context, db *DB, src subquery, cols ...Column) (string, []any, error)
	// UpsertFrom inserts the rows of src into cols, resolving conflicts with c; see
	// TableBase.UpsertFrom.
	UpsertFrom(ctx context.Context, db *DB, src subquery, c Conflict, cols ...Column) (int64, error)
	// UpsertFromSQL renders the statement UpsertFrom sends, without running it.
	UpsertFromSQL(ctx context.Context, db *DB, src subquery, c Conflict, cols ...Column) (string, []any, error)
}
