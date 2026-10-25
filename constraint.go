package keel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5/pgconn"
)

// Constraint names a constraint of a model's table and the message its violation reports. A model
// declares the ones whose message matters with a Constraints method, which keel gen records in the
// generated table and keel.Verify checks against the database:
//
//	func (Location) Constraints() []keel.Constraint {
//		return []keel.Constraint{
//			{Name: "idx_locations_code", Message: "A location with this code already exists"},
//		}
//	}
type Constraint = schema.Constraint

// ConstraintKind is the kind of constraint a statement violated.
type ConstraintKind uint8

const (
	// ConstraintUnique is a unique constraint or unique index (23505).
	ConstraintUnique ConstraintKind = iota + 1
	// ConstraintForeignKey is a foreign key (23503).
	ConstraintForeignKey
	// ConstraintCheck is a check constraint (23514).
	ConstraintCheck
	// ConstraintNotNull is a NOT NULL column (23502).
	ConstraintNotNull
	// ConstraintExclusion is an exclusion constraint (23P01).
	ConstraintExclusion
)

// String returns the kind's name.
func (k ConstraintKind) String() string {
	switch k {
	case ConstraintUnique:
		return "unique"
	case ConstraintForeignKey:
		return "foreign key"
	case ConstraintCheck:
		return "check"
	case ConstraintNotNull:
		return "not null"
	case ConstraintExclusion:
		return "exclusion"
	}
	return "constraint"
}

// ConstraintError is a statement's violation of a constraint, resolved against the model of the
// table: every keel statement returns one for a unique, foreign key, check, not-null or exclusion
// violation, wrapping the Postgres error. Read it with errors.As.
type ConstraintError struct {
	Kind ConstraintKind
	// Table is the violated table's name, and Model its generated schema when Config.Tables lists
	// it, else nil.
	Table string
	Model *schema.Table
	// Constraint is the violated constraint's name; empty for a NOT NULL violation.
	Constraint string
	// Columns are the constraint's columns, without the tenant columns: its key columns in order,
	// then the columns its index expressions and predicate read (lower(code), WHERE is_default).
	// Fields are their JSON names on the model and Labels their labels, empty when the model is
	// unknown; a Field the model declares for the constraint replaces them.
	Columns []string
	Fields  []string
	Labels  []string
	// Message is the message the model declares for the constraint, or "".
	Message string
	Err     *pgconn.PgError
}

func (e *ConstraintError) Error() string {
	if e.Constraint == "" {
		return fmt.Sprintf("keel: %s violation on %s.%s", e.Kind, e.Table, strings.Join(e.Columns, ", "))
	}
	return fmt.Sprintf("keel: %s violation of %s on %s", e.Kind, e.Constraint, e.Table)
}

// Unwrap returns the Postgres error, so Code, IsUnique and the other helpers see it.
func (e *ConstraintError) Unwrap() error { return e.Err }

var constraintKinds = map[string]ConstraintKind{
	"23505": ConstraintUnique, "23503": ConstraintForeignKey, "23514": ConstraintCheck,
	"23502": ConstraintNotNull, "23P01": ConstraintExclusion,
}

// constraintCatalog caches the columns of every constraint and unique index of a table, keyed by
// "schema.table", with an empty schema for the current one: read for Config.Tables when the DB
// opens, and for any other table the first time one of its constraints is violated.
type constraintCatalog struct {
	mu     sync.Mutex
	schema string
	tables map[string]map[string][]string
}

// constraintErr turns a constraint violation in err into a *ConstraintError and returns any other
// error as it is.
func (db *DB) constraintErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*ConstraintError](err); ok {
		return err
	}
	pe := pgError(err)
	if pe == nil {
		return err
	}
	kind, ok := constraintKinds[pe.Code]
	if !ok {
		return err
	}
	ce := &ConstraintError{Kind: kind, Table: pe.TableName, Constraint: pe.ConstraintName, Err: pe}
	ce.Model = db.tableNamed(pe.TableName)
	var cols []string
	if kind == ConstraintNotNull {
		cols = []string{pe.ColumnName}
	} else if pe.ConstraintName != "" {
		cols = db.constraintColumns(pe.SchemaName, pe.TableName, pe.ConstraintName)
	}
	for _, c := range cols {
		if ce.Model == nil {
			ce.Columns = append(ce.Columns, c)
			continue
		}
		i := ce.Model.Column(c)
		if i < 0 {
			ce.Columns = append(ce.Columns, c)
			continue
		}
		col := ce.Model.Columns[i]
		if col.Flags.Has(schema.FlagTenant) {
			continue
		}
		ce.Columns = append(ce.Columns, c)
		ce.Fields = append(ce.Fields, col.JSON)
		ce.Labels = append(ce.Labels, col.Label)
	}
	if ce.Model != nil {
		if c, ok := ce.Model.Constraint(pe.ConstraintName); ok {
			ce.Message = c.Message
			if c.Field != "" {
				ce.Fields, ce.Labels = []string{c.Field}, []string{labelOf(ce.Model, c.Field)}
			}
		}
	}
	return ce
}

// labelOf returns the label of t's column whose JSON name is field, or "".
func labelOf(t *schema.Table, field string) string {
	for _, c := range t.Columns {
		if c.JSON == field {
			return c.Label
		}
	}
	return ""
}

// tableNamed returns the table of Config.Tables named name, or nil.
func (db *DB) tableNamed(name string) *schema.Table {
	db.tablesOnce.Do(func() {
		db.tablesByName = make(map[string]*schema.Table, len(db.cfg.Tables))
		for _, t := range db.cfg.Tables {
			db.tablesByName[t.Name] = t
		}
	})
	return db.tablesByName[name]
}

// constraintColumns returns the columns of the constraint or unique index name on table, from the
// catalog Open read for Config.Tables, or else read now on a connection of its own (the failed
// statement's transaction is aborted) and cached per table. It returns nil when the catalog cannot
// be read within a second.
func (db *DB) constraintColumns(schemaName, table, name string) []string {
	if byName, ok := db.cachedConstraints(schemaName, table); ok {
		return slices.Clone(byName[name])
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if db.loadConstraints(ctx, schemaName, []string{table}) != nil {
		return nil
	}
	db.constraints.mu.Lock()
	defer db.constraints.mu.Unlock()
	key := db.constraints.key(schemaName, table)
	if db.constraints.tables[key] == nil {
		db.constraints.tables[key] = map[string][]string{}
	}
	return slices.Clone(db.constraints.tables[key][name])
}

func (db *DB) cachedConstraints(schemaName, table string) (map[string][]string, bool) {
	db.constraints.mu.Lock()
	defer db.constraints.mu.Unlock()
	byName, ok := db.constraints.tables[db.constraints.key(schemaName, table)]
	return byName, ok
}

// key returns the cache key of table in schemaName; the caller holds mu.
func (c *constraintCatalog) key(schemaName, table string) string {
	if schemaName == "" || schemaName == c.schema {
		return "." + table
	}
	return schemaName + "." + table
}

// prefetchConstraints reads the constraints of every table of Config.Tables in one query, so a
// violation never reads the catalog while the pool may be exhausted. A table not yet created is
// read when one of its constraints is first violated.
func (db *DB) prefetchConstraints(ctx context.Context) {
	if len(db.cfg.Tables) == 0 {
		return
	}
	names := make([]string, len(db.cfg.Tables))
	for i, t := range db.cfg.Tables {
		names[i] = t.Name
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = db.loadConstraints(ctx, "", names)
}

// loadConstraints reads the columns of the constraints and unique indexes of tables in schemaName
// (the current schema when empty) and caches them per table.
func (db *DB) loadConstraints(ctx context.Context, schemaName string, tables []string) error {
	rows, err := db.pool.Query(ctx, constraintCatalogSQL, schemaName, tables)
	if err != nil {
		return err
	}
	read := map[string]map[string][]string{}
	var current string
	for rows.Next() {
		var table, name string
		var cols []string
		if err := rows.Scan(&current, &table, &name, &cols); err != nil {
			rows.Close()
			return err
		}
		if read[table] == nil {
			read[table] = map[string][]string{}
		}
		read[table][name] = cols
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	db.constraints.mu.Lock()
	defer db.constraints.mu.Unlock()
	if db.constraints.tables == nil {
		db.constraints.tables = map[string]map[string][]string{}
	}
	if current != "" {
		db.constraints.schema = current
	}
	for table, byName := range read {
		db.constraints.tables[db.constraints.key(schemaName, table)] = byName
	}
	return nil
}

const constraintCatalogSQL = `WITH target AS (
	SELECT c.oid, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE c.relname = ANY($2::text[]) AND n.nspname = coalesce(nullif($1, ''), current_schema())
)
SELECT current_schema()::text, t.relname, con.conname, array_agg(a.attname ORDER BY k.ord)
  FROM target t
  JOIN pg_constraint con ON con.conrelid = t.oid
  CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
 GROUP BY t.relname, con.conname
UNION ALL
SELECT current_schema()::text, t.relname, i.relname, array_agg(a.attname ORDER BY k.ord)
  FROM target t
  JOIN pg_index x ON x.indrelid = t.oid AND x.indisunique
  JOIN pg_class i ON i.oid = x.indexrelid
  CROSS JOIN LATERAL (
	SELECT u.attnum, u.ord FROM unnest(x.indkey::int2[]) WITH ORDINALITY AS u(attnum, ord)
	 WHERE u.attnum <> 0
	UNION ALL
	SELECT d.refobjsubid::int2, 1000 + d.refobjsubid FROM pg_depend d
	 WHERE d.classid = 'pg_class'::regclass AND d.objid = x.indexrelid
	   AND d.refclassid = 'pg_class'::regclass AND d.refobjid = t.oid AND d.refobjsubid > 0
	   AND d.refobjsubid <> ALL (x.indkey::int2[])
  ) AS k(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
 WHERE NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = x.indexrelid)
 GROUP BY t.relname, i.relname`
