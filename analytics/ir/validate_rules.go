package ir

import (
	"strings"

	"github.com/emoss08/keel/analytics/formula"
)

func (v *validator) columnRules() {
	windows, grouped := 0, 0
	for i := range v.def.Columns {
		c := &v.def.Columns[i]
		if _, ok := v.ids[c.ID]; !ok {
			continue
		}
		at := ptr("/columns", i)
		switch {
		case c.Dimension != nil:
			v.dimensionRules(c.Dimension, at+"/dimension")
		case c.Measure != nil && c.Measure.Filter != nil:
			v.filterTree(c.Measure.Filter, at+"/measure/filter", scopeMeasure, c.Measure)
		case c.Window != nil:
			windows++
			v.windowRules(c.Window, at+"/window")
		}
		if v.levelOf(c.ID) <= formula.Row && v.aggregated {
			grouped++
		}
	}
	if windows > v.lim.MaxWindows {
		v.add(CodeLimit, "/columns", "a report can have at most %d window columns", v.lim.MaxWindows)
	}
	if grouped > v.lim.MaxDimensions {
		v.add(CodeLimit, "/columns", "an aggregated report can group by at most %d columns", v.lim.MaxDimensions)
	}
}

func (v *validator) dimensionRules(d *Dimension, at string) {
	f := v.fields[d.Field]
	if f == nil {
		return
	}
	if f.ToMany {
		v.add(CodeNotAllowed, at+"/field", "%s crosses a to-many relation, so it would repeat rows; aggregate it in a measure instead", d.Field)
		return
	}
	if v.aggregated && !f.Info.Groupable && d.Band == nil {
		v.add(CodeNotAllowed, at+"/field", "%s cannot group an aggregated report; band it into ranges or remove the measures", d.Field)
	}
}

func (v *validator) windowRules(w *Window, at string) {
	if !isOneOf(w.Fn, WindowFns()) {
		return
	}
	if w.Fn.ordered() {
		if c, ok := v.refColumn(at+"/over", w.Over, "ordering"); ok {
			v.windowOver(c, at+"/over")
		}
	} else if w.Over != "" {
		v.add(CodeConflict, at+"/over", "%s does not take an ordering column", w.Fn)
	}
	v.partition(w, at+"/partition")
	isChange := w.Fn == WindowChange || w.Fn == WindowPercentChange
	switch {
	case isChange && (w.Offset < 0 || w.Offset > v.lim.MaxOffset):
		v.add(CodeInvalid, at+"/offset", "the offset must be from 1 to %d rows", v.lim.MaxOffset)
	case !isChange && w.Offset != 0:
		v.add(CodeConflict, at+"/offset", "%s does not take an offset", w.Fn)
	}
	switch {
	case w.Fn == WindowMovingAvg && (w.Size < 2 || w.Size > v.lim.MaxWindowSize):
		v.add(CodeInvalid, at+"/size", "a moving average spans from 2 to %d rows", v.lim.MaxWindowSize)
	case w.Fn != WindowMovingAvg && w.Size != 0:
		v.add(CodeConflict, at+"/size", "%s does not take a size", w.Fn)
	}
	if w.Desc && w.Fn != WindowRank && w.Fn != WindowDenseRank {
		v.add(CodeConflict, at+"/desc", "only rank and dense_rank take a direction")
	}
}

func (v *validator) windowOver(c *Column, at string) {
	switch {
	case c.Window != nil:
		v.add(CodeInvalid, at, "a window is ordered by a dimension, measure or calc column, not another window")
	case v.typeOf(c.ID) != formula.Unknown && !v.typeOf(c.ID).Ordered():
		v.add(CodeType, at, "%q is %s, which has no order", c.ID, v.typeOf(c.ID))
	}
}

func (v *validator) partition(w *Window, at string) {
	if !v.distinct(at, w.Partition, "the partition") {
		return
	}
	for i, id := range w.Partition {
		c, ok := v.refColumn(ptr(at, i), id, "partition")
		switch {
		case !ok:
		case v.levelOf(c.ID) > formula.Row:
			v.add(CodeLevel, ptr(at, i), "a window is partitioned by dimension columns, and %q is not one", id)
		case id == w.Over:
			v.add(CodeConflict, ptr(at, i), "%q orders the window, so it cannot also partition it", id)
		}
	}
}

func (v *validator) having() {
	h := v.def.Having
	if h.Empty() {
		return
	}
	if !v.aggregated {
		v.add(CodeInvalid, "/having", "having filters grouped rows, so the report needs a measure")
		return
	}
	v.filterTree(h, "/having", scopeHaving, nil)
}

func (v *validator) groupings() {
	g := v.def.Groupings
	if g == nil || (!g.Total && len(g.Subtotals) == 0) {
		return
	}
	if !v.aggregated {
		v.add(CodeInvalid, "/groupings", "subtotals and totals need a measure to total")
		return
	}
	if len(g.Subtotals) > v.lim.MaxSubtotals {
		v.add(CodeLimit, "/groupings/subtotals", "a report can have at most %d subtotal levels", v.lim.MaxSubtotals)
		return
	}
	if !v.distinct("/groupings/subtotals", g.Subtotals, "the subtotals") {
		return
	}
	grouped := v.visibleGrouped()
	for i, id := range g.Subtotals {
		at := ptr("/groupings/subtotals", i)
		c, ok := v.refColumn(at, id, "subtotal")
		switch {
		case !ok:
		case c.Hidden || v.levelOf(id) > formula.Row:
			v.add(CodeLevel, at, "subtotals are grouped by visible dimension columns, and %q is not one", id)
		case len(grouped) > 0 && grouped[len(grouped)-1] == id:
			v.add(CodeInvalid, at, "%q is the last grouping column, so its subtotal would repeat every row", id)
		}
	}
}

func (v *validator) visibleGrouped() []string {
	var ids []string
	for _, c := range v.def.Columns {
		if !c.Hidden && v.levelOf(c.ID) <= formula.Row {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

func (v *validator) top() {
	t := v.def.Top
	if t == nil {
		return
	}
	v.count("/top/n", t.N, 1, v.lim.MaxTop, "the number of rows kept")
	if c, ok := v.refColumn("/top/by", t.By, "ranking"); ok {
		switch {
		case c.Window != nil:
			v.add(CodeInvalid, "/top/by", "rows are ranked by a dimension, measure or calc column, not a window")
		case v.pivoted[c.ID]:
			v.add(CodeInvalid, "/top/by", "%q is spread across the pivot; rank by an unpivoted column", c.ID)
		case v.typeOf(c.ID) != formula.Unknown && !v.typeOf(c.ID).Ordered():
			v.add(CodeType, "/top/by", "%q is %s, which has no order", c.ID, v.typeOf(c.ID))
		}
	}
	if !v.distinct("/top/per", t.Per, "per") {
		return
	}
	for i, id := range t.Per {
		if c, ok := v.refColumn(ptr("/top/per", i), id, "group"); ok && v.levelOf(c.ID) > formula.Row {
			v.add(CodeLevel, ptr("/top/per", i), "top-N groups by dimension columns, and %q is not one", id)
		}
	}
}

func (v *validator) limitAndPage() {
	if v.def.Limit.Value < 0 {
		v.add(CodeInvalid, "/limit", "the limit cannot be negative")
	} else if !v.def.Limit.IsZero() {
		v.count("/limit", v.def.Limit, 1, v.lim.MaxLimit, "the limit")
	}
	p := v.def.Page
	if p == nil {
		return
	}
	if p.Size < 1 || p.Size > v.lim.MaxPageSize {
		v.add(CodeInvalid, "/page/size", "a page holds from 1 to %d rows", v.lim.MaxPageSize)
	}
	if len(p.After) > 1024 || !cursorText(p.After) {
		v.add(CodeInvalid, "/page/after", "the cursor is not one this report produced")
	}
}

func cursorText(s string) bool {
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '=' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

func (v *validator) sorts() {
	if len(v.def.Sort) > v.lim.MaxSort {
		v.add(CodeLimit, "/sort", "a report can sort by at most %d columns", v.lim.MaxSort)
		return
	}
	seen := map[string]bool{}
	for i, s := range v.def.Sort {
		at := ptr("/sort", i) + "/column"
		if seen[s.Column] {
			v.add(CodeDuplicate, at, "the report already sorts by %q", s.Column)
			continue
		}
		seen[s.Column] = true
		v.sortColumn(s.Column, at)
	}
}

func (v *validator) sortColumn(id, at string) {
	base, value, cell := cutCell(id)
	if _, ok := v.refColumn(at, base, "sort"); !ok {
		return
	}
	p := v.def.Pivot
	switch {
	case cell && !v.pivoted[base]:
		v.add(CodeInvalid, at, "%q is a pivot cell, but %q is not spread across the pivot", id, base)
	case cell && p != nil && p.Values.List != nil && value != PivotOther && !isOneOf(value, p.Values.List):
		v.add(CodeInvalid, at, "the pivot has no value %q", value)
	case cell && p != nil && value == PivotOther && !p.Other:
		v.add(CodeInvalid, at, "the pivot has no Other column")
	case !cell && v.pivoted[base]:
		v.add(CodeInvalid, at, "%q is spread across pivot columns; sort by one of its cells, such as %q", base, base+PivotSeparator+PivotOther)
	}
}

func cutCell(id string) (base, value string, cell bool) {
	return strings.Cut(id, PivotSeparator)
}
