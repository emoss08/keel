package keel

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/emoss08/keel/schema"
)

// DriftKind names what a Drift found.
type DriftKind string

const (
	// DriftMissingTable is a model whose table or view does not exist.
	DriftMissingTable DriftKind = "missing_table"
	// DriftMissingColumn is a stored field whose column does not exist.
	DriftMissingColumn DriftKind = "missing_column"
	// DriftPrimaryKey is a model whose pk columns are not the table's primary key.
	DriftPrimaryKey DriftKind = "primary_key"
	// DriftNullable is a nullable column read into a field that cannot hold NULL.
	DriftNullable DriftKind = "nullable"
	// DriftGenerated is a generated or always-identity column on a field keel writes.
	DriftGenerated DriftKind = "generated"
	// DriftJSON is a jsonb field on a column that is neither json nor jsonb.
	DriftJSON DriftKind = "json"
	// DriftType is a field whose Go kind cannot hold the column's type.
	DriftType DriftKind = "type"
	// DriftJoinTable is a many-to-many join table that is missing, or lacks a key or tenant column.
	DriftJoinTable DriftKind = "join_table"
	// DriftRelationKey is a relation whose key column or target table does not exist.
	DriftRelationKey DriftKind = "relation_key"
	// DriftConstraint is a constraint a model's Constraints method names that the table lacks.
	DriftConstraint DriftKind = "constraint"
	// DriftMissingFunction is a declared database function (DeclareFunc0 … DeclareFunc3) that does
	// not exist, or exists only as a procedure, aggregate or window function.
	DriftMissingFunction DriftKind = "missing_function"
	// DriftFunctionArity is a declared function none of whose overloads takes the declared number
	// of arguments.
	DriftFunctionArity DriftKind = "function_arity"
	// DriftFunctionResult is a declared function whose overloads of the declared arity return a
	// type the declared result kind cannot hold.
	DriftFunctionResult DriftKind = "function_result"
)

// Drift is one difference between a model and the live database.
type Drift struct {
	Kind DriftKind `json:"kind"`
	// Table is the table the difference is in: the model's, a join table, or a relation's target;
	// for a function drift, the declared function's name.
	Table string `json:"table"`
	// Column is the column, empty for a whole table.
	Column string `json:"column,omitempty"`
	// Field is the model field or relation (Shipment.Weight), empty for a whole table.
	Field string `json:"field,omitempty"`
	// Detail says what differs.
	Detail string `json:"detail"`
	// Fix suggests how to make them agree.
	Fix string `json:"fix"`
}

// String writes the drift on one line: table.column (Field): detail; fix.
func (d Drift) String() string {
	var b strings.Builder
	b.WriteString(d.Table)
	if d.Column != "" {
		b.WriteByte('.')
		b.WriteString(d.Column)
	}
	if d.Field != "" {
		b.WriteString(" (")
		b.WriteString(d.Field)
		b.WriteByte(')')
	}
	b.WriteString(": ")
	b.WriteString(d.Detail)
	b.WriteString("; ")
	b.WriteString(d.Fix)
	return b.String()
}

// Verify compares the metadata of tables (keeltables.All(), say) with the live catalog and returns
// every difference that would make keel's statements fail or decode wrongly: missing tables, views
// and columns, primary keys that differ from the models' pk columns, nullable columns read into
// fields that cannot hold NULL, generated columns on fields keel writes, jsonb fields on columns
// that are not json, Go kinds that cannot hold the column's type, many-to-many join tables without
// their key or shared tenant columns, and relation keys that do not exist. Drifts come in the order
// of tables, then columns; none means the models and the database agree.
//
// Verify reads the catalog in one statement on a pool connection of its own, outside any
// transaction and scope, resolving names through the connection's search_path as keel's statements
// do. Views and materialized views are checked for their columns and types only: Postgres records
// neither a primary key nor nullability for them. A noselect column is not checked for NULL, since
// it is read only when a query picks it. A field of a scanner or other custom type
// (schema.KindOther) is not type-checked, since its Scan method decides what it accepts.
func Verify(ctx context.Context, db *DB, tables []*schema.Table) ([]Drift, error) {
	v := &verifier{rels: map[string]*catalogRel{}}
	seen := map[*schema.Table]bool{}
	for _, t := range tables {
		if t == nil || seen[t] {
			continue
		}
		seen[t] = true
		v.tables = append(v.tables, t)
		v.want(t.Quoted)
		for i := range t.Relations {
			r := &t.Relations[i]
			if r.Target != nil {
				v.want(r.Target.Quoted)
			}
			if r.Kind == schema.ManyToMany {
				v.want(r.JoinQuoted)
			}
		}
	}
	if err := v.load(ctx, db); err != nil {
		return nil, err
	}
	if err := v.loadConstraints(ctx, db); err != nil {
		return nil, err
	}
	v.found = make([][]Drift, len(v.tables))
	for i, t := range v.tables {
		v.cur = i
		v.table(t)
	}
	for i, t := range v.tables {
		if v.rels[t.Quoted] == nil {
			continue
		}
		v.cur = i
		for ri := range t.Relations {
			v.relation(t, &t.Relations[ri])
		}
	}
	var out []Drift
	for _, d := range v.found {
		out = append(out, d...)
	}
	return out, nil
}

// verifier holds the catalog Verify read and the drifts it found, per table of tables; cur is the
// index of the table being checked.
type verifier struct {
	tables  []*schema.Table
	names   []string
	rels    map[string]*catalogRel
	found   [][]Drift
	cur     int
	missing map[[2]string]bool
	// constraints holds the constraint and index names of each table that declares constraints.
	constraints map[string]map[string]bool
}

// catalogRel is a table or view as the catalog describes it.
type catalogRel struct {
	kind string
	cols map[string]*catalogCol
	pk   []string
}

// catalogCol is a column as the catalog describes it: its type (domains resolved to their base
// type), whether it may hold NULL, and whether Postgres computes it.
type catalogCol struct {
	declared  string
	typ       string
	category  string
	enum      bool
	elem      string
	elemEnum  bool
	nullable  bool
	generated bool
	identity  bool
}

// want records a quoted table name to read from the catalog.
func (v *verifier) want(quoted string) {
	if quoted != "" && !slices.Contains(v.names, quoted) {
		v.names = append(v.names, quoted)
	}
}

// verifyCatalogSQL reads, for each quoted name, the relation it resolves to, its columns (domains
// resolved to their base types, array element types) and its primary key.
const verifyCatalogSQL = `SELECT n.name, coalesce(c.relkind::text, ''), coalesce(a.attname::text, ''),
       coalesce(bt.typname::text, ''), coalesce(bt.typcategory::text, ''), coalesce(bt.typtype::text, ''),
       coalesce(et.typname::text, ''), coalesce(et.typtype::text, ''),
       coalesce(NOT a.attnotnull AND NOT ty.typnotnull, false),
       coalesce(a.attgenerated::text, '') <> '', coalesce(a.attidentity::text, '') = 'a',
       coalesce(a.attnum = ANY(i.indkey::int2[]), false), coalesce(format_type(a.atttypid, NULL), '')
  FROM unnest($1::text[]) WITH ORDINALITY AS n(name, ord)
  LEFT JOIN pg_class c ON c.oid = to_regclass(n.name)
  LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
  LEFT JOIN pg_index i ON i.indrelid = c.oid AND i.indisprimary
  LEFT JOIN pg_type ty ON ty.oid = a.atttypid
  LEFT JOIN pg_type bt ON bt.oid = CASE WHEN ty.typtype = 'd' THEN ty.typbasetype ELSE ty.oid END
  LEFT JOIN pg_type et ON et.oid = bt.typelem AND bt.typcategory = 'A'
 ORDER BY n.ord, a.attnum`

// load reads the catalog of every wanted name in one statement.
func (v *verifier) load(ctx context.Context, db *DB) error {
	rows, err := db.pool.Query(ctx, verifyCatalogSQL, v.names)
	if err != nil {
		return fmt.Errorf("keel: verify: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, kind, col, typtype, elemType string
		var cc catalogCol
		var pk bool
		if err := rows.Scan(&name, &kind, &col, &cc.typ, &cc.category, &typtype, &cc.elem, &elemType,
			&cc.nullable, &cc.generated, &cc.identity, &pk, &cc.declared); err != nil {
			return fmt.Errorf("keel: verify: %w", err)
		}
		if kind == "" {
			continue
		}
		rel := v.rels[name]
		if rel == nil {
			rel = &catalogRel{kind: kind, cols: map[string]*catalogCol{}}
			v.rels[name] = rel
		}
		if col == "" {
			continue
		}
		cc.enum, cc.elemEnum = typtype == "e", elemType == "e"
		rel.cols[col] = &cc
		if pk {
			rel.pk = append(rel.pk, col)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("keel: verify: %w", err)
	}
	return nil
}

// add records a drift of the table being checked.
func (v *verifier) add(d Drift) {
	if d.Kind == DriftMissingColumn {
		if v.missing == nil {
			v.missing = map[[2]string]bool{}
		}
		v.missing[[2]string{d.Table, d.Column}] = true
	}
	v.found[v.cur] = append(v.found[v.cur], d)
}

// view reports whether a relation kind is a view or materialized view.
func view(kind string) bool { return kind == "v" || kind == "m" }

// fieldName is t's field or relation name qualified by the model's type name (Shipment.Weight),
// or the model's type name alone for an empty name.
func fieldName(t *schema.Table, name string) string {
	model := t.Model
	if i := strings.LastIndexByte(model, '.'); i >= 0 {
		model = model[i+1:]
	}
	switch {
	case model == "":
		return name
	case name == "":
		return model
	}
	return model + "." + name
}

// table checks one model's table, columns and primary key.
func (v *verifier) table(t *schema.Table) {
	rel := v.rels[t.Quoted]
	if rel == nil {
		v.add(Drift{Kind: DriftMissingTable, Table: t.Name, Field: fieldName(t, ""),
			Detail: "the table does not exist",
			Fix:    "create it in a migration, or fix the table name in the model's keel.Table tag"})
		return
	}
	for i := range t.Columns {
		c := &t.Columns[i]
		if !c.Stored() {
			continue
		}
		v.column(t, rel, c)
	}
	if !view(rel.kind) {
		v.primaryKey(t, rel)
	}
	for _, c := range t.Constraints {
		if !v.constraints[t.Quoted][c.Name] {
			v.add(Drift{Kind: DriftConstraint, Table: t.Name, Field: fieldName(t, ""),
				Detail: fmt.Sprintf("the constraint or unique index %s does not exist", c.Name),
				Fix:    "rename it in the model's Constraints method to the migration's name"})
		}
	}
}

// verifyConstraintsSQL lists the constraints and indexes of every named table.
const verifyConstraintsSQL = `SELECT n.name, k.conname
  FROM unnest($1::text[]) AS n(name)
  CROSS JOIN LATERAL (
	SELECT conname::text FROM pg_constraint WHERE conrelid = to_regclass(n.name)
	UNION ALL
	SELECT i.relname::text FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid
	 WHERE x.indrelid = to_regclass(n.name)
  ) AS k(conname)`

// loadConstraints reads the constraint and index names of the tables that declare constraints.
func (v *verifier) loadConstraints(ctx context.Context, db *DB) error {
	var names []string
	for _, t := range v.tables {
		if len(t.Constraints) > 0 {
			names = append(names, t.Quoted)
		}
	}
	v.constraints = map[string]map[string]bool{}
	if len(names) == 0 {
		return nil
	}
	rows, err := db.pool.Query(ctx, verifyConstraintsSQL, names)
	if err != nil {
		return fmt.Errorf("keel: verify: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			return fmt.Errorf("keel: verify: %w", err)
		}
		if v.constraints[table] == nil {
			v.constraints[table] = map[string]bool{}
		}
		v.constraints[table][name] = true
	}
	return rows.Err()
}

// column checks one stored column of t.
func (v *verifier) column(t *schema.Table, rel *catalogRel, c *schema.Column) {
	field := fieldName(t, c.Field)
	cc := rel.cols[c.Name]
	if cc == nil {
		v.add(Drift{Kind: DriftMissingColumn, Table: t.Name, Column: c.Name, Field: field,
			Detail: "the column does not exist",
			Fix: fmt.Sprintf("add it in a migration (ALTER TABLE %s ADD COLUMN %s %s%s), or fix the column name in %s's keel tag",
				t.Quoted, c.Quoted, sqlTypeOf(c), notNullOf(c), field)})
		return
	}
	if c.Flags.Has(schema.FlagJSONB) && cc.typ != "json" && cc.typ != "jsonb" {
		v.add(Drift{Kind: DriftJSON, Table: t.Name, Column: c.Name, Field: field,
			Detail: fmt.Sprintf("the field is tagged jsonb but the column is %s", cc.typ),
			Fix:    fmt.Sprintf("change the column to jsonb, or drop the jsonb option from %s", field)})
	} else if ok, want := kindHolds(c.Kind, cc); !ok {
		v.add(Drift{Kind: DriftType, Table: t.Name, Column: c.Name, Field: field,
			Detail: fmt.Sprintf("a %s field cannot hold the column's type %s", kindName(c), cc.typ),
			Fix:    fmt.Sprintf("change the column to %s, or change %s's type", want, field)})
	}
	if c.SQLType != "" && !sameType(c.SQLType, cc.declared) {
		v.add(Drift{Kind: DriftType, Table: t.Name, Column: c.Name, Field: field,
			Detail: fmt.Sprintf("the tag declares sqltype=%s but the column is %s", c.SQLType, cc.declared),
			Fix:    fmt.Sprintf("set sqltype=%s in %s's keel tag, or change the column's type", cc.declared, field)})
	}
	if cc.nullable && !view(rel.kind) && !c.Flags.Has(schema.FlagNoSelect) && !holdsNull(c) {
		notNull := "make the column NOT NULL in a migration"
		if cc.generated {
			notNull = "declare the generated column NOT NULL (GENERATED ALWAYS AS (…) STORED NOT NULL)"
		}
		v.add(Drift{Kind: DriftNullable, Table: t.Name, Column: c.Name, Field: field,
			Detail: fmt.Sprintf("the column is nullable but the %s field cannot hold NULL", kindName(c)),
			Fix:    fmt.Sprintf("%s, or read it into a pointer, a Null… type or a nullzero field (%s)", notNull, field)})
	}
	if (cc.generated || cc.identity) && !c.Flags.Has(schema.FlagReadOnly) {
		what := "a generated column"
		if cc.identity {
			what = "a GENERATED ALWAYS identity column"
		}
		v.add(Drift{Kind: DriftGenerated, Table: t.Name, Column: c.Name, Field: field,
			Detail: fmt.Sprintf("the column is %s, which keel writes", what),
			Fix:    fmt.Sprintf("tag %s readonly", field)})
	}
}

// primaryKey compares t's pk columns with its table's primary key.
func (v *verifier) primaryKey(t *schema.Table, rel *catalogRel) {
	model := make([]string, 0, len(t.PK))
	for _, ci := range t.PK {
		model = append(model, t.Columns[ci].Name)
	}
	db := slices.Clone(rel.pk)
	slices.Sort(model)
	slices.Sort(db)
	if slices.Equal(model, db) {
		return
	}
	detail := fmt.Sprintf("the model's pk columns are (%s) but the table's primary key is (%s)",
		strings.Join(model, ", "), strings.Join(db, ", "))
	if len(db) == 0 {
		detail = fmt.Sprintf("the model's pk columns are (%s) but the table has no primary key", strings.Join(model, ", "))
	}
	v.add(Drift{Kind: DriftPrimaryKey, Table: t.Name, Field: fieldName(t, ""), Detail: detail,
		Fix: "tag exactly the primary key's columns pk, or change the table's PRIMARY KEY in a migration"})
}

// relation checks that rel's target exists, that its key columns exist on both sides, and that a
// many-to-many relation's join table carries its keys and the tenant columns parent and target
// share.
func (v *verifier) relation(t *schema.Table, rel *schema.Relation) {
	field := fieldName(t, rel.Name)
	if rel.Kind == schema.Pick {
		return
	}
	if rel.Target == nil {
		v.add(Drift{Kind: DriftRelationKey, Table: t.Name, Field: field,
			Detail: "the relation has no target table",
			Fix:    "import the target model's generated package, so its init links the relation"})
		return
	}
	v.keys(t, rel.Local, field)
	if v.rels[rel.Target.Quoted] == nil {
		if !slices.Contains(v.tables, rel.Target) {
			v.add(Drift{Kind: DriftRelationKey, Table: rel.Target.Name, Field: field,
				Detail: "the relation's target table does not exist",
				Fix:    "create it in a migration, or fix the relation's target model"})
		}
	} else {
		v.keys(rel.Target, rel.Remote, field)
	}
	if rel.Kind == schema.ManyToMany {
		v.joinTable(t, rel, field)
	}
}

// keys reports the key columns cols of t that its table lacks, unless the column check already
// reported them.
func (v *verifier) keys(t *schema.Table, cols []int32, field string) {
	rel := v.rels[t.Quoted]
	if rel == nil {
		return
	}
	for _, ci := range cols {
		c := &t.Columns[ci]
		if rel.cols[c.Name] != nil || v.missing[[2]string{t.Name, c.Name}] {
			continue
		}
		v.add(Drift{Kind: DriftRelationKey, Table: t.Name, Column: c.Name, Field: field,
			Detail: "the relation's key column does not exist",
			Fix: fmt.Sprintf("add it in a migration (ALTER TABLE %s ADD COLUMN %s %s), or fix %s's keel tag",
				t.Quoted, c.Quoted, sqlTypeOf(c), field)})
	}
}

// joinTable checks the join table of t's many-to-many relation rel.
func (v *verifier) joinTable(t *schema.Table, rel *schema.Relation, field string) {
	j := t.JoinTableOf(rel)
	if j == nil {
		return
	}
	cat := v.rels[j.Quoted]
	if cat == nil {
		cols := make([]string, len(j.Columns))
		for i := range j.Columns {
			cols[i] = j.Columns[i].Quoted + " " + sqlTypeOf(&j.Columns[i]) + " NOT NULL"
		}
		v.add(Drift{Kind: DriftJoinTable, Table: j.Name, Field: field, Detail: "the join table does not exist",
			Fix: fmt.Sprintf("create it in a migration (CREATE TABLE %s (%s))", j.Quoted, strings.Join(cols, ", "))})
		return
	}
	for i := range j.Columns {
		c := &j.Columns[i]
		cc := cat.cols[c.Name]
		role := "key"
		if c.Flags.Has(schema.FlagTenant) {
			role = "tenant"
		}
		if cc == nil {
			detail := "the join table lacks its " + role + " column"
			if role == "tenant" {
				detail = fmt.Sprintf("the join table lacks the tenant column %s and %s share", t.Name, rel.Target.Name)
			}
			v.add(Drift{Kind: DriftJoinTable, Table: j.Name, Column: c.Name, Field: field, Detail: detail,
				Fix: fmt.Sprintf("add it in a migration (ALTER TABLE %s ADD COLUMN %s %s NOT NULL), or fix %s's m2m tag",
					j.Quoted, c.Quoted, sqlTypeOf(c), field)})
			continue
		}
		if ok, want := kindHolds(c.Kind, cc); !ok {
			v.add(Drift{Kind: DriftType, Table: j.Name, Column: c.Name, Field: field,
				Detail: fmt.Sprintf("the join table's %s column is %s, which does not match the %s column it joins",
					role, cc.typ, kindName(c)),
				Fix: fmt.Sprintf("change the column to %s", want)})
		}
	}
}

// holdsNull reports whether c's field can hold NULL: a pointer, a Null… scanner, a nullzero field,
// a jsonb value, a byte slice or an array; a scanner or other custom type (KindOther) decides in
// its Scan method, so it is given the benefit of the doubt.
func holdsNull(c *schema.Column) bool {
	if c.MayBeNull() || c.Flags.Has(schema.FlagJSONB) {
		return true
	}
	switch c.Kind {
	case schema.KindBytes, schema.KindStringArray, schema.KindInt64Array, schema.KindOther, schema.KindJSON:
		return true
	}
	return false
}

// textTypes are the base types a string field reads and writes as text.
var textTypes = []string{"text", "varchar", "bpchar", "name", "citext", "uuid", "json", "jsonb", "inet", "cidr",
	"macaddr", "xml"}

// kindHolds reports whether a field of kind k can hold a column of type cc, and the type it
// suggests otherwise.
func kindHolds(k schema.Kind, cc *catalogCol) (bool, string) {
	in := func(want string, types ...string) (bool, string) { return slices.Contains(types, cc.typ), want }
	switch k {
	case schema.KindString:
		if cc.enum || cc.category == "S" {
			return true, ""
		}
		return in("text", textTypes...)
	case schema.KindInt16:
		return in("smallint", "int2")
	case schema.KindInt32:
		return in("integer", "int2", "int4")
	case schema.KindInt64:
		return in("bigint", "int2", "int4", "int8")
	case schema.KindFloat32:
		return in("real", "float4")
	case schema.KindFloat64:
		return in("double precision", "float4", "float8", "numeric")
	case schema.KindBool:
		return in("boolean", "bool")
	case schema.KindBytes:
		return in("bytea", "bytea", "json", "jsonb")
	case schema.KindTime:
		return in("timestamptz", "timestamptz", "timestamp", "date")
	case schema.KindJSON:
		return in("jsonb", "json", "jsonb")
	case schema.KindTSVector:
		return in("tsvector", "tsvector")
	case schema.KindStringArray:
		if cc.category == "A" && (cc.elemEnum || slices.Contains(textTypes, cc.elem)) {
			return true, ""
		}
		return false, "text[]"
	case schema.KindInt64Array:
		if cc.category == "A" && slices.Contains([]string{"int2", "int4", "int8"}, cc.elem) {
			return true, ""
		}
		return false, "bigint[]"
	}
	return true, ""
}

// kindName names a column's Go kind in a drift.
func kindName(c *schema.Column) string {
	switch c.Kind {
	case schema.KindString:
		return "string"
	case schema.KindInt16:
		return "int16"
	case schema.KindInt32:
		return "int32"
	case schema.KindInt64:
		return "int64"
	case schema.KindFloat32:
		return "float32"
	case schema.KindFloat64:
		return "float64"
	case schema.KindBool:
		return "bool"
	case schema.KindBytes:
		return "[]byte"
	case schema.KindTime:
		return "time.Time"
	case schema.KindJSON:
		return "jsonb"
	case schema.KindStringArray:
		return "[]string"
	case schema.KindInt64Array:
		return "[]int64"
	case schema.KindTSVector:
		return "tsvector"
	}
	return "custom"
}

// sqlTypeOf suggests a column type for c in a fix.
func sqlTypeOf(c *schema.Column) string {
	switch c.Kind {
	case schema.KindInt16:
		return "smallint"
	case schema.KindInt32:
		return "integer"
	case schema.KindInt64:
		return "bigint"
	case schema.KindFloat32:
		return "real"
	case schema.KindFloat64:
		return "double precision"
	case schema.KindBool:
		return "boolean"
	case schema.KindBytes:
		return "bytea"
	case schema.KindTime:
		return "timestamptz"
	case schema.KindJSON:
		return "jsonb"
	case schema.KindStringArray:
		return "text[]"
	case schema.KindInt64Array:
		return "bigint[]"
	case schema.KindTSVector:
		return "tsvector"
	case schema.KindOther:
		return "<type>"
	}
	return "text"
}

// notNullOf is " NOT NULL" for a column whose field cannot hold NULL.
func notNullOf(c *schema.Column) string {
	if holdsNull(c) {
		return ""
	}
	return " NOT NULL"
}

// sameType reports whether the declared type name names the catalog's type, format_type's name,
// which carries a schema only when the type is outside the search path.
func sameType(declared, catalog string) bool {
	if declared == catalog {
		return true
	}
	_, unqualified, ok := strings.Cut(declared, ".")
	return ok && unqualified == catalog
}

// verifyFuncsSQL lists, for each schema and name pair (an empty schema resolves through the
// search_path), the overloads of the plain function of that name: argument counts, defaults,
// whether the last argument is variadic, and the result type with domains resolved to their base
// type.
const verifyFuncsSQL = `SELECT n.ord, p.pronargs, p.pronargdefaults, p.provariadic <> 0,
       coalesce(bt.typname::text, ''), coalesce(bt.typcategory::text, ''), coalesce(bt.typtype::text, '')
  FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS n(nsp, name, ord)
  JOIN pg_proc p ON p.proname = n.name AND p.prokind = 'f'
   AND CASE WHEN n.nsp = '' THEN pg_function_is_visible(p.oid)
            ELSE p.pronamespace = (SELECT ns.oid FROM pg_namespace ns WHERE ns.nspname = n.nsp) END
  JOIN pg_type ty ON ty.oid = p.prorettype
  LEFT JOIN pg_type bt ON bt.oid = CASE WHEN ty.typtype = 'd' THEN ty.typbasetype ELSE ty.oid END
 ORDER BY n.ord`

// funcOverload is one overload of a declared function as the catalog describes it.
type funcOverload struct {
	args     int16
	defaults int16
	variadic bool
	result   catalogCol
}

// takes reports whether the overload accepts a call with n arguments.
func (o funcOverload) takes(n int) bool {
	required := int(o.args - o.defaults)
	if o.variadic {
		return n >= required
	}
	return n >= required && n <= int(o.args)
}

// VerifyFuncs checks typed function declarations against the live catalog: each declared function
// must exist as a plain function (not a procedure or an aggregate), resolved through the
// connection's search_path when its name carries no schema, with an overload that takes the
// declared number of arguments (counting defaults and a variadic last argument) and returns a type
// the declared result kind decodes (the rules Verify applies to columns; polymorphic results are
// not checked). It reads the catalog in one statement on a pool connection, outside any
// transaction and scope, and returns one Drift per mismatch in the order of fns; none means every
// call the declarations make resolves.
func VerifyFuncs(ctx context.Context, db *DB, fns ...DeclaredFunc) ([]Drift, error) {
	var decls []*FuncDecl
	for _, f := range fns {
		if f == nil {
			continue
		}
		if d := f.Decl(); d != nil && !slices.Contains(decls, d) {
			decls = append(decls, d)
		}
	}
	if len(decls) == 0 {
		return nil, nil
	}
	nsps := make([]string, len(decls))
	names := make([]string, len(decls))
	for i, d := range decls {
		nsps[i], names[i] = d.nsp, d.proc
	}
	rows, err := db.pool.Query(ctx, verifyFuncsSQL, nsps, names)
	if err != nil {
		return nil, fmt.Errorf("keel: verify functions: %w", err)
	}
	defer rows.Close()
	found := make([][]funcOverload, len(decls))
	for rows.Next() {
		var ord int64
		var o funcOverload
		var typtype string
		if err := rows.Scan(&ord, &o.args, &o.defaults, &o.variadic, &o.result.typ, &o.result.category,
			&typtype); err != nil {
			return nil, fmt.Errorf("keel: verify functions: %w", err)
		}
		o.result.enum = typtype == "e"
		if ord >= 1 && int(ord) <= len(decls) {
			found[ord-1] = append(found[ord-1], o)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keel: verify functions: %w", err)
	}
	var out []Drift
	for i, d := range decls {
		if dr, ok := funcDrift(d, found[i]); ok {
			out = append(out, dr)
		}
	}
	return out, nil
}

// funcDrift compares a declaration with its overloads.
func funcDrift(d *FuncDecl, overloads []funcOverload) (Drift, bool) {
	if len(overloads) == 0 {
		return Drift{Kind: DriftMissingFunction, Table: d.name,
			Detail: "the function does not exist, or is not a plain function, on the search_path",
			Fix:    "create it in a migration, or fix the name given to keel.DeclareFunc"}, true
	}
	var arities []string
	matched := false
	for _, o := range overloads {
		if !o.takes(d.arity) {
			arities = append(arities, arityText(o))
			continue
		}
		matched = true
		if o.result.category == "P" {
			return Drift{}, false
		}
		if holds, _ := kindHolds(d.result, &o.result); holds {
			return Drift{}, false
		}
	}
	if !matched {
		return Drift{Kind: DriftFunctionArity, Table: d.name,
			Detail: fmt.Sprintf("no overload takes %d arguments (overloads take %s)", d.arity,
				strings.Join(arities, ", ")),
			Fix: "declare it with the DeclareFunc of the arity it takes, or add the overload in a migration"}, true
	}
	var results []string
	for _, o := range overloads {
		if o.takes(d.arity) && !slices.Contains(results, o.result.typ) {
			results = append(results, o.result.typ)
		}
	}
	return Drift{Kind: DriftFunctionResult, Table: d.name,
		Detail: fmt.Sprintf("it returns %s, declared as %s", strings.Join(results, " or "), kindNames[d.result]),
		Fix:    "declare it with the expression type of the type it returns"}, true
}

// arityText describes the argument counts an overload takes.
func arityText(o funcOverload) string {
	required := int(o.args - o.defaults)
	switch {
	case o.variadic:
		return fmt.Sprintf("%d or more", required)
	case required == int(o.args):
		return fmt.Sprint(o.args)
	}
	return fmt.Sprintf("%d to %d", required, o.args)
}
