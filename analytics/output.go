package analytics

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/table"
)

// outputColumns returns the output schema: every visible column, pivot cells expanded in place,
// with its translated label, value type, resolved display and sensitivity.
func (c *compiler) outputColumns() []table.Column {
	var out []table.Column
	for i, p := range c.cols {
		if p.def.Hidden {
			continue
		}
		spec := c.displaySpec(i)
		for _, cell := range c.cellsOf(p) {
			out = append(out, table.Column{ID: label(p.def.ID, cell), Label: c.cellLabel(p, cell),
				Type: p.vtype, Display: format.Resolve(p.vtype, c.hint(p), spec), Sensitivity: p.sens})
		}
	}
	return out
}

// displaySpec returns a column's display override, or nil.
func (c *compiler) displaySpec(i int) *format.Spec {
	raw := c.def.Columns[i].Display
	if len(raw) == 0 {
		return nil
	}
	var s format.Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return &s
}

// checkDisplays validates every column's display override against its type.
func (c *compiler) checkDisplays() error {
	for i, p := range c.cols {
		raw := c.def.Columns[i].Display
		if len(raw) == 0 {
			continue
		}
		var s format.Spec
		if err := json.Unmarshal(raw, &s); err != nil {
			return fail(CodeInvalid, "/columns/"+strconv.Itoa(i)+"/display", "the display spec does not decode: %v", err)
		}
		if err := s.Validate(p.vtype); err != nil {
			return fail(CodeInvalid, "/columns/"+strconv.Itoa(i)+"/display", "%v", err)
		}
	}
	return nil
}

// hint returns what the catalog and the definition imply about a column's display.
func (c *compiler) hint(p *colPlan) format.Hint {
	var h format.Hint
	switch {
	case p.def.Dimension != nil:
		d := p.def.Dimension
		if _, f, ok := c.cat.Resolve(c.def.Entity, d.Field); ok {
			h.Kind = kindOf(f.Format)
			h.Labels = c.enumLabels(f)
		}
		h.Bucket = bucketStyle(d.Bucket, c.params)
		if d.Band != nil {
			h.Band = &format.Band{Width: d.Band.Width, Edges: d.Band.Edges}
		}
	case p.def.Measure != nil:
		m := p.def.Measure
		if m.Agg == ir.AggCount || m.Agg == ir.AggCountDistinct {
			h.Kind = format.Count
		} else if _, f, ok := c.cat.Resolve(c.def.Entity, m.Field); ok {
			h.Kind = kindOf(f.Format)
		}
	case p.def.Window != nil:
		switch p.def.Window.Fn {
		case ir.WindowPercentOfTotal, ir.WindowPercentChange:
			h.Kind = format.Percent
		case ir.WindowRank, ir.WindowDenseRank:
			h.Kind = format.Count
		default:
			h = c.hint(c.byID[p.def.Window.Of])
		}
	}
	if p.def.Format != "" {
		h.Kind = kindOf(p.def.Format)
	}
	return h
}

// kindOf returns the format kind a format hint names, or none for one the formatter does not
// know.
func kindOf(name string) format.Kind {
	for _, k := range format.Rules().Kinds {
		if string(k) == name {
			return k
		}
	}
	return format.KindNone
}

// enumLabels maps an enum field's values to their translated labels.
func (c *compiler) enumLabels(f *Field) map[string]string {
	if len(f.Enum) == 0 {
		return nil
	}
	out := make(map[string]string, len(f.Enum))
	for _, v := range f.Enum {
		out[v.Value] = c.cat.Label(c.ctx, v.LabelKey, v.Label)
	}
	return out
}

// bucketStyle returns the date style a bucket implies.
func bucketStyle(g ir.Grain, params map[string]any) format.DateStyle {
	unit := g.Unit
	if g.Param != "" {
		s, _ := params[g.Param].(string)
		unit = ir.Bucket(s)
	}
	switch unit {
	case ir.BucketHour:
		return format.DateStyleDateTime
	case ir.BucketDay, ir.BucketWeek:
		return format.DateStyleDate
	case ir.BucketMonth:
		return format.DateStyleMonthYear
	case ir.BucketQuarter:
		return format.DateStyleQuarter
	case ir.BucketYear:
		return format.DateStyleYear
	}
	return format.DateStyleAuto
}

// cellLabel returns a column's label for a pivot cell.
func (c *compiler) cellLabel(p *colPlan, cell string) string {
	base := c.columnLabel(p)
	if cell == "" {
		return base
	}
	return strings.NewReplacer("{column}", base, "{value}", c.pivotLabel(cell)).
		Replace(c.cat.Label(c.ctx, cellPatternKey, cellLabelPattern))
}

// pivotLabel returns the heading of a pivot value: its listed label, its enum label, or itself.
func (c *compiler) pivotLabel(cell string) string {
	if cell == ir.PivotOther {
		return c.cat.Label(c.ctx, pivotOtherKey, pivotOtherLabel)
	}
	for i, v := range c.pivot.values {
		if v == cell && i < len(c.pivot.labels) {
			return c.pivot.labels[i]
		}
	}
	if _, f, ok := c.cat.Resolve(c.def.Entity, c.pivot.def.Field); ok {
		if l, ok := c.enumLabels(f)[cell]; ok {
			return l
		}
	}
	return cell
}

// columnLabel returns a column's heading: the definition's label, or one derived from its field,
// aggregation or window.
func (c *compiler) columnLabel(p *colPlan) string {
	if p.def.Label != "" {
		return p.def.Label
	}
	switch {
	case p.def.Dimension != nil:
		return c.fieldLabel(p.def.Dimension.Field)
	case p.def.Measure != nil:
		m := p.def.Measure
		if m.Field == "" {
			return c.cat.Label(c.ctx, countRowsKey, countLabel)
		}
		return c.measureLabel(c.cat.Label(c.ctx, aggPrefix+string(m.Agg), aggLabels[m.Agg]), c.fieldLabel(m.Field))
	case p.def.Window != nil:
		fn := p.def.Window.Fn
		return c.measureLabel(c.cat.Label(c.ctx, windowPrefix+string(fn), windowLabels[fn]), c.columnLabel(c.byID[p.def.Window.Of]))
	}
	return p.def.ID
}

// fieldLabel returns a field path's translated label, after the label of the edge that reaches
// the field's entity ("Customer Name").
func (c *compiler) fieldLabel(path string) string {
	via, f, ok := c.cat.Resolve(c.def.Entity, path)
	if !ok {
		return path
	}
	l := c.cat.Label(c.ctx, f.LabelKey, f.Label)
	if len(via) == 0 {
		return l
	}
	e := via[len(via)-1]
	return c.cat.Label(c.ctx, e.LabelKey, e.Label) + " " + l
}

// measureLabel fills the translated measure label pattern with an aggregation's or a window's
// name and what it reads.
func (c *compiler) measureLabel(agg, field string) string {
	return strings.NewReplacer("{agg}", agg, "{field}", field).
		Replace(c.cat.Label(c.ctx, measurePatternKey, measureLabelPattern))
}

// aggLabels are the derived names of the aggregations.
var aggLabels = map[ir.Agg]string{ir.AggCount: "Count", ir.AggCountDistinct: "Distinct count",
	ir.AggSum: "Sum", ir.AggAvg: "Average", ir.AggMin: "Minimum", ir.AggMax: "Maximum"}

// windowLabels are the derived names of the window functions.
var windowLabels = map[ir.WindowFn]string{ir.WindowRunningTotal: "Running total",
	ir.WindowPercentOfTotal: "Share", ir.WindowRank: "Rank", ir.WindowDenseRank: "Dense rank",
	ir.WindowMovingAvg: "Moving average", ir.WindowChange: "Change", ir.WindowPercentChange: "Change %",
	ir.WindowFirst: "First", ir.WindowLast: "Last"}

// slots returns, for each output column, the index of the statement column that holds it, and
// the index of the level column (-1 without grouping sets).
func (c *compiler) slots() ([]int, int) {
	var out []int
	n := 0
	for _, p := range c.cols {
		for range c.cellsOf(p) {
			if !p.def.Hidden {
				out = append(out, n)
			}
			n++
		}
	}
	if len(c.sets) == 0 {
		return out, -1
	}
	return out, n
}

// itoa formats an index.
func itoa(i int) string { return strconv.Itoa(i) }
