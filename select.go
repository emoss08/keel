package keel

import (
	"slices"
	"strconv"
	"strings"

	"github.com/emoss08/keel/schema"
)

// lnode is one table of a read: the statement's base, a relation joined into a statement, or a
// to-many relation loaded by its own statement.
type lnode struct {
	ref      *Ref
	table    *schema.Table
	spec     *query
	parent   *lnode
	rel      int32
	children []*lnode
	joined   bool

	cols     []int32
	presence int
	idx      int
}

func newRoot(q *query) *lnode {
	return &lnode{ref: q.ref, table: q.ref.Table, spec: q, rel: -1, joined: true}
}

// buildTree turns the query's loads into a tree under root.
func buildTree(root *lnode, specs []*loadSpec) error {
	for _, ls := range specs {
		var chain []*Ref
		ref := ls.ref
		for ref != nil && ref != root.ref {
			if !ref.isPath() {
				return errf("load %s does not start at %s", ls.ref.name(), root.ref.name())
			}
			chain = append(chain, ref)
			ref = ref.parent
		}
		if ref == nil {
			return errf("load %s does not start at %s", ls.ref.name(), root.ref.name())
		}
		n := root
		for i := len(chain) - 1; i >= 0; i-- {
			n = n.child(chain[i])
		}
		if ls.q != nil {
			n.spec = ls.q
			if err := buildTree(n, ls.q.loads); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *lnode) child(ref *Ref) *lnode {
	for _, c := range n.children {
		if c.ref == ref {
			return c
		}
	}
	rel := ref.relation()
	c := &lnode{ref: ref, table: ref.Table, parent: n, rel: ref.rel}
	c.joined = !rel.Kind.ToMany() && rel.Kind != schema.Pick
	n.children = append(n.children, c)
	return c
}

// allTenants reports whether n's statement crosses tenants: the query of n or of a node above it,
// up to the root query, declared AllTenants.
func (n *lnode) allTenants() bool {
	for ; n != nil; n = n.parent {
		if n.spec != nil && n.spec.allTenants {
			return true
		}
	}
	return false
}

// statementNodes returns n and the nodes joined into its statement, parents first.
func (n *lnode) statementNodes() []*lnode {
	out := []*lnode{n}
	for i := 0; i < len(out); i++ {
		for _, c := range out[i].children {
			if c.joined {
				out = append(out, c)
			}
		}
	}
	return out
}

// joinPicks joins the pick relations of n's statement into it, so they need no statement of
// their own.
func (n *lnode) joinPicks() {
	nodes := []*lnode{n}
	for i := 0; i < len(nodes); i++ {
		for _, c := range nodes[i].children {
			if nodes[i].table.Relations[c.rel].Kind == schema.Pick {
				c.joined = true
			}
			if c.joined {
				nodes = append(nodes, c)
			}
		}
	}
}

// manyChildren returns the nodes loaded after n's statement: to-many relations and picks.
func (n *lnode) manyChildren() []*lnode {
	var out []*lnode
	for _, sn := range n.statementNodes() {
		for _, c := range sn.children {
			if !c.joined {
				out = append(out, c)
			}
		}
	}
	return out
}

// selectedColumns picks the columns a node reads: the narrowed set or the defaults, plus what keel
// needs to attach rows and load the next level, which under a system scope includes the tenant
// columns its loads match on.
func (n *lnode) selectedColumns(extra []int32, system bool) []int32 {
	var cols []int32
	if n.spec != nil && len(n.spec.exprs) > 0 {
		for _, e := range n.spec.exprs {
			cols = append(cols, e.(columnar).column().Index)
		}
	} else {
		cols = append(cols, n.table.Select...)
	}
	if n.spec != nil && len(n.spec.omit) > 0 {
		cols = slices.DeleteFunc(cols, func(c int32) bool {
			return slices.ContainsFunc(n.spec.omit, func(o Col) bool { return o.Index == c })
		})
	}
	need := func(c int32) {
		if !slices.Contains(cols, c) {
			cols = append(cols, c)
		}
	}
	for _, c := range n.table.PK {
		need(c)
	}
	if n.parent != nil && !n.joined {
		for _, c := range n.parent.table.Relations[n.rel].Remote {
			need(c)
		}
	}
	for _, c := range n.children {
		rel := &n.table.Relations[c.rel]
		for _, lc := range rel.Local {
			need(lc)
		}
		if system && !c.joined {
			local, _ := schema.SharedTenant(n.table, firstTarget(n.table, rel))
			for _, lc := range local {
				need(lc)
			}
		}
	}
	for _, c := range extra {
		need(c)
	}
	if n.spec != nil {
		for _, c := range n.spec.computes {
			if c.err == nil {
				need(c.col)
			}
		}
	}
	return cols
}

// selectOpts shapes a SELECT beyond its query: extra filters, columns and joins, expressions
// selected after the rows' columns (tail), keyset pages, counts, and a FROM item that replaces the table (prefiltered when it applies the tenant and
// interceptor predicates itself).
type selectOpts struct {
	extraWhere func(r *render, alias string)
	extraCols  []int32
	tail       []Expr
	keyset     func(r *render)
	limit      int64
	countOnly  bool
	countCap   int64
	selectOne  bool
	exact      bool
	keyColumn  func(r *render, alias string)
	extraJoin  func(r *render, alias string)

	from        func(r *render, alias string)
	prefiltered bool

	// keysOf, when set, makes the statement select only the columns keyCols of keysOf, one of its
	// statement nodes: the parent keys a pipelined load reads in a subquery. The statement keeps
	// its CTEs, joins, filters and keyset, and its order with its limit and offset when it has
	// them; it drops its other columns, DISTINCT and lock, and the order of an unlimited statement.
	keysOf  *lnode
	keyCols []int32
	// tieBreak are primary key columns of the statement's table appended to its order, so a
	// limited statement selects the same rows in a pipelined load's key subquery as on its own.
	tieBreak []int32
}

// renderSelect writes the SELECT for statement node sn and returns its decoding plan.
func (q *query) renderSelect(r *render, sn *lnode, o selectOpts) *selectPlan {
	sc, alias := r.push(sn.ref, joinLeft, q.allTenants)
	defer r.pop()

	if sn.parent == nil && !o.countOnly && q.single(o) {
		sn.joinPicks()
	}
	nodes := sn.statementNodes()
	plan := &selectPlan{nodes: nodes, parents: make([]int, len(nodes)), keep: make([]bool, len(nodes))}
	for i, n := range nodes {
		n.idx = i
		plan.parents[i] = -1
		if n.parent != nil && i > 0 {
			plan.parents[i] = n.parent.idx
		}
	}

	r.registerComputes(sc, nodes)

	lateral := make([]string, len(q.joins))
	for i, j := range q.joins {
		a := r.newAlias(aliasBase(j.ref))
		lateral[i] = a
		sc.refs = append(sc.refs, scoped{ref: j.ref, alias: a})
	}
	for _, n := range nodes[1:] {
		pa := r.alias(n.parent.ref)
		r.addJoin(sc, n.ref, pa, true)
		if n.spec != nil && len(n.spec.where) > 0 {
			for _, j := range sc.joins {
				if j.ref == n.ref {
					j.on = n.spec.where
				}
			}
		}
	}

	where := r.section(func() {
		wrote := false
		if !o.prefiltered {
			wrote = r.tenantPred(sn.table, alias, q.allTenants)
		}
		if o.extraWhere != nil {
			if wrote {
				r.s(" AND ")
			}
			o.extraWhere(r, alias)
			wrote = true
		}
		if !o.prefiltered {
			wrote = r.preds(r.intercepted(sn.table, OpRead, true), wrote)
			wrote = r.preds(r.readFiltered(sn.table), wrote)
		}
		var ps []Pred
		if sn.spec != nil {
			ps = sn.spec.where
		}
		if sn.parent == nil || !sn.joined {
			wrote = r.preds(ps, wrote)
		}
		if o.keyset != nil {
			if wrote {
				r.s(" AND ")
			}
			o.keyset(r)
		}
	})

	distinctCount := o.countOnly && q.distinct && sn.spec == q
	projected := !o.selectOne && (!o.countOnly || distinctCount) && o.keysOf == nil &&
		((sn.spec == q && q.projection()) || (o.exact && len(q.exprs) > 0))
	sel := r.section(func() {
		if o.keysOf != nil {
			ka := r.alias(o.keysOf.ref)
			for i, c := range o.keyCols {
				if i > 0 {
					r.s(", ")
				}
				r.s(ka)
				r.b('.')
				r.s(o.keysOf.table.Columns[c].Quoted)
			}
			return
		}
		if o.selectOne {
			r.s("1")
			return
		}
		if o.countOnly && !distinctCount {
			switch {
			case len(q.distinctOn) > 0 && sn.spec == q:
				r.s("DISTINCT ON (")
				for i, e := range q.distinctOn {
					if i > 0 {
						r.s(", ")
					}
					e.writeSQL(r)
				}
				r.s(") 1")
			case len(q.group) > 0 || o.countCap > 0 || len(q.distinctOn) > 0:
				r.s("1")
			default:
				r.s("count(*)")
			}
			return
		}
		if q.distinct && sn.spec == q {
			r.s("DISTINCT ")
		}
		if len(q.distinctOn) > 0 && sn.spec == q {
			r.s("DISTINCT ON (")
			for i, e := range q.distinctOn {
				if i > 0 {
					r.s(", ")
				}
				e.writeSQL(r)
			}
			r.s(") ")
		}
		if projected {
			for i, e := range q.exprs {
				if i > 0 {
					r.s(", ")
				}
				writeOutput(r, e)
			}
			return
		}
		first := true
		if o.keyColumn != nil {
			o.keyColumn(r, alias)
			plan.keyed = true
			first = false
		}
		for ni, n := range nodes {
			var extra []int32
			if ni == 0 {
				extra = o.extraCols
			}
			r.checkOmit(n)
			n.cols = n.selectedColumns(extra, r.scope.system)
			na := r.alias(n.ref)
			for _, c := range n.cols {
				if !first {
					r.s(", ")
				}
				first = false
				if n.table.Columns[c].Stored() {
					r.s(na)
					r.b('.')
					r.s(n.table.Columns[c].Quoted)
				} else {
					r.computed(Col{Ref: n.ref, Index: c})
				}
				plan.cols = append(plan.cols, lcol{node: ni, col: c})
			}
			if ni > 0 {
				pc := n.cols[0]
				if len(n.table.PK) > 0 {
					pc = n.table.PK[0]
				}
				n.presence = presenceOf(plan.cols, ni, pc)
			}
		}
		for _, e := range o.tail {
			if !first {
				r.s(", ")
			}
			first = false
			e.writeSQL(r)
		}
		plan.tail = len(o.tail)
	})

	limit := o.limit
	if limit == 0 && sn.spec != nil {
		limit = sn.spec.limit
	}
	if o.countOnly && o.countCap > 0 {
		limit = o.countCap
	}
	offset := int64(0)
	if sn.spec == q && !o.countOnly {
		offset = q.offset
	}

	var group, having, windows, order []byte
	if sn.spec == q {
		group = r.section(func() {
			for i, e := range q.group {
				if i > 0 {
					r.s(", ")
				}
				if pos := outputPosition(r, projected, q.exprs, e); pos > 0 {
					r.s(strconv.Itoa(pos))
					continue
				}
				e.writeSQL(r)
			}
		})
		having = r.section(func() { r.preds(q.having, false) })
		windows = r.section(func() { writeWindows(r, q.windows) })
	}
	if !o.countOnly && sn.spec != nil && (o.keysOf == nil || limit > 0 || offset > 0) {
		order = r.section(func() {
			for i, ord := range sn.spec.order {
				if i > 0 {
					r.s(", ")
				}
				if pos := outputPosition(r, projected && sn.spec == q, q.exprs, ord.e); pos > 0 {
					ord.writeAt(r, pos)
					continue
				}
				ord.writeSQL(r)
			}
			for _, c := range o.tieBreak {
				r.s(", ")
				r.s(alias)
				r.b('.')
				r.s(sn.table.Columns[c].Quoted)
			}
		})
	}

	var explicit []byte
	if sn.spec == q && len(q.joins) > 0 {
		explicit = r.section(func() {
			for i, j := range q.joins {
				r.s(j.kind)
				if j.lateral != nil {
					r.b('(')
					j.lateral.writeSubquery(r, false)
					r.s(") ")
					r.s(lateral[i])
					if j.kind == " LEFT JOIN LATERAL " {
						r.s(" ON TRUE")
					}
					continue
				}
				r.s(r.tableName(j.ref))
				r.b(' ')
				r.s(lateral[i])
				r.s(" ON ")
				wrote := r.tenantPred(j.ref.Table, lateral[i], q.allTenants)
				if !r.preds(j.on, wrote) && !wrote {
					r.s("TRUE")
				}
				if j.ref.isPath() {
					r.pinnedPreds(j.ref.Table, lateral[i], r.readFiltered(j.ref.Table))
				}
			}
		})
	}

	from := r.section(func() {
		if o.from != nil {
			o.from(r, alias)
		} else {
			r.s(r.tableName(sn.ref))
			r.b(' ')
			r.s(alias)
		}
		if o.extraJoin != nil {
			o.extraJoin(r, alias)
		}
		for progress := true; progress; {
			progress = false
			for _, j := range sc.joins {
				if !j.written && (j.parentAlias == alias || sc.joinWritten(j.parentAlias)) {
					r.writeJoin(sc, j)
					progress = true
				}
			}
		}
		r.buf = append(r.buf, explicit...)
		for i := 0; i < len(sc.joins); i++ {
			if !sc.joins[i].written {
				r.writeJoin(sc, sc.joins[i])
			}
		}
	})

	var with []byte
	if sn.spec == q && len(q.ctes) > 0 {
		with = r.section(func() { writeWith(r, q.ctes) })
	}

	r.buf = append(r.buf, with...)
	r.s("SELECT ")
	r.buf = append(r.buf, sel...)
	r.s(" FROM ")
	r.buf = append(r.buf, from...)
	if len(where) > 0 {
		r.s(" WHERE ")
		r.buf = append(r.buf, where...)
	}
	if len(group) > 0 {
		r.s(" GROUP BY ")
		r.buf = append(r.buf, group...)
	}
	if len(having) > 0 {
		r.s(" HAVING ")
		r.buf = append(r.buf, having...)
	}
	if len(windows) > 0 {
		r.s(" WINDOW ")
		r.buf = append(r.buf, windows...)
	}
	if len(order) > 0 {
		r.s(" ORDER BY ")
		r.buf = append(r.buf, order...)
	}
	if limit > 0 {
		r.s(" LIMIT ")
		r.int(limit)
	}
	if offset > 0 {
		r.s(" OFFSET ")
		r.int(offset)
	}
	if sn.spec == q && q.lock != "" && !o.countOnly && o.keysOf == nil {
		strength, wait, _ := strings.Cut(q.lock, "\x00")
		r.b(' ')
		r.s(strength)
		r.s(" OF ")
		r.s(alias)
		r.s(wait)
	}
	r.release(with, sel, explicit, from, where, group, having, windows, order)
	return plan
}

// checkOmit fails the statement when node n's query omits a column of another table.
func (r *render) checkOmit(n *lnode) {
	if n.spec == nil {
		return
	}
	for _, o := range n.spec.omit {
		if o.Ref == nil || o.Ref.Table != n.table {
			r.fail(errf("Omit names a column of %s on a query of %s", tableName(o.Ref), n.table.Name))
		}
	}
}

// single reports whether the statement reads at most two rows in no particular order, so a pick
// joined into it runs its lateral subquery at most twice.
func (q *query) single(o selectOpts) bool {
	limit := o.limit
	if limit == 0 {
		limit = q.limit
	}
	return limit > 0 && limit <= 2 && len(q.order) == 0
}

// presenceOf returns the result position of a node's column, which is NULL when the node's row is
// absent.
func presenceOf(cols []lcol, node int, col int32) int {
	for i, c := range cols {
		if c.node == node && c.col == col {
			return i
		}
	}
	return -1
}

// writeJoin writes the LEFT JOIN of to-one relation path j, or the lateral pick of a pick path,
// and marks it written. The joins of a scope are written parents first: those whose parent is the
// base or an already written join, then the statement's explicit joins, then the rest.
func (r *render) writeJoin(sc *scope, j *join) {
	j.written = true
	rel := j.ref.relation()
	if rel.Kind == schema.Pick {
		r.writePickJoin(sc, j)
		return
	}
	r.s(" LEFT JOIN ")
	r.s(j.ref.Table.Quoted)
	r.b(' ')
	r.s(j.alias)
	r.s(" ON ")
	r.relCond(j.ref.parent.Table, rel, j.parentAlias, j.alias, sc.allTenants)
	for _, p := range j.on {
		if p == nil {
			continue
		}
		r.s(" AND ")
		p.writeSQL(r)
	}
	r.pinnedPreds(j.ref.Table, j.alias, r.readFiltered(j.ref.Table))
}

// writeSubquery writes q as a subquery of the statement r is rendering. A query on a relation path
// is correlated with the scope that holds the path's parent.
func (q *query) writeSubquery(r *render, existsOnly bool) {
	var parentAlias string
	var parentTable *schema.Table
	var rel *schema.Relation
	if q.ref.isPath() {
		parentAlias = r.alias(q.ref.parent)
		parentTable = q.ref.parent.Table
		rel = q.ref.relation()
	}
	sn := newRoot(q)
	o := selectOpts{selectOne: existsOnly, exact: true}
	switch {
	case rel != nil && rel.Kind == schema.Pick:
		o.from = pickFromSubquery(parentTable, rel, parentAlias, q.allTenants)
		o.prefiltered = true
	case rel != nil && rel.Kind == schema.ManyToMany:
		jalias := r.newAlias(rel.JoinAlias)
		target := rel.Target
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
			r.s(target.Columns[rel.Remote[0]].Quoted)
			r.joinTenantEq(parentTable, rel, jalias, alias)
		}
		o.extraWhere = func(r *render, alias string) {
			r.s(jalias)
			r.b('.')
			r.s(rel.JoinLocalQ[0])
			r.s(" = ")
			r.s(parentAlias)
			r.b('.')
			r.s(parentTable.Columns[rel.Local[0]].Quoted)
			if r.scope.system {
				r.tenantEq(parentTable, target, parentAlias, alias)
			}
		}
	case rel != nil:
		o.extraWhere = func(r *render, alias string) {
			r.relCondNoTenant(parentTable, rel, parentAlias, alias)
		}
	}
	q.renderSelect(r, sn, o)
}

// relCondNoTenant writes the key equality of rel; tenant columns are left to the child's own
// tenant predicate.
func (r *render) relCondNoTenant(parent *schema.Table, rel *schema.Relation, parentAlias,
	childAlias string,
) {
	target := rel.Target
	for i := range rel.Local {
		if i > 0 {
			r.s(" AND ")
		}
		r.s(childAlias)
		r.b('.')
		r.s(target.Columns[rel.Remote[i]].Quoted)
		r.s(" = ")
		r.s(parentAlias)
		r.b('.')
		r.s(parent.Columns[rel.Local[i]].Quoted)
	}
	if r.scope.system {
		r.tenantEq(parent, target, parentAlias, childAlias)
	}
}

// joinTenantEq writes the equalities of the tenant columns of a many-to-many relation's join table
// under jalias with the same columns of the table under alias, the relation's parent or target,
// each after " AND ". A join table between two tenant tables carries the tenant columns they share,
// under the same names, so a row of it links a parent and a target of its own tenant even where
// keys repeat across tenants.
func (r *render) joinTenantEq(parent *schema.Table, rel *schema.Relation, jalias, alias string) {
	_, remote := schema.SharedTenant(parent, rel.Target)
	for _, ci := range remote {
		q := rel.Target.Columns[ci].Quoted
		r.s(" AND ")
		r.s(jalias)
		r.b('.')
		r.s(q)
		r.s(" = ")
		r.s(alias)
		r.b('.')
		r.s(q)
	}
}

// tenantEq writes the equalities of the tenant columns child shares with parent
// (schema.SharedTenant), each after " AND ".
func (r *render) tenantEq(parent, child *schema.Table, parentAlias, childAlias string) {
	local, remote := schema.SharedTenant(parent, child)
	for i, ci := range remote {
		r.s(" AND ")
		r.s(childAlias)
		r.b('.')
		r.s(child.Columns[ci].Quoted)
		r.s(" = ")
		r.s(parentAlias)
		r.b('.')
		r.s(parent.Columns[local[i]].Quoted)
	}
}
