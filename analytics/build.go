package analytics

import (
	"encoding/json"
	"fmt"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// The labels of the hidden columns a statement selects besides the report's columns. Labels are
// kept in Go and never written into SQL.
const (
	levelLabel = "__level"
	rankLabel  = "__rank"
)

// label is the label of a column's cell: its id, or "id:value" for a pivot cell.
func label(id, cell string) string {
	if cell == "" {
		return id
	}
	return id + ir.PivotSeparator + cell
}

// pkLabel is the label of the i-th primary key column of a record list.
func pkLabel(i int) string { return fmt.Sprintf("__pk%d", i+1) }

// stage is one statement of the report's pipeline and how to read its columns: the grouped
// statement reads the expressions it selected, a derived statement reads its inner statement's
// columns by label.
type stage struct {
	q     *keel.DynQuery
	exprs map[string]keel.DynExpr
}

// col returns the value of a column of the stage by label.
func (s stage) col(l string) keel.DynExpr {
	if e, ok := s.exprs[l]; ok {
		return e
	}
	return s.q.Path(l)
}

// build compiles the report's statement: the grouped statement, then, when a column is computed
// over the grouped result, a statement over it, then the top-N filter, the order and the limit.
func (c *compiler) build(out *Compiled) (*keel.DynQuery, error) {
	if err := c.planHoists(); err != nil {
		return nil, err
	}
	g, err := c.group()
	if err != nil {
		return nil, err
	}
	final := g
	if c.needsOuter() {
		if final, err = c.outer(g); err != nil {
			return nil, err
		}
		if c.def.Top != nil {
			if final, err = c.top(final); err != nil {
				return nil, err
			}
		}
	}
	c.order(final)
	if err := c.limit(final, out); err != nil {
		return nil, err
	}
	return final.q, final.q.Err()
}

// needsOuter reports whether the report needs a statement over the grouped result: for windows,
// fan-out distinct counts and calculations over them, top-N, or having on such columns.
func (c *compiler) needsOuter() bool {
	if c.def.Top != nil {
		return true
	}
	for _, p := range c.cols {
		if p.outer {
			return true
		}
	}
	return false
}

// source returns a new dynamic query over the report's source: the entity's table, or the
// rollup that answers it.
func (c *compiler) source() *keel.DynQuery {
	if c.rollup != nil {
		return keel.DynFrom(c.rollup.Table)
	}
	return keel.DynFrom(c.base.Table)
}

// group builds the grouped statement: the filters, every column not computed over the grouped
// result, the aggregates hoisted out of those that are, the grouping and having.
func (c *compiler) group() (stage, error) {
	q := c.source()
	f := newFrame(c, q)
	s := stage{q: q, exprs: map[string]keel.DynExpr{}}
	if err := c.where(f); err != nil {
		return s, err
	}
	for _, p := range c.cols {
		if p.outer {
			continue
		}
		for _, cell := range c.cellsOf(p) {
			e, err := f.column(p, cell)
			if err != nil {
				return s, err
			}
			s.selectAs(e, label(p.def.ID, cell))
		}
	}
	if err := c.selectHoists(f, s); err != nil {
		return s, err
	}
	if err := c.grouping(f, s); err != nil {
		return s, err
	}
	if !c.havingOuter() {
		if err := c.having(s, f.env(""), f.q); err != nil {
			return s, err
		}
	}
	if c.needsOuter() && !c.res.Aggregated {
		for i, e := range c.recordKeys(f) {
			s.selectAs(e, pkLabel(i))
		}
	}
	return s, nil
}

// selectAs selects e under label l.
func (s stage) selectAs(e keel.DynExpr, l string) {
	s.q.Select(e.As(l))
	if s.exprs != nil {
		s.exprs[l] = e
	}
}

// where adds the report's filters to a frame's statement.
func (c *compiler) where(f *frame) error {
	p, err := c.filterPred(c.def.Filters, "/filters", f.whereEnv())
	if err != nil || p == nil {
		return err
	}
	f.q.Where(p)
	return nil
}

// grouping groups an aggregated report's statement by its grouping columns, in grouping sets
// with the level column when it has subtotals or a total.
func (c *compiler) grouping(f *frame, s stage) error {
	if !c.res.Aggregated || len(c.grouped) == 0 {
		return nil
	}
	exprs, err := c.groupExprs(f)
	if err != nil {
		return err
	}
	if len(c.sets) == 0 {
		f.q.Group(exprs...)
		return nil
	}
	sets := make([][]keel.Expr, len(c.sets))
	for i, set := range c.sets {
		sets[i] = make([]keel.Expr, len(set))
		for j, k := range set {
			sets[i][j] = exprs[k]
		}
	}
	f.q.GroupingSets(sets...)
	s.selectAs(keel.DynOf(keel.Grouping(exprs...)), levelLabel)
	return nil
}

// groupExprs returns the grouping columns' expressions, the ones the statement selects.
func (c *compiler) groupExprs(f *frame) ([]keel.Expr, error) {
	out := make([]keel.Expr, len(c.grouped))
	for i, p := range c.grouped {
		e, err := f.column(p, "")
		if err != nil {
			return nil, err
		}
		out[i] = e
	}
	return out, nil
}

// recordKeys returns the primary key of a record list's rows, which orders and pages it.
func (c *compiler) recordKeys(f *frame) []keel.DynExpr {
	if c.rollup != nil {
		return nil
	}
	t := c.base.Table
	var out []keel.DynExpr
	for _, pk := range t.PK {
		if !t.Columns[pk].Flags.Has(schema.FlagTenant) {
			out = append(out, f.q.Path(t.Columns[pk].Name))
		}
	}
	return out
}

// havingOuter reports whether having compares a column computed over the grouped result, so it
// filters the outer statement.
func (c *compiler) havingOuter() bool {
	if c.def.Having.Empty() {
		return false
	}
	outer := false
	c.def.Having.Walk(func(f *ir.Filter) {
		if f.Column != "" && c.byID[f.Column].outer {
			outer = true
		}
	})
	for at, t := range c.res.Conditions {
		if len(at) >= len("/having") && at[:len("/having")] == "/having" {
			for _, ref := range t.Node.References(formula.KindColumn) {
				if c.byID[ref].outer {
					outer = true
				}
			}
		}
	}
	return outer
}

// having adds the having filter: to the grouped statement's HAVING, or as a WHERE over the
// grouped result. Subtotal and total rows are kept: they cover every group.
func (c *compiler) having(s stage, env formulaEnv, q *keel.DynQuery) error {
	p, err := c.filterPred(c.def.Having, "/having", condEnv{scope: scopeHaving,
		column: env.column, formula: func(t *formula.Typed) (keel.DynExpr, error) { return c.render(t, env) }})
	if err != nil || p == nil {
		return err
	}
	if len(c.sets) > 0 {
		p = s.col(levelLabel).Ne(keel.DynLit(int64(0)).Cast(keel.TypeInt64)).Or(p)
	}
	if s.exprs != nil {
		q.Having(p)
	} else {
		q.Where(p)
	}
	return nil
}

// planHoists finds the aggregate calls over fields in the calculations computed over the grouped
// result, which the grouped statement computes as hidden columns.
func (c *compiler) planHoists() error {
	n := 0
	add := func(t *formula.Typed, cells []string) {
		walkTyped(t, func(x *formula.Typed) {
			if x.Func == nil || !x.Func.Aggregate {
				return
			}
			for _, cell := range cells {
				key := hoistKey(x.Node, cell)
				if _, ok := c.hoistLabels[key]; !ok {
					n++
					c.hoistLabels[key] = fmt.Sprintf("__h%d", n)
					c.hoistList = append(c.hoistList, hoist{t: x, cell: cell, label: c.hoistLabels[key]})
				}
			}
		})
	}
	for _, p := range c.cols {
		if p.outer && p.def.Calc != nil {
			add(c.res.Calcs[p.def.ID], c.cellsOf(p))
		}
	}
	if c.havingOuter() {
		for at, t := range c.res.Conditions {
			if len(at) >= len("/having") && at[:len("/having")] == "/having" {
				add(t, []string{""})
			}
		}
	}
	return nil
}

// hoist is an aggregate call computed by the grouped statement for a calculation over the
// grouped result.
type hoist struct {
	t     *formula.Typed
	cell  string
	label string
}

// hoistKey identifies a hoisted aggregate call and pivot cell.
func hoistKey(n *formula.Node, cell string) string { return fmt.Sprintf("%p\x00%s", n, cell) }

// walkTyped calls fn for t and every node below it.
func walkTyped(t *formula.Typed, fn func(*formula.Typed)) {
	if t == nil {
		return
	}
	fn(t)
	walkTyped(t.Subject, fn)
	for _, a := range t.Args {
		walkTyped(a, fn)
	}
	walkTyped(t.Else, fn)
}

// selectHoists selects the hoisted aggregate calls in the grouped statement.
func (c *compiler) selectHoists(f *frame, s stage) error {
	for _, h := range c.hoistList {
		e, err := f.aggregateCall(h.t, h.cell)
		if err != nil {
			return err
		}
		s.selectAs(e, h.label)
	}
	return nil
}

// outer builds the statement over the grouped result: its columns read by label, the fan-out
// distinct counts joined by grouping columns and level, windows, the calculations over them, and
// having on them.
func (c *compiler) outer(g stage) (stage, error) {
	o := keel.DynFromQuery(g.q)
	of := &outerFrame{c: c, q: o, cols: map[string]keel.DynExpr{}}
	s := stage{q: o}
	if c.hasFanout() {
		cte, err := c.fanout(o)
		if err != nil {
			return s, err
		}
		of.fan = cte
	}
	for _, p := range c.cols {
		for _, cell := range c.cellsOf(p) {
			e, err := of.column(p, cell)
			if err != nil {
				return s, err
			}
			s.selectAs(e, label(p.def.ID, cell))
		}
	}
	c.carry(g, s)
	if c.havingOuter() {
		if err := c.having(s, of.env(""), o); err != nil {
			return s, err
		}
	}
	return s, nil
}

// carry selects the level and record key columns of an inner stage in a derived one.
func (c *compiler) carry(inner, s stage) {
	if len(c.sets) > 0 {
		s.selectAs(s.q.Path(levelLabel), levelLabel)
	}
	for _, l := range inner.pks() {
		s.selectAs(s.q.Path(l), l)
	}
}

// pks returns the record key labels a stage selects.
func (s stage) pks() []string {
	var out []string
	for i := 0; s.hasLabel(pkLabel(i)); i++ {
		out = append(out, pkLabel(i))
	}
	return out
}

// hasLabel reports whether the stage selects a column with the label.
func (s stage) hasLabel(l string) bool {
	if s.exprs != nil {
		_, ok := s.exprs[l]
		return ok
	}
	return s.q.Path(l).Err() == nil
}

// hasFanout reports whether a column is a fan-out distinct count.
func (c *compiler) hasFanout() bool {
	for _, p := range c.cols {
		if p.fanout {
			return true
		}
	}
	return false
}

// top keeps the first N rows of each group by the ranking column: a row number over the grouped
// result, filtered in a statement over it. Subtotal and total rows are kept.
func (c *compiler) top(s stage) (stage, error) {
	t := c.def.Top
	n, ok := countOf(t.N, c.params)
	if !ok {
		return s, fail(CodeParameter, "/top/n", "the number of rows to keep has no value")
	}
	var parts []keel.Expr
	if len(c.sets) > 0 {
		parts = append(parts, s.col(levelLabel))
	}
	for _, id := range t.Per {
		parts = append(parts, s.col(id))
	}
	by := s.col(t.By)
	orders := []keel.Order{by.Desc().NullsLast()}
	if t.Bottom {
		orders = []keel.Order{by.Asc().NullsLast()}
	}
	orders = append(orders, c.tiebreak(s)...)
	rank := keel.DynOf(keel.RowNumber()).Over(keel.PartitionBy(parts...).OrderBy(orders...))
	s.selectAs(rank, rankLabel)
	o := keel.DynFromQuery(s.q)
	next := stage{q: o}
	keep := o.Path(rankLabel).Lte(keel.DynLit(int64(n)).Cast(keel.TypeInt64))
	if len(c.sets) > 0 {
		keep = o.Path(levelLabel).Ne(keel.DynLit(int64(0)).Cast(keel.TypeInt64)).Or(keep)
	}
	o.Where(keep)
	for _, p := range c.cols {
		for _, cell := range c.cellsOf(p) {
			l := label(p.def.ID, cell)
			next.selectAs(o.Path(l), l)
		}
	}
	c.carry(s, next)
	return next, nil
}

// tiebreak returns the order terms that make the stage's rows unique: the grouping columns and
// the level of an aggregated report, the record key of a record list.
func (c *compiler) tiebreak(s stage) []keel.Order {
	var out []keel.Order
	if c.res.Aggregated {
		for _, p := range c.grouped {
			out = append(out, s.col(label(p.def.ID, "")).Asc())
		}
		if len(c.sets) > 0 {
			out = append(out, s.col(levelLabel).Asc())
		}
		return out
	}
	for _, l := range s.pks() {
		out = append(out, s.col(l).Asc())
	}
	return out
}

// order sorts the final statement: subtotal and total rows after the rows they summarize, then
// the definition's sort, then the tiebreak, so pages and previews are deterministic.
func (c *compiler) order(s stage) {
	var orders []keel.Order
	sorted := map[string]bool{}
	if len(c.sets) > 0 {
		orders = append(orders, c.clusters(s, sorted)...)
	}
	for _, srt := range c.def.Sort {
		if sorted[srt.Column] {
			continue
		}
		sorted[srt.Column] = true
		e := s.col(srt.Column)
		if srt.Desc {
			orders = append(orders, e.Desc().NullsLast())
		} else {
			orders = append(orders, e.Asc())
		}
	}
	for _, o := range c.tiebreak(s) {
		orders = append(orders, o)
	}
	if !c.res.Aggregated && s.exprs != nil && c.rollup == nil {
		for _, e := range c.recordKeysOf(s) {
			orders = append(orders, e.Asc())
		}
	}
	s.q.Order(orders...)
}

// recordKeysOf returns the record key of a record list read by the grouped statement itself.
func (c *compiler) recordKeysOf(s stage) []keel.DynExpr {
	t := c.base.Table
	var out []keel.DynExpr
	for _, pk := range t.PK {
		if !t.Columns[pk].Flags.Has(schema.FlagTenant) {
			out = append(out, s.q.Path(t.Columns[pk].Name))
		}
	}
	return out
}

// clusters returns the order terms that keep each subtotal row after the rows it summarizes and
// the total last: for each grouping column down to the deepest subtotal, whether the row rolls it
// up, then its value in the sort's direction for it; then the level, which puts detail rows
// before the subtotal of their group.
func (c *compiler) clusters(s stage, sorted map[string]bool) []keel.Order {
	deepest := -1
	for _, id := range c.def.Groupings.Subtotals {
		for i, p := range c.grouped {
			if p.def.ID == id {
				deepest = max(deepest, i)
			}
		}
	}
	level := s.col(levelLabel)
	zero := keel.DynLit(int64(0)).Cast(keel.TypeInt64)
	if deepest < 0 {
		return []keel.Order{keel.DynOf(keel.Raw("(? <> ?)", level, zero)).Asc()}
	}
	n := len(c.grouped)
	var out []keel.Order
	for i, p := range c.grouped[:deepest+1] {
		rolled := keel.DynOf(keel.Raw("((? >> ?::integer) & 1)", level, int64(n-1-i)))
		out = append(out, rolled.Asc())
		e := s.col(label(p.def.ID, ""))
		desc := false
		for _, srt := range c.def.Sort {
			if srt.Column == p.def.ID {
				desc = srt.Desc
			}
		}
		if desc {
			out = append(out, e.Desc().NullsLast())
		} else {
			out = append(out, e.Asc())
		}
		sorted[p.def.ID] = true
	}
	return append(out, level.Asc())
}

// limit caps the final statement's rows: the definition's limit, capped by MaxRows, plus the
// probe row; a page or a count has none.
func (c *compiler) limit(s stage, out *Compiled) error {
	if c.opts.Unlimited || c.def.Page != nil {
		return nil
	}
	n := 0
	if !c.def.Limit.IsZero() {
		v, ok := countOf(c.def.Limit, c.params)
		if !ok && c.def.Limit.Param == "" {
			return fail(CodeInvalid, "/limit", "the limit must be positive")
		}
		n = v
	}
	if c.opts.MaxRows > 0 && (n == 0 || n > c.opts.MaxRows) {
		n = c.opts.MaxRows
	}
	out.Limit = n
	if n > 0 {
		probe := 0
		if c.opts.Probe {
			probe = 1
		}
		s.q.Limit(n + probe)
	}
	return nil
}

// filterKey identifies a measure filter for sharing pre-aggregated columns.
func filterKey(f *ir.Filter) string {
	if f == nil {
		return ""
	}
	b, _ := json.Marshal(f)
	return string(b)
}
