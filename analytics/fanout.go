package analytics

import (
	"slices"
	"strconv"
	"strings"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// fanout builds the statement of the distinct counts across to-many paths that pre-aggregation
// cannot answer exactly (a distinct count of values two parents can share), declares it on the
// outer statement and joins it by the grouping columns and level. The statement reads the
// entity's rows with the report's filters, joins each to-many path's rows (each table read in its
// own CTE, with its tenant predicate and row rules, a many-to-many join table included) on their
// keys and the tenant columns they share, groups by the same grouping sets and counts distinct
// values over the joined rows, which a fan-out cannot inflate.
func (c *compiler) fanout(o *keel.DynQuery) (*keel.DynCTE, error) {
	q := c.source()
	f := newFrame(c, q)
	if err := c.where(f); err != nil {
		return nil, err
	}
	ch, err := c.chainFor(f)
	if err != nil {
		return nil, err
	}
	s := stage{q: q, exprs: map[string]keel.DynExpr{}}
	for _, p := range c.grouped {
		e, err := f.column(p, "")
		if err != nil {
			return nil, err
		}
		s.selectAs(e, label(p.def.ID, ""))
	}
	for _, p := range c.cols {
		if !p.fanout {
			continue
		}
		for _, cell := range c.cellsOf(p) {
			e, err := ch.distinct(p, cell)
			if err != nil {
				return nil, err
			}
			s.selectAs(e, label(p.def.ID, cell))
		}
	}
	if err := c.grouping(f, s); err != nil {
		return nil, err
	}
	cte := o.With(q)
	var on []keel.Pred
	for _, p := range c.grouped {
		l := label(p.def.ID, "")
		on = append(on, o.Path(l).IsDistinctFrom(cte.Col(l)).Not())
	}
	if len(c.sets) > 0 {
		on = append(on, o.Path(levelLabel).Eq(cte.Col(levelLabel)))
	}
	o.LeftJoin(cte, on...)
	return cte, nil
}

// chain joins the rows of to-many paths to a fan-out statement, one CTE per to-many edge.
type chain struct {
	c      *compiler
	f      *frame
	levels map[string]*chainLevel
}

// chainLevel is the CTE of one to-many edge's rows: the labels of the columns it selects, by the
// path suffix they read from the edge's table.
type chainLevel struct {
	cte    *keel.DynCTE
	labels map[string]string
}

// chainFor builds the chain of every to-many path the fan-out distinct counts read, their filters
// included.
func (c *compiler) chainFor(f *frame) (*chain, error) {
	ch := &chain{c: c, f: f, levels: map[string]*chainLevel{}}
	needs := map[string][]string{}
	var paths []fieldPath
	for _, p := range c.cols {
		if !p.fanout {
			continue
		}
		fp, err := c.resolvePath(p.def.Measure.Field)
		if err != nil {
			return nil, err
		}
		paths = append(paths, fp)
		var ferr error
		p.def.Measure.Filter.Walk(func(cond *ir.Filter) {
			if cond.Field == "" || ferr != nil {
				return
			}
			fq, err := c.resolvePath(cond.Field)
			if err != nil {
				ferr = err
				return
			}
			paths = append(paths, fq)
		})
		if ferr != nil {
			return nil, ferr
		}
	}
	for _, fp := range paths {
		addNeeds(needs, fp)
	}
	keys := make([]string, 0, len(needs))
	for k := range needs {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return depth(a) - depth(b) })
	for _, k := range keys {
		if err := ch.level(k, needs[k], paths); err != nil {
			return nil, err
		}
	}
	return ch, nil
}

// depth counts the edges of a level key.
func depth(key string) int { return strings.Count(key, ".") + 1 }

// addNeeds records what each to-many level of a path must select: the path's field at its last
// level, and at earlier levels the keys the next level joins on (edgeKeys).
func addNeeds(needs map[string][]string, fp fieldPath) {
	var many []int
	for i, e := range fp.via {
		if e.ToMany {
			many = append(many, i)
		}
	}
	for j, at := range many {
		key := fp.via[:at+1].String()
		if j == len(many)-1 {
			needs[key] = appendNew(needs[key], suffix(fp.via[at+1:], colName(fp.field)))
			continue
		}
		next := fp.via[many[j+1]]
		local, _ := edgeKeys(next)
		for _, lc := range local {
			needs[key] = appendNew(needs[key], suffix(fp.via[at+1:many[j+1]], next.From.Table.Columns[lc].Name))
		}
	}
}

// suffix joins relation names and a column name into a path read from a level's table.
func suffix(via Path, col string) string {
	return strings.Join(append(relNames(via), col), ".")
}

// appendNew appends s unless the list holds it.
func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// level declares the CTE of one to-many edge and joins it to its parent level.
func (ch *chain) level(key string, cols []string, paths []fieldPath) error {
	var fp fieldPath
	at := -1
	for _, p := range paths {
		for i := range p.via {
			if p.via[:i+1].String() == key {
				fp, at = p, i
			}
		}
	}
	edge := fp.via[at]
	rel := edge.relation()
	local, remote := edgeKeys(edge)
	parent, err := ch.parentKeys(fp, at, local)
	if err != nil {
		return err
	}
	cq := keel.DynFrom(edge.Target.Table)
	lv := &chainLevel{labels: map[string]string{}}
	var sel []keel.DynExpr
	for i, r := range remote {
		sel = append(sel, cq.Path(edge.Target.Table.Columns[r].Name).As("r"+strconv.Itoa(i+1)))
	}
	for i, col := range cols {
		if j := slices.IndexFunc(remote, func(r int32) bool { return edge.Target.Table.Columns[r].Name == col }); j >= 0 {
			lv.labels[col] = "r" + strconv.Itoa(j+1)
			continue
		}
		l := "v" + strconv.Itoa(i+1)
		sel = append(sel, cq.Path(strings.Split(col, ".")...).As(l))
		lv.labels[col] = l
	}
	cq.Select(sel...)
	if err := cq.Err(); err != nil {
		return err
	}
	q := ch.f.q
	lv.cte = q.With(cq)
	if rel.Kind == schema.ManyToMany {
		jt := q.With(joinTableQuery(edge.From.Table, rel))
		on := []keel.Pred{jt.Col("l").Eq(parent[0])}
		to := []keel.Pred{lv.cte.Col("r1").Eq(jt.Col("r"))}
		for i := 1; i < len(parent); i++ {
			tc := jt.Col("t" + strconv.Itoa(i))
			on = append(on, tc.Eq(parent[i]))
			to = append(to, lv.cte.Col("r"+strconv.Itoa(i+1)).Eq(tc))
		}
		q.Join(jt, on...)
		q.Join(lv.cte, to...)
	} else {
		on := make([]keel.Pred, len(parent))
		for i, p := range parent {
			on[i] = lv.cte.Col("r" + strconv.Itoa(i+1)).Eq(p)
		}
		q.Join(lv.cte, on...)
	}
	ch.levels[key] = lv
	return nil
}

// edgeKeys returns the columns a to-many edge joins on, as its parent's and its table's column
// indexes: the relation's keys (for many-to-many the parent key and the target key the join table
// refers to), then the tenant columns the two tables share (schema.SharedTenant). Under a tenant
// scope every table's CTE is pinned to the scope's tenant and the tenant equalities are implied;
// across tenants they keep a row of one tenant from joining rows of another whose keys repeat.
func edgeKeys(e *Edge) (local, remote []int32) {
	rel := e.relation()
	local, remote = rel.Local, rel.Remote
	if rel.Kind == schema.ManyToMany {
		local, remote = rel.Local[:1], rel.Remote[:1]
	}
	tl, tr := schema.SharedTenant(e.From.Table, rel.Target)
	return slices.Concat(local, tl), slices.Concat(remote, tr)
}

// parentKeys returns the values of the parent's columns locals that the to-many edge at index at
// of a path joins on: columns of the entity through to-one edges, or columns the previous level
// selected.
func (ch *chain) parentKeys(fp fieldPath, at int, locals []int32) ([]keel.DynExpr, error) {
	prev := -1
	for i := at - 1; i >= 0; i-- {
		if fp.via[i].ToMany {
			prev = i
			break
		}
	}
	out := make([]keel.DynExpr, len(locals))
	for i, local := range locals {
		name := fp.via[at].From.Table.Columns[local].Name
		if prev < 0 {
			out[i] = ch.f.q.Path(append(relNames(fp.via[:at]), name)...)
		} else {
			lv := ch.levels[fp.via[:prev+1].String()]
			out[i] = lv.cte.Col(lv.labels[suffix(fp.via[prev+1:at], name)])
		}
		if err := out[i].Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// joinTableQuery reads the join table of parent's many-to-many relation rel: its key to the parent
// ("l"), to the target ("r") and the tenant columns parent and target share ("t1"…), which the
// join table carries (schema.Table.JoinTableOf). Those make it a tenant table, so the scope's
// tenant predicate filters it like every other table of the statement.
func joinTableQuery(parent *schema.Table, rel *schema.Relation) *keel.DynQuery {
	t := parent.JoinTableOf(rel)
	q := keel.DynFrom(t)
	sel := []keel.DynExpr{q.Path(rel.JoinLocal[0]).As("l"), q.Path(rel.JoinRemote[0]).As("r")}
	for i, ci := range t.Tenant {
		sel = append(sel, q.Path(t.Columns[ci].Name).As("t"+strconv.Itoa(i+1)))
	}
	return q.Select(sel...)
}

// value returns a field of a to-many path from its last level's CTE.
func (ch *chain) value(fp fieldPath) keel.DynExpr {
	last := -1
	for i, e := range fp.via {
		if e.ToMany {
			last = i
		}
	}
	lv := ch.levels[fp.via[:last+1].String()]
	return lv.cte.Col(lv.labels[suffix(fp.via[last+1:], colName(fp.field))])
}

// distinct returns a fan-out distinct count for a pivot cell, with its measure filter over the
// joined rows.
func (ch *chain) distinct(p *colPlan, cell string) (keel.DynExpr, error) {
	m := p.def.Measure
	fp, err := ch.c.resolvePath(m.Field)
	if err != nil {
		return keel.DynExpr{}, err
	}
	pivot, err := ch.f.pivotPred(cell)
	if err != nil {
		return keel.DynExpr{}, err
	}
	at := "/columns/" + itoa(ch.c.indexOf(p)) + "/measure/filter"
	filter, err := ch.c.filterPred(m.Filter, at, condEnv{scope: scopeMeasure,
		field: func(path string) (subject, func(keel.Pred) keel.Pred, error) {
			fq, err := ch.c.resolvePath(path)
			if err != nil {
				return subject{}, nil, err
			}
			return subject{e: ch.value(fq), t: fq.field.Type}, identity, nil
		},
		formula: func(t *formula.Typed) (keel.DynExpr, error) {
			return keel.DynExpr{}, fail(CodeNotAllowed, at, "a distinct count across a to-many relation filters by fields, not calculations")
		},
	})
	if err != nil {
		return keel.DynExpr{}, err
	}
	return aggregate(ir.AggCountDistinct, ch.value(fp), andPred(filter, pivot), formula.Int), nil
}
