package keel

import (
	"context"
	"fmt"
	"strconv"

	"github.com/emoss08/keel/schema"
)

// render writes one statement. Clauses can be written in any order: placeholders are numbered
// by the order arguments are bound, so a clause written early keeps its numbers when the
// statement is assembled.
type render struct {
	buf    []byte
	args   []any
	err    error
	sc     *scope
	nalias int

	scope    *scopeState
	tenantPH []int
	ctx      context.Context
	db       *DB
	bare     bool
}

// scope is one query level: the base table, the tables joined to it and the paths opened by
// Any/None. Column references resolve to the innermost scope that knows their Ref.
type scope struct {
	parent     *scope
	refs       []scoped
	joins      []*join
	joinMode   joinMode
	allTenants bool
}

type joinMode uint8

const (
	joinLeft joinMode = iota
	joinFrom
	joinNone
)

type scoped struct {
	ref   *Ref
	alias string
}

type join struct {
	ref         *Ref
	alias       string
	parentAlias string
	selected    bool
	on          []Pred
}

var aliasNames = func() [64]string {
	var a [64]string
	for i := range a {
		a[i] = "t" + strconv.Itoa(i)
	}
	return a
}()

func (r *render) newAlias() string {
	n := r.nalias
	r.nalias++
	if n < len(aliasNames) {
		return aliasNames[n]
	}
	return "t" + strconv.Itoa(n)
}

func (r *render) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *render) s(str string) { r.buf = append(r.buf, str...) }

func (r *render) b(c byte) { r.buf = append(r.buf, c) }

func (r *render) int(n int64) { r.buf = strconv.AppendInt(r.buf, n, 10) }

func (r *render) arg(v any) {
	r.args = append(r.args, v)
	r.buf = append(r.buf, '$')
	r.buf = strconv.AppendInt(r.buf, int64(len(r.args)), 10)
}

// section runs fn with an empty buffer and returns what it wrote.
func (r *render) section(fn func()) []byte {
	saved := r.buf
	r.buf = make([]byte, 0, 128)
	fn()
	out := r.buf
	r.buf = saved
	return out
}

func (r *render) push(base *Ref, mode joinMode, allTenants bool) (*scope, string) {
	alias := r.newAlias()
	sc := &scope{parent: r.sc, joinMode: mode, allTenants: allTenants}
	sc.refs = append(sc.refs, scoped{ref: base, alias: alias})
	r.sc = sc
	return sc, alias
}

func (r *render) pop() { r.sc = r.sc.parent }

// alias resolves the alias of ref, joining a belongs-to or has-one path into the scope that holds
// its parent when needed.
func (r *render) alias(ref *Ref) string {
	for sc := r.sc; sc != nil; sc = sc.parent {
		for i := range sc.refs {
			if sc.refs[i].ref == ref {
				return sc.refs[i].alias
			}
		}
	}
	if !ref.isPath() {
		r.fail(fmt.Errorf("keel: %s is not part of this query", ref.name()))
		return "t0"
	}
	rel := ref.relation()
	if rel.Kind.ToMany() {
		r.fail(fmt.Errorf(
			"keel: %s is a to-many relation; filter it with Any or None", ref.name(),
		))
		return "t0"
	}
	parentAlias := r.alias(ref.parent)
	sc := r.scopeOf(ref.parent)
	if sc == nil {
		sc = r.sc
	}
	if sc.joinMode == joinNone {
		r.fail(fmt.Errorf("keel: %s cannot be joined here", ref.name()))
		return "t0"
	}
	return r.addJoin(sc, ref, parentAlias, false)
}

func (r *render) scopeOf(ref *Ref) *scope {
	for sc := r.sc; sc != nil; sc = sc.parent {
		for i := range sc.refs {
			if sc.refs[i].ref == ref {
				return sc
			}
		}
	}
	return nil
}

func (r *render) addJoin(sc *scope, ref *Ref, parentAlias string, selected bool) string {
	for _, j := range sc.joins {
		if j.ref == ref {
			j.selected = j.selected || selected
			return j.alias
		}
	}
	alias := r.newAlias()
	sc.joins = append(sc.joins, &join{
		ref: ref, alias: alias, parentAlias: parentAlias, selected: selected,
	})
	sc.refs = append(sc.refs, scoped{ref: ref, alias: alias})
	return alias
}

func (r *render) col(c Col) {
	if r.bare {
		r.s(c.Ref.Table.Columns[c.Index].Quoted)
		return
	}
	alias := r.alias(c.Ref)
	r.s(alias)
	r.b('.')
	r.s(c.Ref.Table.Columns[c.Index].Quoted)
}

// tenantArg writes the placeholder of tenant value i, binding it on first use.
func (r *render) tenantArg(i int) {
	if r.tenantPH == nil {
		r.tenantPH = make([]int, len(r.scope.tenant))
	}
	if r.tenantPH[i] == 0 {
		r.args = append(r.args, r.scope.tenant[i])
		r.tenantPH[i] = len(r.args)
	}
	r.b('$')
	r.int(int64(r.tenantPH[i]))
}

// tenantPred writes the tenant predicate for table t under alias, or nothing when the table has
// no tenant columns. It reports whether it wrote anything.
func (r *render) tenantPred(t *schema.Table, alias string, allTenants bool) bool {
	if !t.HasTenant() {
		return false
	}
	switch {
	case r.scope.system:
		if !allTenants {
			r.fail(fmt.Errorf("%w: %s", ErrAllTenantsRequired, t.Name))
		}
		return false
	case r.scope.tenant == nil:
		r.fail(fmt.Errorf("%w: %s", ErrNoScope, t.Name))
		return false
	case allTenants:
		r.fail(fmt.Errorf("%w: %s", ErrSystemScopeRequired, t.Name))
		return false
	}
	wrote := false
	for _, ci := range t.Tenant {
		pos := r.scope.tenantPos(t.Columns[ci].Name)
		if pos < 0 {
			r.fail(fmt.Errorf(
				"keel: tenant column %s.%s is not in Config.TenantColumns",
				t.Name, t.Columns[ci].Name,
			))
			return wrote
		}
		if wrote {
			r.s(" AND ")
		}
		r.s(alias)
		r.b('.')
		r.s(t.Columns[ci].Quoted)
		r.s(" = ")
		r.tenantArg(pos)
		wrote = true
	}
	return wrote
}

// relCond writes the join condition between the parent alias and the child alias of rel, with
// the shared tenant columns when the scope does not pin them to constants.
func (r *render) relCond(parent *schema.Table, rel *schema.Relation, parentAlias, childAlias string,
	allTenants bool,
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
	if !target.HasTenant() {
		return
	}
	if !r.scope.system && r.scope.tenant != nil {
		if len(rel.Local) > 0 {
			r.s(" AND ")
		}
		r.tenantPred(target, childAlias, false)
		return
	}
	if !allTenants {
		r.fail(fmt.Errorf("%w: %s", ErrAllTenantsRequired, target.Name))
		return
	}
	for _, ci := range target.Tenant {
		name := target.Columns[ci].Name
		pi := parent.Column(name)
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

// writeFromJoins writes the joins of sc as FROM/USING items for UPDATE and DELETE, and returns
// their join conditions for the WHERE clause.
func (r *render) writeFromJoins(sc *scope, keyword string) []byte {
	if len(sc.joins) == 0 {
		return nil
	}
	r.s(keyword)
	for i, j := range sc.joins {
		if i > 0 {
			r.s(", ")
		}
		r.s(j.ref.Table.Quoted)
		r.b(' ')
		r.s(j.alias)
	}
	return r.section(func() {
		for i, j := range sc.joins {
			if i > 0 {
				r.s(" AND ")
			}
			r.relCond(j.ref.parent.Table, j.ref.relation(), j.parentAlias, j.alias, sc.allTenants)
		}
	})
}

// preds writes ps joined by AND, skipping nil predicates. It reports whether it wrote anything.
func (r *render) preds(ps []Pred, wrote bool) bool {
	for _, p := range ps {
		if p == nil {
			continue
		}
		if wrote {
			r.s(" AND ")
		}
		p.writeSQL(r)
		wrote = true
	}
	return wrote
}

// intercepted returns the predicates interceptors add to a statement on t.
func (r *render) intercepted(t *schema.Table, op Op, read bool) []Pred {
	if r.db == nil {
		return nil
	}
	ps, err := r.db.intercept(r.ctx, t, op, read)
	if err != nil {
		r.fail(err)
	}
	return ps
}
