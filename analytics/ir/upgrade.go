package ir

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/emoss08/keel/analytics/formula"
)

type v1Definition struct {
	IRVersion  int        `json:"irVersion"`
	Entity     string     `json:"entity"`
	Columns    []v1Column `json:"columns"`
	Filters    *v1Group   `json:"filters"`
	Having     *v1Group   `json:"having"`
	Sort       []v1Sort   `json:"sort"`
	Limit      int        `json:"limit"`
	Pivot      *v1Pivot   `json:"pivot"`
	Parameters []v1Param  `json:"parameters"`
	Totals     bool       `json:"totals"`
	Charts     []v1Chart  `json:"charts"`
}

type v1Ref struct {
	Path  []string `json:"path"`
	Field string   `json:"field"`
}

func (r v1Ref) path() string {
	if r.Field == "" {
		return ""
	}
	return strings.Join(append(append([]string{}, r.Path...), r.Field), ".")
}

type v1Column struct {
	ID        string          `json:"id"`
	Ref       v1Ref           `json:"ref"`
	Kind      string          `json:"kind"`
	Agg       Agg             `json:"agg"`
	Bucket    Bucket          `json:"bucket"`
	Label     string          `json:"label"`
	Band      *Band           `json:"band"`
	Computed  *v1Computed     `json:"computed"`
	Transform *Transform      `json:"transform"`
	Display   json.RawMessage `json:"display"`
	Filter    *v1Group        `json:"filter"`
}

type v1Computed struct {
	Op         string       `json:"op"`
	LeftID     string       `json:"leftId"`
	RightID    string       `json:"rightId"`
	LeftValue  *json.Number `json:"leftValue"`
	RightValue *json.Number `json:"rightValue"`
	Format     string       `json:"format"`
}

type v1Group struct {
	Op      string     `json:"op"`
	Filters []v1Filter `json:"filters"`
	Groups  []v1Group  `json:"groups"`
}

type v1Filter struct {
	Ref       v1Ref      `json:"ref"`
	Operator  string     `json:"operator"`
	Value     any        `json:"value"`
	Param     string     `json:"param"`
	Agg       Agg        `json:"agg"`
	Transform *Transform `json:"transform"`
}

type v1Sort struct {
	ColumnID  string `json:"columnId"`
	Direction string `json:"direction"`
}

type v1Pivot struct {
	Ref          v1Ref    `json:"ref"`
	Values       []string `json:"values"`
	Labels       []string `json:"labels"`
	MeasureIDs   []string `json:"measureIds"`
	IncludeOther bool     `json:"includeOther"`
}

type v1Param struct {
	Name          string       `json:"name"`
	Label         string       `json:"label"`
	Type          formula.Type `json:"type"`
	Required      bool         `json:"required"`
	Default       any          `json:"default"`
	Multi         bool         `json:"multi"`
	AllowedValues []string     `json:"allowedValues"`
	RefEntity     string       `json:"refEntity"`
}

type v1Chart struct {
	ID            string    `json:"id"`
	Type          ChartType `json:"type"`
	Title         string    `json:"title"`
	XColumnID     string    `json:"xColumnId"`
	SeriesIDs     []string  `json:"seriesIds"`
	Stacked       bool      `json:"stacked"`
	HideLegend    bool      `json:"hideLegend"`
	ShowValues    bool      `json:"showValues"`
	Curved        bool      `json:"curved"`
	Limit         int       `json:"limit"`
	CompareID     string    `json:"compareId"`
	Goal          *v1Goal   `json:"goal"`
	LatColumnID   string    `json:"latColumnId"`
	LngColumnID   string    `json:"lngColumnId"`
	LabelColumnID string    `json:"labelColumnId"`
}

type v1Goal struct {
	Value    *float64 `json:"value"`
	ColumnID string   `json:"columnId"`
	Label    string   `json:"label"`
}

var v1Operators = map[string]Op{
	"eq": OpEq, "ne": OpNe, "gt": OpGt, "gte": OpGte, "lt": OpLt, "lte": OpLte,
	"contains": OpContains, "startswith": OpStartsWith, "endswith": OpEndsWith, "like": OpLike, "ilike": OpILike,
	"in": OpIn, "notin": OpNotIn, "isnull": OpIsNull, "isnotnull": OpIsNotNull, "daterange": OpBetween,
	"lastndays": OpLastNDays, "nextndays": OpNextNDays, "today": OpToday, "yesterday": OpYesterday, "tomorrow": OpTomorrow,
	"thisweek": OpThisWeek, "lastweek": OpLastWeek, "thismonth": OpThisMonth, "lastmonth": OpLastMonth,
	"thisquarter": OpThisQuarter, "lastquarter": OpLastQuarter, "thisyear": OpThisYear, "lastyear": OpLastYear,
}

var v1ComputedOps = map[string]string{"add": "+", "subtract": "-", "multiply": "*", "divide": "/"}

// UpgradeFromTrenovaV1 converts a definition stored in Trenova's IR version 1 to this version:
// field references become dotted paths, computed specs become formulas, having conditions by
// field and aggregation become conditions on measure columns (adding hidden measure columns
// where the report has none to compare), totals become a total grouping, and pivots, parameters,
// display specs and charts carry over. Errors are Errors.
func UpgradeFromTrenovaV1(raw []byte) (Definition, error) {
	var old v1Definition
	if err := strictDecode(raw, &old); err != nil {
		return Definition{}, Errors{{Code: CodeJSON, Message: "not a Trenova IR v1 definition: " + err.Error()}}
	}
	if old.IRVersion != 1 {
		return Definition{}, Errors{{Code: CodeUnsupportedVersion, Message: fmt.Sprintf("Trenova IR version %d is not version 1", old.IRVersion), Pointer: "/irVersion"}}
	}
	u := &upgrader{old: &old, def: Definition{Version: Version, Entity: old.Entity}}
	u.columns()
	u.def.Filters = u.group(old.Filters, "/filters")
	u.def.Having = u.having(old.Having)
	u.layout()
	u.parameters()
	u.charts()
	if len(u.errs) > 0 {
		return Definition{}, u.errs
	}
	return u.def, nil
}

type upgrader struct {
	collector
	old *v1Definition
	def Definition
}

func (u *upgrader) columns() {
	for i, c := range u.old.Columns {
		at := ptr("/columns", i)
		col := Column{ID: c.ID, Label: c.Label, Transform: c.Transform, Display: c.Display}
		switch c.Kind {
		case "dimension":
			col.Dimension = &Dimension{Field: c.Ref.path(), Band: c.Band}
			if c.Bucket != "" {
				col.Dimension.Bucket = Every(c.Bucket)
			}
			u.noAgg(c, at)
		case "measure":
			col.Measure = &Measure{Field: c.Ref.path(), Agg: c.Agg, Filter: u.group(c.Filter, at+"/filter")}
		case "computed":
			col.Calc, col.Format = u.computed(c, at)
		default:
			u.add(CodeInvalid, at+"/kind", "unknown column kind %q", c.Kind)
		}
		u.def.Columns = append(u.def.Columns, col)
	}
}

func (u *upgrader) noAgg(c v1Column, at string) {
	if c.Agg != "" {
		u.add(CodeInvalid, at+"/agg", "dimension %q carries an aggregation", c.ID)
	}
}

func (u *upgrader) computed(c v1Column, at string) (*formula.Expr, string) {
	comp := c.Computed
	if comp == nil {
		u.add(CodeRequired, at+"/computed", "computed column %q has no computed spec", c.ID)
		return nil, ""
	}
	op, ok := v1ComputedOps[comp.Op]
	if !ok {
		u.add(CodeInvalid, at+"/computed/op", "unknown computed operator %q", comp.Op)
		return nil, ""
	}
	left := u.operand(comp.LeftID, comp.LeftValue, at+"/computed/left")
	right := u.operand(comp.RightID, comp.RightValue, at+"/computed/right")
	if left == nil || right == nil {
		return nil, ""
	}
	format := comp.Format
	if format == "none" {
		format = ""
	}
	return formula.Of(formula.Format(formula.Binary(op, left, right))), format
}

func (u *upgrader) operand(id string, value *json.Number, at string) *formula.Node {
	switch {
	case id != "" && value != nil:
		u.add(CodeConflict, at, "an operand is a column or a value, not both")
	case id != "":
		return formula.ColumnRef(id)
	case value != nil:
		f, err := strconv.ParseFloat(value.String(), 64)
		if err != nil {
			u.add(CodeInvalid, at, "%q is not a number", value.String())
			return nil
		}
		digits := strconv.FormatFloat(f, 'f', -1, 64)
		if strings.HasPrefix(digits, "-") {
			return formula.Unary("-", formula.Number(digits[1:]))
		}
		return formula.Number(digits)
	default:
		u.add(CodeRequired, at, "an operand needs a column or a value")
	}
	return nil
}

func (u *upgrader) group(g *v1Group, at string) *Filter {
	if g == nil || (len(g.Filters) == 0 && len(g.Groups) == 0) {
		return nil
	}
	children := make([]Filter, 0, len(g.Filters)+len(g.Groups))
	for i, f := range g.Filters {
		children = append(children, u.leaf(f, ptr(at, "filters", i)))
	}
	for i := range g.Groups {
		if nested := u.group(&g.Groups[i], ptr(at, "groups", i)); nested != nil {
			children = append(children, *nested)
		}
	}
	switch g.Op {
	case "or":
		return Or(children...)
	case "and", "":
		return And(children...)
	default:
		u.add(CodeInvalid, at+"/op", "unknown group operator %q", g.Op)
		return nil
	}
}

func (u *upgrader) op(name, at string) Op {
	op, ok := v1Operators[name]
	if !ok {
		u.add(CodeInvalid, at+"/operator", "operator %q has no equivalent", name)
	}
	return op
}

func (u *upgrader) leaf(f v1Filter, at string) Filter {
	if f.Agg != "" {
		u.add(CodeInvalid, at+"/agg", "a row filter carries an aggregation")
	}
	return Filter{Field: f.Ref.path(), Op: u.op(f.Operator, at), Value: f.Value, Param: f.Param, Transform: f.Transform}
}

func (u *upgrader) having(g *v1Group) *Filter {
	if g == nil || (len(g.Filters) == 0 && len(g.Groups) == 0) {
		return nil
	}
	return u.havingGroup(g, "/having")
}

func (u *upgrader) havingGroup(g *v1Group, at string) *Filter {
	var children []Filter
	for i, f := range g.Filters {
		p := ptr(at, "filters", i)
		children = append(children, Filter{Column: u.measureFor(f.Ref.path(), f.Agg), Op: u.op(f.Operator, p), Value: f.Value, Param: f.Param})
	}
	for i := range g.Groups {
		if len(g.Groups[i].Filters) > 0 || len(g.Groups[i].Groups) > 0 {
			children = append(children, *u.havingGroup(&g.Groups[i], ptr(at, "groups", i)))
		}
	}
	if g.Op == "or" {
		return Or(children...)
	}
	return And(children...)
}

func (u *upgrader) measureFor(path string, agg Agg) string {
	for _, c := range u.def.Columns {
		m := c.Measure
		if m != nil && m.Field == path && m.Agg == agg && m.Filter == nil && c.Transform == nil {
			return c.ID
		}
	}
	base := "having_" + string(agg) + "_" + strings.ReplaceAll(path, ".", "_")
	id := base
	for n := 2; u.hasColumn(id); n++ {
		id = base + "_" + strconv.Itoa(n)
	}
	u.def.Columns = append(u.def.Columns, Column{ID: id, Hidden: true, Measure: &Measure{Field: path, Agg: agg}})
	return id
}

func (u *upgrader) hasColumn(id string) bool {
	_, ok := u.def.ColumnByID(id)
	return ok
}

func (u *upgrader) layout() {
	old := u.old
	if old.Totals {
		u.def.Groupings = &Groupings{Total: true}
	}
	for _, s := range old.Sort {
		u.def.Sort = append(u.def.Sort, Sort{Column: s.ColumnID, Desc: s.Direction == "desc"})
	}
	if old.Limit != 0 {
		u.def.Limit = N(old.Limit)
	}
	if p := old.Pivot; p != nil {
		values := p.Values
		if values == nil {
			values = []string{}
		}
		u.def.Pivot = &Pivot{Field: p.Ref.path(), Values: Values{List: values}, Labels: p.Labels, Columns: p.MeasureIDs, Other: p.IncludeOther}
	}
}

func (u *upgrader) parameters() {
	for _, p := range u.old.Parameters {
		param := Parameter{Name: p.Name, Label: p.Label, Type: p.Type, Required: p.Required, Multi: p.Multi, Default: p.Default, Entity: p.RefEntity}
		for _, a := range p.AllowedValues {
			param.Allowed = append(param.Allowed, allowedValue(p.Type, a))
		}
		u.def.Parameters = append(u.def.Parameters, param)
	}
}

func allowedValue(t formula.Type, s string) any {
	if t.Numeric() {
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return json.Number(s)
		}
	}
	if t == formula.Bool {
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	}
	return s
}

func (u *upgrader) charts() {
	for _, c := range u.old.Charts {
		chart := Chart{
			ID: c.ID, Type: c.Type, Title: c.Title, X: c.XColumnID, Series: c.SeriesIDs,
			Stacked: c.Stacked && chartRule(c.Type).Stacking, HideLegend: c.HideLegend, ShowValues: c.ShowValues,
			Curved: c.Curved, Limit: c.Limit, Compare: c.CompareID, Lat: c.LatColumnID, Lng: c.LngColumnID, PointLabel: c.LabelColumnID,
		}
		if g := c.Goal; g != nil && (g.Value != nil || g.ColumnID != "") {
			chart.Goal = &Goal{Value: g.Value, Column: g.ColumnID, Label: g.Label}
		}
		u.def.Charts = append(u.def.Charts, chart)
	}
}
