package rollup

import (
	"strings"

	"github.com/emoss08/keel/schema"
)

// Keys name a schema's entities, fields and relations the way the analytics catalog does, so a
// report definition's paths can be matched against tables. The zero Keys is DefaultKeys.
type Keys struct {
	// Entity returns the key of a reportable table, such as "shipment".
	Entity func(t *schema.Table) string
	// Field returns the key of a column, such as "proNumber".
	Field func(t *schema.Table, col int32) string
	// Edge returns the key of a relation, such as "customer".
	Edge func(t *schema.Table, rel int32) string
}

// DefaultKeys derives keys in lower camel case from the Go names: the model's type name
// (fleet.Shipment → shipment), the field name (ProNumber → proNumber, CustomerID → customerId) and
// the relation's field name (BillTo → billTo).
func DefaultKeys() Keys {
	return Keys{
		Entity: func(t *schema.Table) string {
			name := t.Model
			if i := strings.LastIndexByte(name, '.'); i >= 0 {
				name = name[i+1:]
			}
			return Camel(name)
		},
		Field: func(t *schema.Table, col int32) string { return Camel(t.Columns[col].Field) },
		Edge:  func(t *schema.Table, rel int32) string { return Camel(t.Relations[rel].Name) },
	}
}

// WithDefaults returns k with the functions it leaves nil taken from DefaultKeys.
func (k Keys) WithDefaults() Keys {
	d := DefaultKeys()
	if k.Entity == nil {
		k.Entity = d.Entity
	}
	if k.Field == nil {
		k.Field = d.Field
	}
	if k.Edge == nil {
		k.Edge = d.Edge
	}
	return k
}

// Camel turns a Go identifier into lower camel case with acronyms as words: ProNumber →
// proNumber, CustomerID → customerId, HTTPServer → httpServer.
func Camel(name string) string {
	words := strings.Split(schema.Snake(name), "_")
	var b strings.Builder
	for i, w := range words {
		if w == "" {
			continue
		}
		if i == 0 {
			b.WriteString(w)
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]))
		b.WriteString(w[1:])
	}
	return b.String()
}

// pascal turns a snake_case name into upper camel case: total_sum → TotalSum.
func pascal(name string) string {
	c := Camel(name)
	if c == "" {
		return c
	}
	return strings.ToUpper(c[:1]) + c[1:]
}

// label turns a snake_case name into words: total_sum → Total Sum.
func label(name string) string {
	words := strings.Split(name, "_")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
