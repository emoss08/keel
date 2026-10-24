package analyticscli

import (
	"fmt"
	"strings"

	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/schema"
)

// valueTypeNames names the report value types as the definition's field types do.
var valueTypeNames = map[schema.ValueType]string{
	schema.ValueString: "string", schema.ValueEnum: "enum", schema.ValueInt: "int",
	schema.ValueFloat: "float", schema.ValueDecimal: "decimal", schema.ValueBool: "bool",
	schema.ValueTime: "timestamp", schema.ValueDate: "date", schema.ValueEpoch: "epoch",
	schema.ValueJSON: "json", schema.ValueArray: "array", schema.ValueBytes: "bytes",
}

// Describe returns a compact description of the reportable entities for a language model's
// context: each entity's key, label, category and doc comment; its fields with their key, value
// type, role, aggregations, format, enum values and doc comment; its relations with their target
// and cardinality; and the rollups that answer reports on it fast. It is derived from the schema
// alone, so it lists every reportable field; a caller's access policy narrows the catalog the
// engine describes at run time.
func Describe(tables []*schema.Table, keys rollup.Keys, rollups ...*rollup.Rollup) string {
	keys = keys.WithDefaults()
	reportable := map[*schema.Table]bool{}
	for _, t := range tables {
		reportable[t] = t.Report != nil
	}
	var b strings.Builder
	b.WriteString("# Reportable entities\n\n")
	b.WriteString("A report starts from one entity. Fields of related entities are reached with dotted paths " +
		"through relations, such as customer.name; a path through a many relation can only be aggregated. " +
		"Measures aggregate with the listed aggregations; dimensions group and filter.\n")
	for _, t := range tables {
		if t.Report == nil {
			continue
		}
		describeTable(&b, t, keys, reportable)
		describeRollups(&b, t, keys, rollups)
	}
	return b.String()
}

// describeTable writes one entity's heading, fields and relations.
func describeTable(b *strings.Builder, t *schema.Table, keys rollup.Keys, reportable map[*schema.Table]bool) {
	fmt.Fprintf(b, "\n## %s: %s", keys.Entity(t), t.Label)
	if t.Report.Category != "" {
		fmt.Fprintf(b, " (%s)", t.Report.Category)
	}
	b.WriteString("\n")
	if t.Doc != "" {
		b.WriteString(t.Doc + "\n")
	}
	b.WriteString("\nFields:\n")
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.Report == nil || !c.Report.Included {
			continue
		}
		fmt.Fprintf(b, "- %s (%s): %s", keys.Field(t, int32(i)), fieldFacts(c), c.Label)
		if c.Doc != "" {
			b.WriteString(". " + c.Doc)
		}
		b.WriteString("\n")
	}
	var rels []string
	for i := range t.Relations {
		r := &t.Relations[i]
		if r.NoReport || !reportable[r.Target] {
			continue
		}
		card := "one"
		if r.Kind.ToMany() {
			card = "many"
		}
		line := fmt.Sprintf("- %s -> %s (%s): %s", keys.Edge(t, int32(i)), keys.Entity(r.Target), card, r.Label)
		if r.Doc != "" {
			line += ". " + r.Doc
		}
		rels = append(rels, line)
	}
	if len(rels) > 0 {
		b.WriteString("\nRelations:\n" + strings.Join(rels, "\n") + "\n")
	}
}

// fieldFacts is the parenthesised summary of a field: type, enum values, role, aggregations and
// format.
func fieldFacts(c *schema.Column) string {
	parts := []string{valueTypeNames[c.Report.Type]}
	if len(c.Enum) > 0 {
		vals := make([]string, len(c.Enum))
		for i, v := range c.Enum {
			vals[i] = v.Value
			if v.Label != v.Value {
				vals[i] += " \"" + v.Label + "\""
			}
		}
		parts[0] += ": " + strings.Join(vals, ", ")
	}
	if c.Report.Kind == schema.Measure {
		parts = append(parts, "measure")
	} else {
		parts = append(parts, "dimension")
	}
	if names := c.Report.Aggs.Names(); len(names) > 0 {
		parts = append(parts, strings.Join(names, " "))
	}
	if c.Report.Format != "" {
		parts = append(parts, "format "+c.Report.Format)
	}
	return strings.Join(parts, "; ")
}

// describeRollups writes the rollups of t: their grain, dimensions and measures by field key.
func describeRollups(b *strings.Builder, t *schema.Table, keys rollup.Keys, rollups []*rollup.Rollup) {
	var lines []string
	key := func(c *schema.Column) string { return keys.Field(t, t.Column(c.Name)) }
	for _, r := range rollups {
		if r.Source() != t {
			continue
		}
		var dims, aggs []string
		for _, d := range r.Dimensions() {
			dims = append(dims, key(d))
		}
		for _, a := range r.Aggregates() {
			arg := "rows"
			if a.Column != nil {
				arg = key(a.Column)
			}
			aggs = append(aggs, string(a.Agg)+" "+arg)
		}
		line := fmt.Sprintf("- %s: by %s of %s in %s", r.Name(), r.Unit(), key(r.GrainColumn()), r.Location())
		if len(dims) > 0 {
			line += " and " + strings.Join(dims, ", ")
		}
		lines = append(lines, line+"; "+strings.Join(aggs, ", "))
	}
	if len(lines) > 0 {
		b.WriteString("\nRollups (reports within their dimensions, at their grain or coarser, run fast):\n" +
			strings.Join(lines, "\n") + "\n")
	}
}
