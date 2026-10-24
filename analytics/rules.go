package analytics

import (
	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
)

// RuleSet is every rule a report builder applies, as data: what validation accepts and the
// compiler renders, published so a client offers exactly that without copying server code. It
// encodes to JSON as one object.
type RuleSet struct {
	// Operators lists the filter operators a condition on each value type can use, in builder
	// order.
	Operators map[formula.Type][]ir.Op `json:"operators"`
	// OperatorInfo describes each operator: its label and the value it takes.
	OperatorInfo []ir.OpInfo `json:"operatorInfo"`
	// HavingOperators are the operators a having condition can use.
	HavingOperators []ir.Op `json:"havingOperators"`
	// Aggregations are the measure aggregations.
	Aggregations []ir.Agg `json:"aggregations"`
	// Buckets are the date buckets, shortest first; a bucket is the instant its period starts in
	// the report's time zone.
	Buckets []ir.Bucket `json:"buckets"`
	// Transforms are the value transforms.
	Transforms []TransformRule `json:"transforms"`
	// Bands bound numeric bands.
	Bands BandRule `json:"bands"`
	// Windows are the window functions.
	Windows []WindowRule `json:"windows"`
	// Charts are the chart types with what each requires.
	Charts []ir.ChartRule `json:"charts"`
	// Display is the display vocabulary: styles per value type, enums and limits.
	Display format.Vocabulary `json:"display"`
	// Functions is the formula function registry.
	Functions []*formula.Function `json:"functions"`
	// FormulaOperators are the formula operators, loosest binding first.
	FormulaOperators []formula.Operator `json:"formulaOperators"`
	// Naming is how output columns are named.
	Naming NamingRule `json:"naming"`
	// Limits are the default definition limits.
	Limits LimitRule `json:"limits"`
}

// TransformRule is one transform: what it applies to and what it takes.
type TransformRule struct {
	// Op is the transform.
	Op ir.TransformOp `json:"op"`
	// Text is true for a transform of text; the others transform numbers.
	Text bool `json:"text"`
	// Precision and Factor say whether it takes a precision or a factor.
	Precision bool `json:"precision"`
	Factor    bool `json:"factor"`
	// MaxPrecision is the most places it keeps.
	MaxPrecision int `json:"maxPrecision,omitempty"`
	// PreservesType is true when the result has the value's type; the others return decimals.
	PreservesType bool `json:"preservesType"`
}

// BandRule bounds numeric bands.
type BandRule struct {
	// MaxEdges is the most boundaries a band can list.
	MaxEdges int `json:"maxEdges"`
}

// WindowRule is one window function: whether it is ordered by a column and what it takes.
type WindowRule struct {
	// Fn is the window function.
	Fn ir.WindowFn `json:"fn"`
	// Ordered is true when it needs an ordering column.
	Ordered bool `json:"ordered"`
	// Numeric is true when it reads a numeric column.
	Numeric bool `json:"numeric"`
	// Offset, Size and Desc say whether it takes an offset, a size and a direction.
	Offset bool `json:"offset"`
	Size   bool `json:"size"`
	Desc   bool `json:"desc"`
}

// NamingRule is how output columns are named.
type NamingRule struct {
	// PivotSeparator joins a pivoted column's id and a value into a cell id ("revenue:Texas").
	PivotSeparator string `json:"pivotSeparator"`
	// PivotOther is the value of a pivot's Other cell.
	PivotOther string `json:"pivotOther"`
	// MeasureLabel is the pattern of a derived measure label, with {agg} and {field}.
	MeasureLabel string `json:"measureLabel"`
	// CountLabel is the label of a count of rows without a field.
	CountLabel string `json:"countLabel"`
	// CellLabel is the pattern of a pivot cell's label, with {column} and {value}.
	CellLabel string `json:"cellLabel"`
}

// LimitRule is the default definition limits a builder enforces as the user types.
type LimitRule struct {
	// MaxColumns caps the columns of a report.
	MaxColumns int `json:"maxColumns"`
	// MaxDimensions caps the grouping columns of an aggregated report.
	MaxDimensions int `json:"maxDimensions"`
	// MaxLimit caps the row limit.
	MaxLimit int `json:"maxLimit"`
	// MaxPageSize caps a page.
	MaxPageSize int `json:"maxPageSize"`
	// MaxPivotValues caps the values of a pivot.
	MaxPivotValues int `json:"maxPivotValues"`
	// MaxWindows caps the window columns.
	MaxWindows int `json:"maxWindows"`
	// MaxPathDepth caps the relations a field path crosses.
	MaxPathDepth int `json:"maxPathDepth"`
	// MaxDays caps the day count of last_n_days and next_n_days.
	MaxDays int `json:"maxDays"`
	// MaxCharts caps the charts of a report.
	MaxCharts int `json:"maxCharts"`
}

// The patterns of derived labels.
const (
	measureLabelPattern = "{agg} of {field}"
	cellLabelPattern    = "{column} · {value}"
	countLabel          = "Count"
)

// Rules returns the rule set. It does not depend on the catalog's contents, so it can be
// published once per keel version.
func (c *Catalog) Rules() RuleSet {
	l := ir.DefaultLimits()
	rs := RuleSet{
		Operators:        ir.OpTable(),
		OperatorInfo:     ir.Ops(),
		HavingOperators:  []ir.Op{ir.OpEq, ir.OpNe, ir.OpGt, ir.OpGte, ir.OpLt, ir.OpLte, ir.OpBetween, ir.OpIn, ir.OpNotIn, ir.OpIsNull, ir.OpIsNotNull},
		Aggregations:     ir.Aggs(),
		Buckets:          ir.Buckets(),
		Bands:            BandRule{MaxEdges: ir.MaxBandEdges},
		Charts:           ir.ChartRules(),
		Display:          format.Rules(),
		Functions:        formula.Functions(),
		FormulaOperators: formula.Operators(),
		Naming: NamingRule{PivotSeparator: ir.PivotSeparator, PivotOther: ir.PivotOther,
			MeasureLabel: measureLabelPattern, CountLabel: countLabel, CellLabel: cellLabelPattern},
		Limits: LimitRule{MaxColumns: l.MaxColumns, MaxDimensions: l.MaxDimensions, MaxLimit: l.MaxLimit,
			MaxPageSize: l.MaxPageSize, MaxPivotValues: l.MaxPivotValues, MaxWindows: l.MaxWindows,
			MaxPathDepth: l.MaxPathDepth, MaxDays: ir.MaxDays, MaxCharts: ir.MaxCharts},
	}
	for _, op := range ir.TransformOps() {
		rs.Transforms = append(rs.Transforms, TransformRule{Op: op, Text: op.Text(),
			Precision: op.UsesPrecision(), Factor: op == ir.TransformScale,
			MaxPrecision: precisionOf(op), PreservesType: op == ir.TransformAbs || op.Text()})
	}
	for _, fn := range ir.WindowFns() {
		rs.Windows = append(rs.Windows, windowRule(fn))
	}
	return rs
}

// precisionOf returns the most places a transform keeps, or zero for one without a precision.
func precisionOf(op ir.TransformOp) int {
	if op.UsesPrecision() {
		return ir.MaxTransformPrecision
	}
	return 0
}

// windowRule describes a window function.
func windowRule(fn ir.WindowFn) WindowRule {
	r := WindowRule{Fn: fn}
	switch fn {
	case ir.WindowRunningTotal, ir.WindowMovingAvg, ir.WindowFirst, ir.WindowLast:
		r.Ordered = true
	case ir.WindowChange, ir.WindowPercentChange:
		r.Ordered, r.Offset = true, true
	case ir.WindowRank, ir.WindowDenseRank:
		r.Desc = true
	}
	switch fn {
	case ir.WindowRunningTotal, ir.WindowPercentOfTotal, ir.WindowMovingAvg, ir.WindowChange, ir.WindowPercentChange:
		r.Numeric = true
	}
	r.Size = fn == ir.WindowMovingAvg
	return r
}
