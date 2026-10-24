package keel

import (
	"strings"

	"github.com/emoss08/keel/schema"
)

// pickHop is one relation a pick subquery walks: the table it starts from, the relation, and the
// aliases of the table it reaches and, for many-to-many, of the join table.
type pickHop struct {
	from      *schema.Table
	rel       *schema.Relation
	alias     string
	joinAlias string
}

// writePick writes the subquery that picks rel's row for one parent: the tables of the path joined
// hop by hop, each scoped to the tenant and filtered by interceptors, ordered by the relation's
// keys and limited to one row. parentAlias is the parent's alias when the parent is a table of the
// statement, so its tenant columns can be compared under a system scope; key writes column i of the
// parent's key the first hop starts from. Without a parent alias, tenant lists the first hop's
// tenant columns to compare under a system scope and tenantKey writes the parent's value of
// tenant[i].
func (r *render) writePick(parent *schema.Table, rel *schema.Relation, allTenants bool, parentAlias string,
	key func(r *render, i int), tenant []int32, tenantKey func(r *render, i int),
) {
	hops := r.pickHops(parent, rel)
	sc := &scope{parent: r.sc, joinMode: joinLeft, allTenants: allTenants}
	for _, h := range hops {
		sc.refs = append(sc.refs, scoped{ref: &Ref{Table: h.rel.Target}, alias: h.alias})
	}
	r.sc = sc
	where := r.section(func() {
		r.pickStart(hops[0], parentAlias, key, tenant, tenantKey)
		r.tenantPred(hops[0].rel.Target, hops[0].alias, allTenants)
		r.pickFilter(hops, rel)
		for _, h := range hops {
			r.preds(r.intercepted(h.rel.Target, OpRead, true), true)
			r.pinnedPreds(h.rel.Target, h.alias, r.readFiltered(h.rel.Target))
		}
	})
	order := r.section(func() { r.pickOrder(hops, rel) })
	from := r.section(func() {
		r.pickFrom(hops, allTenants)
		for _, j := range sc.joins {
			r.writeJoin(sc, j)
		}
	})
	r.pop()
	last := hops[len(hops)-1]
	r.s("SELECT ")
	r.s(last.alias)
	r.s(".* FROM ")
	r.buf = append(r.buf, from...)
	r.s(" WHERE ")
	r.buf = append(r.buf, where...)
	r.s(" ORDER BY ")
	r.buf = append(r.buf, order...)
	r.s(" LIMIT 1")
	r.release(where, order, from)
}

// pickHops resolves the relations of rel's path and gives each table it reaches an alias.
func (r *render) pickHops(parent *schema.Table, rel *schema.Relation) []pickHop {
	hops := make([]pickHop, len(rel.Path))
	t := parent
	for i, ri := range rel.Path {
		hr := &t.Relations[ri]
		h := pickHop{from: t, rel: hr}
		if hr.Kind == schema.ManyToMany {
			h.joinAlias = r.newAlias(hr.JoinAlias)
		}
		h.alias = r.newAlias(hr.Alias)
		hops[i] = h
		t = hr.Target
	}
	return hops
}

// pickStart writes the condition that ties the first hop to the parent's key, every column of it,
// with the tenant columns the parent shares under a system scope: compared with the parent's
// columns under parentAlias, or with tenantKey's values of the first hop's columns tenant.
func (r *render) pickStart(h pickHop, parentAlias string, key func(r *render, i int), tenant []int32,
	tenantKey func(r *render, i int),
) {
	switch {
	case h.rel.Kind == schema.ManyToMany:
		r.s(h.joinAlias)
		r.b('.')
		r.s(h.rel.JoinLocalQ[0])
		r.s(" = ")
		key(r, 0)
		if parentAlias != "" && r.scope.system {
			r.tenantEq(h.from, h.rel.Target, parentAlias, h.alias)
		}
	case parentAlias != "":
		r.relCondNoTenant(h.from, h.rel, parentAlias, h.alias)
	default:
		for i, rc := range h.rel.Remote {
			if i > 0 {
				r.s(" AND ")
			}
			r.s(h.alias)
			r.b('.')
			r.s(h.rel.Target.Columns[rc].Quoted)
			r.s(" = ")
			key(r, i)
		}
	}
	for i, ci := range tenant {
		r.s(" AND ")
		r.s(h.alias)
		r.b('.')
		r.s(h.rel.Target.Columns[ci].Quoted)
		r.s(" = ")
		tenantKey(r, i)
	}
	if h.rel.Target.HasTenant() && !r.scope.system {
		r.s(" AND ")
	}
}

// pickFrom writes the tables of the path: the first hop's table (behind its join table for
// many-to-many) and a JOIN for every further hop, each joined on its relation's keys and tenant.
func (r *render) pickFrom(hops []pickHop, allTenants bool) {
	for i, h := range hops {
		if i > 0 {
			r.s(" JOIN ")
		}
		if h.rel.Kind == schema.ManyToMany {
			r.pickJoinTable(hops, i)
		}
		r.s(h.rel.Target.Quoted)
		r.b(' ')
		r.s(h.alias)
		switch {
		case h.rel.Kind == schema.ManyToMany:
			r.s(" ON ")
			r.s(h.alias)
			r.b('.')
			r.s(h.rel.Target.Columns[h.rel.Remote[0]].Quoted)
			r.s(" = ")
			r.s(h.joinAlias)
			r.b('.')
			r.s(h.rel.JoinRemoteQ[0])
			r.joinTenantEq(h.from, h.rel, h.joinAlias, h.alias)
			if i > 0 {
				r.relTenant(h.from, h.rel.Target, hops[i-1].alias, h.alias, allTenants)
			}
		case i > 0:
			r.s(" ON ")
			r.relCond(h.from, h.rel, hops[i-1].alias, h.alias, allTenants)
		}
	}
}

// pickJoinTable writes the join table of many-to-many hop i and the JOIN keyword of its target;
// after the first hop the join table is joined to the previous table's key.
func (r *render) pickJoinTable(hops []pickHop, i int) {
	h := hops[i]
	r.s(h.rel.JoinQuoted)
	r.b(' ')
	r.s(h.joinAlias)
	if i > 0 {
		r.s(" ON ")
		r.s(h.joinAlias)
		r.b('.')
		r.s(h.rel.JoinLocalQ[0])
		r.s(" = ")
		r.s(hops[i-1].alias)
		r.b('.')
		r.s(h.from.Columns[h.rel.Local[0]].Quoted)
		r.joinTenantEq(h.from, h.rel, h.joinAlias, hops[i-1].alias)
	}
	r.s(" JOIN ")
}

// pickFilter writes the relation's where= conditions: each hop's column compared with the tag's
// constants, written as SQL string literals so the subquery's text stays one shape.
func (r *render) pickFilter(hops []pickHop, rel *schema.Relation) {
	for _, f := range rel.Filter {
		h := hops[f.Hop]
		r.s(" AND ")
		r.s(h.alias)
		r.b('.')
		r.s(h.rel.Target.Columns[f.Column].Quoted)
		if len(f.Values) == 1 {
			r.s(" = ")
			r.s(sqlString(f.Values[0]))
			continue
		}
		r.s(" IN (")
		for i, v := range f.Values {
			if i > 0 {
				r.s(", ")
			}
			r.s(sqlString(v))
		}
		r.b(')')
	}
}

// sqlString quotes s as a SQL string literal.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// pickOrder writes the relation's order keys, descending for a Last pick.
func (r *render) pickOrder(hops []pickHop, rel *schema.Relation) {
	for i, k := range rel.Order {
		if i > 0 {
			r.s(", ")
		}
		h := hops[k.Hop]
		r.s(h.alias)
		r.b('.')
		r.s(h.rel.Target.Columns[k.Column].Quoted)
		if rel.Last {
			r.s(" DESC")
		}
	}
}

// writePickJoin joins a pick relation into a statement: LEFT JOIN LATERAL (…) alias ON TRUE, with
// a load's predicates as the join condition.
func (r *render) writePickJoin(sc *scope, j *join) {
	rel := j.ref.relation()
	parent := j.ref.parent.Table
	r.s(" LEFT JOIN LATERAL (")
	r.writePick(parent, rel, sc.allTenants, j.parentAlias, parentKey(parent, rel, j.parentAlias), nil, nil)
	r.s(") ")
	r.s(j.alias)
	r.s(" ON ")
	if !r.preds(j.on, false) {
		r.s("TRUE")
	}
}

// parentKey writes column i of the key a pick starts from: the parent's local key column under
// parentAlias.
func parentKey(parent *schema.Table, rel *schema.Relation, parentAlias string) func(r *render, i int) {
	return func(r *render, i int) {
		r.s(parentAlias)
		r.b('.')
		r.s(parent.Columns[rel.Local[i]].Quoted)
	}
}

// pickSelect loads a pick relation for every parent key in one statement:
// SELECT k.k, … FROM unnest($1, …) k CROSS JOIN LATERAL (…pick…) alias. When the load crosses
// tenants the key list starts with the parents' values of the first hop's tenant columns tenant.
func pickSelect(r *render, q *query, n *lnode, ix *parentIndex, tenant []int32) *selectPlan {
	parent := n.parent.table
	rel := &parent.Relations[n.rel]
	keyAlias := r.newAlias("k")
	keyAt := func(r *render, i int) {
		r.s(keyAlias)
		r.b('.')
		r.s(ix.keyName(i))
	}
	key := func(r *render, i int) { keyAt(r, len(tenant)+i) }
	return q.renderSelect(r, n, selectOpts{
		prefiltered: true,
		keyColumn: func(r *render, _ string) {
			for i := range ix.cols() {
				if i > 0 {
					r.s(", ")
				}
				keyAt(r, i)
			}
		},
		from: func(r *render, alias string) {
			ix.writeUnnest(r, keyAlias)
			r.s(" CROSS JOIN LATERAL (")
			r.writePick(parent, rel, q.allTenants, "", key, tenant, keyAt)
			r.s(") ")
			r.s(alias)
		},
	})
}

// pickFromSubquery is the FROM item of a subquery on a pick path (Any, None, Exists, InQuery): the
// pick for the outer row, under the subquery's alias.
func pickFromSubquery(parent *schema.Table, rel *schema.Relation, parentAlias string, allTenants bool,
) func(r *render, alias string) {
	return func(r *render, alias string) {
		r.b('(')
		r.writePick(parent, rel, allTenants, parentAlias, parentKey(parent, rel, parentAlias), nil, nil)
		r.s(") ")
		r.s(alias)
	}
}
