package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// Options configures Build.
type Options struct {
	// Translate returns the text for a translation key in the request's language, or fallback,
	// the text keel derived. Keys are an entity's key ("shipment"), a field's or an edge's
	// ("shipment.proNumber", "shipment.customer"), an enum value's ("shipment.status.InTransit"),
	// each with "@description" for its doc comment and an entity's with "@plural"; a category's
	// ("@category.Operations"); and the engine's own words ("@agg.sum", "@window.rank",
	// "@pivot.other", "@pattern.measure"). Catalog.Messages lists every key with its fallback.
	// Nil uses the fallbacks.
	Translate func(ctx context.Context, key, fallback string) string
}

// Catalog is the reportable schema: the entities of the models marked report, their fields and
// edges, derived from the generated schema and checked once. It is read-only after Build and safe
// for concurrent use. It implements ir.Catalog, so definitions validate against it.
type Catalog struct {
	entities []*Entity
	byKey    map[string]*Entity
	byTable  map[*schema.Table]*Entity
	version  string
	opts     Options
}

// Entity is a reportable model: a starting point for reports and a target of edges.
type Entity struct {
	// Key names the entity in definitions: the model name in snake case ("shipment").
	Key string
	// Table is the model's generated schema.
	Table *schema.Table
	// Label and Plural are the derived names, used when Translate has none for Key.
	Label  string
	Plural string
	// Doc is the model's doc comment.
	Doc string
	// Category groups the entity in a builder (report=Category on the table tag).
	Category string
	// Fields are the columns offered to report authors, in column order.
	Fields []*Field
	// Edges are the relations offered to report authors, in relation order.
	Edges []*Edge

	fields map[string]*Field
	edges  map[string]*Edge
}

// Field is a column offered to report authors.
type Field struct {
	// Key names the field in paths: the Go field name in lower camel case ("proNumber").
	Key string
	// Entity is the entity that holds the field.
	Entity *Entity
	// Column is the column's index in the entity's table.
	Column int32
	// Label is the derived label; LabelKey ("shipment.proNumber") is its translation key.
	Label    string
	LabelKey string
	// Doc is the field's doc comment.
	Doc string
	// Type is the field's value type.
	Type formula.Type
	// Kind says whether the field groups (dimension) or aggregates (measure) by default.
	Kind schema.ReportKind
	// Aggs are the aggregations the field supports: those keel gen derived, and avg for a date or
	// time that takes min and max, since the mean of instants is an instant.
	Aggs []ir.Agg
	// Format is the display kind keel gen resolved (format= or the type's KeelFormat).
	Format string
	// Enum lists an enum field's values with their labels.
	Enum []EnumValue
	// Filterable allows conditions on the field; Groupable allows grouping by it.
	Filterable bool
	Groupable  bool
}

// EnumValue is one value of an enum field.
type EnumValue struct {
	// Value is the stored value as text.
	Value string `json:"value"`
	// Label is the derived label; LabelKey is its translation key.
	Label    string `json:"label"`
	LabelKey string `json:"labelKey"`
	// Doc is the constant's doc comment.
	Doc string `json:"doc,omitempty"`
}

// Edge is a relation offered to report authors: a belongs-to, has-one or pick relation leads to
// one row, a has-many or many-to-many relation to many.
type Edge struct {
	// Name names the edge in paths: the Go relation name in lower camel case ("customer").
	Name string
	// From and Target are the entities the edge joins.
	From   *Entity
	Target *Entity
	// Relation is the relation's index in From's table.
	Relation int32
	// Label is the derived label; LabelKey ("shipment.customer") is its translation key.
	Label    string
	LabelKey string
	// Doc is the relation's doc comment.
	Doc string
	// ToMany is true when one row relates to many rows of the target.
	ToMany bool
	// Pick is true for a pick relation: one row chosen from a path.
	Pick bool
}

// relation returns the edge's schema relation.
func (e *Edge) relation() *schema.Relation { return &e.From.Table.Relations[e.Relation] }

// column returns the field's schema column.
func (f *Field) column() *schema.Column { return &f.Entity.Table.Columns[f.Column] }

// numericEnum reports whether an enum field stores its values as integers.
func (f *Field) numericEnum() bool { return f.Type == formula.Enum && integerKind(f.column().Kind) }

// integerKind reports whether a column holds integers.
func integerKind(k schema.Kind) bool {
	return k == schema.KindInt16 || k == schema.KindInt32 || k == schema.KindInt64
}

// Build makes the catalog of the reportable tables among tables (those with Table.Report set) and
// checks its graph: every edge leads to a reportable entity, every pick walks real relations and
// orders by a total order, and joined tables share their tenant columns. Tables that are not
// reportable are skipped, so an application can pass every generated table.
func Build(tables []*schema.Table, opts Options) (*Catalog, error) {
	c := &Catalog{byKey: map[string]*Entity{}, byTable: map[*schema.Table]*Entity{}, opts: opts}
	for _, t := range tables {
		if t == nil || t.Report == nil {
			continue
		}
		if err := c.addEntity(t); err != nil {
			return nil, err
		}
	}
	for _, e := range c.entities {
		if err := c.addEdges(e); err != nil {
			return nil, err
		}
	}
	c.version = c.hash()
	return c, nil
}

// addEntity adds the entity of a reportable table and its fields.
func (c *Catalog) addEntity(t *schema.Table) error {
	model := t.Model
	if i := strings.LastIndexByte(model, '.'); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		model = t.Name
	}
	key := schema.Snake(model)
	if _, dup := c.byKey[key]; dup {
		return fail(CodeCatalog, "", "two reportable models have the entity key %q", key)
	}
	if _, dup := c.byTable[t]; dup {
		return fail(CodeCatalog, "", "table %s is listed twice", t.Name)
	}
	e := &Entity{Key: key, Table: t, Label: orDefault(t.Label, model), Plural: t.Plural, Doc: t.Doc,
		Category: t.Report.Category, fields: map[string]*Field{}, edges: map[string]*Edge{}}
	for i := range t.Columns {
		f, ok := fieldOf(e, int32(i))
		if !ok {
			continue
		}
		if _, dup := e.fields[f.Key]; dup {
			return fail(CodeCatalog, "", "%s has two fields with the key %q", key, f.Key)
		}
		e.fields[f.Key] = f
		e.Fields = append(e.Fields, f)
	}
	c.entities = append(c.entities, e)
	c.byKey[key] = e
	c.byTable[t] = e
	return nil
}

// fieldOf returns the field of column i, or false when the column is not offered.
func fieldOf(e *Entity, i int32) (*Field, bool) {
	col := &e.Table.Columns[i]
	info := col.Report
	if info == nil || !info.Included || !col.Stored() {
		return nil, false
	}
	typ, ok := fieldType(info.Type)
	if !ok {
		return nil, false
	}
	key := lowerCamel(col.Field)
	if key == "" {
		key = lowerCamel(schema.Snake(col.Name))
	}
	f := &Field{Key: key, Entity: e, Column: i, Label: orDefault(col.Label, col.Name),
		LabelKey: e.Key + "." + key, Doc: col.Doc, Type: typ, Kind: info.Kind, Format: info.Format,
		Filterable: true, Groupable: info.Kind == schema.Dimension && typ != formula.JSON}
	aggs := info.Aggs
	if typ.Temporal() && aggs.Has(schema.AggMin|schema.AggMax) {
		aggs |= schema.AggAvg
	}
	for _, name := range aggs.Names() {
		f.Aggs = append(f.Aggs, ir.Agg(name))
	}
	for _, v := range col.Enum {
		f.Enum = append(f.Enum, EnumValue{Value: v.Value, Label: v.Label, Doc: v.Doc,
			LabelKey: f.LabelKey + "." + v.Value})
	}
	return f, true
}

// fieldType maps the value type keel gen resolved to the formula type reports use.
func fieldType(t schema.ValueType) (formula.Type, bool) {
	switch t {
	case schema.ValueString:
		return formula.String, true
	case schema.ValueEnum:
		return formula.Enum, true
	case schema.ValueInt:
		return formula.Int, true
	case schema.ValueFloat:
		return formula.Float, true
	case schema.ValueDecimal:
		return formula.Decimal, true
	case schema.ValueBool:
		return formula.Bool, true
	case schema.ValueTime:
		return formula.Timestamp, true
	case schema.ValueDate:
		return formula.Date, true
	case schema.ValueEpoch:
		return formula.Epoch, true
	case schema.ValueJSON, schema.ValueArray:
		return formula.JSON, true
	}
	return formula.Unknown, false
}

// addEdges adds the edges of an entity's relations, checking where they lead.
func (c *Catalog) addEdges(e *Entity) error {
	t := e.Table
	for i := range t.Relations {
		rel := &t.Relations[i]
		if rel.NoReport {
			continue
		}
		target, err := c.edgeTarget(e, rel)
		if err != nil {
			return err
		}
		if rel.Kind == schema.Pick {
			if err := checkPick(e, rel); err != nil {
				return err
			}
		}
		if err := checkTenants(t, rel.Target); err != nil {
			return err
		}
		name := lowerCamel(rel.Name)
		if _, dup := e.edges[name]; dup {
			return fail(CodeCatalog, "", "%s has two edges named %q", e.Key, name)
		}
		edge := &Edge{Name: name, From: e, Target: target, Relation: int32(i),
			Label: orDefault(rel.Label, rel.Name), LabelKey: e.Key + "." + name, Doc: rel.Doc,
			ToMany: rel.Kind.ToMany(), Pick: rel.Kind == schema.Pick}
		e.edges[name] = edge
		e.Edges = append(e.Edges, edge)
	}
	return nil
}

// edgeTarget returns the reportable entity a relation leads to.
func (c *Catalog) edgeTarget(e *Entity, rel *schema.Relation) (*Entity, error) {
	if rel.Target == nil {
		return nil, fail(CodeCatalog, "", "relation %s of %s has no target table; is its generated package imported?",
			rel.Name, e.Table.Name)
	}
	target, ok := c.byTable[rel.Target]
	if !ok {
		return nil, fail(CodeCatalog, "", "relation %s of %s leads to %s, which is not reportable; mark the relation noreport or the model report",
			rel.Name, e.Table.Name, rel.Target.Name)
	}
	return target, nil
}

// checkPick checks that a pick walks real relations and orders by keys that end with the last
// table's primary key, so exactly one row is picked.
func checkPick(e *Entity, rel *schema.Relation) error {
	if len(rel.Path) == 0 || len(rel.Order) == 0 {
		return fail(CodeCatalog, "", "pick %s of %s needs a path and an order", rel.Name, e.Table.Name)
	}
	t := e.Table
	hops := make([]*schema.Table, len(rel.Path))
	for i, ri := range rel.Path {
		if ri < 0 || int(ri) >= len(t.Relations) || t.Relations[ri].Target == nil {
			return fail(CodeCatalog, "", "pick %s of %s walks a relation that does not exist", rel.Name, e.Table.Name)
		}
		t = t.Relations[ri].Target
		hops[i] = t
	}
	last := len(hops) - 1
	for _, k := range rel.Order {
		if k.Hop < 0 || int(k.Hop) > last || k.Column < 0 || int(k.Column) >= len(hops[k.Hop].Columns) {
			return fail(CodeCatalog, "", "pick %s of %s orders by a column that does not exist", rel.Name, e.Table.Name)
		}
	}
	for _, pk := range hops[last].PK {
		if hops[last].Columns[pk].Flags.Has(schema.FlagTenant) {
			continue
		}
		if !slices.ContainsFunc(rel.Order, func(k schema.PickKey) bool { return int(k.Hop) == last && k.Column == pk }) {
			return fail(CodeCatalog, "", "pick %s of %s does not order by %s's primary key, so the row it picks is not unique",
				rel.Name, e.Table.Name, hops[last].Name)
		}
	}
	return nil
}

// checkTenants checks that two joined tables with tenant columns have the same ones.
func checkTenants(a, b *schema.Table) error {
	if b == nil || !a.HasTenant() || !b.HasTenant() {
		return nil
	}
	if !slices.Equal(tenantNames(a), tenantNames(b)) {
		return fail(CodeCatalog, "", "%s and %s are joined but have different tenant columns", a.Name, b.Name)
	}
	return nil
}

// tenantNames returns a table's tenant column names, sorted.
func tenantNames(t *schema.Table) []string {
	out := make([]string, len(t.Tenant))
	for i, ci := range t.Tenant {
		out[i] = t.Columns[ci].Name
	}
	sort.Strings(out)
	return out
}

// hash returns the catalog's version: a hash of everything a definition depends on (keys,
// types, aggregations, permissions to filter and group, enum values, edges), not labels or docs.
func (c *Catalog) hash() string {
	h := sha256.New()
	for _, e := range c.sorted() {
		h.Write([]byte("e " + e.Key + "\n"))
		for _, f := range e.Fields {
			parts := []string{"f", f.Key, string(f.Type), strconv.FormatBool(f.Filterable),
				strconv.FormatBool(f.Groupable)}
			for _, a := range f.Aggs {
				parts = append(parts, string(a))
			}
			for _, v := range f.Enum {
				parts = append(parts, "="+v.Value)
			}
			h.Write([]byte(strings.Join(parts, " ") + "\n"))
		}
		for _, g := range e.Edges {
			h.Write([]byte("g " + g.Name + " " + g.Target.Key + " " + strconv.FormatBool(g.ToMany) + "\n"))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// sorted returns the entities ordered by key.
func (c *Catalog) sorted() []*Entity {
	out := slices.Clone(c.entities)
	slices.SortFunc(out, func(a, b *Entity) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// Version is a hash of everything a stored definition depends on. A definition saved under
// another version may no longer validate; Check says why.
func (c *Catalog) Version() string { return c.version }

// Entities returns the entities in the order their tables were given.
func (c *Catalog) Entities() []*Entity { return slices.Clone(c.entities) }

// Entity returns the entity with the key.
func (c *Catalog) Entity(key string) (*Entity, bool) {
	e, ok := c.byKey[key]
	return e, ok
}

// EntityOf returns the entity of a table.
func (c *Catalog) EntityOf(t *schema.Table) (*Entity, bool) {
	e, ok := c.byTable[t]
	return e, ok
}

// Field returns a field of the entity by key.
func (e *Entity) Field(key string) (*Field, bool) {
	f, ok := e.fields[key]
	return f, ok
}

// Edge returns an edge of the entity by name.
func (e *Entity) Edge(name string) (*Edge, bool) {
	g, ok := e.edges[name]
	return g, ok
}

// Resolve follows a dotted field path from the entity: the edges it crosses and the field.
func (c *Catalog) Resolve(entity, path string) (Path, *Field, bool) {
	e, ok := c.byKey[entity]
	if !ok {
		return nil, nil, false
	}
	parts := strings.Split(path, ".")
	var via Path
	for _, p := range parts[:len(parts)-1] {
		g, ok := e.edges[p]
		if !ok {
			return nil, nil, false
		}
		via = append(via, g)
		e = g.Target
	}
	f, ok := e.fields[parts[len(parts)-1]]
	return via, f, ok
}

// Label translates a label key through Options.Translate.
func (c *Catalog) Label(ctx context.Context, key, fallback string) string {
	if c.opts.Translate == nil {
		return fallback
	}
	if s := c.opts.Translate(ctx, key, fallback); s != "" {
		return s
	}
	return fallback
}

// HasEntity reports whether an entity has the key. It implements ir.Catalog.
func (c *Catalog) HasEntity(key string) bool {
	_, ok := c.byKey[key]
	return ok
}

// EntityKeys lists the entity keys. It implements ir.Catalog.
func (c *Catalog) EntityKeys() []string {
	out := make([]string, len(c.entities))
	for i, e := range c.entities {
		out[i] = e.Key
	}
	return out
}

// Field describes a field of an entity for validation. It implements ir.Catalog.
func (c *Catalog) Field(entity, key string) (ir.FieldInfo, bool) {
	e, ok := c.byKey[entity]
	if !ok {
		return ir.FieldInfo{}, false
	}
	f, ok := e.fields[key]
	if !ok {
		return ir.FieldInfo{}, false
	}
	info := ir.FieldInfo{Type: f.Type, Aggs: slices.Clone(f.Aggs), Filterable: f.Filterable,
		Groupable: f.Groupable}
	for _, v := range f.Enum {
		info.Enum = append(info.Enum, v.Value)
	}
	return info, true
}

// FieldKeys lists an entity's field keys. It implements ir.Catalog.
func (c *Catalog) FieldKeys(entity string) []string {
	e, ok := c.byKey[entity]
	if !ok {
		return nil
	}
	out := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		out[i] = f.Key
	}
	return out
}

// Edge describes an edge of an entity for validation. It implements ir.Catalog.
func (c *Catalog) Edge(entity, name string) (ir.EdgeInfo, bool) {
	e, ok := c.byKey[entity]
	if !ok {
		return ir.EdgeInfo{}, false
	}
	g, ok := e.edges[name]
	if !ok {
		return ir.EdgeInfo{}, false
	}
	return ir.EdgeInfo{Target: g.Target.Key, ToMany: g.ToMany}, true
}

// EdgeNames lists an entity's edge names. It implements ir.Catalog.
func (c *Catalog) EdgeNames(entity string) []string {
	e, ok := c.byKey[entity]
	if !ok {
		return nil
	}
	out := make([]string, len(e.Edges))
	for i, g := range e.Edges {
		out[i] = g.Name
	}
	return out
}

// lowerCamel lower-cases a Go name's leading capital or leading acronym: ProNumber → proNumber,
// ID → id, URLPath → urlPath.
func lowerCamel(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	n := 0
	for n < len(runes) && unicode.IsUpper(runes[n]) {
		n++
	}
	switch {
	case n == 0:
		return s
	case n == len(runes):
		return strings.ToLower(s)
	case n > 1:
		n--
	}
	for i := range n {
		runes[i] = unicode.ToLower(runes[i])
	}
	return string(runes)
}

// orDefault returns s, or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
