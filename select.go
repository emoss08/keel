package keel

import (
	"slices"
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
	c.joined = !rel.Kind.ToMany()
	n.children = append(n.children, c)
	return c
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

// manyChildren returns the to-many nodes loaded after n's statement.
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
// needs to attach rows and load the next level.
func (n *lnode) selectedColumns(extra []int32) []int32 {
	var cols []int32
	if n.spec != nil && len(n.spec.exprs) > 0 {
		for _, e := range n.spec.exprs {
			cols = append(cols, e.(columnar).column().Index)
		}
	} else {
		cols = append(cols, n.table.Select...)
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
		if rel.Kind == schema.BelongsTo || rel.Kind.ToMany() || rel.Kind == schema.HasOne {
			for _, lc := range rel.Local {
				need(lc)
			}
		}
	}
	for _, c := range extra {
		need(c)
	}
	return cols
}

type selectOpts struct {
	extraWhere func(r *render, alias string)
	extraCols  []int32
	keyset     func(r *render)
	limit      int64
	countOnly  bool
	countCap   int64
	selectOne  bool
	exact      bool
	keyColumn  func(r *render)
	extraJoin  func(r *render, alias string)
}

// renderSelect writes the SELECT for statement node sn and returns its decoding plan.
func (q *query) renderSelect(r *render, sn *lnode, o selectOpts) *selectPlan {
	var with []byte
	if sn.spec == q && len(q.ctes) > 0 {
		with = r.section(func() { writeWith(r, q.ctes) })
	}
	sc, alias := r.push(sn.ref, joinLeft, q.allTenants)
	defer r.pop()

	nodes := sn.statementNodes()
	plan := &selectPlan{nodes: nodes, parents: make([]int, len(nodes)), keep: make([]bool, len(nodes))}
	for i, n := range nodes {
		n.idx = i
		plan.parents[i] = -1
		if n.parent != nil && i > 0 {
			plan.parents[i] = n.parent.idx
		}
	}

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
		wrote := r.tenantPred(sn.table, alias, q.allTenants)
		if o.extraWhere != nil {
			if wrote {
				r.s(" AND ")
			}
			o.extraWhere(r, alias)
			wrote = true
		}
		wrote = r.preds(r.intercepted(sn.table, OpRead, true), wrote)
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

	sel := r.section(func() {
		if o.selectOne {
			r.s("1")
			return
		}
		if o.countOnly {
			if len(q.group) > 0 || o.countCap > 0 || len(q.distinctOn) > 0 {
				r.s("1")
			} else {
				r.s("count(*)")
			}
			return
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
		if (sn.spec == q && q.projection()) || (o.exact && len(q.exprs) > 0) {
			for i, e := range q.exprs {
				if i > 0 {
					r.s(", ")
				}
				e.writeSQL(r)
			}
			return
		}
		first := true
		if o.keyColumn != nil {
			o.keyColumn(r)
			plan.keyed = true
			first = false
		}
		for ni, n := range nodes {
			var extra []int32
			if ni == 0 {
				extra = o.extraCols
			}
			n.cols = n.selectedColumns(extra)
			na := r.alias(n.ref)
			for _, c := range n.cols {
				if !first {
					r.s(", ")
				}
				first = false
				r.s(na)
				r.b('.')
				r.s(n.table.Columns[c].Quoted)
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
	})

	var group, having, order []byte
	if sn.spec == q {
		group = r.section(func() {
			for i, e := range q.group {
				if i > 0 {
					r.s(", ")
				}
				e.writeSQL(r)
			}
		})
		having = r.section(func() { r.preds(q.having, false) })
	}
	if !o.countOnly && sn.spec != nil {
		order = r.section(func() {
			for i, ord := range sn.spec.order {
				if i > 0 {
					r.s(", ")
				}
				ord.writeSQL(r)
			}
		})
	}

	from := r.section(func() {
		r.s(sn.table.Quoted)
		r.b(' ')
		r.s(alias)
		if o.extraJoin != nil {
			o.extraJoin(r, alias)
		}
		var late []*join
		for _, j := range sc.joins {
			if j.parentAlias == alias {
				r.writeJoin(sc, j)
			} else {
				late = append(late, j)
			}
		}
		for i, j := range q.joins {
			if sn.spec != q {
				break
			}
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
			r.s(j.ref.Table.Quoted)
			r.b(' ')
			r.s(lateral[i])
			r.s(" ON ")
			wrote := r.tenantPred(j.ref.Table, lateral[i], q.allTenants)
			if !r.preds(j.on, wrote) && !wrote {
				r.s("TRUE")
			}
		}
		for i := 0; i < len(late); i++ {
			r.writeJoin(sc, late[i])
		}
		for _, j := range sc.joins {
			if !slices.Contains(late, j) && j.parentAlias != alias {
				r.writeJoin(sc, j)
			}
		}
	})

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
	if len(order) > 0 {
		r.s(" ORDER BY ")
		r.buf = append(r.buf, order...)
	}
	limit := o.limit
	if limit == 0 && sn.spec != nil {
		limit = sn.spec.limit
	}
	if o.countOnly && o.countCap > 0 {
		limit = o.countCap
	}
	if limit > 0 {
		r.s(" LIMIT ")
		r.int(limit)
	}
	if sn.spec == q && q.offset > 0 && !o.countOnly {
		r.s(" OFFSET ")
		r.int(q.offset)
	}
	if sn.spec == q && q.lock != "" && !o.countOnly {
		strength, wait, _ := strings.Cut(q.lock, "\x00")
		r.b(' ')
		r.s(strength)
		r.s(" OF ")
		r.s(alias)
		r.s(wait)
	}
	r.release(with, sel, from, where, group, having, order)
	return plan
}

func presenceOf(cols []lcol, node int, col int32) int {
	for i, c := range cols {
		if c.node == node && c.col == col {
			return i
		}
	}
	return -1
}

func (r *render) writeJoin(sc *scope, j *join) {
	rel := j.ref.relation()
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
		}
		o.extraWhere = func(r *render, alias string) {
			r.s(jalias)
			r.b('.')
			r.s(rel.JoinLocalQ[0])
			r.s(" = ")
			r.s(parentAlias)
			r.b('.')
			r.s(parentTable.Columns[rel.Local[0]].Quoted)
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
		for _, ci := range target.Tenant {
			pi := parent.Column(target.Columns[ci].Name)
			if pi < 0 || !parent.Columns[pi].Flags.Has(schema.FlagTenant) {
				continue
			}
			r.s(" AND ")
			r.s(childAlias)
			r.b('.')
			r.s(target.Columns[ci].Quoted)
			r.s(" = ")
			r.s(parentAlias)
			r.b('.')
			r.s(parent.Columns[pi].Quoted)
		}
	}
}
