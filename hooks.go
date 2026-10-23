package keel

import (
	"context"
	"slices"
	"sync"
	"unsafe"

	"github.com/emoss08/keel/schema"
)

// Phase is when a table hook runs.
type Phase uint8

const (
	// BeforeWrite runs inside the transaction before an entity write (Create, CreateMany, Copy,
	// Save, Upsert, CreateOrSkip, DeleteRow). It may change the row or abort the write by returning
	// an error. Bulk writes (Query.Update, Query.Delete) do not run BeforeWrite hooks.
	BeforeWrite Phase = iota
	// AfterWrite runs inside the transaction after the statement, for entity and bulk writes. An error
	// rolls the transaction back: outside a transaction, a write that may run AfterWrite hooks runs in
	// an implicit one.
	AfterWrite
	// Committed runs after the transaction commits. Errors are not returned to the caller;
	// report them from the hook.
	Committed
)

// Op is the kind of write a hook sees.
type Op uint8

const (
	OpCreate Op = iota
	OpUpdate
	OpDelete
	// OpRead is the Op an interceptor sees for queries, loads and subqueries.
	OpRead
)

func (o Op) String() string {
	switch o {
	case OpCreate:
		return "create"
	case OpUpdate:
		return "update"
	case OpRead:
		return "read"
	}
	return "delete"
}

// Mutation is one row written, as a table hook sees it.
type Mutation[T any] struct {
	Op Op
	// Row is the row as written: the entity passed to an entity write, or the row returned by a
	// bulk write. For DeleteRow it is the entity passed, which may hold only the primary key.
	Row *T
	// Old holds the stored values, before the write, of the columns the table's hooks declared
	// with Needs: for every update (Save, an Upsert's conflicting rows, bulk updates) and every
	// delete (DeleteRow, bulk deletes). Other fields are zero. Nil for creates, and when no hook
	// of the table declared Needs.
	Old *T

	changed []int32
}

// Changed reports whether the write set column c. Creates set every column, an Upsert's
// conflicting rows the columns its Conflict assigns, and deletes none.
func (m *Mutation[T]) Changed(c interface{ column() Col }) bool {
	if m.Op == OpCreate {
		return true
	}
	return slices.Contains(m.changed, c.column().Index)
}

// HookOption configures a table hook.
type HookOption func(*hookEntry)

// Needs asks for the previous values of cols on updates and deletes, read in the same statement
// (RETURNING old.…, which needs PostgreSQL 18) into Mutation.Old. It costs nothing unless the table
// has AfterWrite or Committed hooks for the write's op.
func Needs(cols ...interface{ column() Col }) HookOption {
	return func(h *hookEntry) {
		for _, c := range cols {
			h.needs = append(h.needs, c.column().Index)
		}
	}
}

type mutation struct {
	op      Op
	row     unsafe.Pointer
	old     unsafe.Pointer
	changed []int32
}

type hookEntry struct {
	ops   [4]bool
	phase Phase
	needs []int32
	fn    func(ctx context.Context, m *mutation) error
}

type tableHooks struct {
	entries []*hookEntry
	needs   []int32
}

type hooks struct {
	mu      sync.RWMutex
	byTable map[*schema.Table]*tableHooks
}

func (h *hooks) forTable(t *schema.Table) *tableHooks {
	h.mu.RLock()
	th := h.byTable[t]
	h.mu.RUnlock()
	return th
}

func (h *hooks) add(t *schema.Table, e *hookEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byTable == nil {
		h.byTable = make(map[*schema.Table]*tableHooks)
	}
	old := h.byTable[t]
	th := &tableHooks{}
	if old != nil {
		th.entries = append(th.entries, old.entries...)
		th.needs = append(th.needs, old.needs...)
	}
	th.entries = append(th.entries, e)
	for _, c := range e.needs {
		if !slices.Contains(th.needs, c) {
			th.needs = append(th.needs, c)
		}
	}
	h.byTable[t] = th
}

func (th *tableHooks) has(op Op, phase Phase) bool {
	if th == nil {
		return false
	}
	for _, e := range th.entries {
		if e.ops[op] && e.phase == phase {
			return true
		}
	}
	return false
}

func (th *tableHooks) run(ctx context.Context, op Op, phase Phase, m *mutation) error {
	if th == nil {
		return nil
	}
	for _, e := range th.entries {
		if !e.ops[op] || e.phase != phase {
			continue
		}
		if err := e.fn(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// tabler is implemented by Handle, so hook registration takes any generated table.
type tabler[T any] interface {
	TableRef() *Ref
	handle() Handle[T]
}

func (h Handle[T]) handle() Handle[T] { return h }

func register[T any](db *DB, t tabler[T], ops []Op, phase Phase,
	fn func(ctx context.Context, m *Mutation[T]) error, opts []HookOption,
) {
	e := &hookEntry{phase: phase}
	for _, op := range ops {
		e.ops[op] = true
	}
	for _, o := range opts {
		o(e)
	}
	e.fn = func(ctx context.Context, m *mutation) error {
		return fn(ctx, &Mutation[T]{
			Op: m.op, Row: (*T)(m.row), Old: (*T)(m.old), changed: m.changed,
		})
	}
	db.hooks.add(t.TableRef().Table, e)
}

// OnCreate registers a hook for creates of T's table.
func OnCreate[T any](db *DB, t tabler[T], phase Phase,
	fn func(ctx context.Context, m *Mutation[T]) error, opts ...HookOption,
) {
	register(db, t, []Op{OpCreate}, phase, fn, opts)
}

// OnUpdate registers a hook for updates of T's table: Save, Upsert conflicts and bulk updates.
func OnUpdate[T any](db *DB, t tabler[T], phase Phase,
	fn func(ctx context.Context, m *Mutation[T]) error, opts ...HookOption,
) {
	register(db, t, []Op{OpUpdate}, phase, fn, opts)
}

// OnDelete registers a hook for deletes of T's table.
func OnDelete[T any](db *DB, t tabler[T], phase Phase,
	fn func(ctx context.Context, m *Mutation[T]) error, opts ...HookOption,
) {
	register(db, t, []Op{OpDelete}, phase, fn, opts)
}

// OnWrite registers a hook for every write of T's table.
func OnWrite[T any](db *DB, t tabler[T], phase Phase,
	fn func(ctx context.Context, m *Mutation[T]) error, opts ...HookOption,
) {
	register(db, t, []Op{OpCreate, OpUpdate, OpDelete}, phase, fn, opts)
}

// Interceptor sees a statement on a table before it is rendered and may add predicates to its
// WHERE clause. It sees every statement whose base is the table and that has a WHERE clause:
//
//   - reads: every query terminal and Batch item, Page's total, projections, ToSQL, to-many
//     relation loads, Any and None, subqueries, and the source query of InsertFrom (OpRead);
//   - Save and bulk updates (OpUpdate);
//   - DeleteRow and bulk deletes (OpDelete).
//
// On Save and DeleteRow the predicates join the primary key, so a row they exclude is not
// written. Interceptors never see inserts (Create, CreateMany, Copy, Upsert, CreateOrSkip and the
// INSERT of InsertFrom), including the update an Upsert applies to a conflicting row, or tables
// joined into a statement through a relation path.
type Interceptor func(ctx context.Context, q *Intercepted) error

// Intercepted is the statement an interceptor sees: its Op (OpRead, OpUpdate or OpDelete), whether
// it is a read, and its base table.
type Intercepted struct {
	Op    Op
	Read  bool
	Table *schema.Table
	extra []Pred
}

// Where adds predicates to the intercepted statement.
func (i *Intercepted) Where(ps ...Pred) { i.extra = append(i.extra, ps...) }

type interceptors struct {
	mu      sync.RWMutex
	all     []Interceptor
	byTable map[*schema.Table][]Interceptor
}

// Intercept registers fn for the statements whose base is t's table, as Interceptor describes:
// reads, Save, DeleteRow and bulk updates and deletes. Joined relations are not intercepted.
func (db *DB) Intercept(t interface{ TableRef() *Ref }, fn Interceptor) {
	db.icpt.mu.Lock()
	defer db.icpt.mu.Unlock()
	if db.icpt.byTable == nil {
		db.icpt.byTable = make(map[*schema.Table][]Interceptor)
	}
	tbl := t.TableRef().Table
	db.icpt.byTable[tbl] = append(slices.Clip(db.icpt.byTable[tbl]), fn)
}

// InterceptAll registers fn for the statements of every table that Intercept would cover: reads,
// Save, DeleteRow and bulk updates and deletes, never inserts. It runs before the interceptors
// registered for one table.
func (db *DB) InterceptAll(fn Interceptor) {
	db.icpt.mu.Lock()
	defer db.icpt.mu.Unlock()
	db.icpt.all = append(slices.Clip(db.icpt.all), fn)
}

func (db *DB) intercept(ctx context.Context, t *schema.Table, op Op, read bool) ([]Pred, error) {
	db.icpt.mu.RLock()
	all, own := db.icpt.all, db.icpt.byTable[t]
	db.icpt.mu.RUnlock()
	if len(all) == 0 && len(own) == 0 {
		return nil, nil
	}
	ic := &Intercepted{Op: op, Read: read, Table: t}
	for _, fn := range all {
		if err := fn(ctx, ic); err != nil {
			return nil, err
		}
	}
	for _, fn := range own {
		if err := fn(ctx, ic); err != nil {
			return nil, err
		}
	}
	return ic.extra, nil
}
