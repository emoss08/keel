package keel

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emoss08/keel/schema"
)

// Registry finds an application's tables by name, for code that chooses them at run time: report
// compilers, data tables with user-chosen columns, generic exports. Build it once from the
// generated tables (the <Model>Schema functions of the generated packages) and share it; it is
// read-only after NewRegistry.
type Registry struct {
	byName map[string]*schema.Table
	tables []*schema.Table
}

// NewRegistry indexes tables by their SQL name. A table listed twice is an error.
func NewRegistry(tables ...*schema.Table) (*Registry, error) {
	g := &Registry{byName: make(map[string]*schema.Table, len(tables))}
	for _, t := range tables {
		if t == nil {
			return nil, errf("NewRegistry: nil table")
		}
		if _, dup := g.byName[t.Name]; dup {
			return nil, errf("NewRegistry: table %s listed twice", t.Name)
		}
		g.byName[t.Name] = t
		g.tables = append(g.tables, t)
	}
	return g, nil
}

// Table returns the table named name.
func (g *Registry) Table(name string) (*schema.Table, bool) {
	t, ok := g.byName[name]
	return t, ok
}

// Tables returns the registered tables in the order they were given.
func (g *Registry) Tables() []*schema.Table { return slices.Clone(g.tables) }

// Query starts a dynamic query over the table named name. An unknown name makes a query whose
// terminals fail.
func (g *Registry) Query(name string) *DynQuery {
	t, ok := g.byName[name]
	if !ok {
		return &DynQuery{err: errf("unknown table %q", name)}
	}
	return DynFrom(t)
}

// DynQuery is a query whose tables, columns, relations and expressions are chosen at run time. It
// renders through the same machinery as typed queries: every table it references, joined, in a
// subquery or in a pre-aggregation, gets the scope's tenant predicate (or needs AllTenants under a
// system scope), interceptors apply to every statement on a table, and identifiers only ever come
// from the schema. Its result is a typed columnar DynResult.
//
// A DynQuery is a builder: methods change it and return it, and it must not be used by several
// goroutines at once. Builder errors (an unknown name, an invalid expression) are kept and
// returned by Err and by every terminal.
type DynQuery struct {
	q       query
	outs    []DynExpr
	derived []string
	refs    map[dynRefKey]*Ref
	preaggs []*DynPreAgg
	err     error
}

// dynRefKey identifies the reference a relation of a reference leads to.
type dynRefKey struct {
	parent *Ref
	rel    int32
}

// DynFrom starts a dynamic query over table t.
func DynFrom(t *schema.Table) *DynQuery {
	if t == nil {
		return &DynQuery{err: errf("DynFrom: nil table")}
	}
	return &DynQuery{q: query{ref: &Ref{Table: t}}}
}

// DynFromQuery starts a dynamic query over the rows of sub, as a derived table: its columns are
// sub's selected expressions, resolved by label with Path. Use it to filter, group or order the
// result of another dynamic query.
func DynFromQuery(sub *DynQuery) *DynQuery {
	return dynDerived("keel_sub", []*DynQuery{sub}, sub)
}

// DynUnion is the distinct rows of every part: (q1) UNION (q2) …. The parts select the same
// number of expressions; the result's columns take the first part's labels. Filter, order and
// limit it like any dynamic query.
func DynUnion(parts ...*DynQuery) *DynQuery { return dynSet(" UNION ", parts) }

// DynUnionAll is every row of every part: (q1) UNION ALL (q2) ….
func DynUnionAll(parts ...*DynQuery) *DynQuery { return dynSet(" UNION ALL ", parts) }

// DynIntersect is the rows every part returns: (q1) INTERSECT (q2) ….
func DynIntersect(parts ...*DynQuery) *DynQuery { return dynSet(" INTERSECT ", parts) }

// DynExcept is the rows of the first part that no other part returns: (q1) EXCEPT (q2) ….
func DynExcept(parts ...*DynQuery) *DynQuery { return dynSet(" EXCEPT ", parts) }

func dynSet(op string, parts []*DynQuery) *DynQuery {
	if len(parts) < 2 {
		return &DynQuery{err: errf("a set operation needs at least two queries")}
	}
	subs := make([]subquery, len(parts))
	for i, p := range parts {
		subs[i] = p
	}
	return dynDerived("keel_set", parts, Compound{op: op, parts: subs})
}

// dynDerived returns a query over a CTE named name whose body is body and whose columns are the
// outputs of parts[0]; every part must select as many expressions.
func dynDerived(name string, parts []*DynQuery, body subquery) *DynQuery {
	var err error
	var labels []string
	for i, p := range parts {
		if p == nil {
			err = errf("nil dynamic query")
			break
		}
		if err = p.Err(); err != nil {
			break
		}
		outs := p.outputs()
		if i == 0 {
			labels = make([]string, len(outs))
			for j, o := range outs {
				labels[j] = o.Label()
			}
		} else if len(outs) != len(labels) {
			err = errf("set operation parts select %d and %d expressions", len(labels), len(outs))
			break
		}
	}
	if err != nil {
		return &DynQuery{err: err}
	}
	t := numberedCTE(name, positional("c", len(labels)), body)
	return &DynQuery{q: query{ref: t.ref, ctes: []*CTETable{t}}, derived: labels}
}

// positional returns prefix1 … prefixN.
func positional(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + strconv.Itoa(i+1)
	}
	return out
}

// Err returns the first builder error: an unknown table, column or relation, an invalid
// expression, a label selected twice.
func (q *DynQuery) Err() error { return q.err }

// Table returns the query's base table: a registered table, or the derived table of
// DynFromQuery and the set operations.
func (q *DynQuery) Table() *schema.Table {
	if q.q.ref == nil {
		return nil
	}
	return q.q.ref.Table
}

func (q *DynQuery) fail(err error) {
	if q.err == nil && err != nil {
		q.err = err
	}
}

func (q *DynQuery) check(e any) {
	if d, ok := e.(DynExpr); ok {
		q.fail(d.Err())
	}
}

// Path resolves a column by name: the last segment names a column (its SQL name or Go field
// name), the segments before it relations (their Go name or its snake_case form) followed from
// the base table, each of them to-one. Path("status"), Path("customer", "name"). The relations
// are LEFT JOINed with their tenant condition when the expression is used. On a derived query the
// one segment is the label of a column of the inner query.
func (q *DynQuery) Path(segments ...string) DynExpr {
	if q.err != nil {
		return dynFail(q.err)
	}
	return q.resolve(q.q.ref, segments, strings.Join(segments, "."))
}

// resolve follows segments from ref to a column: relations first, then the column.
func (q *DynQuery) resolve(ref *Ref, segments []string, label string) DynExpr {
	if len(segments) == 0 {
		return dynFail(errf("empty column path"))
	}
	last := segments[len(segments)-1]
	if ref == q.q.ref && q.derived != nil {
		if len(segments) != 1 {
			return dynFail(errf("%s: a derived query has no relations", label))
		}
		for i, l := range q.derived {
			if l == last {
				return dynColumn(Col{Ref: ref, Index: int32(i)}, label)
			}
		}
		return dynFail(errf("the derived query has no column %q", last))
	}
	for _, s := range segments[:len(segments)-1] {
		next, err := q.hop(ref, s)
		if err != nil {
			return dynFail(err)
		}
		if next.relation().Kind.ToMany() {
			return dynFail(errf("%s crosses the to-many relation %s; use Rel(…).Any, None or PreAggregate",
				label, s))
		}
		ref = next
	}
	ci := columnIndex(ref.Table, last)
	if ci < 0 {
		return dynFail(errf("table %s has no column %q", ref.Table.Name, last))
	}
	return dynColumn(Col{Ref: ref, Index: ci}, label)
}

// hop returns the reference relation name of parent leads to, the same one each time.
func (q *DynQuery) hop(parent *Ref, name string) (*Ref, error) {
	t := parent.Table
	ri := relationIndex(t, name)
	if ri < 0 {
		return nil, errf("table %s has no relation %q", t.Name, name)
	}
	key := dynRefKey{parent: parent, rel: ri}
	if r, ok := q.refs[key]; ok {
		return r, nil
	}
	rel := &t.Relations[ri]
	if rel.Target == nil {
		return nil, errf("relation %s of %s has no target table; is its generated package imported?",
			rel.Name, t.Name)
	}
	if q.refs == nil {
		q.refs = make(map[dynRefKey]*Ref)
	}
	r := &Ref{Table: rel.Target, parent: parent, rel: ri}
	q.refs[key] = r
	return r, nil
}

// columnIndex finds a column by SQL name or Go field name.
func columnIndex(t *schema.Table, name string) int32 {
	if i := t.Column(name); i >= 0 {
		return i
	}
	for i := range t.Columns {
		if t.Columns[i].Field == name && name != "" {
			return int32(i)
		}
	}
	return -1
}

// relationIndex finds a relation by Go name or its snake_case form.
func relationIndex(t *schema.Table, name string) int32 {
	if i := t.Relation(name); i >= 0 {
		return i
	}
	for i := range t.Relations {
		if schema.Snake(t.Relations[i].Name) == name {
			return int32(i)
		}
	}
	return -1
}

// Select sets the expressions the query returns, in order. Each becomes a column of the result
// named by its Label. Without Select a table query returns the table's default columns.
func (q *DynQuery) Select(es ...DynExpr) *DynQuery {
	for _, e := range es {
		q.check(e)
		for _, o := range q.outs {
			if o.Label() == e.Label() {
				q.fail(errf("label %q selected twice", e.Label()))
			}
		}
		q.outs = append(q.outs, e)
	}
	return q
}

// outputs returns the selected expressions, or the base table's default columns.
func (q *DynQuery) outputs() []DynExpr {
	if len(q.outs) > 0 || q.q.ref == nil {
		return q.outs
	}
	t := q.q.ref.Table
	if q.derived != nil {
		out := make([]DynExpr, len(q.derived))
		for i, l := range q.derived {
			out[i] = dynColumn(Col{Ref: q.q.ref, Index: int32(i)}, l)
		}
		return out
	}
	out := make([]DynExpr, len(t.Select))
	for i, c := range t.Select {
		out[i] = dynColumn(Col{Ref: q.q.ref, Index: c}, t.Columns[c].Name)
	}
	return out
}

// Where adds predicates, joined by AND. Nil predicates are skipped.
func (q *DynQuery) Where(ps ...Pred) *DynQuery {
	for _, p := range ps {
		q.check(p)
	}
	q.q.where = append(q.q.where, ps...)
	return q
}

// Group adds GROUP BY expressions.
func (q *DynQuery) Group(es ...Expr) *DynQuery {
	for _, e := range es {
		q.check(e)
	}
	q.q.group = append(q.q.group, es...)
	return q
}

// GroupingSets groups by each of sets in turn: GROUP BY GROUPING SETS (…), as Query.GroupingSets.
func (q *DynQuery) GroupingSets(sets ...[]Expr) *DynQuery {
	for _, s := range sets {
		for _, e := range s {
			q.check(e)
		}
	}
	q.q.group = append(q.q.group, groupingSets(sets))
	return q
}

// Rollup groups by es and each prefix of es down to the grand total, as Query.Rollup.
func (q *DynQuery) Rollup(es ...Expr) *DynQuery {
	for _, e := range es {
		q.check(e)
	}
	q.q.group = append(q.q.group, rollup(es))
	return q
}

// Cube groups by every subset of es, as Query.Cube.
func (q *DynQuery) Cube(es ...Expr) *DynQuery {
	for _, e := range es {
		q.check(e)
	}
	q.q.group = append(q.q.group, cube(es))
	return q
}

// Having adds HAVING predicates.
func (q *DynQuery) Having(ps ...Pred) *DynQuery {
	for _, p := range ps {
		q.check(p)
	}
	q.q.having = append(q.q.having, ps...)
	return q
}

// Window declares a named window, as Query.Window.
func (q *DynQuery) Window(name string, w Window) *DynQuery {
	q.q.windows = append(q.q.windows, namedWindow{name: name, w: w})
	return q
}

// Order sets the sort order.
func (q *DynQuery) Order(os ...Order) *DynQuery {
	for _, o := range os {
		q.check(o.e)
	}
	q.q.order = append(q.q.order, os...)
	return q
}

// Distinct returns each distinct row once (SELECT DISTINCT).
func (q *DynQuery) Distinct() *DynQuery {
	q.q.distinct = true
	return q
}

// Limit caps the number of rows.
func (q *DynQuery) Limit(n int) *DynQuery {
	q.q.limit = int64(n)
	return q
}

// Offset skips n rows. Page reads by keyset instead.
func (q *DynQuery) Offset(n int) *DynQuery {
	q.q.offset = int64(n)
	return q
}

// AllTenants lets the query cross tenants. It needs a system scope. It covers the query's joined
// relations, Any and None, and pre-aggregations; a query used in With, DynFromQuery or a set
// operation declares it itself.
func (q *DynQuery) AllTenants() *DynQuery {
	q.q.allTenants = true
	return q
}

// Timeout bounds the query's execution.
func (q *DynQuery) Timeout(d time.Duration) *DynQuery {
	q.q.timeout = d
	return q
}

// CustomPlan plans the query for its parameter values on every execution, as Query.CustomPlan.
func (q *DynQuery) CustomPlan() *DynQuery {
	q.q.customPlan = true
	return q
}

// DynCTE is a dynamic query declared on another with With: WITH keel_cte (…) AS (sub). A
// statement names its CTEs keel_cte, keel_cte_2, … in the order it uses them, across every query
// nested in it, so no name shadows another. Its columns are sub's selected expressions, addressed
// by label.
type DynCTE struct {
	t      *CTETable
	labels []string
}

// With declares sub as a common table expression of q and returns it, to Join and to read
// columns from with Col.
func (q *DynQuery) With(sub *DynQuery) *DynCTE {
	if sub == nil {
		q.fail(errf("With: nil dynamic query"))
		return &DynCTE{}
	}
	q.fail(sub.Err())
	outs := sub.outputs()
	labels := make([]string, len(outs))
	for i, o := range outs {
		labels[i] = o.Label()
	}
	t := numberedCTE("keel_cte", positional("c", len(labels)), sub)
	q.q.ctes = append(q.q.ctes, t)
	return &DynCTE{t: t, labels: labels}
}

// Col returns the CTE's column labelled label.
func (c *DynCTE) Col(label string) DynExpr {
	if c.t == nil {
		return dynFail(errf("empty DynCTE"))
	}
	for i, l := range c.labels {
		if l == label {
			return dynColumn(Col{Ref: c.t.ref, Index: int32(i)}, label)
		}
	}
	return dynFail(errf("CTE %s has no column %q", c.t.ref.Table.Name, label))
}

// Labels returns the labels of the CTE's columns.
func (c *DynCTE) Labels() []string { return slices.Clone(c.labels) }

// Join adds an INNER JOIN of a CTE declared with With.
func (q *DynQuery) Join(c *DynCTE, on ...Pred) *DynQuery { return q.joinCTE(" JOIN ", c, on) }

// LeftJoin adds a LEFT JOIN of a CTE declared with With.
func (q *DynQuery) LeftJoin(c *DynCTE, on ...Pred) *DynQuery {
	return q.joinCTE(" LEFT JOIN ", c, on)
}

func (q *DynQuery) joinCTE(kind string, c *DynCTE, on []Pred) *DynQuery {
	if c == nil || c.t == nil {
		q.fail(errf("Join: empty DynCTE"))
		return q
	}
	if !slices.Contains(q.q.ctes, c.t) {
		q.fail(errf("Join: CTE %s is not declared on this query", c.t.ref.Table.Name))
		return q
	}
	for _, p := range on {
		q.check(p)
	}
	q.q.joins = append(q.q.joins, joinClause{kind: kind, ref: c.t.ref, on: on})
	return q
}

// DynRel is a relation path of a dynamic query, to-one or to-many, for semi-joins (Any, None) and
// pre-aggregations. Build it with Rel.
type DynRel struct {
	q    *DynQuery
	hops []*Ref
	name string
	err  error
}

// Rel resolves a relation path from the base table: Rel("moves"), Rel("moves", "stops"),
// Rel("customer").
func (q *DynQuery) Rel(path ...string) *DynRel {
	d := &DynRel{q: q, name: strings.Join(path, ".")}
	switch {
	case q.err != nil:
		d.err = q.err
	case len(path) == 0:
		d.err = errf("empty relation path")
	case q.derived != nil:
		d.err = errf("%s: a derived query has no relations", d.name)
	}
	if d.err != nil {
		return d
	}
	ref := q.q.ref
	for _, s := range path {
		next, err := q.hop(ref, s)
		if err != nil {
			d.err = err
			return d
		}
		d.hops = append(d.hops, next)
		ref = next
	}
	return d
}

// Err returns the error resolving the path.
func (d *DynRel) Err() error { return d.err }

// Col resolves a column of the relation's table, through further to-one relations:
// q.Rel("moves").Col("status"). Use it inside Any, None and pre-aggregated measures.
func (d *DynRel) Col(path ...string) DynExpr {
	if d.err != nil {
		return dynFail(d.err)
	}
	return d.q.resolve(d.hops[len(d.hops)-1], path, d.name+"."+strings.Join(path, "."))
}

// Any is true when a row of the relation matches ps: EXISTS (…), correlated with the row, with the
// related tables' tenant predicates and interceptors. A path through several to-many relations
// nests one EXISTS per to-many relation.
func (d *DynRel) Any(ps ...Pred) DynExpr { return d.exists(ps, false) }

// None is true when no row of the relation matches ps: NOT EXISTS (…).
func (d *DynRel) None(ps ...Pred) DynExpr { return d.exists(ps, true) }

func (d *DynRel) exists(ps []Pred, not bool) DynExpr {
	if d.err != nil {
		return dynFail(d.err)
	}
	for _, p := range ps {
		if x, ok := p.(DynExpr); ok && x.Err() != nil {
			return dynFail(x.Err())
		}
	}
	var cuts []int
	for i, h := range d.hops {
		if h.relation().Kind.ToMany() {
			cuts = append(cuts, i)
		}
	}
	if len(cuts) == 0 {
		cuts = []int{len(d.hops) - 1}
	}
	inner := ps
	var p Pred
	for k := len(cuts) - 1; k >= 0; k-- {
		p = relExistsQuery(&query{ref: d.hops[cuts[k]]}, inner, not && k == 0)
		inner = []Pred{p}
	}
	label := "any " + d.name
	if not {
		label = "none " + d.name
	}
	return newDyn(p, label)
}

// PreAggStrategy chooses how a pre-aggregation reads a to-many relation.
type PreAggStrategy uint8

const (
	// PreAggCTE aggregates the related rows in one grouped pass, keyed by the parent and filtered
	// to the parents the query selects, in a CTE joined to the query: one scan of the related
	// table whatever the number of parents. The default for reports.
	PreAggCTE PreAggStrategy = iota + 1
	// PreAggLateral aggregates each parent's related rows in a LEFT JOIN LATERAL subquery: one
	// index probe per parent, best when the query selects few parents (a page, a top N).
	PreAggLateral
)

// DynPreAgg aggregates the rows of a to-many relation per parent row, so measures over the
// relation do not fan the query's rows out. Measure returns a column of the per-parent result;
// the query can select it, filter on it in Having, or aggregate it again (sum of per-shipment
// distances per customer). A parent without related rows reads NULL.
type DynPreAgg struct {
	rel      *DynRel
	strategy PreAggStrategy
	all      bool
	base     string
	pivot    int
	where    []Pred
	ref      *Ref
	keys     []preAggKey
	measures []Expr
	err      error
}

// preAggKey is one grouping key of a pre-aggregation: a column of the related rows (or of the
// many-to-many join table, by quoted name) and the parent column it equals.
type preAggKey struct {
	child  int32
	joinQ  string
	parent int32
}

// PreAggregate aggregates the relation per parent row with strategy s. The path must contain a
// to-many relation; the first one is the level the rows are aggregated to, and to-many relations
// after it are joined inside the aggregation.
func (d *DynRel) PreAggregate(s PreAggStrategy) *DynPreAgg {
	p := &DynPreAgg{rel: d, strategy: s, pivot: -1, err: d.err}
	if p.err != nil {
		return p
	}
	if s != PreAggCTE && s != PreAggLateral {
		p.err = errf("unknown pre-aggregation strategy %d", s)
		return p
	}
	for i, h := range d.hops {
		if h.relation().Kind.ToMany() {
			p.pivot = i
			break
		}
	}
	if p.pivot < 0 {
		p.err = errf("%s is not a to-many relation path; select its columns with Path", d.name)
		return p
	}
	pivot := d.hops[p.pivot]
	rel := pivot.relation()
	parent, child := pivot.parent.Table, pivot.Table
	if rel.Kind == schema.ManyToMany {
		p.keys = append(p.keys, preAggKey{child: -1, joinQ: rel.JoinLocalQ[0], parent: rel.Local[0]})
	} else {
		for i := range rel.Local {
			p.keys = append(p.keys, preAggKey{child: rel.Remote[i], parent: rel.Local[i]})
		}
	}
	for _, ci := range child.Tenant {
		pi := parent.Column(child.Columns[ci].Name)
		if pi >= 0 && parent.Columns[pi].Flags.Has(schema.FlagTenant) {
			p.keys = append(p.keys, preAggKey{child: ci, parent: pi})
		}
	}
	name := "keel"
	for _, h := range d.hops {
		name += "_" + schema.Snake(h.relation().Name)
	}
	base := name
	n := 1
	for _, o := range d.q.preaggs {
		if o.base == base {
			n++
		}
	}
	if n > 1 {
		name += "_" + strconv.Itoa(n)
	}
	p.base = base
	t := &schema.Table{Name: name, Quoted: schema.Quote(name), Alias: schema.AliasFor(name)}
	for i := range p.keys {
		k := "k" + strconv.Itoa(i+1)
		t.Columns = append(t.Columns, schema.Column{Name: k, Quoted: schema.Quote(k)})
	}
	p.ref = &Ref{Table: t}
	d.q.preaggs = append(d.q.preaggs, p)
	return p
}

// Err returns the error of the pre-aggregation or of its path.
func (p *DynPreAgg) Err() error { return p.err }

// AllParents makes a PreAggCTE aggregate the related rows of every parent in scope instead of
// only the parents the query's filters select: one pass without the semi-join that restricts the
// rows, cheaper when the filters keep most parents, dearer when they keep few. It does nothing to
// a PreAggLateral, which reads each selected parent's rows.
func (p *DynPreAgg) AllParents() *DynPreAgg {
	p.all = true
	return p
}

// Where filters the related rows before they are aggregated.
func (p *DynPreAgg) Where(ps ...Pred) *DynPreAgg {
	for _, x := range ps {
		if d, ok := x.(DynExpr); ok && d.Err() != nil && p.err == nil {
			p.err = d.Err()
		}
	}
	p.where = append(p.where, ps...)
	return p
}

// Measure adds an aggregate over the relation's columns (rel.Col(…).Sum(), DynCount()) and
// returns the column that holds it for each parent row.
func (p *DynPreAgg) Measure(e DynExpr) DynExpr {
	if p.err != nil {
		return dynFail(p.err)
	}
	if err := e.Err(); err != nil {
		p.err = err
		return dynFail(err)
	}
	if e.n.agg == nil || e.n.agg.over != nil {
		return dynFail(errf("Measure needs an aggregate, %s is not one", e.Label()))
	}
	t := p.ref.Table
	c := "c" + strconv.Itoa(len(p.measures)+1)
	t.Columns = append(t.Columns, schema.Column{Name: c, Quoted: schema.Quote(c)})
	p.measures = append(p.measures, e)
	return dynColumn(Col{Ref: p.ref, Index: int32(len(t.Columns) - 1)}, e.Label())
}

// parentRef is the reference the aggregated rows belong to: the base or a to-one path from it.
func (p *DynPreAgg) parentRef() *Ref { return p.rel.hops[p.pivot].parent }

// keyEq writes the join condition between the pre-aggregated rows and their parent.
func (p *DynPreAgg) keyEq() Pred {
	return predExpr{e: func(r *render) {
		a, pa := r.alias(p.ref), r.alias(p.parentRef())
		pt := p.parentRef().Table
		for i, k := range p.keys {
			if i > 0 {
				r.s(" AND ")
			}
			r.s(a)
			r.b('.')
			r.s(p.ref.Table.Columns[i].Quoted)
			r.s(" = ")
			r.s(pa)
			r.b('.')
			r.s(pt.Columns[k.parent].Quoted)
		}
	}}
}

// innerJoins returns the joins inside the aggregation of the hops after the pivot, up to the last
// to-many one: to-many hops fan out with JOIN, to-one hops before them are LEFT JOINed in order.
// To-one hops after the last to-many hop join on demand.
func (p *DynPreAgg) innerJoins() []joinClause {
	hops := p.rel.hops[p.pivot+1:]
	last := -1
	for i, h := range hops {
		if h.relation().Kind.ToMany() {
			last = i
		}
	}
	out := make([]joinClause, 0, last+1)
	for _, h := range hops[:last+1] {
		kind := " LEFT JOIN "
		if h.relation().Kind.ToMany() {
			kind = " JOIN "
		}
		out = append(out, joinClause{kind: kind, ref: h, on: []Pred{hopCond(h)}})
	}
	return out
}

// hopCond writes the condition that joins relation path h to its parent: the key equality of a
// belongs-to, has-one or has-many relation, an EXISTS over the join table of a many-to-many one.
func hopCond(h *Ref) Pred {
	return predExpr{e: func(r *render) {
		rel := h.relation()
		pa, ca := r.alias(h.parent), r.alias(h)
		if rel.Kind != schema.ManyToMany {
			r.relCondNoTenant(h.parent.Table, rel, pa, ca)
			return
		}
		ja := r.newAlias(rel.JoinAlias)
		r.s("EXISTS (SELECT 1 FROM ")
		r.s(rel.JoinQuoted)
		r.b(' ')
		r.s(ja)
		r.s(" WHERE ")
		r.s(ja)
		r.b('.')
		r.s(rel.JoinRemoteQ[0])
		r.s(" = ")
		r.s(ca)
		r.b('.')
		r.s(h.Table.Columns[rel.Remote[0]].Quoted)
		r.s(" AND ")
		r.s(ja)
		r.b('.')
		r.s(rel.JoinLocalQ[0])
		r.s(" = ")
		r.s(pa)
		r.b('.')
		r.s(h.parent.Table.Columns[rel.Local[0]].Quoted)
		r.b(')')
	}}
}

// preAggCTE is the body of a pre-aggregation CTE: the related rows grouped by their parent key,
// restricted to the parents the outer query selects.
type preAggCTE struct {
	p     *DynPreAgg
	outer *DynQuery
}

func (b preAggCTE) writeSubquery(r *render, _ bool) {
	p := b.p
	pivot := p.rel.hops[p.pivot]
	rel := pivot.relation()
	body := query{ref: pivot, joins: p.innerJoins(), allTenants: b.outer.q.allTenants}
	o := selectOpts{exact: true}
	keys := make([]Expr, len(p.keys))
	if rel.Kind == schema.ManyToMany {
		ja := r.newAlias(rel.JoinAlias)
		target := rel.Target
		o.extraJoin = func(r *render, alias string) {
			r.s(" JOIN ")
			r.s(rel.JoinQuoted)
			r.b(' ')
			r.s(ja)
			r.s(" ON ")
			r.s(ja)
			r.b('.')
			r.s(rel.JoinRemoteQ[0])
			r.s(" = ")
			r.s(alias)
			r.b('.')
			r.s(target.Columns[rel.Remote[0]].Quoted)
		}
		for i, k := range p.keys {
			if k.child < 0 {
				keys[i] = rawText(ja + "." + k.joinQ)
			} else {
				keys[i] = Col{Ref: pivot, Index: k.child}
			}
		}
	} else {
		for i, k := range p.keys {
			keys[i] = Col{Ref: pivot, Index: k.child}
		}
	}
	body.exprs = append(slices.Clip(keys), p.measures...)
	body.group = keys
	body.where = slices.Clip(p.where)
	if len(b.outer.q.where) > 0 && !p.all {
		body.where = append(body.where, b.inParents(keys))
	}
	body.renderSelect(r, newRoot(&body), o)
}

// inParents is (keys) IN (SELECT parent keys FROM the outer query's tables WHERE its filter).
func (b preAggCTE) inParents(keys []Expr) Pred {
	return predExpr{e: func(r *render) {
		parent := b.p.parentRef()
		cols := make([]Expr, len(b.p.keys))
		for i, k := range b.p.keys {
			cols[i] = Col{Ref: parent, Index: k.parent}
		}
		sub := query{ref: b.outer.q.ref, where: b.outer.q.where, exprs: cols, joins: b.outer.q.joins,
			allTenants: b.outer.q.allTenants}
		r.b('(')
		writeExprs(r, keys)
		r.s(") IN (")
		sub.renderSelect(r, newRoot(&sub), selectOpts{exact: true})
		r.b(')')
	}}
}

// preAggLateral is the body of a lateral pre-aggregation: the related rows of the current parent,
// aggregated.
type preAggLateral struct {
	p     *DynPreAgg
	outer *DynQuery
}

func (b preAggLateral) writeSubquery(r *render, _ bool) {
	p := b.p
	exprs := make([]Expr, len(p.measures))
	for i, m := range p.measures {
		exprs[i] = aliased{e: m, name: p.ref.Table.Columns[len(p.keys)+i].Quoted}
	}
	body := query{ref: p.rel.hops[p.pivot], joins: p.innerJoins(), where: slices.Clip(p.where),
		exprs: exprs, having: []Pred{hasRelated}, allTenants: b.outer.q.allTenants}
	body.writeSubquery(r, false)
}

// hasRelated is HAVING count(*) > 0: a lateral pre-aggregation of a parent without related rows
// returns no row, so its measures read NULL as they do with PreAggCTE.
var hasRelated Pred = predExpr{e: func(r *render) { r.s("count(*) > 0") }}

// aliased is e AS name in a select list.
type aliased struct {
	e    Expr
	name string
}

func (a aliased) writeSQL(r *render) {
	a.e.writeSQL(r)
	r.s(" AS ")
	r.s(a.name)
}

// outputNames are the quoted names of the result columns of a dynamic statement.
var outputNames = func() []string {
	out := make([]string, 256)
	for i := range out {
		out[i] = "c" + strconv.Itoa(i+1)
	}
	return out
}()

// outputName is the name of result column i.
func outputName(i int) string {
	if i < len(outputNames) {
		return outputNames[i]
	}
	return "c" + strconv.Itoa(i+1)
}

// writeSubquery writes the query's SELECT inside another statement: a CTE body, a set operation,
// a derived table.
func (q *DynQuery) writeSubquery(r *render, _ bool) { q.writeSelect(r, nil) }

// writeSelect writes the query's SELECT with its outputs named c1…cN, followed by extra hidden
// outputs, with the pre-aggregations as CTEs and lateral joins.
func (q *DynQuery) writeSelect(r *render, hidden []Expr) {
	if q.err != nil {
		r.fail(q.err)
		return
	}
	outs := q.outputs()
	st := q.q
	st.exprs = make([]Expr, 0, len(outs)+len(hidden))
	for _, o := range outs {
		st.exprs = append(st.exprs, aliased{e: o, name: outputName(len(st.exprs))})
	}
	for _, h := range hidden {
		st.exprs = append(st.exprs, aliased{e: h, name: outputName(len(st.exprs))})
	}
	st.ctes = slices.Clip(q.q.ctes)
	st.joins = slices.Clip(q.q.joins)
	for _, p := range q.preaggs {
		if p.err != nil {
			r.fail(p.err)
			return
		}
		if len(p.measures) == 0 {
			continue
		}
		switch p.strategy {
		case PreAggCTE:
			st.ctes = append(st.ctes, &CTETable{ref: p.ref, body: preAggCTE{p: p, outer: q}})
			st.joins = append(st.joins, joinClause{kind: " LEFT JOIN ", ref: p.ref,
				on: []Pred{p.keyEq()}})
		case PreAggLateral:
			st.joins = append(st.joins, joinClause{kind: " LEFT JOIN LATERAL ", ref: p.ref,
				lateral: preAggLateral{p: p, outer: q}})
		}
	}
	st.renderSelect(r, newRoot(&st), selectOpts{exact: true})
}

// ToSQL renders the query's statement as it would run under ctx's scope.
func (q *DynQuery) ToSQL(ctx context.Context, db *DB) (string, []any, error) {
	r := newRender(db.resolveScope(ctx), ctx, db)
	defer r.recycle()
	q.writeSelect(r, nil)
	if r.err != nil {
		return "", nil, r.err
	}
	return string(r.buf), r.argsCopy(), nil
}
