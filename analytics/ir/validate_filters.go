package ir

import (
	"fmt"
	"slices"

	"github.com/emoss08/keel/analytics/formula"
)

type scope int

const (
	scopeWhere scope = iota
	scopeHaving
	scopeMeasure
)

func (v *validator) filterTree(f *Filter, at string, sc scope, m *Measure) {
	if f == nil {
		return
	}
	conditions := 0
	v.filterNode(f, at, sc, m, 1, &conditions)
	if conditions > v.lim.MaxConditions {
		v.add(CodeLimit, at, "a filter can have at most %d conditions", v.lim.MaxConditions)
	}
}

func (v *validator) filterNode(f *Filter, at string, sc scope, m *Measure, depth int, conditions *int) {
	if !f.IsGroup() {
		*conditions++
		v.condition(f, at, sc, m)
		return
	}
	switch {
	case f.And != nil && f.Or != nil:
		v.add(CodeConflict, at, "a group is and or or, not both")
		return
	case f.Field != "" || f.Calc != nil || f.Column != "" || f.Op != "" || f.Value != nil || f.Param != "" || f.Transform != nil:
		v.add(CodeConflict, at, "a group holds conditions; it cannot also be one")
		return
	case depth > v.lim.MaxFilterDepth:
		v.add(CodeLimit, at, "filter groups can nest at most %d levels deep", v.lim.MaxFilterDepth)
		return
	}
	key := "and"
	if f.Or != nil {
		key = "or"
	}
	children := f.Children()
	for i := range children {
		v.filterNode(&children[i], ptr(at, key, i), sc, m, depth+1, conditions)
	}
}

type subject struct {
	typ    formula.Type
	enum   []string
	ok     bool
	isBool bool
}

func (v *validator) condition(f *Filter, at string, sc scope, m *Measure) {
	subjects := 0
	for _, set := range []bool{f.Field != "", f.Calc != nil, f.Column != ""} {
		if set {
			subjects++
		}
	}
	switch {
	case subjects == 0:
		v.add(CodeRequired, at, "a condition compares a field, a calc or a column")
		return
	case subjects > 1:
		v.add(CodeConflict, at, "a condition compares exactly one of a field, a calc or a column")
		return
	}
	var s subject
	switch {
	case f.Field != "":
		s = v.fieldSubject(f, at, sc, m)
	case f.Column != "":
		s = v.columnSubject(f, at, sc)
	default:
		s = v.calcSubject(f, at, sc, m)
	}
	if !s.ok {
		return
	}
	if f.Calc != nil && f.Op == "" && f.Value == nil && f.Param == "" {
		if !s.isBool {
			v.add(CodeType, at+"/calc", "a calc used as a condition must be true or false; add op and value to compare it")
		}
		return
	}
	v.comparison(f, at, sc, s)
}

func (v *validator) fieldSubject(f *Filter, at string, sc scope, m *Measure) subject {
	if sc == scopeHaving {
		v.add(CodeInvalid, at+"/field", "having compares output columns; add a measure column for %s and compare that", f.Field)
		return subject{}
	}
	rf, ok := v.field(at+"/field", f.Field)
	if !ok {
		return subject{}
	}
	if rf == nil {
		return subject{ok: true}
	}
	if !rf.Info.Filterable {
		v.add(CodeNotAllowed, at+"/field", "%s cannot be filtered on", f.Field)
		return subject{}
	}
	v.measureScope(rf, at+"/field", m)
	return subject{typ: rf.Info.Type, enum: rf.Info.Enum, ok: true, isBool: rf.Info.Type == formula.Bool}
}

func (v *validator) measureScope(rf *ResolvedField, at string, m *Measure) {
	if m == nil {
		return
	}
	mf := v.fields[m.Field]
	if mf == nil || !mf.ToMany || rf.Edges == mf.Edges {
		return
	}
	v.add(CodeInvalid, at, "this measure aggregates %s rows, so its filter can only use their fields (%s.…); move %s to the report filters", mf.Entity, mf.Edges, rf.Path)
}

func (v *validator) columnSubject(f *Filter, at string, sc scope) subject {
	if sc == scopeMeasure {
		v.add(CodeInvalid, at+"/column", "a measure filter compares fields, not columns")
		return subject{}
	}
	c, ok := v.refColumn(at+"/column", f.Column, "compared")
	if !ok {
		return subject{}
	}
	level := v.levelOf(c.ID)
	switch {
	case level == formula.Window:
		v.add(CodeLevel, at+"/column", "%q is a window column, which cannot be filtered; use top to keep the first rows", c.ID)
		return subject{}
	case sc == scopeWhere && level > formula.Row:
		v.add(CodeLevel, at+"/column", "%q is a value per group; compare it in having", c.ID)
		return subject{}
	case sc == scopeHaving && v.pivoted[c.ID]:
		v.add(CodeInvalid, at+"/column", "%q is spread across the pivot, so having cannot compare it", c.ID)
		return subject{}
	}
	t := v.typeOf(c.ID)
	return subject{typ: t, ok: true, isBool: t == formula.Bool || t == formula.Unknown}
}

func (v *validator) calcSubject(f *Filter, at string, sc scope, m *Measure) subject {
	tree, err := f.Calc.Tree(v.lim.Formula)
	if err != nil {
		v.formula(at+"/calc", err)
		return subject{}
	}
	typed, err := formula.Check(tree, &calcEnv{v: v, at: at + "/calc"})
	if err != nil {
		v.formula(at+"/calc", err)
		return subject{}
	}
	switch {
	case sc != scopeHaving && !typed.RowLevel():
		v.add(CodeLevel, at+"/calc", "this filter runs before grouping, so its calc must be a value per row; compare aggregates in having")
		return subject{}
	case sc == scopeHaving && typed.Level == formula.Window:
		v.add(CodeLevel, at+"/calc", "having runs before window columns are computed, so its calc cannot use them")
		return subject{}
	case sc == scopeHaving && typed.Level == formula.Row && len(tree.References(formula.KindField)) > 0:
		v.add(CodeLevel, at+"/calc", "having compares groups, so fields must be aggregated, as in sum({%s})", tree.References(formula.KindField)[0])
		return subject{}
	}
	if m != nil {
		for _, path := range tree.References(formula.KindField) {
			if rf := v.fields[path]; rf != nil {
				v.measureScope(rf, at+"/calc", m)
			}
		}
	}
	v.conds[at] = typed
	return subject{typ: typed.Type, ok: true, isBool: typed.Type == formula.Bool || typed.Type == formula.Unknown}
}

func (v *validator) comparison(f *Filter, at string, sc scope, s subject) {
	info, known := f.Op.Info()
	switch {
	case f.Op == "":
		v.add(CodeRequired, at+"/op", "choose how the value is compared")
		return
	case !known:
		v.unknown(CodeInvalid, at+"/op", "operator", string(f.Op), names(allOps()))
		return
	case sc == scopeHaving && !slices.Contains(opsAggregate, f.Op):
		v.add(CodeInvalid, at+"/op", "having compares with eq, ne, gt, gte, lt, lte, between, in, not_in, is_null and is_not_null")
		return
	case !opAllowed(f.Op, s.typ):
		v.add(CodeType, at+"/op", "%s does not apply to %s values; use one of %s", f.Op, s.typ, quoteAll(names(OpsFor(s.typ))))
		return
	}
	v.conditionTransform(f, at, sc, s.typ)
	v.operand(f, at, info.Operand, s)
}

func (v *validator) conditionTransform(f *Filter, at string, sc scope, t formula.Type) {
	if f.Transform == nil {
		return
	}
	if sc == scopeHaving {
		v.add(CodeInvalid, at+"/transform", "having compares the value itself and cannot carry a transform")
		return
	}
	v.transformShape(f.Transform, at+"/transform")
	if isOneOf(f.Transform.Op, TransformOps()) {
		v.transformType(f.Transform, t, at+"/transform")
	}
}

func (v *validator) operand(f *Filter, at string, operand Operand, s subject) {
	if operand == OperandNone {
		if f.Value != nil || f.Param != "" {
			v.add(CodeConflict, at, "%s takes no value", f.Op)
		}
		return
	}
	switch {
	case f.Value != nil && f.Param != "":
		v.add(CodeConflict, at, "a condition takes a value or a parameter, not both")
	case f.Param != "":
		v.paramOperand(f, at, operand, s.typ)
	case f.Value == nil:
		v.add(CodeRequired, at+"/value", "%s needs a value", f.Op)
	default:
		v.literalOperand(f, at+"/value", operand, s)
	}
}

func (v *validator) paramOperand(f *Filter, at string, operand Operand, t formula.Type) {
	v.paramUse(at+"/param", f.Param, func(p *Parameter) string {
		multi := operand == OperandList || operand == OperandRange
		switch {
		case multi && !p.Multi:
			return fmt.Sprintf("a multi-value parameter (for %s)", f.Op)
		case !multi && p.Multi:
			return fmt.Sprintf("a single value (for %s)", f.Op)
		case operand == OperandDays && p.Type != formula.Int:
			return "an int day count"
		case operand == OperandText && !p.Type.Textual():
			return "a string pattern"
		case operand != OperandDays && operand != OperandText && !compatible(p.Type, t):
			return fmt.Sprintf("a %s value", t)
		}
		return ""
	})
}

func (v *validator) literalOperand(f *Filter, at string, operand Operand, s subject) {
	list, isList := asList(f.Value)
	switch operand {
	case OperandText:
		if _, ok := f.Value.(string); !ok {
			v.add(CodeType, at, "%s compares with text", f.Op)
		}
	case OperandDays:
		if n, ok := asInt(f.Value); !ok || n < 1 || n > MaxDays {
			v.add(CodeInvalid, at, "the number of days must be a whole number from 1 to %d", MaxDays)
		}
	case OperandList:
		if !isList || len(list) == 0 {
			v.add(CodeType, at, "%s takes a non-empty list of values", f.Op)
			return
		}
		v.values(list, at, s, true)
	case OperandRange:
		if !isList || len(list) != 2 {
			v.add(CodeType, at, "between takes [from, to]")
			return
		}
		v.values(list, at, s, true)
	default:
		v.values([]any{f.Value}, at, s, false)
	}
}

func (v *validator) values(list []any, at string, s subject, indexed bool) {
	for i, x := range list {
		p := at
		if indexed {
			p = ptr(at, i)
		}
		if msg := valueFits(s.typ, x); msg != "" {
			v.add(CodeType, p, "%s", msg)
			continue
		}
		if str, ok := x.(string); ok && len(s.enum) > 0 && !slices.Contains(s.enum, str) {
			v.unknown(CodeInvalid, p, "value", str, s.enum)
		}
	}
}
