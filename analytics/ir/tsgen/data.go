package tsgen

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/ir/internal/schemagen"
)

type transformRule struct {
	Op            ir.TransformOp `json:"op"`
	Text          bool           `json:"text"`
	UsesPrecision bool           `json:"usesPrecision"`
}

type limits struct {
	MaxCharts             int `json:"maxCharts"`
	MaxChartSeries        int `json:"maxChartSeries"`
	MaxChartCategories    int `json:"maxChartCategories"`
	MaxTransformPrecision int `json:"maxTransformPrecision"`
	MaxBandEdges          int `json:"maxBandEdges"`
	MaxDays               int `json:"maxDays"`
	MaxDrillColumns       int `json:"maxDrillColumns"`
	MaxFormulaDigits      int `json:"maxFormulaDigits"`
}

type metaType struct {
	name string
	typ  reflect.Type
}

func metaTypes() []metaType {
	return []metaType{
		{"OpInfo", reflect.TypeFor[ir.OpInfo]()},
		{"ChartRule", reflect.TypeFor[ir.ChartRule]()},
		{"TransformRule", reflect.TypeFor[transformRule]()},
		{"FormulaParam", reflect.TypeFor[formula.Param]()},
		{"FormulaSQL", reflect.TypeFor[formula.SQL]()},
		{"FormulaFunction", reflect.TypeFor[formula.Function]()},
		{"FormulaOperator", reflect.TypeFor[formula.Operator]()},
		{"Limits", reflect.TypeFor[limits]()},
	}
}

func (e *emitter) data(raw []byte) error {
	names := map[reflect.Type]string{}
	for _, m := range metaTypes() {
		names[m.typ] = m.name
	}
	for _, m := range metaTypes() {
		e.line("")
		e.line("export type %s = %s;", m.name, e.structTS(m.typ, names))
	}
	var rules []transformRule
	for _, op := range ir.TransformOps() {
		rules = append(rules, transformRule{Op: op, Text: op.Text(), UsesPrecision: op.UsesPrecision()})
	}
	byType := map[string][]ir.Op{}
	for t, ops := range ir.OpTable() {
		byType[string(t)] = ops
	}
	consts := []struct {
		name, typ string
		value     any
	}{
		{"OPERATORS", "readonly OpInfo[]", ir.Ops()},
		{"OPERATORS_BY_TYPE", "Record<ValueType, readonly Op[]>", byType},
		{"CHART_RULES", "readonly ChartRule[]", ir.ChartRules()},
		{"TRANSFORM_RULES", "readonly TransformRule[]", rules},
		{"FORMULA_FUNCTIONS", "readonly FormulaFunction[]", formula.Functions()},
		{"FORMULA_OPERATORS", "readonly FormulaOperator[]", formula.Operators()},
		{"LIMITS", "Limits", limits{
			MaxCharts: ir.MaxCharts, MaxChartSeries: ir.MaxChartSeries, MaxChartCategories: ir.MaxChartCategories,
			MaxTransformPrecision: ir.MaxTransformPrecision, MaxBandEdges: ir.MaxBandEdges, MaxDays: ir.MaxDays,
			MaxDrillColumns: ir.MaxDrillColumns, MaxFormulaDigits: formula.MaxDigits,
		}},
		{"PIVOT_OTHER", "string", ir.PivotOther},
		{"PIVOT_SEPARATOR", "string", ir.PivotSeparator},
	}
	for _, c := range consts {
		out, err := schemagen.Marshal(c.value)
		if err != nil {
			return err
		}
		e.line("")
		e.line("export const %s: %s = %s;", c.name, c.typ, strings.TrimSuffix(string(out), "\n"))
	}
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return err
	}
	out, err := schemagen.Marshal(schema)
	if err != nil {
		return err
	}
	e.line("")
	e.line("export const DEFINITION_SCHEMA = %s as const;", strings.TrimSuffix(string(out), "\n"))
	return nil
}

func (e *emitter) structTS(t reflect.Type, names map[reflect.Type]string) string {
	type field struct{ name, ts string }
	var fields []field
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		mark := ""
		if strings.Contains(opts, "omitempty") {
			mark = "?"
		}
		fields = append(fields, field{name + mark, e.goTS(f.Type, names)})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
	var b strings.Builder
	b.WriteString("{\n")
	for _, f := range fields {
		b.WriteString("  " + f.name + ": " + f.ts + ";\n")
	}
	b.WriteString("}")
	return b.String()
}

func (e *emitter) goTS(t reflect.Type, names map[reflect.Type]string) string {
	if n, ok := e.byType[t]; ok {
		return n
	}
	if n, ok := names[t]; ok {
		return n
	}
	switch t.Kind() {
	case reflect.Pointer:
		return e.goTS(t.Elem(), names)
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int64, reflect.Float64:
		return "number"
	case reflect.Slice:
		return "readonly " + e.goTS(t.Elem(), names) + "[]"
	case reflect.Map:
		key := "string"
		switch {
		case t.Key().Kind() == reflect.Int:
			key = "number"
		case e.byType[t.Key()] != "":
			return "Partial<Record<" + e.byType[t.Key()] + ", " + e.goTS(t.Elem(), names) + ">>"
		}
		return "Record<" + key + ", " + e.goTS(t.Elem(), names) + ">"
	default:
		return "unknown"
	}
}

const applyFiltersTS = `
/** A dashboard filter with the value currently picked for it. */
export type FilterValue = { filter: DashboardFilter; value: unknown };

/** A dashboard's parameter values and a tile's bindings from report parameter names to dashboard parameter names. */
export type DashboardParams = { values?: Record<string, unknown>; bindings?: Record<string, string> };

function hasFilterValue(value: unknown): boolean {
  if (value === null || value === undefined) return false;
  if (typeof value === "string") return value !== "";
  if (Array.isArray(value)) return value.length > 0;
  return true;
}

function cloneFilter(filter: Filter): Filter {
  if (filter.and) return { and: filter.and.map(cloneFilter) };
  if (filter.or) return { or: filter.or.map(cloneFilter) };
  return { ...filter };
}

function overrideFilter(filter: Filter, picked: FilterValue): boolean {
  const children = filter.and ?? filter.or;
  if (children) {
    let matched = false;
    for (const child of children) {
      if (overrideFilter(child, picked)) matched = true;
    }
    return matched;
  }
  if (filter.param || !filter.field || filter.field !== picked.filter.field) return false;
  filter.op = picked.filter.op;
  filter.value = picked.value;
  delete filter.param;
  return true;
}

function resolveParams(
  definition: Definition,
  params: DashboardParams,
): Record<string, unknown> | undefined {
  const resolved: Record<string, unknown> = {};
  for (const parameter of definition.parameters ?? []) {
    const source = params.bindings?.[parameter.name] || parameter.name;
    const value = params.values?.[source];
    if (value !== undefined && value !== null) resolved[parameter.name] = value;
  }
  return Object.keys(resolved).length > 0 ? resolved : undefined;
}

/**
 * Folds a dashboard's filters and parameters into one tile's report, exactly as ir.ApplyFilters
 * does on the server. The input definition is never modified.
 */
export function applyFilters(
  definition: Definition,
  filters: readonly FilterValue[],
  params: DashboardParams = {},
): { definition: Definition; params?: Record<string, unknown> } {
  const resolved = resolveParams(definition, params);
  const active = filters.filter(
    (picked) => picked.filter.entity === definition.entity && hasFilterValue(picked.value),
  );
  if (active.length === 0) return { definition, params: resolved };
  const tree = definition.filters ? cloneFilter(definition.filters) : undefined;
  const added: Filter[] = [];
  for (const picked of active) {
    if (tree && overrideFilter(tree, picked)) continue;
    added.push({ field: picked.filter.field, op: picked.filter.op, value: picked.value });
  }
  if (added.length === 0) return { definition: { ...definition, filters: tree }, params: resolved };
  return {
    definition: { ...definition, filters: { and: tree ? [...added, tree] : added } },
    params: resolved,
  };
}
`
