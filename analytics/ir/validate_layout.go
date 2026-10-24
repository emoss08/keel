package ir

import (
	"math"

	"github.com/emoss08/keel/analytics/formula"
)

func (v *validator) pivot() {
	p := v.def.Pivot
	if p == nil {
		return
	}
	if !v.aggregated {
		v.add(CodeInvalid, "/pivot", "a pivot spreads measures, so the report needs one")
		return
	}
	v.pivotField(p)
	width := v.pivotValues(p)
	v.pivotColumns(p)
	if p.Other {
		width++
	}
	if width*len(p.Columns) > v.lim.MaxPivotColumns {
		v.add(CodeLimit, "/pivot", "the pivot would produce %d columns; the most is %d", width*len(p.Columns), v.lim.MaxPivotColumns)
	}
}

func (v *validator) pivotField(p *Pivot) {
	f, ok := v.field("/pivot/field", p.Field)
	if !ok {
		return
	}
	for _, c := range v.def.Columns {
		if c.Dimension != nil && c.Dimension.Field == p.Field {
			v.add(CodeConflict, "/pivot/field", "%s is already a dimension column, so it cannot also be the pivot", p.Field)
			return
		}
	}
	switch {
	case f == nil:
	case f.ToMany:
		v.add(CodeNotAllowed, "/pivot/field", "%s crosses a to-many relation and cannot be pivoted", p.Field)
	case !f.Info.Groupable:
		v.add(CodeNotAllowed, "/pivot/field", "%s cannot be pivoted", p.Field)
	}
}

func (v *validator) pivotValues(p *Pivot) int {
	listed, discovered := !p.Values.IsZero(), p.Top != nil
	switch {
	case listed == discovered:
		v.add(CodeConflict, "/pivot", "a pivot lists its values or discovers the top ones, exactly one of the two")
		return 0
	case discovered:
		return v.pivotTop(p)
	case p.Values.Param != "" && p.Values.List != nil:
		v.add(CodeConflict, "/pivot/values", "pivot values are a list or a parameter, not both")
		return 0
	case p.Values.Param != "":
		v.pivotParam(p)
		return v.lim.MaxPivotValues
	}
	return v.pivotList(p)
}

func (v *validator) pivotTop(p *Pivot) int {
	if len(p.Labels) > 0 {
		v.add(CodeConflict, "/pivot/labels", "labels name listed values; discovered values are labelled by themselves")
	}
	v.count("/pivot/top/n", p.Top.N, 1, v.lim.MaxPivotValues, "the number of pivot values")
	if c, ok := v.refColumn("/pivot/top/by", p.Top.By, "ranking"); ok && v.levelOf(c.ID) != formula.Aggregate {
		v.add(CodeLevel, "/pivot/top/by", "pivot values are ranked by a measure or an aggregate calc, and %q is not one", c.ID)
	}
	if p.Top.N.Param != "" {
		return v.lim.MaxPivotValues
	}
	return p.Top.N.Value
}

func (v *validator) pivotParam(p *Pivot) {
	if len(p.Labels) > 0 {
		v.add(CodeConflict, "/pivot/labels", "labels name listed values, not values a parameter supplies")
	}
	t := formula.Unknown
	if f := v.fields[p.Field]; f != nil {
		t = f.Info.Type
	}
	v.paramUse("/pivot/values", p.Values.Param, func(prm *Parameter) string {
		if !prm.Multi || !compatible(prm.Type, t) {
			return "a multi-value parameter of the pivot field's type"
		}
		return ""
	})
}

func (v *validator) pivotList(p *Pivot) int {
	list := p.Values.List
	if len(list) == 0 {
		v.add(CodeRequired, "/pivot/values", "list at least one pivot value")
		return 0
	}
	if len(list) > v.lim.MaxPivotValues {
		v.add(CodeLimit, "/pivot/values", "a pivot can list at most %d values", v.lim.MaxPivotValues)
	}
	v.distinct("/pivot/values", list, "the pivot values")
	for i, x := range list {
		if x == PivotOther {
			v.add(CodeInvalid, ptr("/pivot/values", i), "%q is reserved for the Other column; set other instead", PivotOther)
		}
	}
	if len(p.Labels) > 0 && len(p.Labels) != len(list) {
		v.add(CodeInvalid, "/pivot/labels", "give one label per pivot value (%d values, %d labels)", len(list), len(p.Labels))
	}
	return len(list)
}

func (v *validator) pivotColumns(p *Pivot) {
	if len(p.Columns) == 0 {
		v.add(CodeRequired, "/pivot/columns", "choose at least one measure to spread")
		return
	}
	if !v.distinct("/pivot/columns", p.Columns, "the pivot columns") {
		return
	}
	for i, id := range p.Columns {
		at := ptr("/pivot/columns", i)
		c, ok := v.refColumn(at, id, "pivoted")
		switch {
		case !ok:
		case c.Hidden:
			v.add(CodeInvalid, at, "%q is hidden, so it cannot be spread across the pivot", id)
		case v.levelOf(id) != formula.Aggregate:
			v.add(CodeLevel, at, "a pivot spreads measures and aggregate calcs, and %q is not one", id)
		}
	}
}

type role struct {
	dimension bool
}

func (v *validator) roles() map[string]role {
	roles := map[string]role{}
	p := v.def.Pivot
	for _, c := range v.def.Columns {
		if c.Hidden {
			continue
		}
		r := role{dimension: v.levelOf(c.ID) <= formula.Row}
		roles[c.ID] = r
		if p == nil || !v.pivoted[c.ID] {
			continue
		}
		for _, x := range p.Values.List {
			roles[c.ID+PivotSeparator+x] = r
		}
		if p.Other {
			roles[c.ID+PivotSeparator+PivotOther] = r
		}
	}
	return roles
}

func (v *validator) charts() {
	charts := v.def.Charts
	if len(charts) > MaxCharts {
		v.add(CodeLimit, "/charts", "a report can have at most %d charts", MaxCharts)
		return
	}
	roles := v.roles()
	seen := map[string]bool{}
	for i := range charts {
		c := &charts[i]
		at := ptr("/charts", i)
		switch {
		case c.ID == "":
			v.add(CodeRequired, at+"/id", "give the chart an id")
			continue
		case len(c.ID) > maxIDLength:
			v.add(CodeInvalid, at+"/id", "a chart id can be at most %d characters", maxIDLength)
			continue
		case seen[c.ID]:
			v.add(CodeDuplicate, at+"/id", "chart id %q is used twice", c.ID)
			continue
		case !isOneOf(c.Type, ChartTypes()):
			v.unknown(CodeInvalid, at+"/type", "chart type", string(c.Type), names(ChartTypes()))
			continue
		}
		seen[c.ID] = true
		ch := chartCheck{v: v, c: c, at: at, roles: roles}
		ch.axes()
		ch.series()
		ch.options()
	}
}

type chartCheck struct {
	v     *validator
	c     *Chart
	at    string
	roles map[string]role
}

func (ch chartCheck) ref(field, id string, wantDimension, wantValue bool) {
	r, ok := ch.roles[id]
	switch {
	case !ok:
		ch.v.unknown(CodeUnknownColumn, ch.at+"/"+field, "column", id, keys(ch.roles))
	case wantDimension && !r.dimension:
		ch.v.add(CodeLevel, ch.at+"/"+field, "this chart groups by a dimension column, and %q is a value", id)
	case wantValue && r.dimension:
		ch.v.add(CodeLevel, ch.at+"/"+field, "%q is a dimension column and cannot be drawn as a value", id)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (ch chartCheck) axes() {
	c, rule := ch.c, chartRule(ch.c.Type)
	switch {
	case c.Type == ChartKPI || c.Type == ChartMap:
		if c.X != "" {
			ch.v.add(CodeConflict, ch.at+"/x", "a %s has no category axis", c.Type)
		}
	case c.X == "":
		ch.v.add(CodeRequired, ch.at+"/x", "choose the column on the horizontal axis")
	default:
		ch.ref("x", c.X, rule.Dimension, !rule.Dimension)
	}
	if rule.Coordinates {
		ch.coordinates()
	} else if c.Lat != "" || c.Lng != "" || c.PointLabel != "" {
		ch.v.add(CodeConflict, ch.at, "only a map takes lat, lng and pointLabel")
	}
}

func (ch chartCheck) coordinates() {
	c := ch.c
	for _, coord := range []struct{ field, id, what string }{{"lat", c.Lat, "latitude"}, {"lng", c.Lng, "longitude"}} {
		if coord.id == "" {
			ch.v.add(CodeRequired, ch.at+"/"+coord.field, "a map needs a %s column", coord.what)
			continue
		}
		ch.ref(coord.field, coord.id, false, false)
	}
	if c.Lat != "" && c.Lat == c.Lng {
		ch.v.add(CodeConflict, ch.at+"/lng", "latitude and longitude cannot be the same column")
	}
	if c.PointLabel != "" {
		ch.ref("pointLabel", c.PointLabel, false, false)
	}
}

func (ch chartCheck) series() {
	c, rule := ch.c, chartRule(ch.c.Type)
	switch {
	case len(c.Series) == 0 && c.Type != ChartMap:
		ch.v.add(CodeRequired, ch.at+"/series", "choose at least one value to draw")
		return
	case len(c.Series) > MaxChartSeries:
		ch.v.add(CodeLimit, ch.at+"/series", "a chart draws at most %d values", MaxChartSeries)
		return
	case rule.SingleSeries && len(c.Series) > 1:
		ch.v.add(CodeInvalid, ch.at+"/series", "a %s chart draws exactly one value", c.Type)
		return
	}
	if !ch.v.distinct(ch.at+"/series", c.Series, "the series") {
		return
	}
	for i, id := range c.Series {
		ch.ref(ptr("series", i), id, false, true)
	}
	if c.Stacked && !rule.Stacking {
		ch.v.add(CodeConflict, ch.at+"/stacked", "a %s chart cannot be stacked", c.Type)
	}
}

func (ch chartCheck) options() {
	c := ch.c
	if c.Limit < 0 || c.Limit > MaxChartCategories {
		ch.v.add(CodeInvalid, ch.at+"/limit", "the category limit must be from 0 to %d", MaxChartCategories)
	}
	if c.Compare != "" {
		if c.Type != ChartKPI {
			ch.v.add(CodeConflict, ch.at+"/compare", "only a KPI tile shows a comparison")
		} else {
			ch.ref("compare", c.Compare, false, true)
		}
	}
	g := c.Goal
	switch {
	case g == nil || (g.Value == nil && g.Column == ""):
	case g.Value != nil && g.Column != "":
		ch.v.add(CodeConflict, ch.at+"/goal", "a target is a fixed number or a column, not both")
	case g.Value != nil && (math.IsNaN(*g.Value) || math.IsInf(*g.Value, 0)):
		ch.v.add(CodeInvalid, ch.at+"/goal/value", "the target must be a finite number")
	case g.Column != "":
		ch.ref("goal/column", g.Column, false, true)
	}
}
