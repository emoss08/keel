package analytics

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/schema"
)

// View is the catalog as one caller sees it: every entity with the caller's access, translated
// labels, and the access to each of its fields and edges. It is what a report builder offers and
// what an AI agent is told about.
type View struct {
	// Version is the catalog's version.
	Version string `json:"version"`
	// Entities are the catalog's entities, allowed or not, in catalog order.
	Entities []EntityView `json:"entities"`

	cat *Catalog
	dec *decisions
	ctx context.Context
}

// EntityView is one entity of a View.
type EntityView struct {
	Key      string      `json:"key"`
	Label    string      `json:"label"`
	Plural   string      `json:"plural,omitempty"`
	Doc      string      `json:"doc,omitempty"`
	Category string      `json:"category,omitempty"`
	Access   Access      `json:"access"`
	Fields   []FieldView `json:"fields"`
	Edges    []EdgeView  `json:"edges"`
}

// FieldView is one field of a View, with the operators a condition on it can use.
type FieldView struct {
	Key        string       `json:"key"`
	Label      string       `json:"label"`
	Doc        string       `json:"doc,omitempty"`
	Type       formula.Type `json:"type"`
	Measure    bool         `json:"measure,omitempty"`
	Aggs       []ir.Agg     `json:"aggs"`
	Operators  []ir.Op      `json:"operators"`
	Format     string       `json:"format,omitempty"`
	Enum       []EnumValue  `json:"enum,omitempty"`
	Filterable bool         `json:"filterable"`
	Groupable  bool         `json:"groupable"`
	Access     Access       `json:"access"`
}

// EdgeView is one edge of a View.
type EdgeView struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Doc    string `json:"doc,omitempty"`
	Target string `json:"target"`
	ToMany bool   `json:"toMany,omitempty"`
	Pick   bool   `json:"pick,omitempty"`
	Access Access `json:"access"`
}

// For returns the caller's view of the catalog: policy decides access (a nil policy allows
// everything) and Options.Translate the labels. Fields and edges are those of each entity itself;
// Field resolves a path through edges with the access the compiler checks for it.
func (c *Catalog) For(ctx context.Context, policy Policy) *View {
	v := &View{Version: c.version, cat: c, dec: newDecisions(c, policy), ctx: ctx}
	for _, e := range c.entities {
		ev := EntityView{Key: e.Key, Label: c.Label(ctx, e.Key, e.Label),
			Plural: c.Label(ctx, e.Key+".plural", e.Plural), Doc: e.Doc,
			Category: c.Label(ctx, "category."+e.Category, e.Category), Access: v.dec.entity(ctx, e)}
		for _, f := range e.Fields {
			ev.Fields = append(ev.Fields, v.fieldView(f, nil))
		}
		for _, g := range e.Edges {
			ev.Edges = append(ev.Edges, EdgeView{Name: g.Name, Label: c.Label(ctx, g.LabelKey, g.Label),
				Doc: g.Doc, Target: g.Target.Key, ToMany: g.ToMany, Pick: g.Pick,
				Access: v.dec.edge(ctx, g, nil)})
		}
		v.Entities = append(v.Entities, ev)
	}
	return v
}

// fieldView describes a field reached through via.
func (v *View) fieldView(f *Field, via Path) FieldView {
	c, ctx := v.cat, v.ctx
	fv := FieldView{Key: f.Key, Label: c.Label(ctx, f.LabelKey, f.Label), Doc: f.Doc, Type: f.Type,
		Measure: f.Kind == schema.Measure, Aggs: slices.Clone(f.Aggs), Operators: ir.OpsFor(f.Type),
		Format: f.Format, Filterable: f.Filterable, Groupable: f.Groupable, Access: v.dec.path(ctx, via, f)}
	for _, e := range f.Enum {
		e.Label = c.Label(ctx, e.LabelKey, e.Label)
		fv.Enum = append(fv.Enum, e)
	}
	return fv
}

// Entity returns the view of an entity.
func (v *View) Entity(key string) (*EntityView, bool) {
	for i := range v.Entities {
		if v.Entities[i].Key == key {
			return &v.Entities[i], true
		}
	}
	return nil, false
}

// Field resolves a dotted path from an entity and returns the field with the access the
// compiler checks for it: every edge on the path and the field, each reached through the edges
// before it.
func (v *View) Field(entity, path string) (FieldView, bool) {
	via, f, ok := v.cat.Resolve(entity, path)
	if !ok {
		return FieldView{}, false
	}
	return v.fieldView(f, via), true
}

// Describe returns a compact text description of the entities the caller may start from, their
// allowed fields and edges, for a language model's context: one line per entity and one per
// field, with types, aggregations and enum values.
func (v *View) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "catalog %s\n", v.Version)
	for _, e := range v.Entities {
		if !e.Access.Allowed {
			continue
		}
		fmt.Fprintf(&b, "entity %s: %s", e.Key, e.Label)
		if e.Category != "" {
			fmt.Fprintf(&b, " [%s]", e.Category)
		}
		if e.Doc != "" {
			fmt.Fprintf(&b, " — %s", oneLine(e.Doc))
		}
		b.WriteByte('\n')
		for _, f := range e.Fields {
			if f.Access.Allowed {
				describeField(&b, f)
			}
		}
		for _, g := range e.Edges {
			if !g.Access.Allowed {
				continue
			}
			many := ""
			if g.ToMany {
				many = " (many)"
			}
			fmt.Fprintf(&b, "  edge %s -> %s%s: %s\n", g.Name, g.Target, many, g.Label)
		}
	}
	return b.String()
}

// describeField writes one field line of Describe.
func describeField(b *strings.Builder, f FieldView) {
	fmt.Fprintf(b, "  %s %s", f.Key, f.Type)
	if len(f.Enum) > 0 {
		vals := make([]string, len(f.Enum))
		for i, e := range f.Enum {
			vals[i] = e.Value
		}
		fmt.Fprintf(b, "{%s}", strings.Join(vals, "|"))
	}
	if f.Measure {
		b.WriteString(" measure")
	}
	if len(f.Aggs) > 0 {
		aggs := make([]string, len(f.Aggs))
		for i, a := range f.Aggs {
			aggs[i] = string(a)
		}
		fmt.Fprintf(b, " agg=%s", strings.Join(aggs, ","))
	}
	if !f.Groupable {
		b.WriteString(" nogroup")
	}
	fmt.Fprintf(b, ": %s", f.Label)
	if f.Doc != "" {
		fmt.Fprintf(b, " — %s", oneLine(f.Doc))
	}
	b.WriteByte('\n')
}

// oneLine collapses a doc comment to one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Diagnostic is one problem a stored definition has against the current catalog and policy.
type Diagnostic struct {
	// Code classifies the problem: a definition error's code, or "denied".
	Code string `json:"code"`
	// Message explains it.
	Message string `json:"message"`
	// Pointer locates it in the definition.
	Pointer string `json:"pointer,omitempty"`
	// Reason is the policy's reason code for a denial.
	Reason string `json:"reason,omitempty"`
	// Suggestions are names close to an unknown one.
	Suggestions []string `json:"suggestions,omitempty"`
}

// Check re-validates a stored definition against the catalog and the policy and returns its
// problems, none when it still runs. A sweep over definitions whose catalog version differs from
// Version marks the ones that need attention.
func (c *Catalog) Check(ctx context.Context, def *ir.Definition, policy Policy) []Diagnostic {
	res, err := ir.Resolve(def, c, ir.Limits{})
	if err != nil {
		return diagnostics(err)
	}
	dec := newDecisions(c, policy)
	var out []Diagnostic
	for _, d := range accessProblems(ctx, c, dec, res) {
		out = append(out, Diagnostic{Code: string(CodeDenied), Message: d.Message, Pointer: d.Pointer,
			Reason: d.Reason})
	}
	return out
}

// diagnostics turns definition errors into diagnostics.
func diagnostics(err error) []Diagnostic {
	es, ok := err.(ir.Errors)
	if !ok {
		return []Diagnostic{{Code: string(CodeInvalid), Message: err.Error()}}
	}
	out := make([]Diagnostic, len(es))
	for i, e := range es {
		out[i] = Diagnostic{Code: string(e.Code), Message: e.Message, Pointer: e.Pointer,
			Suggestions: e.Suggestions}
	}
	return out
}

// accessProblems returns a denial for the entity and for every field path the definition reads
// that the policy does not allow.
func accessProblems(ctx context.Context, c *Catalog, dec *decisions, res *ir.Resolved) []*Error {
	var out []*Error
	e := c.byKey[res.Definition.Entity]
	if a := dec.entity(ctx, e); !a.Allowed {
		out = append(out, &Error{Code: CodeDenied, Message: "you cannot report on " + e.Key,
			Pointer: "/entity", Reason: a.Reason})
	}
	paths := make([]string, 0, len(res.Fields))
	for p := range res.Fields {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		via, f, ok := c.Resolve(res.Definition.Entity, p)
		if !ok {
			continue
		}
		if a := dec.path(ctx, via, f); !a.Allowed {
			out = append(out, &Error{Code: CodeDenied, Message: "you cannot read " + p, Reason: a.Reason,
				Pointer: pointerOf(res.Definition, p)})
		}
	}
	return out
}

// pointerOf finds the first place a definition references a field path, for a denial's pointer.
func pointerOf(def *ir.Definition, path string) string {
	for i, col := range def.Columns {
		switch {
		case col.Dimension != nil && col.Dimension.Field == path:
			return fmt.Sprintf("/columns/%d/dimension/field", i)
		case col.Measure != nil && col.Measure.Field == path:
			return fmt.Sprintf("/columns/%d/measure/field", i)
		case col.Calc != nil && slices.Contains(calcFields(col), path):
			return fmt.Sprintf("/columns/%d/calc", i)
		}
	}
	if def.Pivot != nil && def.Pivot.Field == path {
		return "/pivot/field"
	}
	return "/filters"
}

// calcFields returns the field paths a calc column references.
func calcFields(col ir.Column) []string {
	tree, err := col.Calc.Tree(formula.Limits{})
	if err != nil {
		return nil
	}
	return tree.References(formula.KindField)
}

// sensitivityOf returns the highest sensitivity of a set of accesses.
func sensitivityOf(as ...Access) table.Sensitivity {
	var s table.Sensitivity
	for _, a := range as {
		s = max(s, a.Sensitivity)
	}
	return s
}
