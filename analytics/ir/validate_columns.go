package ir

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/emoss08/keel/analytics/formula"
)

const maxIDLength = 64

func (v *validator) columnShapes() {
	visible := 0
	for i := range v.def.Columns {
		c := &v.def.Columns[i]
		at := ptr("/columns", i)
		switch {
		case c.ID == "":
			v.add(CodeRequired, at+"/id", "give the column an id")
			continue
		case !formula.ValidColumnID(c.ID) || len(c.ID) > maxIDLength:
			v.add(CodeInvalid, at+"/id", "%q is not a column id; use letters, digits and underscores, at most %d", c.ID, maxIDLength)
			continue
		}
		if _, dup := v.ids[c.ID]; dup {
			v.add(CodeDuplicate, at+"/id", "column id %q is used twice", c.ID)
			continue
		}
		v.ids[c.ID] = i
		if !c.Hidden {
			visible++
		}
		v.columnShape(c, at)
	}
	if visible == 0 && len(v.ids) > 0 {
		v.add(CodeInvalid, "/columns", "at least one column must be visible")
	}
}

func (v *validator) columnShape(c *Column, at string) {
	kinds := 0
	for _, set := range []bool{c.Dimension != nil, c.Measure != nil, c.Calc != nil, c.Window != nil} {
		if set {
			kinds++
		}
	}
	switch {
	case kinds == 0:
		v.add(CodeRequired, at, "a column is a dimension, a measure, a calc or a window; give it one")
	case kinds > 1:
		v.add(CodeConflict, at, "a column is exactly one of a dimension, a measure, a calc or a window")
	}
	if len(c.Label) > maxLabel {
		v.add(CodeLimit, at+"/label", "a label can be at most %d characters", maxLabel)
	}
	if len(c.Format) > maxIDLength || (c.Format != "" && !formula.ValidName(c.Format)) {
		v.add(CodeInvalid, at+"/format", "%q is not a format hint", c.Format)
	}
	if t := bytes.TrimSpace(c.Display); len(t) > 0 && t[0] != '{' {
		v.add(CodeInvalid, at+"/display", "a display spec is an object")
	}
	v.transformShape(c.Transform, at+"/transform")
}

func (v *validator) transformShape(t *Transform, at string) {
	if t == nil {
		return
	}
	if !isOneOf(t.Op, TransformOps()) {
		v.unknown(CodeInvalid, at+"/op", "transform", string(t.Op), names(TransformOps()))
		return
	}
	if t.Precision != nil {
		switch {
		case !t.Op.UsesPrecision():
			v.add(CodeConflict, at+"/precision", "%s does not take a precision", t.Op)
		case *t.Precision < 0 || *t.Precision > MaxTransformPrecision:
			v.add(CodeInvalid, at+"/precision", "precision must be from 0 to %d", MaxTransformPrecision)
		}
	}
	switch {
	case t.Op == TransformScale && t.Factor == nil:
		v.add(CodeRequired, at+"/factor", "scale needs a factor")
	case t.Op == TransformScale && (*t.Factor == 0 || math.IsNaN(*t.Factor) || math.IsInf(*t.Factor, 0)):
		v.add(CodeInvalid, at+"/factor", "the scale factor must be a non-zero finite number")
	case t.Op != TransformScale && t.Factor != nil:
		v.add(CodeConflict, at+"/factor", "%s does not take a factor", t.Op)
	}
}

func names[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, x := range values {
		out[i] = string(x)
	}
	return out
}

func (v *validator) field(at, path string) (*ResolvedField, bool) {
	if path == "" {
		v.add(CodeRequired, at, "choose a field")
		return nil, false
	}
	if !formula.ValidPath(path) {
		v.add(CodeInvalid, at, "%q is not a field path; write edges and the field separated by dots, such as customer.name", path)
		return nil, false
	}
	if strings.Count(path, ".") > v.lim.MaxPathDepth {
		v.add(CodeLimit, at, "%q crosses more than %d relations", path, v.lim.MaxPathDepth)
		return nil, false
	}
	if v.cat == nil {
		return nil, true
	}
	if f, ok := v.fields[path]; ok {
		return f, true
	}
	f, perr := resolvePath(v.cat, v.def.Entity, path)
	if perr != nil {
		e := v.add(CodeUnknownField, at, "%s", perr.message)
		suggest(e, perr.name, perr.candidates)
		return nil, false
	}
	v.fields[path] = f
	return f, true
}

func (v *validator) fieldType(path string) (formula.Type, []string, bool) {
	if !formula.ValidPath(path) || strings.Count(path, ".") > v.lim.MaxPathDepth {
		return formula.Unknown, nil, false
	}
	if v.cat == nil {
		return formula.Unknown, nil, true
	}
	if f, ok := v.fields[path]; ok {
		return f.Info.Type, nil, true
	}
	f, perr := resolvePath(v.cat, v.def.Entity, path)
	if perr != nil {
		return formula.Unknown, perr.candidates, false
	}
	v.fields[path] = f
	return f.Info.Type, nil, true
}

func (v *validator) columnTypes() {
	for _, c := range v.def.Columns {
		if _, ok := v.ids[c.ID]; ok {
			v.resolveColumn(c.ID)
		}
	}
	for _, c := range v.def.Columns {
		if v.levelOf(c.ID) == formula.Aggregate {
			v.aggregated = true
		}
	}
}

func (v *validator) resolveColumn(id string) (formula.Type, formula.Level) {
	s := v.state[id]
	if s == nil {
		s = &colState{}
		v.state[id] = s
	}
	if s.done || s.visiting {
		return s.typ, s.level
	}
	s.visiting = true
	i := v.ids[id]
	c := &v.def.Columns[i]
	at := ptr("/columns", i)
	t, level := v.kindType(c, at)
	if c.Transform != nil && isOneOf(c.Transform.Op, TransformOps()) {
		v.transformType(c.Transform, t, at+"/transform")
		if t != formula.Unknown {
			t = c.Transform.Op.ResultType(t)
		}
	}
	s.typ, s.level, s.visiting, s.done = t, level, false, true
	return t, level
}

func (v *validator) kindType(c *Column, at string) (formula.Type, formula.Level) {
	switch {
	case c.Dimension != nil:
		return v.dimension(c.Dimension, at+"/dimension"), formula.Row
	case c.Measure != nil:
		return v.measure(c.Measure, at+"/measure"), formula.Aggregate
	case c.Calc != nil:
		return v.calc(c, at+"/calc")
	case c.Window != nil:
		return v.window(c, at+"/window"), formula.Window
	default:
		return formula.Unknown, formula.Row
	}
}

func (v *validator) transformType(t *Transform, in formula.Type, at string) {
	switch {
	case in == formula.Unknown:
	case t.Op.Text() && !in.Textual():
		v.add(CodeType, at+"/op", "%s applies to text, and this column is %s", t.Op, in)
	case !t.Op.Text() && !in.Numeric():
		v.add(CodeType, at+"/op", "%s applies to numbers, and this column is %s", t.Op, in)
	}
}

func (v *validator) dimension(d *Dimension, at string) formula.Type {
	f, ok := v.field(at+"/field", d.Field)
	t := formula.Unknown
	if ok && f != nil {
		t = f.Info.Type
	}
	if !d.Bucket.IsZero() && d.Band != nil {
		v.add(CodeConflict, at+"/band", "a dimension is bucketed in time or banded into ranges, not both")
	}
	v.bucket(d.Bucket, t, at+"/bucket")
	v.band(d.Band, t, at+"/band")
	return t
}

func (v *validator) bucket(g Grain, t formula.Type, at string) {
	switch {
	case g.IsZero():
		return
	case g.Param != "" && g.Unit != "":
		v.add(CodeConflict, at, "a bucket is a period or a parameter, not both")
	case g.Param != "":
		v.paramUse(at, g.Param, func(p *Parameter) string {
			if p.Type != formula.String || p.Multi || !bucketValues(p) {
				return "a single string whose values are buckets (" + strings.Join(names(Buckets()), ", ") + ")"
			}
			return ""
		})
	case !isOneOf(g.Unit, Buckets()):
		v.unknown(CodeInvalid, at, "bucket", string(g.Unit), names(Buckets()))
	}
	if t != formula.Unknown && !t.Temporal() {
		v.add(CodeType, at, "only dates and times are bucketed, and this field is %s", t)
	}
}

func bucketValues(p *Parameter) bool {
	values := append([]any{}, p.Allowed...)
	if p.Default != nil {
		values = append(values, p.Default)
	}
	for _, x := range values {
		s, ok := x.(string)
		if !ok || !isOneOf(Bucket(s), Buckets()) {
			return false
		}
	}
	return true
}

func (v *validator) band(b *Band, t formula.Type, at string) {
	if b == nil {
		return
	}
	if err := b.Validate(); err != nil {
		v.add(CodeInvalid, at, "%s", err.Error())
		return
	}
	switch {
	case t == formula.Unknown:
	case !t.Numeric():
		v.add(CodeType, at, "only numbers are banded into ranges, and this field is %s", t)
	case t == formula.Int && !b.whole():
		v.add(CodeInvalid, at, "this field holds whole numbers, so its range boundaries must be whole numbers too")
	}
}

// Validate reports why the band is malformed, or nil.
func (b *Band) Validate() error {
	if b.Width != 0 && len(b.Edges) > 0 {
		return errors.New("a band takes a fixed width or a list of boundaries, not both")
	}
	if b.Width != 0 {
		if b.Width < 0 || math.IsInf(b.Width, 0) || math.IsNaN(b.Width) {
			return errors.New("the band width must be a positive, finite number")
		}
		return nil
	}
	if len(b.Edges) < 2 {
		return errors.New("a band needs a width or at least two boundaries")
	}
	if len(b.Edges) > MaxBandEdges {
		return fmt.Errorf("a band can have at most %d boundaries", MaxBandEdges)
	}
	for i, e := range b.Edges {
		if math.IsNaN(e) || math.IsInf(e, 0) {
			return errors.New("band boundaries must be finite numbers")
		}
		if i > 0 && e <= b.Edges[i-1] {
			return errors.New("band boundaries must ascend, with no repeats")
		}
	}
	return nil
}

// MaxBandEdges is the most boundaries a band can have.
const MaxBandEdges = 24

func (b *Band) whole() bool {
	if b.Width != math.Trunc(b.Width) {
		return false
	}
	for _, e := range b.Edges {
		if e != math.Trunc(e) {
			return false
		}
	}
	return true
}

func (v *validator) measure(m *Measure, at string) formula.Type {
	if !isOneOf(m.Agg, Aggs()) {
		if m.Agg == "" {
			v.add(CodeRequired, at+"/agg", "choose an aggregation")
		} else {
			v.unknown(CodeInvalid, at+"/agg", "aggregation", string(m.Agg), names(Aggs()))
		}
		return formula.Unknown
	}
	if m.Field == "" {
		if m.Agg != AggCount {
			v.add(CodeRequired, at+"/field", "choose the field to %s; only count works without one", m.Agg)
		}
		return formula.Int
	}
	f, ok := v.field(at+"/field", m.Field)
	if !ok {
		return formula.Unknown
	}
	in := formula.Unknown
	if f != nil {
		in = f.Info.Type
		if !f.Info.supports(m.Agg) {
			v.add(CodeNotAllowed, at+"/agg", "%s is not available for %s", m.Agg, m.Field)
			return formula.Unknown
		}
	}
	t, err := formula.CallType(string(m.Agg), in)
	if err != nil {
		v.add(CodeType, at+"/agg", "%s cannot aggregate %s, which is %s", m.Agg, m.Field, in)
		return formula.Unknown
	}
	return t
}

func (v *validator) calc(c *Column, at string) (formula.Type, formula.Level) {
	tree, err := c.Calc.Tree(v.lim.Formula)
	if err != nil {
		v.formula(at, err)
		return formula.Unknown, formula.Row
	}
	v.trees[c.ID] = tree
	typed, err := formula.Check(tree, &calcEnv{v: v, self: c.ID, at: at})
	if err != nil {
		v.formula(at, err)
		return formula.Unknown, typed.Level
	}
	v.calcs[c.ID] = typed
	if typed.Type == formula.Null {
		v.add(CodeType, at, "this calculation is always null")
	}
	return typed.Type, typed.Level
}

type calcEnv struct {
	v    *validator
	self string
	at   string
}

func (e *calcEnv) Column(id string) (formula.ColumnInfo, bool) {
	if _, ok := e.v.ids[id]; !ok {
		return formula.ColumnInfo{}, false
	}
	if s := e.v.state[id]; id == e.self || (s != nil && s.visiting) {
		e.v.add(CodeInvalid, e.at, "[%s] refers back to this column; calculations cannot form a cycle", id)
		return formula.ColumnInfo{Level: formula.Row}, true
	}
	if e.v.pivoted[id] && !e.v.pivoted[e.self] {
		e.v.add(CodeInvalid, e.at, "[%s] is spread across the pivot, so a calculation over it must be pivoted too", id)
	}
	t, level := e.v.resolveColumn(id)
	return formula.ColumnInfo{Type: t, Level: level}, true
}

func (e *calcEnv) ColumnIDs() []string { return e.v.columnIDs() }

func (e *calcEnv) Field(path string) (formula.Type, []string, bool) { return e.v.fieldType(path) }

func (e *calcEnv) Param(name string) (formula.ParamInfo, bool) {
	p, ok := e.v.params[name]
	if !ok {
		return formula.ParamInfo{}, false
	}
	return formula.ParamInfo{Type: p.Type, Multi: p.Multi}, true
}

func (e *calcEnv) ParamNames() []string { return e.v.paramNames() }

func (v *validator) window(c *Column, at string) formula.Type {
	w := c.Window
	if !isOneOf(w.Fn, WindowFns()) {
		v.unknown(CodeInvalid, at+"/fn", "window function", string(w.Fn), names(WindowFns()))
		return formula.Unknown
	}
	of, ok := v.refColumn(at+"/of", w.Of, "window's")
	if !ok {
		return formula.Unknown
	}
	if of.ID == c.ID || of.Window != nil {
		v.add(CodeInvalid, at+"/of", "a window reads a dimension, measure or calc column, not another window")
		return formula.Unknown
	}
	if v.pivoted[of.ID] {
		v.add(CodeInvalid, at+"/of", "%q is spread across the pivot; a window cannot read it", of.ID)
	}
	in, _ := v.resolveColumn(of.ID)
	return v.windowType(w, in, at)
}

func (v *validator) windowType(w *Window, in formula.Type, at string) formula.Type {
	if w.Fn.numeric() && in != formula.Unknown && !in.Numeric() {
		v.add(CodeType, at+"/of", "%s needs a numeric column, and %q is %s", w.Fn, w.Of, in)
		return formula.Unknown
	}
	switch w.Fn {
	case WindowRank, WindowDenseRank:
		return formula.Int
	case WindowPercentOfTotal, WindowPercentChange:
		return formula.Decimal
	case WindowMovingAvg:
		if in == formula.Float || in == formula.Unknown {
			return in
		}
		return formula.Decimal
	default:
		return in
	}
}
