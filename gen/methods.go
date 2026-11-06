package gen

import (
	"go/token"
	"strings"
)

// methodSpec is one typed method keel gen writes on every table or query type. params, results,
// args and body are Go source in which {keel}, {context}, {time} and {unsafe} stand for the imports
// and {row} for the model's pointer type. args are what a builder passes on to QueryCore; body is
// the body of a typed write or row terminal; doc completes the method's one-line doc comment.
//
// A write or terminal calls keel's non-generic pointer function (CreatePtr, ListPtrs, ...) and
// converts the rows to {row} itself, rather than call keel's generic Create or List: a program has
// a generated package per domain, and each one that called a generic function would compile its
// own copy of it.
type methodSpec struct {
	name    string
	params  string
	results string
	args    string
	body    string
	doc     string
}

// newQueryCore is the keel function a table's Query builds its query's engine with.
const newQueryCore = "NewQueryCore"

// lateralSource is keel's exported name for the parameter of LeftJoinLateral and JoinLateral,
// interface{ subquery; TableRef() *Ref }, which generated code cannot spell: subquery is unexported.
const lateralSource = "LateralSource"

// ctxDB are the leading parameters of every typed write and row terminal.
const ctxDB = "ctx {context}.Context, db *{keel}.DB"

// queryBuilders are the builders of keel.Query: the generated query type calls the QueryCore
// method of the same name and returns itself. Clone is written apart, since it returns a copy.
var queryBuilders = []methodSpec{
	{name: "Where", params: "ps ...{keel}.Pred", args: "ps...",
		doc: "adds predicates, joined by AND. Nil predicates are skipped."},
	{name: "Pick", params: "es ...{keel}.Expr", args: "es...",
		doc: "selects only es."},
	{name: "Omit", params: "cols ...{keel}.Column", args: "cols...",
		doc: "leaves cols out of the columns the query reads."},
	{name: "Load", params: "ls ...{keel}.Loader", args: "ls...",
		doc: "loads relations into the rows."},
	{name: "Order", params: "os ...{keel}.Order", args: "os...",
		doc: "sets the sort order."},
	{name: "Distinct",
		doc: "returns each distinct row once: SELECT DISTINCT."},
	{name: "Group", params: "es ...{keel}.Expr", args: "es...",
		doc: "adds GROUP BY expressions."},
	{name: "GroupingSets", params: "sets ...[]{keel}.Expr", args: "sets...",
		doc: "groups by each of sets in turn: GROUP BY GROUPING SETS."},
	{name: "Rollup", params: "es ...{keel}.Expr", args: "es...",
		doc: "groups by es and by each prefix of es down to the grand total."},
	{name: "Cube", params: "es ...{keel}.Expr", args: "es...",
		doc: "groups by every subset of es."},
	{name: "Window", params: "name string, w {keel}.Window", args: "name, w",
		doc: "declares a named window (WINDOW name AS (…))."},
	{name: "Having", params: "ps ...{keel}.Pred", args: "ps...",
		doc: "adds HAVING predicates."},
	{name: "Limit", params: "n int", args: "n",
		doc: "caps the number of rows."},
	{name: "Offset", params: "n int", args: "n",
		doc: "skips n rows."},
	{name: "DistinctOn", params: "es ...{keel}.Expr", args: "es...",
		doc: "keeps the first row of each group of es (DISTINCT ON)."},
	{name: "AllTenants",
		doc: "lets the query cross tenants. It needs a system scope."},
	{name: "CustomPlan",
		doc: "plans the query for its parameter values on every execution."},
	{name: "Timeout", params: "d {time}.Duration", args: "d",
		doc: "bounds the query's execution."},
	{name: "Lock", params: "strength {keel}.LockStrength, wait ...{keel}.LockWait", args: "strength, wait...",
		doc: "locks the table's selected rows."},
	{name: "Join", params: "other interface{ TableRef() *{keel}.Ref }, on ...{keel}.Pred", args: "other, on...",
		doc: "adds an INNER JOIN of another table on the given predicates."},
	{name: "LeftJoin", params: "other interface{ TableRef() *{keel}.Ref }, on ...{keel}.Pred", args: "other, on...",
		doc: "adds a LEFT JOIN of another table on the given predicates."},
	{name: "LeftJoinLateral", params: "sub {keel}." + lateralSource, args: "sub",
		doc: "adds LEFT JOIN LATERAL (sub) ON TRUE."},
	{name: "JoinLateral", params: "sub {keel}." + lateralSource, args: "sub",
		doc: "adds CROSS JOIN LATERAL (sub)."},
	{name: "Cache", params: "opts ...{keel}.CacheOption", args: "opts...",
		doc: "serves the query's reads from the result cache."},
	{name: "With", params: "ctes ...*{keel}.CTETable", args: "ctes...",
		doc: "declares CTEs the query uses."},
	{name: "Compute", params: "col {keel}.Column, e {keel}.Expr", args: "col, e",
		doc: "selects e into the computed field col of the rows."},
	{name: "LoadStrategy", params: "s {keel}.LoadStrategy", args: "s",
		doc: "chooses how the query's relations load."},
}

// tableStarters are the builders keel.Handle starts a query with: the generated table type calls
// them on a new query.
var tableStarters = []string{
	"Where", "Pick", "Load", "Omit", "Order", "Limit", "Group", "GroupingSets", "Rollup", "Cube", "AllTenants",
	"Join", "LeftJoin", "LeftJoinLateral", "JoinLateral", "Lock", "DistinctOn", "Cache", "LoadStrategy", "With",
	"Compute",
}

// rowPtr and rowPtrs pass a row and a slice of rows of the model's pointer type to keel's pointer
// functions; asRows reads a slice of row pointers back as rows. A slice of pointers of any type
// has the layout of []unsafe.Pointer, so the conversions copy nothing.
const (
	rowPtr  = "{unsafe}.Pointer(row)"
	rowPtrs = "*(*[]{unsafe}.Pointer)({unsafe}.Pointer(&rows))"
	asRows  = "*(*[]{row})({unsafe}.Pointer(&rows))"
)

// tableWrites are the typed writes of keel.Handle: the generated table type calls keel's pointer
// function with its keel.TableBase.
var tableWrites = []methodSpec{
	{name: "Create", params: "row {row}", results: "error",
		body: "return {keel}.CreatePtr(ctx, db, &t.TableBase, " + rowPtr + ")",
		doc:  "inserts row."},
	{name: "CreateMany", params: "rows []{row}", results: "error",
		body: "return {keel}.CreateManyPtrs(ctx, db, &t.TableBase, " + rowPtrs + ")",
		doc:  "inserts rows with as few statements as Postgres's parameter limit allows."},
	{name: "Save", params: "row {row}, cols ...{keel}.Column", results: "error",
		body: "return {keel}.SavePtr(ctx, db, &t.TableBase, " + rowPtr + ", cols...)",
		doc:  "updates row by its primary key."},
	{name: "SaveExcept", params: "row {row}, cols ...{keel}.Column", results: "error",
		body: "return {keel}.SaveExceptPtr(ctx, db, &t.TableBase, " + rowPtr + ", cols...)",
		doc:  "is Save writing every updatable column except cols."},
	{name: "DeleteRow", params: "row {row}", results: "error",
		body: "return {keel}.DeleteRowPtr(ctx, db, &t.TableBase, " + rowPtr + ")",
		doc:  "deletes row by its primary key."},
	{name: "Upsert", params: "rows []{row}, c {keel}.Conflict", results: "([]bool, error)",
		body: "return {keel}.UpsertPtrs(ctx, db, &t.TableBase, " + rowPtrs + ", c)",
		doc:  "inserts rows, updating the existing row on conflict."},
	{name: "CreateOrSkip", params: "rows []{row}, targets ...{keel}.Expr", results: "(int64, error)",
		body: "return {keel}.CreateOrSkipPtrs(ctx, db, &t.TableBase, " + rowPtrs + ", targets...)",
		doc:  "inserts rows, skipping those that conflict on targets."},
	{name: "Copy", params: "rows []{row}", results: "(int64, error)",
		body: "return {keel}.CopyPtrs(ctx, db, &t.TableBase, " + rowPtrs + ")",
		doc:  "inserts rows through Postgres's binary COPY protocol."},
}

// queryTerminals are the row terminals of keel.Query: the generated query type calls keel's
// pointer function with its keel.QueryCore and converts the rows to the model's pointer type.
var queryTerminals = []methodSpec{
	{name: "One", results: "({row}, error)",
		body: "row, err := {keel}.OnePtr(ctx, db, &q.QueryCore)\n\treturn ({row})(row), err",
		doc:  "returns the only matching row, or keel.ErrNotFound or keel.ErrNotSingular."},
	{name: "First", results: "({row}, error)",
		body: "row, err := {keel}.FirstPtr(ctx, db, &q.QueryCore)\n\treturn ({row})(row), err",
		doc:  "returns the first row in the query's order, or keel.ErrNotFound."},
	{name: "List", results: "([]{row}, error)",
		body: "rows, err := {keel}.ListPtrs(ctx, db, &q.QueryCore)\n\treturn " + asRows + ", err",
		doc:  "returns the matching rows. The query must have a Limit."},
	{name: "All", results: "([]{row}, error)",
		body: "rows, err := {keel}.AllPtrs(ctx, db, &q.QueryCore)\n\treturn " + asRows + ", err",
		doc:  "returns every matching row. Use it only for sets known to be small."},
	{name: "UpdateReturning", params: "sets ...{keel}.Assignment", results: "([]{row}, error)",
		body: "rows, err := {keel}.UpdateReturningPtrs(ctx, db, &q.QueryCore, sets...)\n\treturn " + asRows + ", err",
		doc:  "sets columns on every matching row and returns the updated rows."},
	{name: "Page", params: "p {keel}.PageRequest", results: "({keel}.Page[{row}], error)",
		body: "page, err := {keel}.PagePtrs(ctx, db, &q.QueryCore, p)\n" +
			"\treturn *(*{keel}.Page[{row}])({unsafe}.Pointer(&page)), err",
		doc: "returns the page of rows after the request's cursor."},
	{name: "Each", params: "size int, fn func(ctx {context}.Context, batch []{row}) error", results: "error",
		body: "return {keel}.EachPtrs(ctx, db, &q.QueryCore, size, " +
			"func(ctx {context}.Context, rows []{unsafe}.Pointer) error {\n\t\treturn fn(ctx, " + asRows + ")\n\t})",
		doc: "runs the query page by page and calls fn with each batch of at most size rows."},
}

// tableBaseMembers are the exported fields and methods the generated table type gets from the
// keel.TableBase it embeds.
var tableBaseMembers = []string{
	"Ref", "Any", "None", "TableRef", "InsertFrom", "InsertFromSQL", "UpsertFrom", "UpsertFromSQL",
}

// queryBuilder returns the builder spec named name.
func queryBuilder(name string) methodSpec {
	for _, s := range queryBuilders {
		if s.name == name {
			return s
		}
	}
	panic("keel gen: no query builder " + name)
}

// tableMethodNames are the methods keel gen writes on every table type for keel.Handle.
func tableMethodNames() []string {
	names := append([]string{"Query"}, tableStarters...)
	for _, s := range tableWrites {
		names = append(names, s.name)
	}
	return names
}

// queryMethodNames are the methods keel gen writes on every query type for keel.Query.
func queryMethodNames() []string {
	names := make([]string, 0, len(queryBuilders)+1+len(queryTerminals))
	for _, s := range queryBuilders {
		names = append(names, s.name)
	}
	names = append(names, "Clone")
	for _, s := range queryTerminals {
		names = append(names, s.name)
	}
	return names
}

// tableMemberNames are the names the generated table type of m declares or promotes besides its
// columns: the methods keel gen writes, keel.TableBase and what it promotes, As and, when m has
// one, Get. A column or relation of one of these names would collide with it.
func tableMemberNames(m *model) map[string]bool {
	names := map[string]bool{"TableBase": true, "As": true}
	if _, ok := m.getKey(); ok {
		names["Get"] = true
	}
	for _, n := range tableMethodNames() {
		names[n] = true
	}
	for _, n := range tableBaseMembers {
		names[n] = true
	}
	return names
}

// assignMembers names the generated table's column fields and relation methods. A column or
// relation named like a member the table type already has (a typed method, a TableBase member,
// As or Get) is written with the suffix Col or Rel, so every generated table implements the whole
// Handle interface and no model field silently hides a method.
func assignMembers(src *source, m *model) error {
	taken := tableMemberNames(m)
	used := map[string]string{}
	claim := func(pos token.Pos, field, member string) error {
		if prev, ok := used[member]; ok {
			return src.errorf(pos, "%s.%s: the generated %sTable already has a member %s for %s; rename the field",
				m.name, field, m.plural, member, prev)
		}
		used[member] = field
		return nil
	}
	for _, c := range m.columns {
		c.member = c.field
		if taken[c.field] {
			c.member = c.field + "Col"
		}
		if taken[c.member] {
			return src.errorf(c.pos, "%s.%s: %s is a member of the generated %sTable; rename the field", m.name,
				c.field, c.member, m.plural)
		}
		if err := claim(c.pos, c.field, c.member); err != nil {
			return err
		}
	}
	for _, r := range m.rels {
		r.member = r.field
		if taken[r.field] {
			r.member = r.field + "Rel"
		}
		if taken[r.member] {
			return src.errorf(r.pos, "%s.%s: %s is a member of the generated %sTable; rename the field", m.name,
				r.field, r.member, m.plural)
		}
		if err := claim(r.pos, r.field, r.member); err != nil {
			return err
		}
	}
	return nil
}

// getKey returns the primary key column the generated Get takes, and whether m has a Get: a
// single key column besides the tenant columns that is not a pointer.
func (m *model) getKey() (*column, bool) {
	pk, err := m.singleKey()
	if err != nil || pk.ptr {
		return nil, false
	}
	return pk, true
}

// typedNames are the names one table's typed methods are written with: the import names, the
// model and its pointer type, and the generated table and query types.
type typedNames struct {
	keel, context, time, unsafe string
	model, row                  string
	table, query                string
}

// expand replaces the placeholders of a method spec's source.
func (n typedNames) expand(s string) string {
	return strings.NewReplacer("{keel}", n.keel, "{context}", n.context, "{time}", n.time, "{unsafe}", n.unsafe,
		"{row}", n.row).Replace(s)
}

// joinArgs joins two parameter or argument lists, either of which may be empty.
func joinArgs(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + ", " + b
}

// emitTyped writes the query type of one table, the compile-time check that the query type
// implements keel.Query, and the typed methods of both, from the method specs. It writes no check
// that the table implements keel.Handle: instantiating Handle for the model would make the package
// compile a wrapper for each of Handle's methods; keel's tests check that generated tables
// implement it.
func emitTyped(w func(string, ...any), n typedNames) {
	qt := n.expand("{keel}.Query[{row}]")
	w("// %s is a query over %s.\n", n.query, n.model)
	w("type %s struct{ %s.QueryCore }\n\n", n.query, n.keel)
	w("var _ %s = (*%s)(nil)\n\n", qt, n.query)

	w("// Query starts a query over the table.\n")
	w("func (t *%s) Query() %s {\n\treturn &%s{QueryCore: %s.%s(t.Ref)}\n}\n\n", n.table, qt, n.query, n.keel,
		newQueryCore)
	for _, name := range tableStarters {
		s := queryBuilder(name)
		w("// %s starts a query; see %s.%s.\n", name, n.query, name)
		w("func (t *%s) %s(%s) %s {\n\treturn t.Query().%s(%s)\n}\n\n", n.table, name, n.expand(s.params), qt,
			name, s.args)
	}
	for _, s := range tableWrites {
		w("// %s %s\n", s.name, s.doc)
		w("func (t *%s) %s(%s) %s {\n\t%s\n}\n\n", n.table, s.name,
			n.expand(joinArgs(ctxDB, s.params)), n.expand(s.results), n.expand(s.body))
	}

	for _, s := range queryBuilders {
		w("// %s %s\n", s.name, s.doc)
		w("func (q *%s) %s(%s) %s {\n\tq.QueryCore.%s(%s)\n\treturn q\n}\n\n", n.query, s.name,
			n.expand(s.params), qt, s.name, s.args)
	}
	w("// Clone returns an independent copy of the query.\n")
	w("func (q *%s) Clone() %s {\n\treturn &%s{QueryCore: *q.QueryCore.Clone()}\n}\n\n", n.query, qt, n.query)
	for _, s := range queryTerminals {
		w("// %s %s\n", s.name, s.doc)
		w("func (q *%s) %s(%s) %s {\n\t%s\n}\n\n", n.query, s.name,
			n.expand(joinArgs(ctxDB, s.params)), n.expand(s.results), n.expand(s.body))
	}
}
