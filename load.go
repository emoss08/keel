package keel

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// keyKind is the Go kind of a relation key column.
type keyKind uint8

const (
	keyString keyKind = iota
	keyInt
)

// keyKindsOf returns the key kinds of columns cols of t.
func keyKindsOf(t *schema.Table, cols []int32) ([]keyKind, error) {
	kinds := make([]keyKind, len(cols))
	for i, c := range cols {
		k, err := keyKindOf(t, c)
		if err != nil {
			return nil, err
		}
		kinds[i] = k
	}
	return kinds, nil
}

func keyKindOf(t *schema.Table, col int32) (keyKind, error) {
	switch t.Columns[col].Kind {
	case schema.KindString:
		return keyString, nil
	case schema.KindInt64, schema.KindInt32, schema.KindInt16:
		return keyInt, nil
	}
	return 0, errf("%s.%s: relation keys must be string or integer columns", t.Name, t.Columns[col].Name)
}

// relKey is one relation key value. It is comparable, so keys index parents without boxing. A key
// of several columns is their values joined in s (see joinKey).
type relKey struct {
	s string
	n int64
}

// maxKeyParts is the number of key columns read without allocating the parts.
const maxKeyParts = 4

// readKeys reads key columns cols of row as one relKey: the column's value for one column, the
// values joined for several. ok is false when a column is NULL, or holds a nullzero zero value.
func readKeys(t *schema.Table, row unsafe.Pointer, cols []int32, kinds []keyKind) (relKey, bool) {
	if len(cols) == 1 {
		return readKey(t, row, cols[0])
	}
	var buf [maxKeyParts]relKey
	parts := buf[:0]
	for _, c := range cols {
		k, ok := readKey(t, row, c)
		if !ok {
			return relKey{}, false
		}
		parts = append(parts, k)
	}
	return joinKey(parts, kinds), true
}

// joinKey encodes the parts of a key of several columns as one comparable relKey: the values in
// order, integers in decimal, separated by NUL, which text values cannot hold.
func joinKey(parts []relKey, kinds []keyKind) relKey {
	b := make([]byte, 0, 48)
	for i, p := range parts {
		if i > 0 {
			b = append(b, 0)
		}
		if kinds[i] == keyString {
			b = append(b, p.s...)
		} else {
			b = strconv.AppendInt(b, p.n, 10)
		}
	}
	return relKey{s: string(b)}
}

// readKey reads a key column of row. ok is false for a NULL key, and for the zero value of a
// nullzero column, which stores it as NULL.
func readKey(t *schema.Table, row unsafe.Pointer, col int32) (relKey, bool) {
	k, ok := readKeyValue(t, row, col)
	if ok && k == (relKey{}) && t.Columns[col].Flags.Has(schema.FlagNullZero) {
		return relKey{}, false
	}
	return k, ok
}

// readKeyValue reads a key column of row without reflection. ok is false for a nil pointer.
func readKeyValue(t *schema.Table, row unsafe.Pointer, col int32) (relKey, bool) {
	switch p := t.Ptr(row, col).(type) {
	case *string:
		return relKey{s: *p}, true
	case **string:
		if *p == nil {
			return relKey{}, false
		}
		return relKey{s: **p}, true
	case *int64:
		return relKey{n: *p}, true
	case **int64:
		if *p == nil {
			return relKey{}, false
		}
		return relKey{n: **p}, true
	case *int32:
		return relKey{n: int64(*p)}, true
	case **int32:
		if *p == nil {
			return relKey{}, false
		}
		return relKey{n: int64(**p)}, true
	case *int16:
		return relKey{n: int64(*p)}, true
	case **int16:
		if *p == nil {
			return relKey{}, false
		}
		return relKey{n: int64(**p)}, true
	}
	return relKey{}, false
}

// keyScanners decode the leading key columns of a keyed statement into one relKey.
type keyScanners []keyScanner

// scan decodes the key columns of a row: the column's value for one column, the values joined
// for several.
func (ks keyScanners) scan(m *pgtype.Map, fds []pgconn.FieldDescription, raw [][]byte, kinds []keyKind) (relKey, error) {
	if len(ks) == 1 {
		return ks[0].scan(m, fds[0], raw[0], kinds[0])
	}
	var buf [maxKeyParts]relKey
	parts := buf[:0]
	for i := range ks {
		k, err := ks[i].scan(m, fds[i], raw[i], kinds[i])
		if err != nil {
			return relKey{}, err
		}
		parts = append(parts, k)
	}
	return joinKey(parts, kinds), nil
}

// keyScanner decodes one leading key column of a keyed statement into a relKey.
type keyScanner struct {
	plan pgtype.ScanPlan
	s    string
	n    int64
}

func (k *keyScanner) scan(m *pgtype.Map, fd pgconn.FieldDescription, raw []byte, kind keyKind) (relKey, error) {
	var dst any = &k.s
	if kind == keyInt {
		dst = &k.n
	}
	if k.plan == nil {
		k.plan = m.PlanScan(fd.DataTypeOID, fd.Format, dst)
	}
	if err := k.plan.Scan(raw, dst); err != nil {
		return relKey{}, fmt.Errorf("keel: scan relation key: %w", err)
	}
	return relKey{s: k.s, n: k.n}, nil
}

// parentIndex groups the parents of a to-many load by key and keeps the distinct keys as one keel
// array parameter per key column, bound to = ANY($1) or unnested.
type parentIndex struct {
	kinds []keyKind
	byKey map[relKey][]unsafe.Pointer
	strs  []textArray
	ints  []int8Array
}

// indexParents indexes parents by their key columns cols, skipping parents whose key is NULL.
func indexParents(t *schema.Table, parents []unsafe.Pointer, cols []int32, kinds []keyKind) *parentIndex {
	ix := &parentIndex{kinds: kinds, byKey: make(map[relKey][]unsafe.Pointer, len(parents)),
		strs: make([]textArray, len(cols)), ints: make([]int8Array, len(cols))}
	for _, p := range parents {
		k, ok := readKeys(t, p, cols, kinds)
		if !ok {
			continue
		}
		ps, seen := ix.byKey[k]
		if !seen {
			ix.addKey(t, p, cols, k)
		}
		ix.byKey[k] = append(ps, p)
	}
	return ix
}

// addKey appends the values of a new key to the key arrays; k is the key of one column, the
// columns are read again for several.
func (ix *parentIndex) addKey(t *schema.Table, p unsafe.Pointer, cols []int32, k relKey) {
	for i, c := range cols {
		part := k
		if len(cols) > 1 {
			part, _ = readKey(t, p, c)
		}
		if ix.kinds[i] == keyString {
			ix.strs[i] = append(ix.strs[i], part.s)
		} else {
			ix.ints[i] = append(ix.ints[i], part.n)
		}
	}
}

func (ix *parentIndex) len() int { return len(ix.byKey) }

// cols is the number of key columns.
func (ix *parentIndex) cols() int { return len(ix.kinds) }

// arg is the key array of key column i to bind.
func (ix *parentIndex) arg(i int) any {
	if ix.kinds[i] == keyString {
		return ix.strs[i]
	}
	return ix.ints[i]
}

// arrayType is the array type key array i is cast to where Postgres cannot infer it.
func (ix *parentIndex) arrayType(i int) string {
	if ix.kinds[i] == keyString {
		return "text[]"
	}
	return "int8[]"
}

// keyName is the name of key column i of an unnested key list: k for a single column, k1, k2, …
// for several.
func (ix *parentIndex) keyName(i int) string {
	if ix.cols() == 1 {
		return "k"
	}
	return "k" + strconv.Itoa(i+1)
}

// writeUnnest writes unnest($1::t1[], …) AS alias(k…): the distinct keys as rows.
func (ix *parentIndex) writeUnnest(r *render, alias string) {
	r.s("unnest(")
	for i := range ix.cols() {
		if i > 0 {
			r.s(", ")
		}
		r.arg(ix.arg(i))
		r.s("::")
		r.s(ix.arrayType(i))
	}
	r.s(") AS ")
	r.s(alias)
	r.b('(')
	for i := range ix.cols() {
		if i > 0 {
			r.s(", ")
		}
		r.s(ix.keyName(i))
	}
	r.b(')')
}

// writeKeyEq writes the equalities of columns cols of the table under alias with the unnested key
// columns of keyAlias.
func (ix *parentIndex) writeKeyEq(r *render, t *schema.Table, alias string, cols []int32, keyAlias string) {
	for i, c := range cols {
		if i > 0 {
			r.s(" AND ")
		}
		r.s(alias)
		r.b('.')
		r.s(t.Columns[c].Quoted)
		r.s(" = ")
		r.s(keyAlias)
		r.b('.')
		r.s(ix.keyName(i))
	}
}

// writeIn writes the filter of a to-many load on columns cols of the table under alias: col =
// ANY($1) for one key column, (cols) IN (SELECT * FROM unnest(…)) for several.
func (ix *parentIndex) writeIn(r *render, t *schema.Table, alias string, cols []int32) {
	ix.writeTupleIn(r, func(r *render, i int) {
		r.s(alias)
		r.b('.')
		r.s(t.Columns[cols[i]].Quoted)
	})
}

// writeTupleIn writes the filter of a load on the expressions col writes, one per key column:
// col = ANY($1) for one key column, (cols) IN (SELECT * FROM unnest(…)) for several.
func (ix *parentIndex) writeTupleIn(r *render, col func(r *render, i int)) {
	if ix.cols() == 1 {
		col(r, 0)
		r.s(" = ANY(")
		r.arg(ix.arg(0))
		r.b(')')
		return
	}
	r.b('(')
	for i := range ix.cols() {
		if i > 0 {
			r.s(", ")
		}
		col(r, i)
	}
	r.s(") IN (SELECT * FROM ")
	ix.writeUnnest(r, r.newAlias("k"))
	r.b(')')
}

// crossTenant returns the tenant columns a load from parent to child matches on before the
// relation's keys when its statement crosses tenants, under a system scope: the tenant columns the
// two tables share (tenantPairs), as parent's and child's column indexes. Parents of several
// tenants may hold the same key, so the load fetches and groups children by (tenant columns, keys).
// Under a tenant scope every parent and child row is pinned to the scope's tenant, so the keys
// alone match and there are none.
func crossTenant(sc *scopeState, parent, child *schema.Table) (local, remote []int32) {
	if !sc.system {
		return nil, nil
	}
	return tenantPairs(parent, child)
}

// tenantPairs returns the tenant columns child shares with parent: each tenant column of child
// that parent carries as a tenant column too, as parent's and child's column indexes.
func tenantPairs(parent, child *schema.Table) (local, remote []int32) {
	if !parent.HasTenant() {
		return nil, nil
	}
	for _, ci := range child.Tenant {
		pi := parent.Column(child.Columns[ci].Name)
		if pi >= 0 && parent.Columns[pi].Flags.Has(schema.FlagTenant) {
			local = append(local, pi)
			remote = append(remote, ci)
		}
	}
	return local, remote
}

// firstTarget is the table a load of rel starts from: the target of the first relation of a
// pick's path, the relation's target otherwise.
func firstTarget(parent *schema.Table, rel *schema.Relation) *schema.Table {
	if rel.Kind == schema.Pick {
		return parent.Relations[rel.Path[0]].Target
	}
	return rel.Target
}

// pending is one to-many load: the node and the parent rows it attaches to.
type pending struct {
	node    *lnode
	parents []unsafe.Pointer
}

func pendingOf(plan *selectPlan, many []*lnode) []pending {
	var work []pending
	for _, n := range many {
		if rows := plan.rows[n.parent.idx]; len(rows) > 0 {
			work = append(work, pending{node: n, parents: rows})
		}
	}
	return work
}

// loadPending sends one statement per to-many node of a level, all in one round trip, then loads
// the next level.
func loadPending(ctx context.Context, run *runner, work []pending) error {
	if len(work) == 0 {
		return nil
	}
	stmts := make([]*stmt, 0, len(work))
	plans := make([]*selectPlan, 0, len(work))
	deeper := false
	for _, w := range work {
		s, plan, err := loadStmt(ctx, run, w)
		if err != nil {
			return err
		}
		if s == nil {
			continue
		}
		stmts = append(stmts, s)
		plans = append(plans, plan)
		deeper = deeper || len(w.node.manyChildren()) > 0
	}
	if err := run.batch(ctx, stmts, !deeper); err != nil {
		return err
	}
	var next []pending
	for i, plan := range plans {
		next = append(next, pendingOf(plan, plan.nodes[0].manyChildren())...)
		if err := plans[i].afterLoad(ctx); err != nil {
			return err
		}
	}
	return loadPending(ctx, run, next)
}

// loadStmt builds the statement that loads a to-many or pick node for its parents: a filter over
// the parents' keys (= ANY($1) for one key column, (cols) IN (SELECT * FROM unnest(…)) for several),
// through the join table for many-to-many, LATERAL per parent key when the load has a limit or picks
// a row. Under a system scope the keys start with the tenant columns parent and child share
// (crossTenant), in the statement and in the grouping of children under their parents. It returns
// a nil statement when no parent has a key.
func loadStmt(ctx context.Context, run *runner, w pending) (*stmt, *selectPlan, error) {
	n := w.node
	parent := n.parent.table
	rel := &parent.Relations[n.rel]
	if rel.Kind == schema.ManyToMany && len(rel.Local) != 1 {
		return nil, nil, errf("relation %s.%s: many-to-many loads need a single-column key", parent.Name, rel.Name)
	}
	tenantLocal, tenantRemote := crossTenant(run.sc, parent, firstTarget(parent, rel))
	local := slices.Concat(tenantLocal, rel.Local)
	remote := slices.Concat(tenantRemote, rel.Remote)
	kinds, err := keyKindsOf(parent, local)
	if err != nil {
		return nil, nil, err
	}
	ix := indexParents(parent, w.parents, local, kinds)
	if ix.len() == 0 {
		return nil, nil, nil
	}

	q := query{ref: n.ref}
	if n.spec != nil {
		q = *n.spec
		q.ref = n.ref
	}
	q.allTenants = n.allTenants()
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	keyed := rel.Kind == schema.ManyToMany || rel.Kind == schema.Pick

	var plan *selectPlan
	switch {
	case rel.Kind == schema.Pick:
		if q.limit > 0 {
			return nil, nil, errf("relation %s.%s: pick loads do not take a Limit", parent.Name, rel.Name)
		}
		plan = pickSelect(r, &q, n, ix, tenantRemote)
	case rel.Kind == schema.ManyToMany:
		plan = q.renderSelect(r, n, m2mOpts(r, parent, rel, ix, tenantRemote))
		if q.limit > 0 {
			return nil, nil, errf("relation %s.%s: many-to-many loads do not take a Limit", parent.Name, rel.Name)
		}
	case q.limit > 0:
		plan = lateralSelect(r, &q, n, ix, remote, tenantRemote)
	default:
		plan = q.renderSelect(r, n, selectOpts{
			extraCols:  tenantRemote,
			extraWhere: func(r *render, alias string) { ix.writeIn(r, n.table, alias, remote) },
		})
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	plan.keyKinds = kinds
	plan.sizeHint = min(ix.len(), maxSlab)
	plan.track()
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: n.table.Name,
		customPlan: q.customPlan}
	child := n.table
	s.scan = func(rows pgx.Rows) error {
		return plan.scan(run.sc, s.sql, rows, func(row unsafe.Pointer, key relKey) {
			if !keyed {
				var ok bool
				if key, ok = readKeys(child, row, remote, kinds); !ok {
					return
				}
			}
			for _, p := range ix.byKey[key] {
				parent.Attach(p, n.rel, row)
			}
		})
	}
	return s, plan, nil
}

// m2mOpts reads a many-to-many relation through its join table, joined to the target on its key
// and the tenant columns they share, returning the parent key first: the join table's key to the
// parent, after the target's tenant columns tenant when the load crosses tenants, since a parent's
// targets belong to its tenant.
func m2mOpts(r *render, parent *schema.Table, rel *schema.Relation, ix *parentIndex, tenant []int32,
) selectOpts {
	target, remote := rel.Target, rel.Remote[0]
	jalias := r.newAlias(rel.JoinAlias)
	key := func(alias string) func(r *render, i int) {
		return func(r *render, i int) {
			if i < len(tenant) {
				r.s(alias)
				r.b('.')
				r.s(target.Columns[tenant[i]].Quoted)
				return
			}
			r.s(jalias)
			r.b('.')
			r.s(rel.JoinLocalQ[0])
		}
	}
	return selectOpts{
		keyColumn: func(r *render, alias string) {
			col := key(alias)
			for i := range ix.cols() {
				if i > 0 {
					r.s(", ")
				}
				col(r, i)
			}
		},
		extraJoin: func(r *render, alias string) {
			r.s(" JOIN ")
			r.s(rel.JoinQuoted)
			r.b(' ')
			r.s(jalias)
			r.s(" ON ")
			r.s(jalias)
			r.b('.')
			r.s(rel.JoinRemoteQ[0])
			r.s(" = ")
			r.s(alias)
			r.b('.')
			r.s(target.Columns[remote].Quoted)
			r.joinTenantEq(parent, rel, jalias, alias)
		},
		extraWhere: func(r *render, alias string) {
			ix.writeTupleIn(r, key(alias))
		},
	}
}

// lateralSelect loads at most q.limit rows per parent key:
// SELECT … FROM unnest($1, …) k CROSS JOIN LATERAL (SELECT … WHERE fk = k.k … LIMIT n). extra are
// columns the rows must carry besides the query's.
func lateralSelect(r *render, q *query, n *lnode, ix *parentIndex, remote, extra []int32) *selectPlan {
	keyAlias := r.newAlias("k")
	var plan *selectPlan
	body := r.section(func() {
		plan = q.renderSelect(r, n, selectOpts{
			extraCols:  extra,
			extraWhere: func(r *render, alias string) { ix.writeKeyEq(r, n.table, alias, remote, keyAlias) },
		})
	})
	outer := r.newAlias("lat")
	r.s("SELECT ")
	r.s(outer)
	r.s(".* FROM ")
	ix.writeUnnest(r, keyAlias)
	r.s(" CROSS JOIN LATERAL (")
	r.buf = append(r.buf, body...)
	r.s(") ")
	r.s(outer)
	r.release(body)
	return plan
}
