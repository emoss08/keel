package analytics

import (
	"github.com/emoss08/keel/analytics/ir"
)

// The parts of translation keys that are not catalog keys. They start with "@", which no entity,
// field or edge key contains, so no key of one kind is ever a key of another.
const (
	pluralSuffix      = "@plural"
	docSuffix         = "@description"
	categoryPrefix    = "@category."
	aggPrefix         = "@agg."
	windowPrefix      = "@window."
	countRowsKey      = "@agg.count.rows"
	pivotOtherKey     = "@pivot.other"
	measurePatternKey = "@pattern.measure"
	cellPatternKey    = "@pattern.cell"
)

// pivotOtherLabel is the heading of a pivot's Other cell.
const pivotOtherLabel = "Other"

// Messages returns every key the catalog passes to Options.Translate with the text it falls back
// to, for an application's translation pipeline (keel analytics i18n writes it as JSON):
//
//	shipment                         an entity's label           Shipment
//	shipment@plural                  its plural                  Shipments
//	shipment@description             its doc comment
//	shipment.proNumber               a field's label             PRO Number
//	shipment.proNumber@description   the field's doc comment
//	shipment.status.InTransit        an enum value's label       In Transit
//	shipment.customer                an edge's label             Customer
//	@category.Operations             a category                  Operations
//	@agg.sum, @window.rank, …        the engine's own words      Sum, Rank, …
//	@pattern.measure                 a derived measure label     {agg} of {field}
//
// A key with nothing to say (an empty doc comment) is left out.
func (c *Catalog) Messages() map[string]string {
	out := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	for _, e := range c.entities {
		put(e.Key, e.Label)
		put(e.Key+pluralSuffix, e.Plural)
		put(e.Key+docSuffix, e.Doc)
		if e.Category != "" {
			put(categoryPrefix+e.Category, e.Category)
		}
		for _, f := range e.Fields {
			put(f.LabelKey, f.Label)
			put(f.LabelKey+docSuffix, f.Doc)
			for _, v := range f.Enum {
				put(v.LabelKey, v.Label)
				put(v.LabelKey+docSuffix, v.Doc)
			}
		}
		for _, g := range e.Edges {
			put(g.LabelKey, g.Label)
			put(g.LabelKey+docSuffix, g.Doc)
		}
	}
	for _, agg := range ir.Aggs() {
		put(aggPrefix+string(agg), aggLabels[agg])
	}
	for _, fn := range ir.WindowFns() {
		put(windowPrefix+string(fn), windowLabels[fn])
	}
	put(countRowsKey, countLabel)
	put(pivotOtherKey, pivotOtherLabel)
	put(measurePatternKey, measureLabelPattern)
	put(cellPatternKey, cellLabelPattern)
	return out
}
