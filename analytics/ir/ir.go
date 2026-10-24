// Package ir is the report definition of keel/analytics: the types an application stores for a
// user-built report, their JSON encoding and JSON Schema, versioning with upgrades, validation
// with user-facing errors, dashboard filter merging and drill-through rewriting.
//
// The package does no SQL and touches no database. Checks that need the catalog run through the
// Catalog interface, which the compiler implements.
package ir

import (
	_ "embed"
	"encoding/json"

	"github.com/emoss08/keel/analytics/formula"
)

//go:generate go run ./internal/gen

// Version is the version of the definition format this package reads and writes.
const Version = 2

//go:embed schema.json
var schema []byte

// Schema returns the JSON Schema of a Definition, generated from the Go types.
func Schema() []byte { return append([]byte(nil), schema...) }

// Definition is a report: the entity it starts from, its output columns, filters, layout,
// parameters and charts.
type Definition struct {
	// Version is the definition format version; Decode upgrades older versions.
	Version int `json:"version"`
	// Entity is the catalog key of the entity the report starts from, such as "shipment".
	Entity string `json:"entity"`
	// Columns are the report's columns in display order.
	Columns []Column `json:"columns"`
	// Filters narrow the source rows before grouping.
	Filters *Filter `json:"filters,omitempty"`
	// Having narrows the grouped rows by output columns.
	Having *Filter `json:"having,omitempty"`
	// Groupings adds subtotal and total rows.
	Groupings *Groupings `json:"groupings,omitempty"`
	// Top keeps the first N rows per group by a column.
	Top *Top `json:"top,omitempty"`
	// Pivot spreads measure columns across the values of a field.
	Pivot *Pivot `json:"pivot,omitempty"`
	// Sort orders the output rows.
	Sort []Sort `json:"sort,omitempty"`
	// Limit caps the number of output rows; zero means the runner's limit.
	Limit Count `json:"limit,omitzero"`
	// Page asks for one page of the sorted output.
	Page *Page `json:"page,omitempty"`
	// Parameters are the values the report asks for when it runs.
	Parameters []Parameter `json:"parameters,omitempty"`
	// Charts draw the output; they never change what is queried.
	Charts []Chart `json:"charts,omitempty"`
}

// Column is one column of a report: exactly one of a dimension, a measure, a calculation or a
// window over another column.
type Column struct {
	// ID names the column in formulas, sort, charts and output; letters, digits and underscores.
	ID string `json:"id"`
	// Label is the column heading; empty derives one from the field.
	Label string `json:"label,omitempty"`
	// Hidden computes the column for other columns, having or sort without returning it.
	Hidden bool `json:"hidden,omitempty"`
	// Dimension groups by a field.
	Dimension *Dimension `json:"dimension,omitempty"`
	// Measure aggregates a field.
	Measure *Measure `json:"measure,omitempty"`
	// Calc computes the column from a formula over other columns, fields and parameters.
	Calc *formula.Expr `json:"calc,omitempty"`
	// Window computes the column across the grouped rows.
	Window *Window `json:"window,omitempty"`
	// Transform rewrites the column's value in SQL.
	Transform *Transform `json:"transform,omitempty"`
	// Format is a format hint for the display spec, such as "money" or "percent".
	Format string `json:"format,omitempty"`
	// Display is the author's display spec, an object of the analytics/format package.
	Display json.RawMessage `json:"display,omitempty"`
}

// Dimension groups the report by a field, optionally bucketed in time or banded into ranges.
type Dimension struct {
	// Field is the dotted path of the field, such as "customer.name".
	Field string `json:"field"`
	// Bucket truncates a date or time to a period, or names a parameter that chooses one.
	Bucket Grain `json:"bucket,omitzero"`
	// Band groups a number into ranges.
	Band *Band `json:"band,omitempty"`
}

// Measure aggregates a field over each group, optionally over a subset of its rows.
type Measure struct {
	// Field is the dotted path of the aggregated field; empty counts rows.
	Field string `json:"field,omitempty"`
	// Agg is the aggregation.
	Agg Agg `json:"agg"`
	// Filter narrows the rows this measure aggregates without narrowing the report.
	Filter *Filter `json:"filter,omitempty"`
}

// Window computes a value across the grouped rows from another column.
type Window struct {
	// Of is the column the window reads.
	Of string `json:"of"`
	// Fn is the window function.
	Fn WindowFn `json:"fn"`
	// Over is the column that orders the rows, for running, moving, change and first/last.
	Over string `json:"over,omitempty"`
	// Partition restarts the window for each combination of these dimension columns.
	Partition []string `json:"partition,omitempty"`
	// Offset is how many rows back change and percent_change compare with; zero means one.
	Offset int `json:"offset,omitempty"`
	// Size is the number of rows a moving average spans.
	Size int `json:"size,omitempty"`
	// Desc ranks the largest value first.
	Desc bool `json:"desc,omitempty"`
}

// Transform rewrites a value in SQL, so exports, sorting and filters see the rewritten value.
type Transform struct {
	// Op is the transform.
	Op TransformOp `json:"op"`
	// Precision is the number of decimal places for round, ceil, floor and truncate.
	Precision *int `json:"precision,omitempty"`
	// Factor is the multiplier for scale.
	Factor *float64 `json:"factor,omitempty"`
}

// Band groups a number into ranges: a fixed width, or ascending boundaries where the last range
// is open-ended. A grouped value is the lower bound of its range.
type Band struct {
	// Width is the size of every range.
	Width float64 `json:"width,omitempty"`
	// Edges are ascending range boundaries.
	Edges []float64 `json:"edges,omitempty"`
}

// Groupings adds subtotal rows and a grand total row, computed in the same statement as the rows.
type Groupings struct {
	// Subtotals lists dimension columns; each adds rows grouped by that column and every
	// dimension column before it.
	Subtotals []string `json:"subtotals,omitempty"`
	// Total adds a grand total row.
	Total bool `json:"total,omitempty"`
}

// Top keeps the first N rows of each group, ranked by a column.
type Top struct {
	// N is how many rows to keep, or a parameter.
	N Count `json:"n"`
	// By is the column that ranks the rows.
	By string `json:"by"`
	// Per lists the dimension columns that form the groups; empty ranks the whole result.
	Per []string `json:"per,omitempty"`
	// Bottom keeps the smallest values instead of the largest.
	Bottom bool `json:"bottom,omitempty"`
}

// Pivot spreads measure columns into one column per value of a field.
type Pivot struct {
	// Field is the dotted path of the field whose values become columns.
	Field string `json:"field"`
	// Values lists the values to spread, or names a multi-value parameter.
	Values Values `json:"values,omitzero"`
	// Top discovers the values when the report runs: the first N by a measure.
	Top *PivotTop `json:"top,omitempty"`
	// Labels are column headings for listed values, in the same order.
	Labels []string `json:"labels,omitempty"`
	// Columns are the measure columns to spread.
	Columns []string `json:"columns"`
	// Other adds a column for every value not listed or discovered, nulls included.
	Other bool `json:"other,omitempty"`
}

// PivotTop discovers pivot values: the N values with the largest measure.
type PivotTop struct {
	// N is how many values to discover, or a parameter.
	N Count `json:"n"`
	// By is the measure column that ranks the values.
	By string `json:"by"`
}

// Sort orders the output by a column.
type Sort struct {
	// Column is an output column id, or a pivot cell "column:value".
	Column string `json:"column"`
	// Desc sorts largest first.
	Desc bool `json:"desc,omitempty"`
}

// Page asks for one page of the sorted output with keyset paging.
type Page struct {
	// Size is the number of rows in the page.
	Size int `json:"size"`
	// After is the opaque cursor of the previous page's last row.
	After string `json:"after,omitempty"`
}

// Parameter is a value the report asks for when it runs.
type Parameter struct {
	// Name is how filters, formulas and settings reference the parameter.
	Name string `json:"name"`
	// Label is the prompt shown to the person running the report.
	Label string `json:"label,omitempty"`
	// Type is the value type.
	Type formula.Type `json:"type"`
	// Required refuses to run without a value or a default.
	Required bool `json:"required,omitempty"`
	// Multi takes a list of values.
	Multi bool `json:"multi,omitempty"`
	// Default is used when no value is given; a list when Multi.
	Default any `json:"default,omitempty"`
	// Allowed restricts the values that can be given.
	Allowed []any `json:"allowed,omitempty"`
	// Entity is the catalog key of the entity a ref parameter points at.
	Entity string `json:"entity,omitempty"`
}

// Chart draws columns the report already returns.
type Chart struct {
	// ID names the chart.
	ID string `json:"id"`
	// Type is the visual encoding.
	Type ChartType `json:"type"`
	// Title is the chart heading.
	Title string `json:"title,omitempty"`
	// X is the dimension column on the category axis, or the measure on a scatter plot's x axis.
	X string `json:"x,omitempty"`
	// Series are the value columns drawn.
	Series []string `json:"series,omitempty"`
	// Stacked stacks the series.
	Stacked bool `json:"stacked,omitempty"`
	// HideLegend hides the legend.
	HideLegend bool `json:"hideLegend,omitempty"`
	// ShowValues labels each mark with its value.
	ShowValues bool `json:"showValues,omitempty"`
	// Curved smooths lines.
	Curved bool `json:"curved,omitempty"`
	// Limit caps the categories drawn; zero draws all.
	Limit int `json:"limit,omitempty"`
	// Compare is the value column a KPI tile compares with.
	Compare string `json:"compare,omitempty"`
	// Goal draws a target line or a KPI target.
	Goal *Goal `json:"goal,omitempty"`
	// Lat is the latitude column of a map.
	Lat string `json:"lat,omitempty"`
	// Lng is the longitude column of a map.
	Lng string `json:"lng,omitempty"`
	// PointLabel names each point of a map; empty uses the first dimension.
	PointLabel string `json:"pointLabel,omitempty"`
}

// Goal is a chart target: a fixed number or a value column.
type Goal struct {
	// Value is a fixed target.
	Value *float64 `json:"value,omitempty"`
	// Column is a value column holding the target.
	Column string `json:"column,omitempty"`
	// Label names the target.
	Label string `json:"label,omitempty"`
}

// ColumnByID returns the column with the given id.
func (d *Definition) ColumnByID(id string) (*Column, bool) {
	for i := range d.Columns {
		if d.Columns[i].ID == id {
			return &d.Columns[i], true
		}
	}
	return nil, false
}

// Parameter returns the parameter with the given name.
func (d *Definition) Parameter(name string) (*Parameter, bool) {
	for i := range d.Parameters {
		if d.Parameters[i].Name == name {
			return &d.Parameters[i], true
		}
	}
	return nil, false
}
