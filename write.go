package keel

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Create inserts row. The autoid column, the tenant columns and the autocreate and autoupdate
// columns are filled first; readonly columns are read back.
func (h Handle[T]) Create(ctx context.Context, db *DB, row *T) error {
	return db.insert(ctx, h.Ref.Table, []unsafe.Pointer{unsafe.Pointer(row)})
}

// CreateMany inserts rows with as few statements as Postgres's parameter limit allows, all sent
// in one round trip.
func (h Handle[T]) CreateMany(ctx context.Context, db *DB, rows []*T) error {
	if len(rows) == 0 {
		return nil
	}
	ps := make([]unsafe.Pointer, len(rows))
	for i, r := range rows {
		ps[i] = unsafe.Pointer(r)
	}
	return db.insert(ctx, h.Ref.Table, ps)
}

// Save updates row by its primary key. With a version column it compares the stored version,
// increments it and returns a *ConflictError (matching ErrConflict) when another writer got
// there first. cols narrows the columns written; the autoupdate column is always written.
// Interceptors' predicates join the primary key in the WHERE clause: a row they exclude is not
// written, and Save reports it as ErrConflict on a table with a version column, else ErrNotFound.
func (h Handle[T]) Save(ctx context.Context, db *DB, row *T, cols ...interface{ column() Col }) error {
	return db.save(ctx, h.Ref, unsafe.Pointer(row), cols)
}

// DeleteRow deletes row by its primary key. Interceptors' predicates join the primary key in the
// WHERE clause. It returns ErrNotFound when no row was deleted, including a row they exclude.
func (h Handle[T]) DeleteRow(ctx context.Context, db *DB, row *T) error {
	return db.deleteRow(ctx, h.Ref, unsafe.Pointer(row))
}

// Conflict is the ON CONFLICT clause of an upsert. Its methods return a changed copy.
type Conflict struct {
	cols    []Col
	where   []Pred
	update  []Col
	set     []Assignment
	nothing bool
}

// OnConflict names the unique columns an upsert conflicts on.
func OnConflict(cols ...interface{ column() Col }) Conflict {
	c := Conflict{}
	for _, col := range cols {
		c.cols = append(c.cols, col.column())
	}
	return c
}

// Where matches a partial unique index: ON CONFLICT (…) WHERE …. The predicates must not bind
// values, since Postgres matches them against the index predicate when it plans the statement.
func (c Conflict) Where(ps ...Pred) Conflict {
	c.where = append(slices.Clip(c.where), ps...)
	return c
}

// Update names the columns a conflicting row takes from the proposed row: SET col = EXCLUDED.col.
// Without Update or Set, a conflicting row takes every updatable column.
func (c Conflict) Update(cols ...interface{ column() Col }) Conflict {
	c.update = slices.Clip(c.update)
	for _, col := range cols {
		c.update = append(c.update, col.column())
	}
	return c
}

// Set adds assignments for a conflicting row, built as for Query.Update: To binds a value, Add adds
// to the stored value, ToNull, ToDefault and ToExpr set the rest. Inside an expression the table's
// columns read the stored row and a column's Excluded reads the proposed row:
//
//	keel.OnConflict(c.Code).Set(c.Name.ToExpr(c.Name.Excluded()), c.Hits.Add(1))
//
// Update and Set combine; a column must not be assigned twice. Unless Set assigns them, the
// autoupdate column takes the proposed row's stamp and the version column is incremented.
func (c Conflict) Set(sets ...Assignment) Conflict {
	c.set = append(slices.Clip(c.set), sets...)
	return c
}

// check validates the clause against ref's table and returns the columns the update of a
// conflicting row assigns, in the order the SET clause writes them: Update's, Set's (or every
// updatable column without either), then the autoupdate column.
func (c *Conflict) check(ref *Ref) ([]int32, error) {
	t := ref.Table
	if len(c.cols) == 0 {
		return nil, errf("OnConflict needs the conflicting columns")
	}
	for _, col := range c.cols {
		if col.Ref.Table != t {
			return nil, errf("OnConflict names a column of %s on %s", col.Ref.Table.Name, t.Name)
		}
	}
	if c.nothing {
		return nil, nil
	}
	var changed []int32
	assign := func(col Col) error {
		if col.Ref.Table != t {
			return errf("OnConflict assigns a column of %s on %s", col.Ref.Table.Name, t.Name)
		}
		if slices.Contains(changed, col.Index) {
			return errf("OnConflict assigns %s.%s twice", t.Name, t.Columns[col.Index].Name)
		}
		changed = append(changed, col.Index)
		return nil
	}
	for _, col := range c.update {
		if err := assign(col); err != nil {
			return nil, err
		}
	}
	for _, a := range c.set {
		if err := assign(a.col); err != nil {
			return nil, err
		}
	}
	if len(changed) == 0 {
		changed = append(changed, t.Update...)
	}
	if t.AutoUpdate >= 0 && !slices.Contains(changed, t.AutoUpdate) {
		changed = append(changed, t.AutoUpdate)
	}
	return changed, nil
}

// write writes the ON CONFLICT clause of an upsert on ref's table, inserted under the table's
// alias. changed is what check returned. Columns resolve to the stored row; Excluded is valid only
// in the SET clause.
func (c *Conflict) write(r *render, ref *Ref, changed []int32) {
	t := ref.Table
	r.s(" ON CONFLICT (")
	for i, col := range c.cols {
		if i > 0 {
			r.s(", ")
		}
		r.s(t.Columns[col.Index].Quoted)
	}
	r.b(')')
	if hasPreds(c.where) {
		bound := len(r.args)
		r.bare = true
		r.s(" WHERE ")
		r.preds(c.where, false)
		r.bare = false
		if len(r.args) > bound {
			r.fail(errf("OnConflict Where must not bind values; use constants"))
		}
	}
	if c.nothing {
		r.s(" DO NOTHING")
		return
	}
	sc := &scope{parent: r.sc, joinMode: joinNone}
	sc.refs = append(sc.refs, scoped{ref: ref, alias: t.Alias})
	r.sc = sc
	r.excluded = true
	defer func() {
		r.pop()
		r.excluded = false
	}()
	r.s(" DO UPDATE SET ")
	for i, ci := range changed {
		if i > 0 {
			r.s(", ")
		}
		q := t.Columns[ci].Quoted
		r.s(q)
		if a := c.assignment(ci); a != nil {
			r.s(" = ")
			a.e(r)
			continue
		}
		r.s(" = EXCLUDED.")
		r.s(q)
	}
	if t.Version >= 0 && !slices.Contains(changed, t.Version) {
		q := t.Columns[t.Version].Quoted
		r.s(", ")
		r.s(q)
		r.s(" = ")
		r.s(t.Alias)
		r.b('.')
		r.s(q)
		r.s(" + 1")
	}
}

// assignment returns the Set assignment of column ci, or nil when the column takes the proposed
// row's value.
func (c *Conflict) assignment(ci int32) *Assignment {
	for i := range c.set {
		if c.set[i].col.Index == ci {
			return &c.set[i]
		}
	}
	return nil
}

// Upsert inserts rows, updating the existing row on conflict. It reports, per row, whether it
// was inserted (true) or updated (false). Rows that conflict get the stored version and readonly
// columns back. Inserted rows run the create hooks and updated rows the update hooks; outside a
// transaction the upsert runs in an implicit one when the table has AfterWrite hooks for either,
// so a failing hook rolls every row back.
func (h Handle[T]) Upsert(ctx context.Context, db *DB, rows []*T, c Conflict) ([]bool, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ps := make([]unsafe.Pointer, len(rows))
	for i, r := range rows {
		ps[i] = unsafe.Pointer(r)
	}
	return db.upsert(ctx, h.Ref, ps, c)
}

// CreateOrSkip inserts rows, skipping those that conflict on cols, and returns how many were
// inserted. Inserted rows run the create hooks; skipped rows run none.
func (h Handle[T]) CreateOrSkip(ctx context.Context, db *DB, rows []*T,
	cols ...interface{ column() Col },
) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	ps := make([]unsafe.Pointer, len(rows))
	for i, r := range rows {
		ps[i] = unsafe.Pointer(r)
	}
	c := OnConflict(cols...)
	c.nothing = true
	ins, err := db.upsert(ctx, h.Ref, ps, c)
	var n int64
	for _, b := range ins {
		if b {
			n++
		}
	}
	return n, err
}

// InsertFrom inserts the rows of src into cols: INSERT INTO t (cols) SELECT …. src must pick one
// expression per column, in order. Under a tenant scope the tenant columns are filled from the
// scope and must not be listed.
func (h Handle[T]) InsertFrom(ctx context.Context, db *DB, src subquery,
	cols ...interface{ column() Col },
) (int64, error) {
	return db.insertFrom(ctx, h.Ref, src, cols)
}

// hookCtx returns the context a write's hooks run with: ctx carrying the runner's implicit
// transaction, so a hook's own statements through keel (Exec, reads, writes) join the write and
// roll back with it, or ctx itself when the runner owns no transaction.
func (r *runner) hookCtx(ctx context.Context) context.Context {
	if !r.own || r.tx == nil {
		return ctx
	}
	return context.WithValue(ctx, txKey{}, r.tx)
}

// writeRunner returns the runner of a write. With tx, set when the write may run AfterWrite hooks,
// it opens an implicit transaction outside one, so a failing hook rolls the write back.
func (db *DB) writeRunner(ctx context.Context, tx bool) (*runner, error) {
	r, err := db.newRunner(ctx, true)
	if err != nil {
		return nil, err
	}
	if r.tx == nil && tx {
		r.tx = &txState{db: db, implicit: true, scopeCtx: ctx}
		r.own = true
	}
	return r, nil
}

// prepareInsert fills the autoid, tenant and timestamp columns of row and runs the Before hooks.
func (db *DB) prepareInsert(ctx context.Context, sc *scopeState, t *schema.Table, th *tableHooks,
	row unsafe.Pointer, st stamps,
) error {
	if t.AutoID >= 0 {
		if zeroAt(t, row, t.AutoID) {
			if db.cfg.NewID == nil {
				return errf("%s.%s is autoid but Config.NewID is not set", t.Name,
					t.Columns[t.AutoID].Name)
			}
			if err := assign(t.Ptr(row, t.AutoID), db.cfg.NewID(t.IDPrefix)); err != nil {
				return err
			}
		}
	}
	if err := fillTenant(sc, t, row); err != nil {
		return err
	}
	if t.AutoCreate >= 0 {
		if err := assign(t.Ptr(row, t.AutoCreate), st.created); err != nil {
			return err
		}
	}
	if t.AutoUpdate >= 0 {
		if err := assign(t.Ptr(row, t.AutoUpdate), st.updated); err != nil {
			return err
		}
	}
	if t.BeforeCreate != nil {
		if err := t.BeforeCreate(ctx, row); err != nil {
			return err
		}
	}
	if t.BeforeSave != nil {
		if err := t.BeforeSave(ctx, row); err != nil {
			return err
		}
	}
	return th.run(ctx, OpCreate, BeforeWrite, &mutation{op: OpCreate, row: row})
}

// fillTenant sets a row's zero tenant columns from the scope and refuses a row of another tenant.
func fillTenant(sc *scopeState, t *schema.Table, row unsafe.Pointer) error {
	if !t.HasTenant() {
		return nil
	}
	if sc.system {
		for _, ci := range t.Tenant {
			if zeroAt(t, row, ci) {
				return errf("%s.%s is empty; a system scope must set tenant columns", t.Name,
					t.Columns[ci].Name)
			}
		}
		return nil
	}
	if sc.tenant == nil {
		return fmt.Errorf("%w: %s", ErrNoScope, t.Name)
	}
	for _, ci := range t.Tenant {
		pos := sc.tenantPos(t.Columns[ci].Name)
		if pos < 0 {
			return errf("tenant column %s.%s is not in Config.TenantColumns", t.Name,
				t.Columns[ci].Name)
		}
		if zeroAt(t, row, ci) {
			if err := assign(t.Ptr(row, ci), sc.tenant[pos]); err != nil {
				return err
			}
			continue
		}
		if !equalAt(t, row, ci, sc.tenant[pos]) {
			return fmt.Errorf("%w: %s.%s", ErrTenantMismatch, t.Name, t.Columns[ci].Name)
		}
	}
	return nil
}

func (db *DB) insert(ctx context.Context, t *schema.Table, rows []unsafe.Pointer) error {
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th.has(OpCreate, AfterWrite))
	if err != nil {
		return err
	}
	st := db.stampsFor(t)
	hctx := run.hookCtx(ctx)
	for _, row := range rows {
		if err := db.prepareInsert(hctx, run.sc, t, th, row, st); err != nil {
			return run.close(ctx, err)
		}
	}
	stmts, err := db.insertStmts(ctx, t, rows, &insertSpec{})
	if err != nil {
		return run.close(ctx, err)
	}
	run.touchRows(t, rows)
	err = run.batch(ctx, stmts, !th.has(OpCreate, AfterWrite))
	if err == nil {
		err = db.afterWrite(ctx, run, th, OpCreate, rows, nil, nil)
	}
	return run.close(ctx, err)
}

// insertSpec shapes the INSERT statements of a write. An upsert sets conflict and its changed
// columns, the scope its SET clause renders under, inserted, which its scanner fills, and, when
// its update hooks need previous values, needs and olds, which its scanner fills for the rows
// that conflicted. CreateOrSkip sets skip and keys instead.
type insertSpec struct {
	conflict *Conflict
	ref      *Ref
	changed  []int32
	sc       *scopeState
	upsert   bool
	skip     bool
	inserted []bool
	keys     map[string]bool
	needs    []int32
	olds     []unsafe.Pointer
}

// insertStmts builds the INSERT statements of a write. One row is inserted with VALUES; more rows
// are inserted through unnest with one array per column, in chunks that share one statement text.
func (db *DB) insertStmts(ctx context.Context, t *schema.Table, rows []unsafe.Pointer, spec *insertSpec) ([]*stmt, error) {
	var types []string
	if len(rows) > 1 {
		var err error
		if types, err = db.columnTypes(ctx, t); err != nil {
			return nil, err
		}
	}
	var ret []int32
	switch {
	case spec.skip:
		ret = t.PK
	case spec.upsert:
		ret = returningCols(t, true)
	default:
		ret = t.Returning
	}
	stmts := make([]*stmt, 0, (len(rows)+unnestChunk-1)/unnestChunk)
	for start := 0; start < len(rows); start += unnestChunk {
		chunk := rows[start:min(start+unnestChunk, len(rows))]
		r := newRender(spec.sc, ctx, db)
		r.s("INSERT INTO ")
		r.s(t.Quoted)
		r.s(" AS ")
		r.s(t.Alias)
		r.s(" (")
		for i, c := range t.Insert {
			if i > 0 {
				r.s(", ")
			}
			r.s(t.Columns[c].Quoted)
		}
		r.s(") ")
		if types == nil {
			r.s("VALUES (")
			for i, c := range t.Insert {
				if i > 0 {
					r.s(", ")
				}
				r.arg(t.Value(chunk[0], c))
			}
			r.b(')')
		} else if err := db.writeUnnestSource(r, t, types, chunk); err != nil {
			r.recycle()
			return nil, err
		}
		if spec.conflict != nil {
			spec.conflict.write(r, spec.ref, spec.changed)
		}
		if spec.upsert || len(ret) > 0 {
			r.s(" RETURNING ")
			first := true
			if spec.upsert {
				r.s("(xmax = 0)")
				first = false
			}
			for _, c := range ret {
				if !first {
					r.s(", ")
				}
				first = false
				r.s(t.Alias)
				r.b('.')
				r.s(t.Columns[c].Quoted)
			}
			for _, c := range spec.needs {
				r.s(", old.")
				r.s(t.Columns[c].Quoted)
			}
		}
		if r.err != nil {
			err := r.err
			r.recycle()
			return nil, err
		}
		s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "insert", table: t.Name}
		r.recycle()
		switch {
		case spec.skip:
			s.scan = skipScanner(t, spec)
		case spec.upsert:
			base := start
			var olds []unsafe.Pointer
			if spec.olds != nil {
				olds = spec.olds[base : base+len(chunk)]
			}
			s.scan = returningScanner(t, chunk, ret, spec.inserted[base:base+len(chunk)],
				spec.needs, olds)
		case len(ret) > 0:
			s.scan = returningScanner(t, chunk, ret, nil, nil, nil)
		}
		stmts = append(stmts, s)
	}
	return stmts, nil
}

// returningCols are the columns read back after an insert: readonly columns, and on upserts the
// version, which a conflicting row increments.
func returningCols(t *schema.Table, upsert bool) []int32 {
	if !upsert || t.Version < 0 {
		return t.Returning
	}
	out := append([]int32(nil), t.Returning...)
	return append(out, t.Version)
}

// returningScanner reads RETURNING rows back into rows, in order. For an upsert, inserted is
// non-nil: the first column says whether each row was inserted, and with olds the columns after
// cols, the previous values of needs, are read into a new row in olds for each row that conflicted.
func returningScanner(t *schema.Table, rows []unsafe.Pointer, cols []int32, inserted []bool,
	needs []int32, olds []unsafe.Pointer,
) func(pgx.Rows) error {
	return func(res pgx.Rows) error {
		m := res.Conn().TypeMap()
		fds := res.FieldDescriptions()
		off := 0
		if inserted != nil {
			off = 1
		}
		plans := make([]pgtype.ScanPlan, off+len(cols)+len(needs))
		i := 0
		for res.Next() {
			if i >= len(rows) {
				break
			}
			raw := res.RawValues()
			if inserted != nil {
				if plans[0] == nil {
					plans[0] = m.PlanScan(fds[0].DataTypeOID, fds[0].Format, &inserted[i])
				}
				if err := plans[0].Scan(raw[0], &inserted[i]); err != nil {
					return err
				}
			}
			for j, c := range cols {
				if err := scanAt(m, fds, raw, plans, off+j, t, t.Ptr(rows[i], c), c); err != nil {
					return err
				}
			}
			if olds != nil && !inserted[i] {
				old := t.New()
				for j, c := range needs {
					k := off + len(cols) + j
					if err := scanAt(m, fds, raw, plans, k, t, t.Ptr(old, c), c); err != nil {
						return err
					}
				}
				olds[i] = old
			}
			i++
		}
		return nil
	}
}

// scanAt decodes column k of a row into dst, the field of table column c, planning the scan on
// first use.
func scanAt(m *pgtype.Map, fds []pgconn.FieldDescription, raw [][]byte, plans []pgtype.ScanPlan,
	k int, t *schema.Table, dst any, c int32,
) error {
	if plans[k] == nil {
		plans[k] = planColumn(m, fds[k].DataTypeOID, fds[k].Format, &t.Columns[c], dst)
	}
	if err := plans[k].Scan(raw[k], dst); err != nil {
		return fmt.Errorf("keel: scan %s.%s: %w", t.Name, t.Columns[c].Name, err)
	}
	return nil
}

// skipScanner collects the primary keys of the rows an insert … ON CONFLICT DO NOTHING inserted.
func skipScanner(t *schema.Table, spec *insertSpec) func(pgx.Rows) error {
	return func(res pgx.Rows) error {
		for res.Next() {
			vals, err := res.Values()
			if err != nil {
				return err
			}
			spec.keys[keyOf(vals)] = true
		}
		return nil
	}
}

func keyOf(vals []any) string {
	b := make([]byte, 0, 64)
	for _, v := range vals {
		b = fmt.Appendf(b, "%v\x00", normalize(v))
	}
	return string(b)
}

func rowKey(t *schema.Table, row unsafe.Pointer) string {
	vals := make([]any, len(t.PK))
	for i, c := range t.PK {
		vals[i] = t.Value(row, c)
	}
	return keyOf(vals)
}

func (db *DB) upsert(ctx context.Context, ref *Ref, rows []unsafe.Pointer, c Conflict) ([]bool, error) {
	t := ref.Table
	changed, err := c.check(ref)
	if err != nil {
		return nil, err
	}
	th := db.hooks.forTable(t)
	afterHooks := th.has(OpCreate, AfterWrite) || !c.nothing && th.has(OpUpdate, AfterWrite)
	run, err := db.writeRunner(ctx, afterHooks)
	if err != nil {
		return nil, err
	}
	st := db.stampsFor(t)
	hctx := run.hookCtx(ctx)
	for _, row := range rows {
		if err := db.prepareInsert(hctx, run.sc, t, th, row, st); err != nil {
			return nil, run.close(ctx, err)
		}
	}
	spec := &insertSpec{conflict: &c, ref: ref, changed: changed, sc: run.sc, upsert: !c.nothing,
		skip: c.nothing, inserted: make([]bool, len(rows))}
	if c.nothing {
		spec.keys = make(map[string]bool, len(rows))
	} else if th != nil && len(th.needs) > 0 &&
		(th.has(OpUpdate, AfterWrite) || th.has(OpUpdate, Committed)) {
		spec.needs = th.needs
		spec.olds = make([]unsafe.Pointer, len(rows))
	}
	inserted := spec.inserted
	stmts, err := db.insertStmts(ctx, t, rows, spec)
	if err != nil {
		return nil, run.close(ctx, err)
	}
	run.touchRows(t, rows)
	err = run.batch(ctx, stmts, !afterHooks)
	if err == nil && c.nothing {
		for i, row := range rows {
			inserted[i] = spec.keys[rowKey(t, row)]
		}
	}
	if err == nil {
		var created, updated, olds []unsafe.Pointer
		for i, row := range rows {
			switch {
			case inserted[i]:
				created = append(created, row)
			case !c.nothing:
				updated = append(updated, row)
				if spec.olds != nil {
					olds = append(olds, spec.olds[i])
				}
			}
		}
		err = db.afterWrite(ctx, run, th, OpCreate, created, nil, nil)
		if err == nil {
			err = db.afterWrite(ctx, run, th, OpUpdate, updated, olds, changed)
		}
	}
	return inserted, run.close(ctx, err)
}

func (db *DB) save(ctx context.Context, ref *Ref, row unsafe.Pointer, cols []interface{ column() Col }) error {
	t := ref.Table
	if len(t.PK) == 0 {
		return errf("%s has no primary key; Save needs one", t.Name)
	}
	update := t.Update
	if len(cols) > 0 {
		update = make([]int32, 0, len(cols)+1)
		for _, c := range cols {
			col := c.column()
			if col.Ref.Table != t || !slices.Contains(t.Update, col.Index) {
				return errf("Save cannot write %s.%s", col.Ref.Table.Name,
					col.Ref.Table.Columns[col.Index].Name)
			}
			update = append(update, col.Index)
		}
		if t.AutoUpdate >= 0 && !slices.Contains(update, t.AutoUpdate) {
			update = append(update, t.AutoUpdate)
		}
	}
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th.has(OpUpdate, AfterWrite))
	if err != nil {
		return err
	}
	if err := db.prepareSave(run.hookCtx(ctx), run.sc, t, th, row, update); err != nil {
		return run.close(ctx, err)
	}

	var needs []int32
	if th != nil {
		needs = th.needs
	}
	r := newRender(run.sc, ctx, db)
	defer r.recycle()
	r.s("UPDATE ")
	r.s(t.Quoted)
	r.b(' ')
	r.s(t.Alias)
	r.s(" SET ")
	for i, c := range update {
		if i > 0 {
			r.s(", ")
		}
		r.s(t.Columns[c].Quoted)
		r.s(" = ")
		r.arg(t.Value(row, c))
	}
	var oldVersion int64
	if t.Version >= 0 {
		v, _ := t.Value(row, t.Version).(int64)
		oldVersion = v
		q := t.Columns[t.Version].Quoted
		r.s(", ")
		r.s(q)
		r.s(" = ")
		r.s(t.Alias)
		r.b('.')
		r.s(q)
		r.s(" + 1")
	}
	from, guard := r.interceptRow(ref, OpUpdate, " FROM ")
	r.buf = append(r.buf, from...)
	r.s(" WHERE ")
	pkWhere(r, t, row)
	if t.Version >= 0 {
		r.s(" AND ")
		r.s(t.Alias)
		r.b('.')
		r.s(t.Columns[t.Version].Quoted)
		r.s(" = ")
		r.arg(oldVersion)
	}
	if t.HasTenant() && !run.sc.system {
		r.s(" AND ")
		r.tenantPred(t, t.Alias, false)
	}
	if len(guard) > 0 {
		r.s(" AND ")
		r.buf = append(r.buf, guard...)
	}
	r.release(from, guard)
	ret := append([]int32(nil), t.Returning...)
	if t.Version >= 0 {
		ret = append(ret, t.Version)
	}
	var old unsafe.Pointer
	if len(needs) > 0 && th.has(OpUpdate, AfterWrite) || len(needs) > 0 && th.has(OpUpdate, Committed) {
		old = t.New()
	}
	if len(ret) > 0 || old != nil {
		r.s(" RETURNING ")
		first := true
		for _, c := range ret {
			if !first {
				r.s(", ")
			}
			first = false
			r.s(t.Alias)
			r.b('.')
			r.s(t.Columns[c].Quoted)
		}
		if old != nil {
			for _, c := range needs {
				if !first {
					r.s(", ")
				}
				first = false
				r.s("old.")
				r.s(t.Columns[c].Quoted)
			}
		}
	}
	if r.err != nil {
		return run.close(ctx, r.err)
	}
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "update", table: t.Name}
	found := false
	if len(ret) > 0 || old != nil {
		s.scan = func(rows pgx.Rows) error {
			if !rows.Next() {
				return nil
			}
			found = true
			return scanInto(rows, t, row, ret, old, needs)
		}
	}
	run.touchRow(t, row)
	err = run.one(ctx, s, !th.has(OpUpdate, AfterWrite))
	if err == nil {
		if s.scan == nil {
			found = s.tag.RowsAffected() > 0
		}
		if !found {
			if t.Version >= 0 {
				err = &ConflictError{Table: t.Name, Version: oldVersion}
			} else {
				err = ErrNotFound
			}
		}
	}
	if err == nil {
		var olds []unsafe.Pointer
		if old != nil {
			olds = []unsafe.Pointer{old}
		}
		err = db.afterWrite(ctx, run, th, OpUpdate, []unsafe.Pointer{row}, olds, update)
	}
	return run.close(ctx, err)
}

func (db *DB) prepareSave(ctx context.Context, sc *scopeState, t *schema.Table, th *tableHooks,
	row unsafe.Pointer, update []int32,
) error {
	if err := checkTenant(sc, t, row); err != nil {
		return err
	}
	if t.AutoUpdate >= 0 {
		if err := assign(t.Ptr(row, t.AutoUpdate), stampValue(t.Columns[t.AutoUpdate].Kind,
			db.now())); err != nil {
			return err
		}
	}
	if t.BeforeUpdate != nil {
		if err := t.BeforeUpdate(ctx, row); err != nil {
			return err
		}
	}
	if t.BeforeSave != nil {
		if err := t.BeforeSave(ctx, row); err != nil {
			return err
		}
	}
	return th.run(ctx, OpUpdate, BeforeWrite, &mutation{op: OpUpdate, row: row, changed: update})
}

// checkTenant refuses a row whose tenant columns differ from the tenant scope.
func checkTenant(sc *scopeState, t *schema.Table, row unsafe.Pointer) error {
	if !t.HasTenant() || sc.system {
		return nil
	}
	if sc.tenant == nil {
		return fmt.Errorf("%w: %s", ErrNoScope, t.Name)
	}
	for _, ci := range t.Tenant {
		pos := sc.tenantPos(t.Columns[ci].Name)
		if pos < 0 {
			continue
		}
		if !equalAt(t, row, ci, sc.tenant[pos]) {
			return fmt.Errorf("%w: %s.%s", ErrTenantMismatch, t.Name, t.Columns[ci].Name)
		}
	}
	return nil
}

// interceptRow renders the predicates interceptors add to a single-row UPDATE or DELETE of ref's
// table, which the statement writes under the table's alias. It returns the relations those
// predicates join, as FROM or USING items (keyword), and the predicates with the joins'
// conditions, for the WHERE clause. Both are nil when no interceptor adds a predicate.
func (r *render) interceptRow(ref *Ref, op Op, keyword string) (from, where []byte) {
	ps := r.intercepted(ref.Table, op, false)
	if !hasPreds(ps) {
		return nil, nil
	}
	sc := &scope{parent: r.sc, joinMode: joinFrom, allTenants: r.scope.system}
	sc.refs = append(sc.refs, scoped{ref: ref, alias: r.newAlias(ref.Table.Alias)})
	r.sc = sc
	defer r.pop()
	where = r.section(func() { r.preds(ps, false) })
	var conds []byte
	from = r.section(func() { conds = r.writeFromJoins(sc, keyword) })
	if len(conds) > 0 {
		where = append(where, " AND "...)
		where = append(where, conds...)
		r.release(conds)
	}
	return from, where
}

func pkWhere(r *render, t *schema.Table, row unsafe.Pointer) {
	for i, c := range t.PK {
		if i > 0 {
			r.s(" AND ")
		}
		r.s(t.Alias)
		r.b('.')
		r.s(t.Columns[c].Quoted)
		r.s(" = ")
		r.arg(t.Value(row, c))
	}
}

// scanInto reads one RETURNING row: ret columns into row, then needs columns into old.
func scanInto(rows pgx.Rows, t *schema.Table, row unsafe.Pointer, ret []int32, old unsafe.Pointer,
	needs []int32,
) error {
	m := rows.Conn().TypeMap()
	fds := rows.FieldDescriptions()
	raw := rows.RawValues()
	i := 0
	for _, c := range ret {
		dst := t.Ptr(row, c)
		if err := planColumn(m, fds[i].DataTypeOID, fds[i].Format, &t.Columns[c], dst).Scan(raw[i], dst); err != nil {
			return fmt.Errorf("keel: scan %s.%s: %w", t.Name, t.Columns[c].Name, err)
		}
		i++
	}
	if old == nil {
		return nil
	}
	for _, c := range needs {
		dst := t.Ptr(old, c)
		if err := planColumn(m, fds[i].DataTypeOID, fds[i].Format, &t.Columns[c], dst).Scan(raw[i], dst); err != nil {
			return fmt.Errorf("keel: scan old %s.%s: %w", t.Name, t.Columns[c].Name, err)
		}
		i++
	}
	return nil
}

func (db *DB) deleteRow(ctx context.Context, ref *Ref, row unsafe.Pointer) error {
	t := ref.Table
	if len(t.PK) == 0 {
		return errf("%s has no primary key; DeleteRow needs one", t.Name)
	}
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th.has(OpDelete, AfterWrite))
	if err != nil {
		return err
	}
	if err := checkTenant(run.sc, t, row); err != nil {
		return run.close(ctx, err)
	}
	hctx := run.hookCtx(ctx)
	if t.BeforeDelete != nil {
		if err := t.BeforeDelete(hctx, row); err != nil {
			return run.close(ctx, err)
		}
	}
	if err := th.run(hctx, OpDelete, BeforeWrite, &mutation{op: OpDelete, row: row}); err != nil {
		return run.close(ctx, err)
	}
	r := newRender(run.sc, ctx, db)
	defer r.recycle()
	r.s("DELETE FROM ")
	r.s(t.Quoted)
	r.b(' ')
	r.s(t.Alias)
	using, guard := r.interceptRow(ref, OpDelete, " USING ")
	r.buf = append(r.buf, using...)
	r.s(" WHERE ")
	pkWhere(r, t, row)
	if t.HasTenant() && !run.sc.system {
		r.s(" AND ")
		r.tenantPred(t, t.Alias, false)
	}
	if len(guard) > 0 {
		r.s(" AND ")
		r.buf = append(r.buf, guard...)
	}
	r.release(using, guard)
	var old unsafe.Pointer
	if th != nil && len(th.needs) > 0 && (th.has(OpDelete, AfterWrite) || th.has(OpDelete, Committed)) {
		old = t.New()
		r.s(" RETURNING ")
		for i, c := range th.needs {
			if i > 0 {
				r.s(", ")
			}
			r.s("old.")
			r.s(t.Columns[c].Quoted)
		}
	}
	if r.err != nil {
		return run.close(ctx, r.err)
	}
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "delete", table: t.Name}
	if old != nil {
		s.scan = func(rows pgx.Rows) error {
			if !rows.Next() {
				return nil
			}
			return scanInto(rows, t, row, nil, old, th.needs)
		}
	}
	run.touchRow(t, row)
	err = run.one(ctx, s, !th.has(OpDelete, AfterWrite))
	if err == nil && s.tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	if err == nil {
		var olds []unsafe.Pointer
		if old != nil {
			olds = []unsafe.Pointer{old}
		}
		err = db.afterWrite(ctx, run, th, OpDelete, []unsafe.Pointer{row}, olds, nil)
	}
	return run.close(ctx, err)
}

func (db *DB) insertFrom(ctx context.Context, ref *Ref, src subquery,
	cols []interface{ column() Col },
) (int64, error) {
	t := ref.Table
	if len(cols) == 0 {
		return 0, errf("InsertFrom needs the target columns")
	}
	run, err := db.newRunner(ctx, true)
	if err != nil {
		return 0, err
	}
	r := newRender(run.sc, ctx, db)
	defer r.recycle()
	target := make([]int32, 0, len(cols)+len(t.Tenant))
	for _, c := range cols {
		col := c.column()
		if col.Ref.Table != t {
			return 0, run.close(ctx, errf("InsertFrom names a column of %s on %s",
				col.Ref.Table.Name, t.Name))
		}
		if col.Ref.Table.Columns[col.Index].Flags.Has(schema.FlagTenant) && !run.sc.system {
			return 0, run.close(ctx, errf("InsertFrom fills tenant columns from the scope; do not list %s",
				t.Columns[col.Index].Name))
		}
		target = append(target, col.Index)
	}
	tenantCols := 0
	if t.HasTenant() && !run.sc.system {
		if run.sc.tenant == nil {
			return 0, run.close(ctx, fmt.Errorf("%w: %s", ErrNoScope, t.Name))
		}
		target = append(target, t.Tenant...)
		tenantCols = len(t.Tenant)
	}
	r.s("INSERT INTO ")
	r.s(t.Quoted)
	r.s(" (")
	for i, c := range target {
		if i > 0 {
			r.s(", ")
		}
		r.s(t.Columns[c].Quoted)
	}
	r.s(") ")
	listed := target[:len(target)-tenantCols]
	switch {
	case hasNullZeroLiteral(t, listed):
		writeNullZeroSource(r, run.sc, t, listed, tenantCols > 0, src)
	case tenantCols > 0:
		r.s("SELECT s.*")
		for _, ci := range t.Tenant {
			r.s(", ")
			r.tenantArg(run.sc.tenantPos(t.Columns[ci].Name))
		}
		r.s(" FROM (")
		src.writeSubquery(r, false)
		r.s(") s")
	default:
		src.writeSubquery(r, false)
	}
	if r.err != nil {
		return 0, run.close(ctx, r.err)
	}
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "insert", table: t.Name}
	n, err := run.bulk(ctx, s, t, "", nil, true)
	return n, run.close(ctx, err)
}

// nullZeroLiterals are the SQL literals of the zero values of the kinds a nullzero column compares
// with in InsertFrom; the zero values of the other kinds (nil slices and maps) are NULL already.
var nullZeroLiterals = map[schema.Kind]string{
	schema.KindString: "''", schema.KindTSVector: "''", schema.KindInt16: "0", schema.KindInt32: "0",
	schema.KindInt64: "0", schema.KindFloat32: "0", schema.KindFloat64: "0", schema.KindBool: "false",
	schema.KindTime: "'0001-01-01 00:00:00+00'",
}

// hasNullZeroLiteral reports whether a column of cols is nullzero with a zero value that is not
// NULL in SQL, so InsertFrom must turn it into NULL.
func hasNullZeroLiteral(t *schema.Table, cols []int32) bool {
	for _, c := range cols {
		if t.Columns[c].Flags.Has(schema.FlagNullZero) && nullZeroLiterals[t.Columns[c].Kind] != "" {
			return true
		}
	}
	return false
}

// writeNullZeroSource writes the SELECT of an InsertFrom whose listed columns include nullzero
// ones: SELECT nullif(s.c1, <zero literal>), s.c2, … [, tenant values] FROM (src) AS s(c1, c2, …),
// so a query that yields a zero value stores NULL, as every other write does.
func writeNullZeroSource(r *render, sc *scopeState, t *schema.Table, cols []int32, tenant bool, src subquery) {
	r.s("SELECT ")
	for i, c := range cols {
		if i > 0 {
			r.s(", ")
		}
		lit := ""
		if t.Columns[c].Flags.Has(schema.FlagNullZero) {
			lit = nullZeroLiterals[t.Columns[c].Kind]
		}
		if lit != "" {
			r.s("nullif(")
		}
		r.s("s.")
		r.s(outputName(i))
		if lit != "" {
			r.s(", ")
			r.s(lit)
			r.b(')')
		}
	}
	if tenant {
		for _, ci := range t.Tenant {
			r.s(", ")
			r.tenantArg(sc.tenantPos(t.Columns[ci].Name))
		}
	}
	r.s(" FROM (")
	src.writeSubquery(r, false)
	r.s(") AS s(")
	for i := range cols {
		if i > 0 {
			r.s(", ")
		}
		r.s(outputName(i))
	}
	r.b(')')
}

// afterWrite runs the After hooks now, in the write's transaction, and queues the Committed hooks.
func (db *DB) afterWrite(ctx context.Context, run *runner, th *tableHooks, op Op,
	rows, olds []unsafe.Pointer, changed []int32,
) error {
	if th == nil || len(rows) == 0 {
		return nil
	}
	hctx := run.hookCtx(ctx)
	for i, row := range rows {
		m := &mutation{op: op, row: row, changed: changed}
		if i < len(olds) {
			m.old = olds[i]
		}
		if err := th.run(hctx, op, AfterWrite, m); err != nil {
			return err
		}
	}
	if !th.has(op, Committed) {
		return nil
	}
	fire := func(ctx context.Context) {
		for i, row := range rows {
			m := &mutation{op: op, row: row, changed: changed}
			if i < len(olds) {
				m.old = olds[i]
			}
			_ = th.run(ctx, op, Committed, m)
		}
	}
	if run.tx != nil {
		run.tx.afterCommit = append(run.tx.afterCommit, fire)
		return nil
	}
	fire(ctx)
	return nil
}

// stamps are the autocreate and autoupdate values of one write, boxed once for all its rows.
type stamps struct {
	created any
	updated any
}

func (db *DB) stampsFor(t *schema.Table) stamps {
	now := db.now()
	var st stamps
	if t.AutoCreate >= 0 {
		st.created = stampValue(t.Columns[t.AutoCreate].Kind, now)
	}
	if t.AutoUpdate >= 0 {
		st.updated = stampValue(t.Columns[t.AutoUpdate].Kind, now)
	}
	return st
}

// zeroAt reports whether a column of row holds its zero value, reading through the field pointer
// so nothing is boxed.
func zeroAt(t *schema.Table, row unsafe.Pointer, col int32) bool {
	switch p := t.Ptr(row, col).(type) {
	case *string:
		return *p == ""
	case *int64:
		return *p == 0
	case *int32:
		return *p == 0
	case **string:
		return *p == nil || **p == ""
	}
	return isZero(t.Value(row, col))
}

// equalAt reports whether a column of row equals v, a normalised scope value.
func equalAt(t *schema.Table, row unsafe.Pointer, col int32, v any) bool {
	switch p := t.Ptr(row, col).(type) {
	case *string:
		s, ok := v.(string)
		return ok && *p == s
	case *int64:
		n, ok := v.(int64)
		return ok && *p == n
	}
	return normalize(t.Value(row, col)) == v
}

func stampValue(k schema.Kind, t time.Time) any {
	if k == schema.KindTime {
		return t
	}
	return t.Unix()
}

func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case int64:
		return x == 0
	case int32:
		return x == 0
	}
	rv := reflect.ValueOf(v)
	return rv.IsZero()
}

// normalize turns a value of a named basic type into its basic type, so tenant values compare
// equal however the application typed them.
func normalize(v any) any {
	switch v.(type) {
	case string, int64, int32, int16, bool:
		return v
	case nil:
		return nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return rv.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	}
	return v
}

// assign stores v into the field dst points to.
func assign(dst, v any) error {
	switch d := dst.(type) {
	case *string:
		if s, ok := v.(string); ok {
			*d = s
			return nil
		}
	case *int64:
		if n, ok := v.(int64); ok {
			*d = n
			return nil
		}
	case **int64:
		if n, ok := v.(int64); ok {
			*d = &n
			return nil
		}
	case *time.Time:
		if t, ok := v.(time.Time); ok {
			*d = t
			return nil
		}
	case **time.Time:
		if t, ok := v.(time.Time); ok {
			*d = &t
			return nil
		}
	}
	dv := reflect.ValueOf(dst)
	if dv.Kind() != reflect.Pointer || dv.IsNil() {
		return errf("cannot assign %T to %T", v, dst)
	}
	ev := dv.Elem()
	vv := reflect.ValueOf(v)
	if ev.Kind() == reflect.Pointer {
		if !vv.Type().ConvertibleTo(ev.Type().Elem()) {
			return errf("cannot assign %T to %T", v, dst)
		}
		p := reflect.New(ev.Type().Elem())
		p.Elem().Set(vv.Convert(ev.Type().Elem()))
		ev.Set(p)
		return nil
	}
	if !vv.Type().ConvertibleTo(ev.Type()) {
		return errf("cannot assign %T to %T", v, dst)
	}
	ev.Set(vv.Convert(ev.Type()))
	return nil
}
