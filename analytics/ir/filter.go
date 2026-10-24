package ir

import (
	"encoding/json"
	"errors"

	"github.com/emoss08/keel/analytics/formula"
)

// Filter is a node of a filter tree: a group (And or Or, possibly empty) or a condition. A
// condition compares a subject, which is exactly one of Field, Calc or Column, with Op and a
// Value or a Param. A Calc with no Op is a condition when the formula is true.
type Filter struct {
	// And holds conditions that must all hold.
	And []Filter
	// Or holds conditions of which one must hold.
	Or []Filter
	// Field is the dotted path of the field compared.
	Field string
	// Calc is a row-level formula compared, or a boolean formula used as the condition.
	Calc *formula.Expr
	// Column is the output column compared; in Having, or a row-level column in Filters.
	Column string
	// Op is the comparison.
	Op Op
	// Value is the value compared with: a scalar, a list for in and not_in, [from, to] for
	// between, a day count for last_n_days and next_n_days.
	Value any
	// Param names the parameter that supplies the value instead.
	Param string
	// Transform rewrites the subject before it is compared.
	Transform *Transform
}

// And returns a group whose conditions must all hold.
func And(filters ...Filter) *Filter { return &Filter{And: nonNil(filters)} }

// Or returns a group of which one condition must hold.
func Or(filters ...Filter) *Filter { return &Filter{Or: nonNil(filters)} }

func nonNil(filters []Filter) []Filter {
	if filters == nil {
		return []Filter{}
	}
	return filters
}

// Where returns a condition comparing a field with a value.
func Where(field string, op Op, value any) Filter { return Filter{Field: field, Op: op, Value: value} }

// WhereParam returns a condition comparing a field with a parameter.
func WhereParam(field string, op Op, param string) Filter {
	return Filter{Field: field, Op: op, Param: param}
}

// IsGroup reports whether the node is an and or or group.
func (f *Filter) IsGroup() bool { return f.And != nil || f.Or != nil }

// Children returns a group's conditions.
func (f *Filter) Children() []Filter {
	if f.And != nil {
		return f.And
	}
	return f.Or
}

// Empty reports whether the tree holds no condition at all.
func (f *Filter) Empty() bool {
	if f == nil {
		return true
	}
	if !f.IsGroup() {
		return false
	}
	for i := range f.Children() {
		if !f.Children()[i].Empty() {
			return false
		}
	}
	return true
}

// Walk calls fn for every condition in the tree, depth first.
func (f *Filter) Walk(fn func(*Filter)) {
	if f == nil {
		return
	}
	if !f.IsGroup() {
		fn(f)
		return
	}
	children := f.Children()
	for i := range children {
		children[i].Walk(fn)
	}
}

// Clone returns a deep copy of the tree; values are shared.
func (f *Filter) Clone() *Filter {
	if f == nil {
		return nil
	}
	c := *f
	c.And = cloneFilters(f.And)
	c.Or = cloneFilters(f.Or)
	return &c
}

func cloneFilters(in []Filter) []Filter {
	if in == nil {
		return nil
	}
	out := make([]Filter, len(in))
	for i := range in {
		out[i] = *in[i].Clone()
	}
	return out
}

type filterJSON struct {
	And       []Filter      `json:"and,omitempty"`
	Or        []Filter      `json:"or,omitempty"`
	Field     string        `json:"field,omitempty"`
	Calc      *formula.Expr `json:"calc,omitempty"`
	Column    string        `json:"column,omitempty"`
	Op        Op            `json:"op,omitempty"`
	Value     any           `json:"value,omitempty"`
	Param     string        `json:"param,omitempty"`
	Transform *Transform    `json:"transform,omitempty"`
}

// MarshalJSON encodes a group as {"and": […]} or {"or": […]} and a condition by its keys.
func (f Filter) MarshalJSON() ([]byte, error) {
	switch {
	case f.And != nil:
		return json.Marshal(map[string][]Filter{"and": f.And})
	case f.Or != nil:
		return json.Marshal(map[string][]Filter{"or": f.Or})
	}
	return json.Marshal(filterJSON{Field: f.Field, Calc: f.Calc, Column: f.Column, Op: f.Op, Value: f.Value, Param: f.Param, Transform: f.Transform})
}

// UnmarshalJSON decodes a group or a condition, rejecting unknown keys and keeping numbers exact.
func (f *Filter) UnmarshalJSON(data []byte) error {
	var j filterJSON
	if err := strictDecode(data, &j); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, hasAnd := raw["and"]
	_, hasOr := raw["or"]
	if (hasAnd || hasOr) && len(raw) > 1 {
		return errors.New("a filter is a group ({\"and\": […]} or {\"or\": […]}) or a condition, not both")
	}
	*f = Filter{Field: j.Field, Calc: j.Calc, Column: j.Column, Op: j.Op, Value: j.Value, Param: j.Param, Transform: j.Transform}
	switch {
	case hasAnd:
		f.And = nonNil(j.And)
	case hasOr:
		f.Or = nonNil(j.Or)
	}
	return nil
}
