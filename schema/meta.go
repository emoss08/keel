package schema

// EnumValue is one constant of a column's named Go type, as keel gen read it from the type's
// package.
type EnumValue struct {
	// Value is the constant's value: the string itself, or the integer in decimal.
	Value string
	// Label is derived from the constant's name without the type's name (StatusInTransit → "In
	// Transit").
	Label string
	// Doc is the constant's doc comment.
	Doc string
}

// ReportKind says whether a report uses a column to group rows or to aggregate them.
type ReportKind uint8

const (
	Dimension ReportKind = iota + 1
	Measure
)

// ValueType is the kind of value a report reads from a column, derived from its Go type and name.
type ValueType uint8

const (
	ValueOther ValueType = iota
	ValueString
	ValueEnum
	ValueInt
	ValueFloat
	ValueDecimal
	ValueBool
	ValueTime
	ValueDate
	ValueEpoch
	ValueJSON
	ValueArray
	ValueBytes
	ValueTSVector
)

// Numeric reports whether values of the type can be summed and averaged.
func (v ValueType) Numeric() bool { return v == ValueInt || v == ValueFloat || v == ValueDecimal }

// Ordered reports whether values of the type have a minimum and a maximum.
func (v ValueType) Ordered() bool {
	switch v {
	case ValueString, ValueTime, ValueDate, ValueEpoch:
		return true
	}
	return v.Numeric()
}

// Agg is a set of aggregations a report may apply to a column.
type Agg uint8

const (
	AggCount Agg = 1 << iota
	AggCountDistinct
	AggSum
	AggAvg
	AggMin
	AggMax
)

const (
	// AggsCount are the aggregations every column takes.
	AggsCount = AggCount | AggCountDistinct
	// AggsOrdered are the aggregations of an ordered, non-numeric column.
	AggsOrdered = AggsCount | AggMin | AggMax
	// AggsNumeric are the aggregations of a numeric column.
	AggsNumeric = AggsOrdered | AggSum | AggAvg
)

// aggNames maps each aggregation to its tag name, in the order Agg declares them.
var aggNames = [...]struct {
	agg  Agg
	name string
}{
	{AggCount, "count"}, {AggCountDistinct, "count_distinct"}, {AggSum, "sum"}, {AggAvg, "avg"},
	{AggMin, "min"}, {AggMax, "max"},
}

// Has reports whether every aggregation in b is in a.
func (a Agg) Has(b Agg) bool { return a&b == b }

// Names returns the tag names of the aggregations in a, in declaration order.
func (a Agg) Names() []string {
	var out []string
	for _, n := range aggNames {
		if a.Has(n.agg) {
			out = append(out, n.name)
		}
	}
	return out
}

// ParseAgg returns the aggregation named name (count, count_distinct, sum, avg, min or max).
func ParseAgg(name string) (Agg, bool) {
	for _, n := range aggNames {
		if n.name == name {
			return n.agg, true
		}
	}
	return 0, false
}

// ReportInfo is how reports present a column, resolved by keel gen from the column's Go type,
// name, keys and tag options. Analytics reads it as it is.
type ReportInfo struct {
	// Included reports whether the column is offered to report authors.
	Included bool
	Kind     ReportKind
	Type     ValueType
	Aggs     Agg
	// Format names how values are displayed: the tag's format= option, else the KeelFormat method
	// of the column's type.
	Format string
}

// TableReport marks a model as a report's starting entity.
type TableReport struct {
	// Category groups the entity in a report builder; empty when the tag says only report.
	Category string
}

// PickKey is one key of a pick relation's order: a column of the table reached by hop Hop of
// the relation's path.
type PickKey struct {
	Hop    int32
	Column int32
}

// PickFilter is one condition of a pick relation: the column of the table reached by hop Hop
// holds one of Values. The values are constants written into the pick's subquery.
type PickFilter struct {
	Hop    int32
	Column int32
	Values []string
}
