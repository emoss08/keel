// Package table is the typed, columnar result that analytics produces and its writers and diffs
// consume: a schema of columns and a stream of row batches, each column held in one typed
// vector with a null bitmap.
//
// The analytics runner implements Reader over a Postgres result. It decodes each column's
// binary values (pgx's RawValues) straight into the slices of a value.Vector, setting a null
// bit for a nil raw value, and hands the batch over without copying or boxing anything. Every
// writer in analytics/export and analytics/diff reads only this interface, so a result can be
// written while it streams, at any size.
//
// Table and Builder are the in-memory implementation, for results read back from storage
// (export.ReadJSON), tests and application code.
package table

import (
	"context"
	"fmt"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/value"
)

// Column describes one column of a result.
type Column struct {
	// ID is the column's stable identifier in the report definition ("revenue").
	ID string `json:"id"`
	// Label is the column's display name, already translated.
	Label string `json:"label"`
	// Type is the type of the column's values, which decides the vector slice that holds them.
	Type value.Type `json:"type"`
	// Display is the column's resolved display (format.Resolve).
	Display format.Resolved `json:"display"`
	// Sensitivity is the highest sensitivity of the fields the column reads.
	Sensitivity Sensitivity `json:"sensitivity,omitempty"`
}

// Batch is a run of rows, column by column. Columns[i] holds column i of the schema and its
// vectors are at least Len long.
type Batch struct {
	// Len is the number of rows.
	Len int
	// Levels gives each row's grouping level; nil means every row is a detail row.
	Levels []Level
	// Columns holds one vector per schema column, in schema order.
	Columns []value.Vector
}

// Level returns row i's grouping level.
func (b *Batch) Level(i int) Level {
	if b.Levels == nil {
		return Detail
	}
	return b.Levels[i]
}

// Reader is a result read batch by batch.
//
// Next returns the next batch, or io.EOF after the last one. A batch, its vectors and their
// contents are valid until the next call to Next, so a reader may reuse its buffers; a caller
// that keeps rows copies them (Collect does). Truncated is meaningful after io.EOF and reports
// that rows beyond a row limit were left out of the result.
type Reader interface {
	// Columns returns the result's schema. It is fixed for the life of the reader.
	Columns() []Column
	// Next returns the next batch, or io.EOF when there are no more.
	Next(ctx context.Context) (*Batch, error)
	// Truncated reports, after io.EOF, that the result was cut at a row limit.
	Truncated() bool
}

// Level is a row's grouping level. A result with subtotals and a grand total (GROUPING SETS)
// interleaves summary rows with detail rows; the level says which is which.
type Level uint8

// The grouping levels. Levels between Detail and Total are subtotals: Subtotal(n) aggregates
// over the n innermost grouping columns, so a higher level is coarser.
const (
	// Detail is an ordinary row.
	Detail Level = 0
	// Total is the grand total over every row.
	Total Level = 255
)

// Subtotal returns the level of a subtotal that rolls up the n innermost grouping columns, n
// from 1 to 254.
func Subtotal(n int) Level { return Level(min(max(n, 1), 254)) }

// IsSubtotal reports whether the level is a subtotal.
func (l Level) IsSubtotal() bool { return l != Detail && l != Total }

// Sensitivity is how sensitive a column's data is, from least to most. A column that reads
// several fields carries the highest of their sensitivities.
type Sensitivity uint8

// The sensitivities.
const (
	// Public data may be shown to anyone who can see the report.
	Public Sensitivity = iota
	// Internal data is for the organization's own staff.
	Internal
	// Restricted data is for staff who need it.
	Restricted
	// Confidential data is the most protected.
	Confidential
)

// sensitivityNames are the sensitivities' names, in order.
var sensitivityNames = [...]string{"public", "internal", "restricted", "confidential"}

// String returns the sensitivity's name.
func (s Sensitivity) String() string {
	if int(s) < len(sensitivityNames) {
		return sensitivityNames[s]
	}
	return ""
}

// MarshalText writes the sensitivity's name.
func (s Sensitivity) MarshalText() ([]byte, error) {
	if int(s) >= len(sensitivityNames) {
		return nil, fmt.Errorf("table: invalid sensitivity %d", s)
	}
	return []byte(sensitivityNames[s]), nil
}

// UnmarshalText reads a sensitivity's name.
func (s *Sensitivity) UnmarshalText(b []byte) error {
	for i, name := range sensitivityNames {
		if name == string(b) {
			*s = Sensitivity(i)
			return nil
		}
	}
	return fmt.Errorf("table: unknown sensitivity %q", b)
}
