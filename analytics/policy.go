package analytics

import (
	"context"
	"strings"
	"sync"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/schema"
)

// Access is a policy's decision about an entity, a field or an edge.
type Access struct {
	// Allowed reports whether the caller may use it.
	Allowed bool `json:"allowed"`
	// Reason is the policy's code for a denial, for the application to translate
	// ("needs_field_group").
	Reason string `json:"reason,omitempty"`
	// Sensitivity is how sensitive the field's data is; output columns carry the highest
	// sensitivity of the fields they read.
	Sensitivity table.Sensitivity `json:"sensitivity,omitempty"`
}

// Allowed is the access of something the caller may use.
func Allowed() Access { return Access{Allowed: true} }

// Denied is the access of something the caller may not use, with the reason code a builder shows.
func Denied(reason string) Access { return Access{Reason: reason} }

// WithSensitivity returns the access with a sensitivity.
func (a Access) WithSensitivity(s table.Sensitivity) Access {
	a.Sensitivity = s
	return a
}

// Path is the edges walked from a report's entity to reach a field or an edge.
type Path []*Edge

// String joins the edge names with dots ("customer.region").
func (p Path) String() string {
	names := make([]string, len(p))
	for i, e := range p {
		names[i] = e.Name
	}
	return strings.Join(names, ".")
}

// Policy decides what a caller may report on. The catalog view (Catalog.For) and the compiler
// consult the same policy, so a builder offers exactly what a run accepts. Each method is asked at
// most once per entity, field (with its path) and edge (with its path) per request, so an
// implementation can load the caller's permissions once and answer from them.
//
// Entity decides whether the caller may start a report from an entity. Field and Edge decide
// whether the caller may read a field or cross an edge reached through via, the edges walked from
// the report's entity (empty for the entity's own fields and edges). Rows returns row rules for
// every reference a statement makes to the entity's table (the base, joins, picks, EXISTS,
// pre-aggregations): predicates over the table's own columns, built from a generated handle or a
// dynamic query on the table (keel.DynFrom(e.Table).Path("owner_id").Eq(user)), applied beside
// tenancy, which keel applies by construction whatever the policy says. A report reading a table
// with row rules is never answered from a rollup.
type Policy interface {
	Entity(ctx context.Context, e *Entity) Access
	Field(ctx context.Context, f *Field, via Path) Access
	Edge(ctx context.Context, e *Edge, via Path) Access
	Rows(ctx context.Context, e *Entity) []keel.Pred
}

// decisions asks a policy each question once per request and remembers the answer. A nil policy
// allows everything and has no row rules.
type decisions struct {
	p   Policy
	cat *Catalog

	mu       sync.Mutex
	entities map[*Entity]Access
	fields   map[string]Access
	edges    map[string]Access
	rows     map[*Entity][]keel.Pred
}

// newDecisions returns the memo of one request's policy decisions.
func newDecisions(cat *Catalog, p Policy) *decisions {
	return &decisions{p: p, cat: cat, entities: map[*Entity]Access{}, fields: map[string]Access{},
		edges: map[string]Access{}, rows: map[*Entity][]keel.Pred{}}
}

// entity returns the access to an entity.
func (d *decisions) entity(ctx context.Context, e *Entity) Access {
	if d.p == nil {
		return Allowed()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.entities[e]; ok {
		return a
	}
	a := d.p.Entity(ctx, e)
	d.entities[e] = a
	return a
}

// field returns the access to a field reached through via.
func (d *decisions) field(ctx context.Context, f *Field, via Path) Access {
	if d.p == nil {
		return Allowed()
	}
	key := f.LabelKey + "@" + via.key()
	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.fields[key]; ok {
		return a
	}
	a := d.p.Field(ctx, f, via)
	d.fields[key] = a
	return a
}

// edge returns the access to an edge reached through via.
func (d *decisions) edge(ctx context.Context, e *Edge, via Path) Access {
	if d.p == nil {
		return Allowed()
	}
	key := e.LabelKey + "@" + via.key()
	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.edges[key]; ok {
		return a
	}
	a := d.p.Edge(ctx, e, via)
	d.edges[key] = a
	return a
}

// key identifies a path by its edges' translation keys, which are unique across entities.
func (p Path) key() string {
	keys := make([]string, len(p))
	for i, e := range p {
		keys[i] = e.LabelKey
	}
	return strings.Join(keys, "/")
}

// rowsOf returns the row rules of an entity.
func (d *decisions) rowsOf(ctx context.Context, e *Entity) []keel.Pred {
	if d.p == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ps, ok := d.rows[e]; ok {
		return ps
	}
	ps := d.p.Rows(ctx, e)
	d.rows[e] = ps
	return ps
}

// filter is the keel read filter that applies the row rules of every reportable table a
// statement references.
func (d *decisions) filter() keel.ReadFilter {
	return func(ctx context.Context, t *schema.Table) ([]keel.Pred, error) {
		e, ok := d.cat.byTable[t]
		if !ok {
			return nil, nil
		}
		return d.rowsOf(ctx, e), nil
	}
}

// hasRules reports whether any of the tables has row rules.
func (d *decisions) hasRules(ctx context.Context, tables []*schema.Table) bool {
	for _, t := range tables {
		if e, ok := d.cat.byTable[t]; ok && len(d.rowsOf(ctx, e)) > 0 {
			return true
		}
	}
	return false
}

// path checks the access to every edge of a path and to the field at its end, returning the
// field's access with the path's denial when an edge is denied.
func (d *decisions) path(ctx context.Context, via Path, f *Field) Access {
	for i, e := range via {
		if a := d.edge(ctx, e, via[:i]); !a.Allowed {
			return a
		}
	}
	if f == nil {
		return Allowed()
	}
	return d.field(ctx, f, via)
}
