package analytics

import (
	"slices"

	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// planColumns plans every column: its output type, whether it is computed over the grouped
// result, and the sensitivity of the fields it reads.
func (c *compiler) planColumns() error {
	for i := range c.def.Columns {
		col := &c.def.Columns[i]
		out, _ := c.res.Output(col.ID)
		p := &colPlan{def: col, out: out, vtype: valueType(out.Type)}
		c.cols = append(c.cols, p)
		c.byID[col.ID] = p
	}
	for _, p := range c.cols {
		if err := c.planKind(p); err != nil {
			return err
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range c.cols {
			if p.outer || p.def.Calc == nil {
				continue
			}
			for _, ref := range c.calcRefs(p) {
				if c.byID[ref].outer {
					p.outer, changed = true, true
					break
				}
			}
		}
	}
	for _, p := range c.cols {
		p.sens = c.sensitivity(p, map[string]bool{})
	}
	return nil
}

// planKind plans what a column's kind decides: windows are computed over the grouped result,
// and so are distinct counts across a to-many path that pre-aggregation cannot answer exactly.
func (c *compiler) planKind(p *colPlan) error {
	switch {
	case p.def.Window != nil:
		p.outer = true
	case p.def.Measure != nil && p.def.Measure.Field != "":
		fp, err := c.resolvePath(p.def.Measure.Field)
		if err != nil {
			return err
		}
		if fp.many >= 0 && p.def.Measure.Agg == ir.AggCountDistinct && !distinctByParent(fp) {
			p.outer, p.fanout = true, true
		}
	case p.def.Dimension != nil:
		if _, err := c.resolvePath(p.def.Dimension.Field); err != nil {
			return err
		}
	}
	return nil
}

// calcRefs returns the column ids a calc column references.
func (c *compiler) calcRefs(p *colPlan) []string {
	if t, ok := c.res.Calcs[p.def.ID]; ok {
		return t.Node.References(formula.KindColumn)
	}
	return nil
}

// sensitivity returns the highest sensitivity of the fields a column reads, through the columns
// it reads.
func (c *compiler) sensitivity(p *colPlan, seen map[string]bool) table.Sensitivity {
	if seen[p.def.ID] {
		return 0
	}
	seen[p.def.ID] = true
	var s table.Sensitivity
	var paths []string
	switch {
	case p.def.Dimension != nil:
		paths = append(paths, p.def.Dimension.Field)
	case p.def.Measure != nil && p.def.Measure.Field != "":
		paths = append(paths, p.def.Measure.Field)
	case p.def.Window != nil:
		s = c.sensitivity(c.byID[p.def.Window.Of], seen)
	case p.def.Calc != nil:
		if t, ok := c.res.Calcs[p.def.ID]; ok {
			paths = t.Node.References(formula.KindField)
		}
		for _, ref := range c.calcRefs(p) {
			s = max(s, c.sensitivity(c.byID[ref], seen))
		}
	}
	for _, path := range paths {
		via, f, ok := c.cat.Resolve(c.def.Entity, path)
		if ok {
			s = max(s, c.dec.path(c.ctx, via, f).Sensitivity)
		}
	}
	return s
}

// valueType maps an output's formula type to the type of its vector.
func valueType(t formula.Type) value.Type {
	switch t {
	case formula.Enum:
		return value.Enum
	case formula.Ref:
		return value.Ref
	case formula.Int:
		return value.Int
	case formula.Decimal:
		return value.Decimal
	case formula.Float:
		return value.Float
	case formula.Bool:
		return value.Bool
	case formula.Timestamp, formula.Date:
		return value.Time
	case formula.Epoch:
		return value.Epoch
	case formula.JSON:
		return value.JSON
	}
	return value.String
}

// pivotPlan is a pivot's values and the cells each pivoted column spreads into.
type pivotPlan struct {
	def    *ir.Pivot
	values []string
	labels []string
}

// dynamic reports whether the pivot discovers its values when the report runs.
func (p *pivotPlan) dynamic() bool { return p.def.Top != nil }

// cells returns the cell values of a pivoted column: each value, then Other.
func (p *pivotPlan) cells() []string {
	out := slices.Clone(p.values)
	if p.def.Other {
		out = append(out, ir.PivotOther)
	}
	return out
}

// planPivot plans the pivot's values: listed, from a parameter, or discovered.
func (c *compiler) planPivot() error {
	pv := c.def.Pivot
	if pv == nil {
		return nil
	}
	if _, err := c.resolvePath(pv.Field); err != nil {
		return err
	}
	p := &pivotPlan{def: pv}
	switch {
	case pv.Top != nil:
		p.values = slices.Clone(c.opts.PivotValues)
	case pv.Values.Param != "":
		v, ok := c.params[pv.Values.Param]
		if !ok {
			return fail(CodeParameter, "/pivot/values", "the pivot's parameter %q has no value", pv.Values.Param)
		}
		p.values = stringsOf(v)
	default:
		p.values, p.labels = slices.Clone(pv.Values.List), slices.Clone(pv.Labels)
	}
	for _, id := range pv.Columns {
		c.byID[id].pivoted = true
	}
	for _, col := range c.cols {
		if col.def.Calc != nil && slices.Contains(pv.Columns, col.def.ID) {
			col.pivoted = true
		}
	}
	c.pivot = p
	return nil
}

// cellsOf returns the cells a column is computed for: "" for an unpivoted column.
func (c *compiler) cellsOf(p *colPlan) []string {
	if !p.pivoted || c.pivot == nil {
		return []string{""}
	}
	return c.pivot.cells()
}

// planGroupings plans the grouping columns of an aggregated report and its grouping sets.
func (c *compiler) planGroupings() {
	if !c.res.Aggregated {
		return
	}
	for _, p := range c.cols {
		if p.out.Grouped() {
			c.grouped = append(c.grouped, p)
		}
	}
	g := c.def.Groupings
	if g == nil || (!g.Total && len(g.Subtotals) == 0) {
		return
	}
	all := make([]int, len(c.grouped))
	for i := range all {
		all[i] = i
	}
	c.sets = [][]int{all}
	for _, id := range g.Subtotals {
		at := slices.IndexFunc(c.grouped, func(p *colPlan) bool { return p.def.ID == id })
		c.sets = append(c.sets, slices.Clone(all[:at+1]))
	}
	if g.Total {
		c.sets = append(c.sets, []int{})
	}
}

// levelMasks maps each grouping set's GROUPING() mask to the level of its rows: the mask has a 1
// for each grouping column, first column most significant, that the set does not group by.
func (c *compiler) levelMasks() map[int64]table.Level {
	if len(c.sets) == 0 {
		return nil
	}
	n := len(c.grouped)
	out := make(map[int64]table.Level, len(c.sets))
	for _, set := range c.sets {
		var mask int64
		rolled := 0
		for i, p := range c.grouped {
			if slices.Contains(set, i) {
				continue
			}
			mask |= 1 << (n - 1 - i)
			if !p.def.Hidden {
				rolled++
			}
		}
		switch {
		case mask == 0:
			out[mask] = table.Detail
		case len(set) == 0:
			out[mask] = table.Total
		default:
			out[mask] = table.Subtotal(rolled)
		}
	}
	return out
}
