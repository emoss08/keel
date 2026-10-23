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
	"github.com/jackc/pgx/v5/pgtype"
)

const maxParams = 65535

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
func (h Handle[T]) Save(ctx context.Context, db *DB, row *T, cols ...interface{ column() Col }) error {
	return db.save(ctx, h.Ref, unsafe.Pointer(row), cols)
}

// DeleteRow deletes row by its primary key. It returns ErrNotFound when no row was deleted.
func (h Handle[T]) DeleteRow(ctx context.Context, db *DB, row *T) error {
	return db.deleteRow(ctx, h.Ref.Table, unsafe.Pointer(row))
}

// Conflict is the ON CONFLICT clause of an upsert.
type Conflict struct {
	cols    []Col
	where   []Pred
	update  []Col
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

// Where matches a partial unique index: ON CONFLICT (…) WHERE ….
func (c Conflict) Where(ps ...Pred) Conflict {
	c.where = append(c.where, ps...)
	return c
}

// Update names the columns a conflicting row takes from the proposed row. Without columns every
// updatable column is taken.
func (c Conflict) Update(cols ...interface{ column() Col }) Conflict {
	for _, col := range cols {
		c.update = append(c.update, col.column())
	}
	return c
}

// Upsert inserts rows, updating the existing row on conflict. It reports, per row, whether it
// was inserted (true) or updated (false). Rows that conflict get the stored version and readonly
// columns back.
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
// inserted.
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

func (db *DB) writeRunner(ctx context.Context, th *tableHooks, op Op) (*runner, error) {
	r, err := db.newRunner(ctx, true)
	if err != nil {
		return nil, err
	}
	if r.tx == nil && th.has(op, AfterWrite) {
		r.tx = &txState{db: db, implicit: true, scopeCtx: ctx}
		r.own = true
	}
	return r, nil
}

// prepareInsert fills the autoid, tenant and timestamp columns of row and runs the Before hooks.
func (db *DB) prepareInsert(ctx context.Context, sc *scopeState, t *schema.Table, th *tableHooks,
	row unsafe.Pointer, now time.Time,
) error {
	if t.AutoID >= 0 {
		if isZero(t.Value(row, t.AutoID)) {
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
		if err := assign(t.Ptr(row, t.AutoCreate), stampValue(t.Columns[t.AutoCreate].Kind,
			now)); err != nil {
			return err
		}
	}
	if t.AutoUpdate >= 0 {
		if err := assign(t.Ptr(row, t.AutoUpdate), stampValue(t.Columns[t.AutoUpdate].Kind,
			now)); err != nil {
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
			if isZero(t.Value(row, ci)) {
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
		cur := t.Value(row, ci)
		if isZero(cur) {
			if err := assign(t.Ptr(row, ci), sc.tenant[pos]); err != nil {
				return err
			}
			continue
		}
		if normalize(cur) != normalize(sc.tenant[pos]) {
			return fmt.Errorf("%w: %s.%s", ErrTenantMismatch, t.Name, t.Columns[ci].Name)
		}
	}
	return nil
}

func (db *DB) insert(ctx context.Context, t *schema.Table, rows []unsafe.Pointer) error {
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th, OpCreate)
	if err != nil {
		return err
	}
	now := db.now()
	for _, row := range rows {
		if err := db.prepareInsert(ctx, run.sc, t, th, row, now); err != nil {
			return run.close(ctx, err)
		}
	}
	stmts := insertStmts(t, rows, &insertSpec{})
	err = run.batch(ctx, stmts, !th.has(OpCreate, AfterWrite))
	if err == nil {
		err = db.afterWrite(ctx, run, th, OpCreate, rows, nil, nil)
	}
	return run.close(ctx, err)
}

// insertSpec shapes the INSERT statements of a write.
type insertSpec struct {
	suffix   string
	upsert   bool
	skip     bool
	inserted []bool
	keys     map[string]bool
}

// insertStmts splits rows into INSERT statements under the parameter limit.
func insertStmts(t *schema.Table, rows []unsafe.Pointer, spec *insertSpec) []*stmt {
	per := max(maxParams/max(len(t.Insert), 1), 1)
	var ret []int32
	switch {
	case spec.skip:
		ret = t.PK
	case spec.upsert:
		ret = returningCols(t, true)
	default:
		ret = t.Returning
	}
	var stmts []*stmt
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		chunk := rows[start:end]
		r := &render{}
		r.s("INSERT INTO ")
		r.s(t.Quoted)
		r.s(" AS t0 (")
		for i, c := range t.Insert {
			if i > 0 {
				r.s(", ")
			}
			r.s(t.Columns[c].Quoted)
		}
		r.s(") VALUES ")
		for ri, row := range chunk {
			if ri > 0 {
				r.s(", ")
			}
			r.b('(')
			for i, c := range t.Insert {
				if i > 0 {
					r.s(", ")
				}
				r.arg(t.Value(row, c))
			}
			r.b(')')
		}
		r.s(spec.suffix)
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
				r.s("t0.")
				r.s(t.Columns[c].Quoted)
			}
		}
		s := &stmt{sql: string(r.buf), args: r.args, op: "insert", table: t.Name}
		switch {
		case spec.skip:
			s.scan = skipScanner(t, spec)
		case spec.upsert || len(ret) > 0:
			base := start
			s.scan = returningScanner(t, chunk, ret, spec.upsert, func(i int, v bool) {
				spec.inserted[base+i] = v
			})
		}
		stmts = append(stmts, s)
	}
	return stmts
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

// returningScanner reads RETURNING rows back into rows, in order. With flagged, the first column
// says whether the row was inserted.
func returningScanner(t *schema.Table, rows []unsafe.Pointer, cols []int32, flagged bool,
	flag func(i int, v bool),
) func(pgx.Rows) error {
	return func(res pgx.Rows) error {
		m := res.Conn().TypeMap()
		fds := res.FieldDescriptions()
		off := 0
		if flagged {
			off = 1
		}
		plans := make([]pgtype.ScanPlan, len(cols)+off)
		i := 0
		for res.Next() {
			if i >= len(rows) {
				break
			}
			raw := res.RawValues()
			if flagged {
				var b bool
				if plans[0] == nil {
					plans[0] = m.PlanScan(fds[0].DataTypeOID, fds[0].Format, &b)
				}
				if err := plans[0].Scan(raw[0], &b); err != nil {
					return err
				}
				flag(i, b)
			}
			for j, c := range cols {
				dst := t.Ptr(rows[i], c)
				if plans[j+off] == nil {
					plans[j+off] = m.PlanScan(fds[j+off].DataTypeOID, fds[j+off].Format, dst)
				}
				if err := plans[j+off].Scan(raw[j+off], dst); err != nil {
					return fmt.Errorf("keel: scan %s.%s: %w", t.Name, t.Columns[c].Name, err)
				}
			}
			i++
		}
		return nil
	}
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
	if len(c.cols) == 0 {
		return nil, errf("OnConflict needs the conflicting columns")
	}
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th, OpCreate)
	if err != nil {
		return nil, err
	}
	now := db.now()
	for _, row := range rows {
		if err := db.prepareInsert(ctx, run.sc, t, th, row, now); err != nil {
			return nil, run.close(ctx, err)
		}
	}
	r := &render{scope: run.sc, bare: true}
	r.s(" ON CONFLICT (")
	for i, col := range c.cols {
		if col.Ref.Table != t {
			return nil, run.close(ctx, errf("OnConflict names a column of %s on %s",
				col.Ref.Table.Name, t.Name))
		}
		if i > 0 {
			r.s(", ")
		}
		r.s(t.Columns[col.Index].Quoted)
	}
	r.b(')')
	if hasPreds(c.where) {
		r.s(" WHERE ")
		r.preds(c.where, false)
	}
	if len(r.args) > 0 {
		return nil, run.close(ctx, errf("OnConflict Where must not bind values; use constants"))
	}
	if c.nothing {
		r.s(" DO NOTHING")
	} else {
		update := c.update
		if len(update) == 0 {
			for _, ci := range t.Update {
				update = append(update, Col{Ref: ref, Index: ci})
			}
		}
		r.s(" DO UPDATE SET ")
		touched := make(map[int32]bool, len(update))
		for i, col := range update {
			if i > 0 {
				r.s(", ")
			}
			q := t.Columns[col.Index].Quoted
			r.s(q)
			r.s(" = EXCLUDED.")
			r.s(q)
			touched[col.Index] = true
		}
		if t.AutoUpdate >= 0 && !touched[t.AutoUpdate] {
			q := t.Columns[t.AutoUpdate].Quoted
			r.s(", ")
			r.s(q)
			r.s(" = EXCLUDED.")
			r.s(q)
		}
		if t.Version >= 0 {
			q := t.Columns[t.Version].Quoted
			r.s(", ")
			r.s(q)
			r.s(" = t0.")
			r.s(q)
			r.s(" + 1")
		}
	}
	if r.err != nil {
		return nil, run.close(ctx, r.err)
	}
	spec := &insertSpec{suffix: string(r.buf), upsert: !c.nothing, skip: c.nothing,
		inserted: make([]bool, len(rows))}
	if c.nothing {
		spec.keys = make(map[string]bool, len(rows))
	}
	inserted := spec.inserted
	stmts := insertStmts(t, rows, spec)
	err = run.batch(ctx, stmts, !th.has(OpCreate, AfterWrite) && !th.has(OpUpdate, AfterWrite))
	if err == nil && c.nothing {
		for i, row := range rows {
			inserted[i] = spec.keys[rowKey(t, row)]
		}
	}
	if err == nil {
		var created, updated []unsafe.Pointer
		for i, row := range rows {
			if inserted[i] {
				created = append(created, row)
			} else if !c.nothing {
				updated = append(updated, row)
			}
		}
		err = db.afterWrite(ctx, run, th, OpCreate, created, nil, nil)
		if err == nil {
			err = db.afterWrite(ctx, run, th, OpUpdate, updated, nil, nil)
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
	run, err := db.writeRunner(ctx, th, OpUpdate)
	if err != nil {
		return err
	}
	if err := db.prepareSave(ctx, run.sc, t, th, row, update); err != nil {
		return run.close(ctx, err)
	}

	var needs []int32
	if th != nil {
		needs = th.needs
	}
	r := &render{scope: run.sc, ctx: ctx, db: db}
	r.s("UPDATE ")
	r.s(t.Quoted)
	r.s(" t0 SET ")
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
		r.s(" = t0.")
		r.s(q)
		r.s(" + 1")
	}
	r.s(" WHERE ")
	pkWhere(r, t, row)
	if t.Version >= 0 {
		r.s(" AND t0.")
		r.s(t.Columns[t.Version].Quoted)
		r.s(" = ")
		r.arg(oldVersion)
	}
	if t.HasTenant() && !run.sc.system {
		r.s(" AND ")
		r.tenantPred(t, "t0", false)
	}
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
			r.s("t0.")
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
	s := &stmt{sql: string(r.buf), args: r.args, op: "update", table: t.Name}
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
		if normalize(t.Value(row, ci)) != normalize(sc.tenant[pos]) {
			return fmt.Errorf("%w: %s.%s", ErrTenantMismatch, t.Name, t.Columns[ci].Name)
		}
	}
	return nil
}

func pkWhere(r *render, t *schema.Table, row unsafe.Pointer) {
	for i, c := range t.PK {
		if i > 0 {
			r.s(" AND ")
		}
		r.s("t0.")
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
		if err := m.PlanScan(fds[i].DataTypeOID, fds[i].Format, dst).Scan(raw[i], dst); err != nil {
			return fmt.Errorf("keel: scan %s.%s: %w", t.Name, t.Columns[c].Name, err)
		}
		i++
	}
	if old == nil {
		return nil
	}
	for _, c := range needs {
		dst := t.Ptr(old, c)
		if err := m.PlanScan(fds[i].DataTypeOID, fds[i].Format, dst).Scan(raw[i], dst); err != nil {
			return fmt.Errorf("keel: scan old %s.%s: %w", t.Name, t.Columns[c].Name, err)
		}
		i++
	}
	return nil
}

func (db *DB) deleteRow(ctx context.Context, t *schema.Table, row unsafe.Pointer) error {
	if len(t.PK) == 0 {
		return errf("%s has no primary key; DeleteRow needs one", t.Name)
	}
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th, OpDelete)
	if err != nil {
		return err
	}
	if err := checkTenant(run.sc, t, row); err != nil {
		return run.close(ctx, err)
	}
	if t.BeforeDelete != nil {
		if err := t.BeforeDelete(ctx, row); err != nil {
			return run.close(ctx, err)
		}
	}
	if err := th.run(ctx, OpDelete, BeforeWrite, &mutation{op: OpDelete, row: row}); err != nil {
		return run.close(ctx, err)
	}
	r := &render{scope: run.sc, ctx: ctx, db: db}
	r.s("DELETE FROM ")
	r.s(t.Quoted)
	r.s(" t0 WHERE ")
	pkWhere(r, t, row)
	if t.HasTenant() && !run.sc.system {
		r.s(" AND ")
		r.tenantPred(t, "t0", false)
	}
	if r.err != nil {
		return run.close(ctx, r.err)
	}
	s := &stmt{sql: string(r.buf), args: r.args, op: "delete", table: t.Name}
	err = run.one(ctx, s, !th.has(OpDelete, AfterWrite))
	if err == nil && s.tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	if err == nil {
		err = db.afterWrite(ctx, run, th, OpDelete, []unsafe.Pointer{row}, nil, nil)
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
	r := &render{scope: run.sc, ctx: ctx, db: db}
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
	if tenantCols > 0 {
		r.s("SELECT s.*")
		for _, ci := range t.Tenant {
			r.s(", ")
			r.tenantArg(run.sc.tenantPos(t.Columns[ci].Name))
		}
		r.s(" FROM (")
		src.writeSubquery(r, false)
		r.s(") s")
	} else {
		src.writeSubquery(r, false)
	}
	if r.err != nil {
		return 0, run.close(ctx, r.err)
	}
	s := &stmt{sql: string(r.buf), args: r.args, op: "insert", table: t.Name}
	err = run.one(ctx, s, true)
	return s.tag.RowsAffected(), run.close(ctx, err)
}

// afterWrite runs the After hooks now and queues the Committed hooks.
func (db *DB) afterWrite(ctx context.Context, run *runner, th *tableHooks, op Op,
	rows, olds []unsafe.Pointer, changed []int32,
) error {
	if th == nil || len(rows) == 0 {
		return nil
	}
	for i, row := range rows {
		m := &mutation{op: op, row: row, changed: changed}
		if i < len(olds) {
			m.old = olds[i]
		}
		if err := th.run(ctx, op, AfterWrite, m); err != nil {
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
