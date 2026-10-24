package gen

import (
	"bytes"
	"fmt"
	"go/types"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/emoss08/keel/schema"
)

// shared collects the values one generated file declares once and references from many columns:
// enum value lists and report facts.
type shared struct {
	sch     string
	model   string
	enums   map[*types.Named]string
	enumOf  map[string][]schema.EnumValue
	reports map[schema.ReportInfo]string
	taken   map[string]bool
}

// newShared returns the shared values of the generated file of model package p, qualifying schema
// names with sch.
func newShared(sch string, p *pkgModels) *shared {
	return &shared{sch: sch, model: p.path, enums: map[*types.Named]string{}, enumOf: map[string][]schema.EnumValue{},
		reports: map[schema.ReportInfo]string{}, taken: map[string]bool{}}
}

// name returns base, numbered when another value already took it.
func (s *shared) name(base string) string {
	n := base
	for i := 2; s.taken[n]; i++ {
		n = base + strconv.Itoa(i)
	}
	s.taken[n] = true
	return n
}

// enum returns the expression of the enum values of c's type.
func (s *shared) enum(c *column) string {
	v, ok := s.enums[c.enum]
	if !ok {
		obj := c.enum.Obj()
		base := obj.Name()
		if obj.Pkg().Path() != s.model {
			base = exportedName(obj.Pkg().Name()) + base
		}
		v = s.name("enum" + base)
		s.enums[c.enum] = v
		s.enumOf[v] = c.values
	}
	return v + "[:]"
}

// report returns the expression of a pointer to the shared report facts ri.
func (s *shared) report(ri schema.ReportInfo) string {
	v, ok := s.reports[ri]
	if !ok {
		v = s.name(reportVarName(ri))
		s.reports[ri] = v
	}
	return "&" + v
}

// reportVarName names report facts by their content: report, the kind, the value type, the
// format, the aggregations when they differ from the type's, and Hidden when excluded.
func reportVarName(ri schema.ReportInfo) string {
	var b strings.Builder
	b.WriteString("report")
	b.WriteString(map[schema.ReportKind]string{schema.Dimension: "Dim", schema.Measure: "Measure"}[ri.Kind])
	b.WriteString(identWords(valueTypeNames[ri.Type]))
	b.WriteString(identWords(ri.Format))
	if ri.Aggs != defaultAggs(ri.Type) {
		b.WriteString("Agg")
		b.WriteString(identWords(strings.Join(ri.Aggs.Names(), " ")))
	}
	if !ri.Included {
		b.WriteString("Hidden")
	}
	return b.String()
}

// defaultAggs returns the aggregations keel gen derives for a value type.
func defaultAggs(t schema.ValueType) schema.Agg {
	switch {
	case t.Numeric():
		return schema.AggsNumeric
	case t.Ordered():
		return schema.AggsOrdered
	}
	return schema.AggsCount
}

// identWords turns free text into an identifier fragment: letters and digits kept, each word
// capitalised.
func identWords(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			up = true
			continue
		}
		if up {
			r = unicode.ToUpper(r)
			up = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// write declares the shared values, ordered by name.
func (s *shared) write(b *bytes.Buffer) {
	var names []string
	reports := map[string]schema.ReportInfo{}
	for ri, n := range s.reports {
		names = append(names, n)
		reports[n] = ri
	}
	for n := range s.enumOf {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if values, ok := s.enumOf[n]; ok {
			s.writeEnum(b, n, values)
			continue
		}
		fmt.Fprintf(b, "var %s = %s\n\n", n, s.reportLit(reports[n]))
	}
}

// writeEnum declares an enum value list as an array, so columns reference it statically.
func (s *shared) writeEnum(b *bytes.Buffer, name string, values []schema.EnumValue) {
	fmt.Fprintf(b, "var %s = [...]%s.EnumValue{\n", name, s.sch)
	for _, v := range values {
		fmt.Fprintf(b, "\t{Value: %s, Label: %s%s},\n", lit(v.Value), lit(v.Label), optStr("Doc", v.Doc))
	}
	b.WriteString("}\n\n")
}

// reportLit writes report facts as a literal.
func (s *shared) reportLit(ri schema.ReportInfo) string {
	var parts []string
	if ri.Included {
		parts = append(parts, "Included: true")
	}
	parts = append(parts,
		"Kind: "+s.sch+"."+map[schema.ReportKind]string{schema.Dimension: "Dimension", schema.Measure: "Measure"}[ri.Kind],
		"Type: "+s.sch+"."+valueTypeIdents[ri.Type],
		"Aggs: "+s.aggsLit(ri.Aggs))
	if ri.Format != "" {
		parts = append(parts, "Format: "+lit(ri.Format))
	}
	return s.sch + ".ReportInfo{" + strings.Join(parts, ", ") + "}"
}

// aggsLit writes an aggregation set, by its schema name when it has one.
func (s *shared) aggsLit(a schema.Agg) string {
	switch a {
	case schema.AggsNumeric:
		return s.sch + ".AggsNumeric"
	case schema.AggsOrdered:
		return s.sch + ".AggsOrdered"
	case schema.AggsCount:
		return s.sch + ".AggsCount"
	}
	var parts []string
	for _, n := range a.Names() {
		parts = append(parts, s.sch+".Agg"+identWords(n))
	}
	return strings.Join(parts, " | ")
}

// valueTypeIdents are the schema identifiers of value types.
var valueTypeIdents = map[schema.ValueType]string{
	schema.ValueOther: "ValueOther", schema.ValueString: "ValueString", schema.ValueEnum: "ValueEnum",
	schema.ValueInt: "ValueInt", schema.ValueFloat: "ValueFloat", schema.ValueDecimal: "ValueDecimal",
	schema.ValueBool: "ValueBool", schema.ValueTime: "ValueTime", schema.ValueDate: "ValueDate",
	schema.ValueEpoch: "ValueEpoch", schema.ValueJSON: "ValueJSON", schema.ValueArray: "ValueArray",
	schema.ValueBytes: "ValueBytes", schema.ValueTSVector: "ValueTSVector",
}

// optStr writes ", name: value" for a non-empty string field of a literal.
func optStr(name, v string) string {
	if v == "" {
		return ""
	}
	return ", " + name + ": " + lit(v)
}

// tableMeta writes the label, doc and report fields of a table literal.
func (s *shared) tableMeta(m *model) string {
	out := fmt.Sprintf("\tLabel: %s,\n\tPlural: %s,\n", lit(m.label), lit(m.pluralLabel))
	if m.doc != "" {
		out += "\tDoc: " + lit(m.doc) + ",\n"
	}
	if m.report != nil {
		out += fmt.Sprintf("\tReport: &%s.TableReport{%s},\n", s.sch, strings.TrimPrefix(optStr("Category",
			m.report.Category), ", "))
	}
	return out
}

// columnMeta writes the label, doc, JSON name, enum, report and text search configuration fields
// of a column literal.
func (s *shared) columnMeta(c *column) string {
	out := ", Label: " + lit(c.label) + optStr("Doc", c.doc) + optStr("JSON", c.json)
	if c.enum != nil {
		out += ", Enum: " + s.enum(c)
	}
	if c.report != nil {
		out += ", Report: " + s.report(*c.report)
	}
	return out + optStr("TSConfig", c.tag.TSConfig)
}

// relationMeta writes the label, doc, JSON name, report and pick fields of a relation literal.
func (s *shared) relationMeta(r *relation) string {
	out := ", Label: " + lit(r.label) + optStr("Doc", r.doc) + optStr("JSON", r.json)
	if r.tag.NoReport {
		out += ", NoReport: true"
	}
	if r.kind != schema.Pick {
		return out
	}
	keys := make([]string, len(r.order))
	for i, k := range r.order {
		keys[i] = fmt.Sprintf("{Hop: %d, Column: %d}", k.Hop, k.Column)
	}
	out += fmt.Sprintf(", Path: %s, Order: []%s.PickKey{%s}", ints(r.path), s.sch, strings.Join(keys, ", "))
	if r.tag.PickLast {
		out += ", Last: true"
	}
	return out
}
