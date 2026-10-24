package ir

import "reflect"

// DashboardFilter is a dashboard-wide condition on a field. It narrows every tile whose report
// starts from the same entity, without the report declaring anything.
type DashboardFilter struct {
	// ID names the filter; its value is supplied under this id.
	ID string `json:"id"`
	// Label is the filter's heading on the dashboard.
	Label string `json:"label,omitempty"`
	// Entity is the entity whose reports the filter narrows.
	Entity string `json:"entity"`
	// Field is the dotted path of the field compared.
	Field string `json:"field"`
	// Op is the comparison.
	Op Op `json:"op"`
	// Default is the value used before anyone picks one.
	Default any `json:"default,omitempty"`
}

// FilterValue is a dashboard filter with the value currently picked for it.
type FilterValue struct {
	// Filter is the dashboard filter.
	Filter DashboardFilter
	// Value is the value picked; nil, "" and an empty list pick nothing.
	Value any
}

// Params are a dashboard's parameter values and a tile's bindings from report parameter names to
// dashboard parameter names.
type Params struct {
	// Values are the dashboard parameter values by name.
	Values map[string]any
	// Bindings map a report parameter name to the dashboard parameter that feeds it.
	Bindings map[string]string
}

// ApplyFilters folds a dashboard's filters and parameters into one tile's report, as Trenova's
// dashboards do on the server and in the browser:
//   - a filter applies when it targets the report's entity and has a value (nil, "" and an empty
//     list mean "not filtering");
//   - a condition of the report on the same field, not bound to a parameter, takes the filter's
//     operator and value instead of being ANDed with it, wherever it is nested;
//   - every other filter is ANDed on top, with the report's own filter tree nested intact so its
//     and/or grouping keeps its meaning;
//   - each report parameter takes the dashboard value named by its binding, else the value of the
//     same name; parameters without a value are left out, and nil means none applied.
//
// The input definition is never modified.
func ApplyFilters(def *Definition, filters []FilterValue, params Params) (*Definition, map[string]any) {
	resolved := resolveParams(def, params)
	if def == nil {
		return nil, resolved
	}
	var active []FilterValue
	for _, f := range filters {
		if f.Filter.Entity == def.Entity && hasValue(f.Value) {
			active = append(active, f)
		}
	}
	if len(active) == 0 {
		return def, resolved
	}
	tree := def.Filters.Clone()
	var added []Filter
	for _, f := range active {
		if tree != nil && override(tree, f) {
			continue
		}
		added = append(added, Filter{Field: f.Filter.Field, Op: f.Filter.Op, Value: f.Value})
	}
	out := *def
	out.Filters = tree
	if len(added) > 0 {
		if tree != nil {
			added = append(added, *tree)
		}
		out.Filters = And(added...)
	}
	return &out, resolved
}

func override(f *Filter, fv FilterValue) bool {
	if !f.IsGroup() {
		if f.Param != "" || f.Field == "" || f.Field != fv.Filter.Field {
			return false
		}
		f.Op, f.Value, f.Param = fv.Filter.Op, fv.Value, ""
		return true
	}
	matched := false
	children := f.Children()
	for i := range children {
		if override(&children[i], fv) {
			matched = true
		}
	}
	return matched
}

func hasValue(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return s != ""
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		return rv.Len() > 0
	}
	return true
}

func resolveParams(def *Definition, params Params) map[string]any {
	if def == nil || len(def.Parameters) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, p := range def.Parameters {
		source := p.Name
		if bound := params.Bindings[p.Name]; bound != "" {
			source = bound
		}
		if v, ok := params.Values[source]; ok && v != nil {
			out[p.Name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
