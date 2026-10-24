package analytics

import (
	"context"
	"io"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// Result is a report's rows held in memory: the batches the dynamic query layer decoded, read
// as table.Reader without copying, since a value.Vector has the layout of a keel.DynColumn. It
// can be read as many times as needed with Reader.
type Result struct {
	columns   []table.Column
	batches   []*table.Batch
	truncated bool
	cursor    string
	rollup    string
	cached    bool

	next int
}

// Columns returns the output schema.
func (r *Result) Columns() []table.Column { return r.columns }

// Next returns the next batch, or io.EOF after the last one.
func (r *Result) Next(ctx context.Context) (*table.Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.next >= len(r.batches) {
		return nil, io.EOF
	}
	r.next++
	return r.batches[r.next-1], nil
}

// Truncated reports that rows past the limit were left out.
func (r *Result) Truncated() bool { return r.truncated }

// Cursor returns the cursor of the page after this one, empty on the last page or for a result
// that is not a page.
func (r *Result) Cursor() string { return r.cursor }

// Rollup names the rollup table that answered the report, or "".
func (r *Result) Rollup() string { return r.rollup }

// Cached reports that the result came from the cache.
func (r *Result) Cached() bool { return r.cached }

// Len returns the number of rows, detail and summary.
func (r *Result) Len() int {
	n := 0
	for _, b := range r.batches {
		n += b.Len
	}
	return n
}

// Reader returns a new reader over the result's batches, from the first.
func (r *Result) Reader() table.Reader {
	return &Result{columns: r.columns, batches: r.batches, truncated: r.truncated, cursor: r.cursor,
		rollup: r.rollup, cached: r.cached}
}

// batchOf turns a decoded dynamic result into a batch of the output schema: each output column's
// vector is the dynamic column's slices, and the level column's masks become row levels. A result
// longer than limit (the probe row) is cut to limit rows.
func (c *Compiled) batchOf(res *keel.DynResult, limit int) (*table.Batch, bool, error) {
	b := &table.Batch{Len: res.Rows, Columns: make([]value.Vector, len(c.Columns))}
	truncated := false
	if limit > 0 && b.Len > limit {
		b.Len, truncated = limit, true
	}
	for i, slot := range c.slots {
		if slot >= len(res.Columns) {
			return nil, false, fail(CodeNotAllowed, "", "the statement returned %d columns", len(res.Columns))
		}
		v, err := vectorOf(&res.Columns[slot], c.Columns[i].Type)
		if err != nil {
			return nil, false, err
		}
		b.Columns[i] = v
	}
	if c.level >= 0 && c.level < len(res.Columns) {
		masks := &res.Columns[c.level]
		b.Levels = make([]table.Level, res.Rows)
		for i := range res.Rows {
			if i < len(masks.Int64) {
				b.Levels[i] = c.levelOf(masks.Int64[i])
			}
		}
	}
	return b, truncated, nil
}

// vectorOf returns a dynamic column as a vector of type t, sharing its slices. A numeric column
// read as another numeric type is converted.
func vectorOf(dc *keel.DynColumn, t value.Type) (value.Vector, error) {
	v := value.Vector{Nulls: value.Bitmap(dc.Nulls)}
	switch {
	case t.Textual() && dc.Type == keel.TypeString:
		v.String = dc.String
	case (t == value.Int || t == value.Epoch) && dc.Type == keel.TypeInt64:
		v.Int64 = dc.Int64
	case t == value.Decimal && dc.Type == keel.TypeNumeric:
		v.Numeric = dc.Numeric
	case t == value.Float && dc.Type == keel.TypeFloat64:
		v.Float64 = dc.Float64
	case t == value.Bool && dc.Type == keel.TypeBool:
		v.Bool = dc.Bool
	case t == value.Time && (dc.Type == keel.TypeTime || dc.Type == keel.TypeDate):
		v.Time = dc.Time
	case t == value.JSON && dc.Type == keel.TypeJSON:
		v.JSON = dc.JSON
	default:
		return convertVector(dc, t, v)
	}
	return v, nil
}

// convertVector converts a numeric column decoded as another numeric type: an integer or a float
// read as a decimal, an integer or a decimal read as a float, a decimal read as an integer.
func convertVector(dc *keel.DynColumn, t value.Type, v value.Vector) (value.Vector, error) {
	n := len(dc.Int64) + len(dc.Float64) + len(dc.Numeric)
	switch {
	case t == value.Decimal && dc.Type == keel.TypeInt64:
		v.Numeric = make([]keel.Decimal, n)
		for i, x := range dc.Int64 {
			v.Numeric[i] = value.DecimalFromInt(x)
		}
	case t == value.Decimal && dc.Type == keel.TypeFloat64:
		v.Numeric = make([]keel.Decimal, n)
		for i, x := range dc.Float64 {
			v.Numeric[i] = value.DecimalFromFloat(x)
		}
	case t == value.Float && dc.Type == keel.TypeInt64:
		v.Float64 = make([]float64, n)
		for i, x := range dc.Int64 {
			v.Float64[i] = float64(x)
		}
	case t == value.Float && dc.Type == keel.TypeNumeric:
		v.Float64 = make([]float64, n)
		for i, x := range dc.Numeric {
			v.Float64[i], _ = x.Float64()
		}
	case (t == value.Int || t == value.Epoch) && dc.Type == keel.TypeNumeric:
		v.Int64 = make([]int64, n)
		for i, x := range dc.Numeric {
			f, _ := x.Float64()
			v.Int64[i] = int64(f)
		}
	default:
		return v, fail(CodeNotAllowed, "", "column %s returned %s values, not %s", dc.Name, dc.Type, t)
	}
	return v, nil
}

// batchBytes estimates the memory a batch's values take, for the byte cap.
func batchBytes(b *table.Batch) int64 {
	var n int64
	for i := range b.Columns {
		v := &b.Columns[i]
		n += int64(8 * (len(v.Int64) + len(v.Float64) + len(v.Bool) + len(v.Time)))
		for _, s := range v.String[:min(len(v.String), b.Len)] {
			n += int64(len(s))
		}
		for _, s := range v.Numeric[:min(len(v.Numeric), b.Len)] {
			n += int64(len(s))
		}
		for _, s := range v.JSON[:min(len(v.JSON), b.Len)] {
			n += int64(len(s))
		}
	}
	return n
}
