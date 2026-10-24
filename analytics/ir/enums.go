package ir

import (
	"slices"

	"github.com/emoss08/keel/analytics/formula"
)

// Agg is the aggregation of a measure. Each is the formula function of the same name.
type Agg string

const (
	// AggCount counts rows, or non-null values of a field.
	AggCount Agg = "count"
	// AggCountDistinct counts distinct non-null values.
	AggCountDistinct Agg = "count_distinct"
	// AggSum totals the values.
	AggSum Agg = "sum"
	// AggAvg averages the values.
	AggAvg Agg = "avg"
	// AggMin takes the smallest value.
	AggMin Agg = "min"
	// AggMax takes the largest value.
	AggMax Agg = "max"
)

// Aggs returns every aggregation.
func Aggs() []Agg {
	return []Agg{AggCount, AggCountDistinct, AggSum, AggAvg, AggMin, AggMax}
}

// Bucket is a period a date or time dimension is truncated to.
type Bucket string

const (
	// BucketHour truncates to the hour.
	BucketHour Bucket = "hour"
	// BucketDay truncates to the day.
	BucketDay Bucket = "day"
	// BucketWeek truncates to the ISO week, starting Monday.
	BucketWeek Bucket = "week"
	// BucketMonth truncates to the month.
	BucketMonth Bucket = "month"
	// BucketQuarter truncates to the quarter.
	BucketQuarter Bucket = "quarter"
	// BucketYear truncates to the year.
	BucketYear Bucket = "year"
)

// Buckets returns every bucket, shortest first.
func Buckets() []Bucket {
	return []Bucket{BucketHour, BucketDay, BucketWeek, BucketMonth, BucketQuarter, BucketYear}
}

// WindowFn is the function of a window column.
type WindowFn string

const (
	// WindowRunningTotal is the cumulative sum of the column in Over order.
	WindowRunningTotal WindowFn = "running_total"
	// WindowPercentOfTotal is the column's share of its partition's total, from 0 to 1.
	WindowPercentOfTotal WindowFn = "percent_of_total"
	// WindowRank ranks rows by the column, with gaps after ties.
	WindowRank WindowFn = "rank"
	// WindowDenseRank ranks rows by the column, without gaps after ties.
	WindowDenseRank WindowFn = "dense_rank"
	// WindowMovingAvg averages the column over the Size rows ending at each row in Over order.
	WindowMovingAvg WindowFn = "moving_avg"
	// WindowChange is the column minus its value Offset rows earlier in Over order.
	WindowChange WindowFn = "change"
	// WindowPercentChange is the change divided by the earlier value.
	WindowPercentChange WindowFn = "percent_change"
	// WindowFirst is the column's value in the partition's first row in Over order.
	WindowFirst WindowFn = "first"
	// WindowLast is the column's value in the partition's last row in Over order.
	WindowLast WindowFn = "last"
)

// WindowFns returns every window function.
func WindowFns() []WindowFn {
	return []WindowFn{WindowRunningTotal, WindowPercentOfTotal, WindowRank, WindowDenseRank,
		WindowMovingAvg, WindowChange, WindowPercentChange, WindowFirst, WindowLast}
}

func (f WindowFn) ordered() bool {
	switch f {
	case WindowRunningTotal, WindowMovingAvg, WindowChange, WindowPercentChange, WindowFirst, WindowLast:
		return true
	default:
		return false
	}
}

func (f WindowFn) numeric() bool {
	switch f {
	case WindowRunningTotal, WindowPercentOfTotal, WindowMovingAvg, WindowChange, WindowPercentChange:
		return true
	default:
		return false
	}
}

// TransformOp is a value transform.
type TransformOp string

const (
	// TransformRound rounds to Precision decimal places.
	TransformRound TransformOp = "round"
	// TransformCeil rounds up to Precision decimal places.
	TransformCeil TransformOp = "ceil"
	// TransformFloor rounds down to Precision decimal places.
	TransformFloor TransformOp = "floor"
	// TransformTruncate truncates to Precision decimal places.
	TransformTruncate TransformOp = "truncate"
	// TransformAbs takes the absolute value.
	TransformAbs TransformOp = "abs"
	// TransformScale multiplies by Factor.
	TransformScale TransformOp = "scale"
	// TransformUpper upper-cases text.
	TransformUpper TransformOp = "upper"
	// TransformLower lower-cases text.
	TransformLower TransformOp = "lower"
	// TransformTitle title-cases text.
	TransformTitle TransformOp = "title"
	// TransformTrim trims surrounding spaces.
	TransformTrim TransformOp = "trim"
)

// MaxTransformPrecision is the most decimal places a transform keeps.
const MaxTransformPrecision = 6

// TransformOps returns every transform.
func TransformOps() []TransformOp {
	return []TransformOp{TransformRound, TransformCeil, TransformFloor, TransformTruncate, TransformAbs,
		TransformScale, TransformUpper, TransformLower, TransformTitle, TransformTrim}
}

// Text reports whether the transform applies to text; the others apply to numbers.
func (o TransformOp) Text() bool {
	return o == TransformUpper || o == TransformLower || o == TransformTitle || o == TransformTrim
}

// UsesPrecision reports whether the transform takes a precision.
func (o TransformOp) UsesPrecision() bool {
	return o == TransformRound || o == TransformCeil || o == TransformFloor || o == TransformTruncate
}

// ResultType returns the type a transform produces from a value of type t.
func (o TransformOp) ResultType(t formula.Type) formula.Type {
	switch {
	case o.Text():
		return formula.String
	case o == TransformAbs:
		return t
	default:
		return formula.Decimal
	}
}

// ChartType is the visual encoding of a chart.
type ChartType string

const (
	// ChartBar is vertical bars.
	ChartBar ChartType = "bar"
	// ChartHBar is horizontal bars.
	ChartHBar ChartType = "hbar"
	// ChartLine is lines.
	ChartLine ChartType = "line"
	// ChartArea is filled areas.
	ChartArea ChartType = "area"
	// ChartPie is a pie.
	ChartPie ChartType = "pie"
	// ChartDonut is a donut.
	ChartDonut ChartType = "donut"
	// ChartScatter plots two measures against each other.
	ChartScatter ChartType = "scatter"
	// ChartKPI is a single number tile.
	ChartKPI ChartType = "kpi"
	// ChartMap plots rows at coordinates.
	ChartMap ChartType = "map"
)

// ChartTypes returns every chart type.
func ChartTypes() []ChartType {
	return []ChartType{ChartBar, ChartHBar, ChartLine, ChartArea, ChartPie, ChartDonut, ChartScatter, ChartKPI, ChartMap}
}

// ChartRule is what a chart type requires, published so a builder offers what validation accepts.
type ChartRule struct {
	// Type is the chart type.
	Type ChartType `json:"type"`
	// Dimension is true when the chart plots series against a dimension column.
	Dimension bool `json:"dimension"`
	// SingleSeries is true when the chart draws exactly one value column.
	SingleSeries bool `json:"singleSeries"`
	// Coordinates is true when the chart positions rows by latitude and longitude.
	Coordinates bool `json:"coordinates"`
	// Stacking is true when the series can be stacked.
	Stacking bool `json:"stacking"`
}

// ChartRules returns the rule of every chart type.
func ChartRules() []ChartRule {
	out := make([]ChartRule, 0, len(ChartTypes()))
	for _, t := range ChartTypes() {
		out = append(out, ChartRule{
			Type:         t,
			Dimension:    !slices.Contains([]ChartType{ChartScatter, ChartKPI, ChartMap}, t),
			SingleSeries: slices.Contains([]ChartType{ChartPie, ChartDonut, ChartKPI, ChartScatter, ChartMap}, t),
			Coordinates:  t == ChartMap,
			Stacking:     slices.Contains([]ChartType{ChartBar, ChartHBar, ChartArea}, t),
		})
	}
	return out
}

func chartRule(t ChartType) ChartRule {
	for _, r := range ChartRules() {
		if r.Type == t {
			return r
		}
	}
	return ChartRule{}
}

const (
	// MaxCharts is the most charts a report carries.
	MaxCharts = 6
	// MaxChartSeries is the most value columns a chart draws.
	MaxChartSeries = 12
	// MaxChartCategories is the largest chart category limit.
	MaxChartCategories = 200
	// PivotOther is the value of a pivot's "Other" cell, as in "revenue:__other__".
	PivotOther = "__other__"
	// PivotSeparator joins a pivoted column id and a value into a cell id.
	PivotSeparator = ":"
)
