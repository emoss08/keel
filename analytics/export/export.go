// Package export writes a result (a table.Reader) as CSV, a JSON envelope, an XLSX workbook or
// an Arrow IPC stream, streaming: each writer holds one batch at a time, so memory stays flat
// however many rows the result has.
//
// The human formats (formatted CSV and XLSX) render every cell through the column's display
// (analytics/format) in the requested locale and time zone, label subtotal and total rows, and
// end with a notice when the result was truncated. The machine formats (raw CSV, the JSON
// envelope and Arrow) carry exact values: decimals keep every digit and instants stay instants.
//
// Every writer stops at Options.MaxBytes: once the output reaches the limit it reads no more
// rows, writes the truncation notice (or marker) and closes the document, so the result is
// always a complete file.
package export

import (
	"io"
	"time"

	"golang.org/x/text/language"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// Options are what every writer takes.
type Options struct {
	// Meta describes the report run.
	Meta Meta
	// Locale is the language formatted cells are written in; the zero tag is American English.
	Locale language.Tag
	// Location is the time zone dates are written in; nil is UTC.
	Location *time.Location
	// MaxBytes stops the output once it reaches this many bytes; zero is no limit. A writer
	// checks it between rows (every 512 rows for XLSX, whose output is compressed, and every
	// batch for Arrow), so the file can pass the limit by that much plus its closing bytes.
	MaxBytes int64
	// Text holds the words the writers put in a document; empty fields keep the English
	// defaults.
	Text Text
}

// Meta describes a report run.
type Meta struct {
	// Title is the report's name.
	Title string `json:"title"`
	// Description is the report's description.
	Description string `json:"description,omitempty"`
	// GeneratedAt is when the run started.
	GeneratedAt time.Time `json:"generatedAt"`
	// RequestedBy names who asked for the run.
	RequestedBy string `json:"requestedBy,omitempty"`
	// Params are the run's parameter values, in the order the report declares them.
	Params []Param `json:"params,omitempty"`
}

// Param is one parameter value of a run.
type Param struct {
	// Name is the parameter's name.
	Name string `json:"name"`
	// Value is its value.
	Value any `json:"value"`
}

// Text holds the words a writer puts in a document, for an application to translate.
type Text struct {
	// Total labels the grand-total row ("Total").
	Total string
	// Subtotal labels a subtotal row ("Subtotal").
	Subtotal string
	// RowsTruncated is the notice written when the result was cut at a row limit.
	RowsTruncated string
	// BytesTruncated is the notice written when the output reached MaxBytes.
	BytesTruncated string
	// DataSheet names an XLSX data sheet ("Data"); later sheets add " (2)", " (3)", ….
	DataSheet string
	// InfoSheet names the XLSX sheet describing the run ("Report").
	InfoSheet string
	// Report, Description, GeneratedAt, RequestedBy and Parameter label the info sheet's rows.
	Report, Description, GeneratedAt, RequestedBy, Parameter string
}

// withDefaults fills empty words with their English defaults.
func (t Text) withDefaults() Text {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&t.Total, "Total")
	def(&t.Subtotal, "Subtotal")
	def(&t.RowsTruncated, "Results were truncated at the row limit; narrow your filters or use a larger export format.")
	def(&t.BytesTruncated, "Results were truncated at the size limit; narrow your filters or choose fewer columns.")
	def(&t.DataSheet, "Data")
	def(&t.InfoSheet, "Report")
	def(&t.Report, "Report")
	def(&t.Description, "Description")
	def(&t.GeneratedAt, "Generated At")
	def(&t.RequestedBy, "Requested By")
	def(&t.Parameter, "Parameter")
	return t
}

// notice returns the truncation notice for why.
func (t Text) notice(why Truncation) string {
	if why == TruncatedBytes {
		return t.BytesTruncated
	}
	return t.RowsTruncated
}

// Stats reports what a writer wrote.
type Stats struct {
	// Rows is the number of detail rows written.
	Rows int64
	// Bytes is the size of the output.
	Bytes int64
	// Truncated says why the output holds fewer rows than the result, if it does.
	Truncated Truncation
}

// Truncation is why an output was cut short.
type Truncation uint8

// The truncations.
const (
	// NotTruncated is a complete output.
	NotTruncated Truncation = iota
	// TruncatedRows is a result the query cut at its row limit.
	TruncatedRows
	// TruncatedBytes is an output the writer stopped at MaxBytes.
	TruncatedBytes
)

// String returns "", "rows" or "bytes".
func (t Truncation) String() string {
	switch t {
	case TruncatedRows:
		return "rows"
	case TruncatedBytes:
		return "bytes"
	}
	return ""
}

// location returns the time zone, UTC when none is set.
func (o Options) location() *time.Location {
	if o.Location == nil {
		return time.UTC
	}
	return o.Location
}

// formatters returns a formatter per column in the options' locale and time zone.
func (o Options) formatters(cols []table.Column) []*format.Formatter {
	out := make([]*format.Formatter, len(cols))
	for i, c := range cols {
		out[i] = format.New(c.Type, c.Display, format.Options{Locale: o.Locale, Location: o.Location})
	}
	return out
}

// over reports whether a writer that has produced n bytes must stop.
func (o Options) over(n int64) bool { return o.MaxBytes > 0 && n >= o.MaxBytes }

// labelColumn returns the column that carries a summary row's label: the first null one, or -1.
func labelColumn(b *table.Batch, row int) int {
	for i := range b.Columns {
		if b.Columns[i].Null(row) {
			return i
		}
	}
	return -1
}

// counter counts the bytes written through it.
type counter struct {
	w io.Writer
	n int64
}

// Write writes p and counts it.
func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// numberLike reports whether a column's values are numbers whose rendering starts with a sign
// rather than with text an author or user wrote.
func numberLike(t value.Type) bool { return t.Numeric() || t == value.Epoch }
