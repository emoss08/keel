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
// array parameter per key column, bound to = ANY($1) or unnested. A pipelined load's index has no
// arrays: sub writes its keys as a subquery over the parent level's statement, and group fills
// byKey once the parents' rows are read, before the load's own.
type parentIndex struct {
	kinds []keyKind
	byKey map[relKey][]unsafe.Pointer
	strs  []textArray
	ints  []int8Array
	sub   func(r *render)
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

// group indexes parents by their key columns cols, skipping parents whose key is NULL, for a
// pipelined load whose keys the statement read itself.
func (ix *parentIndex) group(t *schema.Table, parents []unsafe.Pointer, cols []int32) {
	ix.byKey = make(map[relKey][]unsafe.Pointer, len(parents))
	for _, p := range parents {
		k, ok := readKeys(t, p, cols, ix.kinds)
		if !ok {
			continue
		}
		ix.byKey[k] = append(ix.byKey[k], p)
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

// writeUnnest writes unnest($1::t1[], …) AS alias(k…): the distinct keys as rows. A pipelined
// load's keys are (SELECT DISTINCT * FROM (subquery) kq) AS alias(k…).
func (ix *parentIndex) writeUnnest(r *render, alias string) {
	if ix.sub != nil {
		r.s("(SELECT DISTINCT * FROM (")
		ix.sub(r)
		r.s(") ")
		r.s(r.newAlias("kq"))
		r.s(") AS ")
		r.s(alias)
		ix.writeKeyNames(r)
		return
	}
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
	ix.writeKeyNames(r)
}

// writeKeyNames writes the column list (k…) of an unnested key list.
func (ix *parentIndex) writeKeyNames(r *render) {
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
// col = ANY($1) for one key column, (cols) IN (SELECT * FROM unnest(…)) for several, and
// col IN (subquery) or (cols) IN (subquery) for a pipelined load.
func (ix *parentIndex) writeTupleIn(r *render, col func(r *render, i int)) {
	if ix.sub != nil {
		if ix.cols() == 1 {
			col(r, 0)
		} else {
			r.b('(')
			for i := range ix.cols() {
				if i > 0 {
					r.s(", ")
				}
				col(r, i)
			}
			r.b(')')
		}
		r.s(" IN (")
		ix.sub(r)
		r.b(')')
		return
	}
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
// two tables share (schema.SharedTenant), as parent's and child's column indexes. Parents of
// several tenants may hold the same key, so the load fetches and groups children by (tenant
// columns, keys). Under a tenant scope every parent and child row is pinned to the scope's tenant,
// so the keys alone match and there are none.
func crossTenant(sc *scopeState, parent, child *schema.Table) (local, remote []int32) {
	if !sc.system {
		return nil, nil
	}
	return schema.SharedTenant(parent, child)
}

// firstTarget is the table a load of rel starts from: the target of the first relation of a
// pick's path, the relation's target otherwise.
func firstTarget(parent *schema.Table, rel *schema.Relation) *schema.Table {
	if rel.Kind == schema.Pick {
		return parent.Relations[rel.Path[0]].Target
	}
	return rel.Target
}

// LoadStrategy is how a read loads its to-many relations, many-to-many relations and the picks it
// does not join: Config.LoadStrategy sets the default and Query.LoadStrategy chooses for one query
// and the loads below it.
type LoadStrategy uint8

const (
	// LoadPipelined sends the read's statement and the statement of every level of its relation
	// tree in one round trip. A level selects its parents' keys with a subquery over the parent
	// level's own statement (its filters, joins, order, limit, offset and keyset), nested for
	// deeper levels, so it needs no keys read back. It runs only where every statement reads one
	// snapshot: in a transaction at RepeatableRead or Serializable, or outside a transaction,
	// where keel runs the read in an implicit REPEATABLE READ READ ONLY transaction of its own (in
	// the same round trip). A level whose parent level's rows are not the same every time its
	// statement runs loads per level (see the guide's "One round trip"). It is the default.
	LoadPipelined LoadStrategy = iota + 1
	// LoadPerLevel sends one round trip per level: each level reads its parents' keys from the
	// rows of the level before it and binds them as arrays (= ANY($1), unnest).
	LoadPerLevel
)

// LoadStrategy chooses how the query's relations load, overriding Config.LoadStrategy for the
// query and every load below it; a load can choose again for its own subtree:
// Load(m.LoadStrategy(keel.LoadPerLevel)).
func (q *Query[T]) LoadStrategy(s LoadStrategy) *Query[T] {
	q.q.loadStrategy = s
	return q
}

// strategy is the LoadStrategy of node n's statement: its own query's or the nearest one above
// it that chose one, else Config.LoadStrategy.
func (n *lnode) strategy(db *DB) LoadStrategy {
	for ; n != nil; n = n.parent {
		if n.spec != nil && n.spec.loadStrategy != 0 {
			return n.spec.loadStrategy
		}
	}
	if db.cfg.LoadStrategy == LoadPerLevel {
		return LoadPerLevel
	}
	return LoadPipelined
}

// pipelinedAny reports whether any of nodes loads with LoadPipelined.
func pipelinedAny(db *DB, nodes []*lnode) bool {
	for _, n := range nodes {
		if n.strategy(db) == LoadPipelined {
			return true
		}
	}
	return false
}

// oneSnapshot reports whether every statement the runner sends from now on reads one snapshot,
// or will once holdSnapshot has run: in a transaction at RepeatableRead or Serializable, in an
// implicit read-only transaction keel owns and has not begun, or outside any transaction, where
// holdSnapshot opens one. A joined transaction (JoinTx) has no known level and never does.
func (r *runner) oneSnapshot() bool {
	tx := r.tx
	switch {
	case tx == nil:
		return true
	case r.own && !tx.begun && tx.readOnly:
		return true
	}
	return tx.snapshot()
}

// holdSnapshot makes the runner's statements read one snapshot, as oneSnapshot promised: the
// implicit read-only transaction keel owns begins at RepeatableRead, and a runner outside any
// transaction opens such a transaction, on its connection when it has one. BEGIN rides in the next
// round trip and COMMIT in the last, so the snapshot costs no round trip.
func (r *runner) holdSnapshot(ctx context.Context) {
	if r.tx == nil {
		r.tx = &txState{db: r.db, readOnly: true, implicit: true, scopeCtx: ctx, scope: r.sc, rep: r.rep}
		if r.conn != nil {
			r.tx.conn, r.tx.pooled = r.conn, r.conn
			r.conn = nil
		}
		r.own = true
	}
	if r.own && !r.tx.begun && r.tx.readOnly && r.tx.opts.isolation == 0 {
		r.tx.opts.isolation = RepeatableRead
	}
}

// stableRows reports whether the read's statement selects the same rows every time it runs on one
// snapshot, so a subquery over it selects its rows' keys, and returns the primary key columns to
// append to its order to make it so. A statement without a limit or offset is stable. A limited
// one is stable when the read fails unless the statement returned every row it matched (One:
// whole), or when its order ends in a unique key (stableOrder). A lock (SKIP LOCKED skips rows a
// second read may not), DISTINCT ON, grouping and a limited DISTINCT are not.
func (q *query) stableRows(root *lnode, fo fetchOpts, system bool) (bool, []int32) {
	if q.lock != "" || len(q.distinctOn) > 0 || len(q.group) > 0 || len(q.having) > 0 {
		return false, nil
	}
	limit := fo.limit
	if limit == 0 {
		limit = q.limit
	}
	switch {
	case limit == 0 && q.offset == 0:
		return true, nil
	case q.distinct:
		return false, nil
	case fo.whole && q.offset == 0:
		return true, nil
	}
	return stableOrder(q.order, root, system)
}

// stableOrder reports whether order, the order of a limited statement on node sn's table, can end
// in a unique key, and returns the primary key columns it lacks, which keel appends as a
// tie-break; under a tenant scope the tenant columns of the key are constants and need no place.
// An empty order is not stable: keel does not order a statement the caller left unordered.
func stableOrder(order []Order, sn *lnode, system bool) (bool, []int32) {
	t := sn.table
	if len(order) == 0 || len(t.PK) == 0 {
		return false, nil
	}
	var missing []int32
	for _, pk := range t.PK {
		if !system && slices.Contains(t.Tenant, pk) {
			continue
		}
		if !ordersBy(order, sn, pk) {
			missing = append(missing, pk)
		}
	}
	return true, missing
}

// ordersBy reports whether order names column col of node sn's table, through sn's reference or
// the table's base handle.
func ordersBy(order []Order, sn *lnode, col int32) bool {
	for _, o := range order {
		c := o.col
		if c == nil {
			if cc, ok := o.e.(Col); ok {
				c = &cc
			}
		}
		if c == nil || c.Ref == nil || c.Index != col || c.Ref.Table != sn.table {
			continue
		}
		if c.Ref == sn.ref || (!c.Ref.isPath() && c.Ref.alias == "") {
			return true
		}
	}
	return false
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

// later is a level a pipeline leaves to a round trip of its own: the nodes many of plan's
// statement, which load per level once its rows are read.
type later struct {
	plan *selectPlan
	many []*lnode
}

// level is a statement of a relation tree whose rows parent the next level's: the root read or a
// relation load. keys writes the statement again as a subquery that selects only the key columns
// cols of its statement node of; stable reports whether that subquery selects the rows the
// statement reads, so the next level can filter on it.
type level struct {
	plan   *selectPlan
	stable bool
	keys   func(r *render, of *lnode, cols []int32)
}

// pipeline is the relation loads of one round trip: their statements, each after its parent
// level's, their plans, and the levels left to load per level after the round trip.
type pipeline struct {
	snapshot bool
	stmts    []*stmt
	plans    []*selectPlan
	later    []later
	subq     int
}

// add adds the loads of nodes many of lv's statement and of their subtrees. A node loads in the
// pipeline's round trip, filtered by a subquery over lv's statement, when the runner reads one
// snapshot, lv is stable and the node loads with LoadPipelined; otherwise it loads per level
// after the round trip.
func (pl *pipeline) add(ctx context.Context, run *runner, lv level, many []*lnode) error {
	var rest []*lnode
	for _, n := range many {
		if !pl.snapshot || !lv.stable || n.strategy(run.db) != LoadPipelined {
			rest = append(rest, n)
			continue
		}
		l, err := newLoad(run, n, true)
		if err != nil {
			return err
		}
		of, cols, parent, at := n.parent, l.local, lv.plan, n.parent.idx
		l.ix = &parentIndex{kinds: l.kinds, sub: func(r *render) { lv.keys(r, of, cols) }}
		s, plan, err := l.statement(ctx, run, func() []unsafe.Pointer { return parent.rows[at] })
		if err != nil {
			return err
		}
		pl.stmts = append(pl.stmts, s)
		pl.plans = append(pl.plans, plan)
		pl.subq++
		if err := pl.add(ctx, run, l.level(plan), plan.nodes[0].manyChildren()); err != nil {
			return err
		}
	}
	if len(rest) > 0 {
		pl.later = append(pl.later, later{plan: lv.plan, many: rest})
	}
	return nil
}

// piped reports whether the round trip carries pipelined loads, which need holdSnapshot.
func (pl *pipeline) piped() bool { return pl != nil && pl.subq > 0 }

// deeper reports whether levels are left to load per level after the round trip.
func (pl *pipeline) deeper() bool { return pl != nil && len(pl.later) > 0 }

// pending returns the per-level loads left after the round trip, once its rows are read.
func (pl *pipeline) pending() []pending {
	if pl == nil {
		return nil
	}
	var work []pending
	for _, l := range pl.later {
		work = append(work, pendingOf(l.plan, l.many)...)
	}
	return work
}

// afterLoad runs the AfterLoad hooks of the pipeline's loads, deeper levels first.
func (pl *pipeline) afterLoad(ctx context.Context) error {
	if pl == nil {
		return nil
	}
	for i := len(pl.plans) - 1; i >= 0; i-- {
		if err := pl.plans[i].afterLoad(ctx); err != nil {
			return err
		}
	}
	return nil
}

// loadPending sends one statement per to-many node of a level, filtered by the parents' keys
// bound as arrays, with the pipelined loads of their subtrees, all in one round trip, then loads
// the levels left after it.
func loadPending(ctx context.Context, run *runner, work []pending) error {
	if len(work) == 0 {
		return nil
	}
	pl := &pipeline{snapshot: run.oneSnapshot()}
	for _, w := range work {
		l, err := newLoad(run, w.node, pl.snapshot)
		if err != nil {
			return err
		}
		l.ix = indexParents(l.parent, w.parents, l.local, l.kinds)
		if l.ix.len() == 0 {
			continue
		}
		s, plan, err := l.statement(ctx, run, nil)
		if err != nil {
			return err
		}
		pl.stmts = append(pl.stmts, s)
		pl.plans = append(pl.plans, plan)
		if err := pl.add(ctx, run, l.level(plan), plan.nodes[0].manyChildren()); err != nil {
			return err
		}
	}
	if pl.piped() {
		run.holdSnapshot(ctx)
	}
	if err := run.batch(ctx, pl.stmts, !pl.deeper()); err != nil {
		return err
	}
	if err := loadPending(ctx, run, pl.pending()); err != nil {
		return err
	}
	return pl.afterLoad(ctx)
}

// load is the statement that loads a to-many or pick node for its parents: a filter over the
// parents' keys (= ANY($1) for one key column, (cols) IN (SELECT * FROM unnest(…)) for several, or
// a subquery over the parent level's statement when pipelined), through the join table for
// many-to-many, LATERAL per parent key when the load has a limit or picks a row. Under a system
// scope the keys start with the tenant columns parent and child share (crossTenant), in the
// statement and in the grouping of children under their parents.
type load struct {
	n            *lnode
	parent       *schema.Table
	rel          *schema.Relation
	q            query
	ix           *parentIndex
	local        []int32
	remote       []int32
	tenantRemote []int32
	kinds        []keyKind
	stable       bool
	tieBreak     []int32
}

// newLoad prepares the load of node n. When snapshot is set and n has pipelined loads below it,
// it decides whether n's statement is stable for them: a per-parent Limit load must end its order
// in a unique key (stableOrder), which keel then appends; every other load is.
func newLoad(run *runner, n *lnode, snapshot bool) (*load, error) {
	parent := n.parent.table
	rel := &parent.Relations[n.rel]
	if rel.Kind == schema.ManyToMany && len(rel.Local) != 1 {
		return nil, errf("relation %s.%s: many-to-many loads need a single-column key", parent.Name, rel.Name)
	}
	tenantLocal, tenantRemote := crossTenant(run.sc, parent, firstTarget(parent, rel))
	l := &load{n: n, parent: parent, rel: rel, tenantRemote: tenantRemote,
		local: slices.Concat(tenantLocal, rel.Local), remote: slices.Concat(tenantRemote, rel.Remote)}
	kinds, err := keyKindsOf(parent, l.local)
	if err != nil {
		return nil, err
	}
	l.kinds = kinds
	l.q = query{ref: n.ref}
	if n.spec != nil {
		l.q = *n.spec
		l.q.ref = n.ref
	}
	l.q.allTenants = n.allTenants()
	if l.q.limit > 0 {
		switch rel.Kind {
		case schema.Pick:
			return nil, errf("relation %s.%s: pick loads do not take a Limit", parent.Name, rel.Name)
		case schema.ManyToMany:
			return nil, errf("relation %s.%s: many-to-many loads do not take a Limit", parent.Name, rel.Name)
		}
	}
	l.stable = true
	if l.q.limit > 0 {
		l.stable = false
		if snapshot && pipelinedAny(run.db, n.manyChildren()) {
			l.stable, l.tieBreak = stableOrder(l.q.order, n, run.sc.system)
		}
	}
	return l, nil
}

// render writes the load's statement with o's key subquery and tie-break options.
func (l *load) render(r *render, o selectOpts) *selectPlan {
	n := l.n
	switch {
	case l.rel.Kind == schema.Pick:
		return pickSelect(r, &l.q, n, l.ix, l.tenantRemote, o)
	case l.rel.Kind == schema.ManyToMany:
		return l.q.renderSelect(r, n, m2mOpts(r, l.parent, l.rel, l.ix, l.tenantRemote, o))
	case l.q.limit > 0:
		o.extraCols = l.tenantRemote
		return lateralSelect(r, &l.q, n, l.ix, l.remote, o)
	}
	o.extraCols = l.tenantRemote
	o.extraWhere = func(r *render, alias string) { l.ix.writeIn(r, n.table, alias, l.remote) }
	return l.q.renderSelect(r, n, o)
}

// level is the load's statement as the parent level of the loads below it.
func (l *load) level(plan *selectPlan) level {
	return level{plan: plan, stable: l.stable, keys: func(r *render, of *lnode, cols []int32) {
		l.render(r, selectOpts{keysOf: of, keyCols: cols, tieBreak: l.tieBreak})
	}}
}

// statement renders the load's statement. Its scanner attaches each row to the parents with its
// key; a pipelined load (parents set) first groups the parent level's rows, read before its own.
func (l *load) statement(ctx context.Context, run *runner, parents func() []unsafe.Pointer,
) (*stmt, *selectPlan, error) {
	r := newRender(run.sc, ctx, run.db)
	defer r.recycle()
	plan := l.render(r, selectOpts{tieBreak: l.tieBreak})
	if r.err != nil {
		return nil, nil, r.err
	}
	plan.keyKinds = l.kinds
	plan.sizeHint = min(l.ix.len(), maxSlab)
	plan.track()
	n, parent, ix := l.n, l.parent, l.ix
	s := &stmt{sql: string(r.buf), args: r.argsCopy(), op: "select", table: n.table.Name,
		customPlan: l.q.customPlan}
	keyed := l.rel.Kind == schema.ManyToMany || l.rel.Kind == schema.Pick
	child, local, remote, kinds := n.table, l.local, l.remote, l.kinds
	s.scan = func(rows pgx.Rows) error {
		if parents != nil {
			ix.group(parent, parents(), local)
			plan.sizeHint = min(ix.len(), maxSlab)
		}
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
// targets belong to its tenant. It adds them to o.
func m2mOpts(r *render, parent *schema.Table, rel *schema.Relation, ix *parentIndex, tenant []int32,
	o selectOpts,
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
	o.keyColumn = func(r *render, alias string) {
		col := key(alias)
		for i := range ix.cols() {
			if i > 0 {
				r.s(", ")
			}
			col(r, i)
		}
	}
	o.extraJoin = func(r *render, alias string) {
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
	}
	o.extraWhere = func(r *render, alias string) {
		ix.writeTupleIn(r, key(alias))
	}
	return o
}

// lateralSelect loads at most q.limit rows per parent key:
// SELECT … FROM unnest($1, …) k CROSS JOIN LATERAL (SELECT … WHERE fk = k.k … LIMIT n), with o's
// columns and options on the inner statement.
func lateralSelect(r *render, q *query, n *lnode, ix *parentIndex, remote []int32, o selectOpts) *selectPlan {
	keyAlias := r.newAlias("k")
	o.extraWhere = func(r *render, alias string) { ix.writeKeyEq(r, n.table, alias, remote, keyAlias) }
	var plan *selectPlan
	body := r.section(func() {
		plan = q.renderSelect(r, n, o)
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
