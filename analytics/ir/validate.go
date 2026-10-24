package ir

import (
	"slices"

	"github.com/emoss08/keel/analytics/formula"
)

// Limits bound what a definition may ask for. A zero field takes its default.
type Limits struct {
	// MaxColumns caps the columns (default 64).
	MaxColumns int
	// MaxDimensions caps the grouping columns of an aggregated report (default 16).
	MaxDimensions int
	// MaxLimit caps Limit (default 100,000).
	MaxLimit int
	// MaxPageSize caps a page (default 1,000).
	MaxPageSize int
	// MaxPivotValues caps listed or discovered pivot values (default 50).
	MaxPivotValues int
	// MaxPivotColumns caps the columns a pivot produces (default 120).
	MaxPivotColumns int
	// MaxWindows caps the window columns (default 8).
	MaxWindows int
	// MaxTop caps top-N (default 1,000).
	MaxTop int
	// MaxFilterDepth caps the nesting of filter groups (default 6).
	MaxFilterDepth int
	// MaxConditions caps the conditions of one filter tree (default 100).
	MaxConditions int
	// MaxPathDepth caps the relations a field path crosses (default 4).
	MaxPathDepth int
	// MaxParameters caps the parameters (default 32).
	MaxParameters int
	// MaxSort caps the sort columns (default 8).
	MaxSort int
	// MaxSubtotals caps the subtotal levels (default 4).
	MaxSubtotals int
	// MaxWindowSize caps a moving average's rows (default 366).
	MaxWindowSize int
	// MaxOffset caps a change's offset (default 366).
	MaxOffset int
	// Formula bounds each formula.
	Formula formula.Limits
}

// DefaultLimits returns the limits a zero Limits stands for.
func DefaultLimits() Limits {
	return Limits{
		MaxColumns:      64,
		MaxDimensions:   16,
		MaxLimit:        100_000,
		MaxPageSize:     1_000,
		MaxPivotValues:  50,
		MaxPivotColumns: 120,
		MaxWindows:      8,
		MaxTop:          1_000,
		MaxFilterDepth:  6,
		MaxConditions:   100,
		MaxPathDepth:    4,
		MaxParameters:   32,
		MaxSort:         8,
		MaxSubtotals:    4,
		MaxWindowSize:   366,
		MaxOffset:       366,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	return Limits{
		MaxColumns:      pick(l.MaxColumns, d.MaxColumns),
		MaxDimensions:   pick(l.MaxDimensions, d.MaxDimensions),
		MaxLimit:        pick(l.MaxLimit, d.MaxLimit),
		MaxPageSize:     pick(l.MaxPageSize, d.MaxPageSize),
		MaxPivotValues:  pick(l.MaxPivotValues, d.MaxPivotValues),
		MaxPivotColumns: pick(l.MaxPivotColumns, d.MaxPivotColumns),
		MaxWindows:      pick(l.MaxWindows, d.MaxWindows),
		MaxTop:          pick(l.MaxTop, d.MaxTop),
		MaxFilterDepth:  pick(l.MaxFilterDepth, d.MaxFilterDepth),
		MaxConditions:   pick(l.MaxConditions, d.MaxConditions),
		MaxPathDepth:    pick(l.MaxPathDepth, d.MaxPathDepth),
		MaxParameters:   pick(l.MaxParameters, d.MaxParameters),
		MaxSort:         pick(l.MaxSort, d.MaxSort),
		MaxSubtotals:    pick(l.MaxSubtotals, d.MaxSubtotals),
		MaxWindowSize:   pick(l.MaxWindowSize, d.MaxWindowSize),
		MaxOffset:       pick(l.MaxOffset, d.MaxOffset),
		Formula:         l.Formula,
	}
}

// Output is a column as the report produces it: its value type and level.
type Output struct {
	// ID is the column id.
	ID string
	// Type is the value type the column returns.
	Type formula.Type
	// Level is row for grouping columns, aggregate for measures, window for windows.
	Level formula.Level
	// Hidden columns are computed but not returned.
	Hidden bool
}

// Grouped reports whether the column is a value per row, which groups an aggregated report.
func (o Output) Grouped() bool { return o.Level <= formula.Row }

// Resolved is a definition validated against a catalog, with everything the compiler needs typed.
type Resolved struct {
	// Definition is the validated definition.
	Definition *Definition
	// Columns are the outputs in column order, hidden columns included.
	Columns []Output
	// Fields holds every field path the definition references, by path.
	Fields map[string]*ResolvedField
	// Calcs holds the checked formula of every calc column, by column id.
	Calcs map[string]*formula.Typed
	// Conditions holds the checked formula of every calc condition, by JSON pointer.
	Conditions map[string]*formula.Typed
	// Aggregated is true when the report groups rows: it has a measure or an aggregate calc.
	Aggregated bool
}

// Output returns the output column with the given id.
func (r *Resolved) Output(id string) (Output, bool) {
	for _, o := range r.Columns {
		if o.ID == id {
			return o, true
		}
	}
	return Output{}, false
}

// Validate checks everything about a definition that needs no catalog: structure, ids,
// references between columns, formulas (with fields typed as unknown), parameter binding, charts
// and limits. The error is Errors.
func Validate(def *Definition, limits Limits) error {
	v := newValidator(def, nil, limits)
	v.run()
	if len(v.errs) > 0 {
		return v.errs
	}
	return nil
}

// Resolve validates a definition against a catalog: everything Validate checks, plus that
// entities, relations and fields exist (with suggestions), operators, aggregations, buckets,
// bands and transforms suit the field types, formulas type-check, and to-many paths are used
// only where they can be. The error is Errors.
func Resolve(def *Definition, cat Catalog, limits Limits) (*Resolved, error) {
	v := newValidator(def, cat, limits)
	v.run()
	if len(v.errs) > 0 {
		return nil, v.errs
	}
	return v.resolved(), nil
}

type colState struct {
	visiting bool
	done     bool
	typ      formula.Type
	level    formula.Level
}

type validator struct {
	collector
	def        *Definition
	cat        Catalog
	lim        Limits
	ids        map[string]int
	params     map[string]*Parameter
	state      map[string]*colState
	fields     map[string]*ResolvedField
	calcs      map[string]*formula.Typed
	conds      map[string]*formula.Typed
	trees      map[string]*formula.Node
	pivoted    map[string]bool
	aggregated bool
}

func newValidator(def *Definition, cat Catalog, limits Limits) *validator {
	return &validator{
		def:     def,
		cat:     cat,
		lim:     limits.withDefaults(),
		ids:     map[string]int{},
		params:  map[string]*Parameter{},
		state:   map[string]*colState{},
		fields:  map[string]*ResolvedField{},
		calcs:   map[string]*formula.Typed{},
		conds:   map[string]*formula.Typed{},
		trees:   map[string]*formula.Node{},
		pivoted: map[string]bool{},
	}
}

func (v *validator) run() {
	if v.def == nil {
		v.add(CodeRequired, "", "a definition is required")
		return
	}
	if !v.envelope() {
		return
	}
	v.parameters()
	v.columnShapes()
	if v.def.Pivot != nil {
		for _, id := range v.def.Pivot.Columns {
			v.pivoted[id] = true
		}
	}
	v.columnTypes()
	v.columnRules()
	v.filterTree(v.def.Filters, "/filters", scopeWhere, nil)
	v.having()
	v.groupings()
	v.top()
	v.pivot()
	v.sorts()
	v.limitAndPage()
	v.charts()
}

func (v *validator) resolved() *Resolved {
	r := &Resolved{Definition: v.def, Fields: v.fields, Calcs: v.calcs, Conditions: v.conds, Aggregated: v.aggregated}
	for _, c := range v.def.Columns {
		s := v.state[c.ID]
		r.Columns = append(r.Columns, Output{ID: c.ID, Type: s.typ, Level: s.level, Hidden: c.Hidden})
	}
	return r
}

func (v *validator) envelope() bool {
	d := v.def
	if d.Version != Version {
		v.add(CodeUnsupportedVersion, "/version", "version %d is not supported; this keel reads version %d (Decode upgrades older definitions)", d.Version, Version)
		return false
	}
	switch {
	case d.Entity == "":
		v.add(CodeRequired, "/entity", "choose the entity the report starts from")
		return false
	case !formula.ValidName(d.Entity):
		v.add(CodeInvalid, "/entity", "%q is not an entity key", d.Entity)
		return false
	case v.cat != nil && !v.cat.HasEntity(d.Entity):
		v.unknown(CodeUnknownEntity, "/entity", "entity", d.Entity, v.cat.EntityKeys())
		return false
	}
	if len(d.Columns) == 0 {
		v.add(CodeRequired, "/columns", "add at least one column")
		return false
	}
	if len(d.Columns) > v.lim.MaxColumns {
		v.add(CodeLimit, "/columns", "a report can have at most %d columns", v.lim.MaxColumns)
		return false
	}
	return true
}

func (v *validator) columnIDs() []string {
	ids := make([]string, 0, len(v.def.Columns))
	for _, c := range v.def.Columns {
		ids = append(ids, c.ID)
	}
	return ids
}

func (v *validator) paramNames() []string {
	names := make([]string, 0, len(v.def.Parameters))
	for _, p := range v.def.Parameters {
		names = append(names, p.Name)
	}
	return names
}

func (v *validator) column(id string) (*Column, bool) {
	i, ok := v.ids[id]
	if !ok {
		return nil, false
	}
	return &v.def.Columns[i], true
}

func (v *validator) refColumn(p, id, role string) (*Column, bool) {
	if id == "" {
		v.add(CodeRequired, p, "choose the %s column", role)
		return nil, false
	}
	c, ok := v.column(id)
	if !ok {
		v.unknown(CodeUnknownColumn, p, "column", id, v.columnIDs())
	}
	return c, ok
}

func (v *validator) levelOf(id string) formula.Level {
	if s, ok := v.state[id]; ok {
		return s.level
	}
	return formula.Row
}

func (v *validator) typeOf(id string) formula.Type {
	if s, ok := v.state[id]; ok {
		return s.typ
	}
	return formula.Unknown
}

func (v *validator) distinct(p string, ids []string, what string) bool {
	seen := map[string]bool{}
	for i, id := range ids {
		if seen[id] {
			v.add(CodeDuplicate, ptr(p, i), "%q is listed twice in %s", id, what)
			return false
		}
		seen[id] = true
	}
	return true
}

func (v *validator) count(p string, c Count, lo, hi int, what string) {
	if c.Param != "" {
		v.paramUse(p, c.Param, func(prm *Parameter) string {
			if prm.Type != formula.Int || prm.Multi {
				return "a single int"
			}
			return ""
		})
		return
	}
	if c.Value < lo || c.Value > hi {
		v.add(CodeInvalid, p, "%s must be from %d to %d", what, lo, hi)
	}
}

func (v *validator) paramUse(p, name string, want func(*Parameter) string) (*Parameter, bool) {
	prm, ok := v.params[name]
	if !ok {
		v.unknown(CodeUnknownParameter, p, "parameter", name, v.paramNames())
		return nil, false
	}
	if need := want(prm); need != "" {
		v.add(CodeType, p, "parameter %q is used where %s is needed", name, need)
		return prm, false
	}
	return prm, true
}

func isOneOf[T comparable](v T, all []T) bool { return slices.Contains(all, v) }
