package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/emoss08/keel/analytics/ir"
)

// DrillOptions configures Drill.
type DrillOptions struct {
	// Policy decides which fields the record list may show; nil allows everything.
	Policy Policy
	// Location is the report's time zone, the one its buckets were computed in; nil is UTC.
	Location *time.Location
	// Params are the parameter values the report ran with.
	Params map[string]any
	// Columns are the record list's columns; empty takes the entity's fields the caller may read,
	// up to ir.MaxDrillColumns.
	Columns []ir.Column
	// PivotValues are the values a discovered or parameter pivot produced, for its Other cell.
	PivotValues []string
	// Limit caps the records; zero leaves the runner's limit.
	Limit int
}

// Drill returns the record list behind one cell of an aggregated report: the report's filters,
// each grouping value of the clicked row (buckets and bands inverted into the half-open ranges
// they covered, in the report's time zone), and the clicked measure's own filter and pivot
// scope. row holds the row's grouping columns, as a result returns them; column is the clicked
// column or pivot cell ("revenue:Texas"), empty for the whole row.
func Drill(ctx context.Context, cat *Catalog, def *ir.Definition, row []ir.DrillValue, column string,
	opts DrillOptions,
) (*ir.Definition, error) {
	if def == nil {
		return nil, fail(CodeInvalid, "", "a definition is required")
	}
	if _, ok := cat.Entity(def.Entity); !ok {
		return nil, fail(CodeInvalid, "/entity", "unknown entity %q", def.Entity)
	}
	cols := opts.Columns
	if len(cols) == 0 {
		cols = defaultDrillColumns(ctx, cat, def.Entity, opts.Policy)
	}
	out, err := ir.Drill(def, ir.DrillRequest{Row: row, Column: column, Location: opts.Location,
		Params: opts.Params, Columns: cols, PivotValues: opts.PivotValues, Limit: opts.Limit},
		ir.CatalogTypes(cat, def.Entity))
	if err != nil {
		return nil, invalid(err)
	}
	return out, nil
}

// defaultDrillColumns returns the entity's own fields the caller may read, up to the record
// list's limit.
func defaultDrillColumns(ctx context.Context, cat *Catalog, entity string, policy Policy) []ir.Column {
	e, _ := cat.Entity(entity)
	dec := newDecisions(cat, policy)
	var out []ir.Column
	for _, f := range e.Fields {
		if len(out) >= ir.MaxDrillColumns-4 {
			break
		}
		if !dec.field(ctx, f, nil).Allowed || f.Type == "json" {
			continue
		}
		out = append(out, ir.Column{ID: fmt.Sprintf("f%d", len(out)+1), Dimension: &ir.Dimension{Field: f.Key}})
	}
	return out
}
