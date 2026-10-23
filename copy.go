package keel

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Copy inserts rows through Postgres's binary COPY protocol and returns how many it inserted. It
// streams the rows, so it beats CreateMany for loads of many thousands of rows.
//
// Rows are prepared as Create prepares them: the autoid, tenant, autocreate and autoupdate columns
// are filled, a row of another tenant is refused, and the BeforeCreate, BeforeSave and BeforeWrite
// hooks run. COPY returns nothing, so readonly columns are not read back, and AfterWrite and
// Committed hooks see the rows as written. Enum columns need their types registered on the
// connection, which keel does unless Config.SkipEnums is set.
//
// Copy joins the transaction on the context. Outside one it runs in an implicit transaction when
// Config.OnBegin is set, after the OnBegin statements, or when the table has AfterWrite hooks.
func (h Handle[T]) Copy(ctx context.Context, db *DB, rows []*T) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	ps := make([]unsafe.Pointer, len(rows))
	for i, r := range rows {
		ps[i] = unsafe.Pointer(r)
	}
	return db.copyRows(ctx, h.Ref.Table, ps)
}

func (db *DB) copyRows(ctx context.Context, t *schema.Table, rows []unsafe.Pointer) (int64, error) {
	th := db.hooks.forTable(t)
	run, err := db.writeRunner(ctx, th, OpCreate)
	if err != nil {
		return 0, err
	}
	st := db.stampsFor(t)
	for _, row := range rows {
		if err := db.prepareInsert(ctx, run.sc, t, th, row, st); err != nil {
			return 0, run.close(ctx, err)
		}
	}
	conn, err := run.started(ctx)
	if err != nil {
		return 0, run.close(ctx, err)
	}
	names := make([]string, len(t.Insert))
	for i, c := range t.Insert {
		names[i] = t.Columns[c].Name
	}
	s := &stmt{sql: copySQL(t), op: "copy", table: t.Name}
	stmts := []*stmt{s}
	src := &copySource{t: t, rows: rows, vals: make([]any, len(t.Insert))}
	events := db.startEvents(ctx, stmts)
	start := time.Now()
	n, err := conn.CopyFrom(ctx, pgx.Identifier{t.Name}, names, src)
	s.tag = pgconn.NewCommandTag("COPY " + strconv.FormatInt(n, 10))
	db.endEvents(ctx, events, stmts, time.Since(start), err)
	if err == nil {
		err = db.afterWrite(ctx, run, th, OpCreate, rows, nil, nil)
	}
	if err := run.close(ctx, err); err != nil {
		return 0, err
	}
	return n, nil
}

// copySQL is the statement observers see for a COPY of t's insert columns.
func copySQL(t *schema.Table) string {
	var b strings.Builder
	b.WriteString("COPY ")
	b.WriteString(t.Quoted)
	b.WriteString(" (")
	for i, c := range t.Insert {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(t.Columns[c].Quoted)
	}
	b.WriteString(") FROM STDIN (FORMAT binary)")
	return b.String()
}

// copySource feeds CopyFrom one row at a time through the generated Value accessor, which writes a
// nil jsonb map or slice as {} or [] and a nil array as an empty array. It reuses one value slice,
// which pgx encodes before asking for the next row.
type copySource struct {
	t    *schema.Table
	rows []unsafe.Pointer
	next int
	vals []any
}

// Next advances to the next row.
func (s *copySource) Next() bool {
	if s.next >= len(s.rows) {
		return false
	}
	s.next++
	return true
}

// Values returns the insert columns of the current row.
func (s *copySource) Values() ([]any, error) {
	row := s.rows[s.next-1]
	for i, c := range s.t.Insert {
		s.vals[i] = s.t.Value(row, c)
	}
	return s.vals, nil
}

// Err reports no error: reading a row cannot fail.
func (s *copySource) Err() error { return nil }
