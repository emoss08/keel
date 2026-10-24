package ir

import (
	"slices"
	"strings"

	"github.com/emoss08/keel/analytics/formula"
)

// Catalog is what validation needs to know about the reportable schema. The analytics catalog
// implements it; tests use a fake. Entity keys, field keys and edge names are the catalog's own.
type Catalog interface {
	// HasEntity reports whether a reportable entity has the key.
	HasEntity(key string) bool
	// EntityKeys lists every reportable entity, for suggestions.
	EntityKeys() []string
	// Field returns a field of an entity.
	Field(entity, key string) (FieldInfo, bool)
	// FieldKeys lists an entity's fields, for suggestions.
	FieldKeys(entity string) []string
	// Edge returns a relation of an entity by name.
	Edge(entity, name string) (EdgeInfo, bool)
	// EdgeNames lists an entity's relations, for suggestions.
	EdgeNames(entity string) []string
}

// FieldInfo is what validation needs to know about a field.
type FieldInfo struct {
	// Type is the field's value type.
	Type formula.Type
	// Aggs are the aggregations the field supports.
	Aggs []Agg
	// Filterable allows conditions on the field.
	Filterable bool
	// Groupable allows grouping by the field in an aggregated report.
	Groupable bool
	// Enum lists the values of an enum field.
	Enum []string
}

// EdgeInfo is what validation needs to know about a relation.
type EdgeInfo struct {
	// Target is the key of the entity the relation reaches.
	Target string
	// ToMany is true when one row relates to many rows of the target.
	ToMany bool
}

// ResolvedField is a field path resolved against the catalog.
type ResolvedField struct {
	// Path is the dotted path as written.
	Path string
	// Entity is the key of the entity that holds the field.
	Entity string
	// Edges is the path without the field, joined with dots.
	Edges string
	// Info describes the field.
	Info FieldInfo
	// ToMany is true when the path crosses a to-many relation.
	ToMany bool
}

type pathError struct {
	message    string
	name       string
	candidates []string
}

func resolvePath(cat Catalog, entity, path string) (*ResolvedField, *pathError) {
	parts := strings.Split(path, ".")
	current, toMany := entity, false
	for i, part := range parts[:len(parts)-1] {
		edge, ok := cat.Edge(current, part)
		if !ok {
			prefix := strings.Join(parts[:i], ".")
			return nil, &pathError{
				message:    "unknown relation " + quoteName(part) + " on " + current,
				name:       path,
				candidates: prefixed(prefix, cat.EdgeNames(current), parts[i+1:]),
			}
		}
		current, toMany = edge.Target, toMany || edge.ToMany
	}
	last := parts[len(parts)-1]
	info, ok := cat.Field(current, last)
	if !ok {
		prefix := strings.Join(parts[:len(parts)-1], ".")
		return nil, &pathError{
			message:    "unknown field " + quoteName(last) + " on " + current,
			name:       path,
			candidates: prefixed(prefix, cat.FieldKeys(current), nil),
		}
	}
	return &ResolvedField{Path: path, Entity: current, Edges: strings.Join(parts[:len(parts)-1], "."), Info: info, ToMany: toMany}, nil
}

func quoteName(s string) string { return "\"" + s + "\"" }

func prefixed(prefix string, names []string, rest []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		p := slices.Concat(nonEmpty(prefix), []string{n}, rest)
		out = append(out, strings.Join(p, "."))
	}
	return out
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func (f FieldInfo) supports(a Agg) bool { return slices.Contains(f.Aggs, a) }
