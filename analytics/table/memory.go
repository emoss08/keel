package table

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/value"
)

// Table is a result held in memory. Its Reader yields its batches in order and can be taken as
// many times as needed.
type Table struct {
	// Columns is the schema.
	Columns []Column
	// Batches holds the rows.
	Batches []*Batch
	// Truncated reports that the result was cut at a row limit.
	Truncated bool
}

// Reader returns a reader over the table's batches.
func (t *Table) Reader() Reader { return &tableReader{t: t} }

// Len returns the number of rows, detail and summary.
func (t *Table) Len() int {
	n := 0
	for _, b := range t.Batches {
		n += b.Len
	}
	return n
}

// tableReader reads a Table's batches in order.
type tableReader struct {
	t    *Table
	next int
}

// Columns returns the table's schema.
func (r *tableReader) Columns() []Column { return r.t.Columns }

// Next returns the next batch, or io.EOF after the last.
func (r *tableReader) Next(ctx context.Context) (*Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.next >= len(r.t.Batches) {
		return nil, io.EOF
	}
	r.next++
	return r.t.Batches[r.next-1], nil
}

// Truncated reports the table's truncation.
func (r *tableReader) Truncated() bool { return r.t.Truncated }

// Collect reads r to the end into a Table, copying each batch so the reader may reuse its
// buffers.
func Collect(ctx context.Context, r Reader) (*Table, error) {
	t := &Table{Columns: r.Columns()}
	for {
		b, err := r.Next(ctx)
		if errors.Is(err, io.EOF) {
			t.Truncated = r.Truncated()
			return t, nil
		}
		if err != nil {
			return nil, err
		}
		t.Batches = append(t.Batches, cloneBatch(b))
	}
}

// cloneBatch deep-copies a batch.
func cloneBatch(b *Batch) *Batch {
	out := &Batch{Len: b.Len, Columns: make([]value.Vector, len(b.Columns))}
	if b.Levels != nil {
		out.Levels = slices.Clone(b.Levels[:b.Len])
	}
	for i := range b.Columns {
		out.Columns[i] = cloneVector(&b.Columns[i], b.Len)
	}
	return out
}

// cloneVector deep-copies the first n rows of a vector.
func cloneVector(v *value.Vector, n int) value.Vector {
	out := value.Vector{Nulls: slices.Clone(v.Nulls)}
	switch {
	case v.String != nil:
		out.String = slices.Clone(v.String[:n])
	case v.Int64 != nil:
		out.Int64 = slices.Clone(v.Int64[:n])
	case v.Float64 != nil:
		out.Float64 = slices.Clone(v.Float64[:n])
	case v.Bool != nil:
		out.Bool = slices.Clone(v.Bool[:n])
	case v.Time != nil:
		out.Time = slices.Clone(v.Time[:n])
	case v.Numeric != nil:
		out.Numeric = slices.Clone(v.Numeric[:n])
	case v.JSON != nil:
		out.JSON = make([][]byte, n)
		for i, x := range v.JSON[:n] {
			out.JSON[i] = slices.Clone(x)
		}
	}
	return out
}

// BatchRows is the number of rows a Builder puts in each batch.
const BatchRows = 1024

// Builder builds a Table row by row from Go values.
type Builder struct {
	t   *Table
	cur *Batch
}

// NewBuilder returns a builder for a table with the given schema.
func NewBuilder(columns []Column) *Builder {
	return &Builder{t: &Table{Columns: columns}}
}

// Append adds a detail row. See AppendLevel for the values each column type accepts.
func (b *Builder) Append(values ...any) error { return b.AppendLevel(Detail, values...) }

// AppendLevel adds a row at a grouping level. There is one value per column, in schema order;
// nil is null. A column accepts:
//
//	String, Enum, Ref  string
//	Int                any integer type
//	Epoch              any integer type, or a time.Time
//	Decimal            keel.Decimal, a decimal string, any integer type or a float
//	Float              float64, float32 or any integer type
//	Bool               bool
//	Time               time.Time
//	JSON               []byte or string holding JSON text
func (b *Builder) AppendLevel(level Level, values ...any) error {
	if len(values) != len(b.t.Columns) {
		return fmt.Errorf("table: row has %d values for %d columns", len(values), len(b.t.Columns))
	}
	if b.cur == nil || b.cur.Len == BatchRows {
		b.cur = &Batch{Columns: make([]value.Vector, len(b.t.Columns))}
		b.t.Batches = append(b.t.Batches, b.cur)
	}
	row := b.cur.Len
	for i, x := range values {
		if err := appendValue(&b.cur.Columns[i], b.t.Columns[i].Type, row, x); err != nil {
			b.truncate(row)
			return fmt.Errorf("table: column %q: %w", b.t.Columns[i].ID, err)
		}
	}
	if level != Detail && b.cur.Levels == nil {
		b.cur.Levels = make([]Level, row, BatchRows)
	}
	if b.cur.Levels != nil {
		b.cur.Levels = append(b.cur.Levels, level)
	}
	b.cur.Len++
	return nil
}

// truncate drops a partly appended row from the current batch.
func (b *Builder) truncate(row int) {
	for i := range b.cur.Columns {
		v := &b.cur.Columns[i]
		v.String = clip(v.String, row)
		v.Int64 = clip(v.Int64, row)
		v.Float64 = clip(v.Float64, row)
		v.Bool = clip(v.Bool, row)
		v.Time = clip(v.Time, row)
		v.Numeric = clip(v.Numeric, row)
		v.JSON = clip(v.JSON, row)
		if v.Nulls.Has(row) {
			v.Nulls.Clear(row)
		}
	}
}

// clip shortens s to n elements when it is longer.
func clip[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Table returns the table built so far. Appending after Table adds to the same table.
func (b *Builder) Table() *Table { return b.t }

// errType reports a Go value a column cannot hold.
var errType = errors.New("value does not fit the column's type")

// appendValue appends a Go value to a column of type t as row row.
func appendValue(v *value.Vector, t value.Type, row int, x any) error {
	if x == nil {
		setNull(v, row)
		return appendZero(v, t)
	}
	switch t {
	case value.String, value.Enum, value.Ref:
		s, ok := x.(string)
		if !ok {
			return fmt.Errorf("%w: %T", errType, x)
		}
		v.String = append(v.String, s)
	case value.Int, value.Epoch:
		i, err := intOf(x, t == value.Epoch)
		if err != nil {
			return err
		}
		v.Int64 = append(v.Int64, i)
	case value.Decimal:
		n, err := numericOf(x)
		if err != nil {
			return err
		}
		v.Numeric = append(v.Numeric, n)
	case value.Float:
		f, err := floatOf(x)
		if err != nil {
			return err
		}
		v.Float64 = append(v.Float64, f)
	default:
		return appendOther(v, t, x)
	}
	return nil
}

// appendOther appends a bool, time or JSON value.
func appendOther(v *value.Vector, t value.Type, x any) error {
	switch x := x.(type) {
	case bool:
		if t == value.Bool {
			v.Bool = append(v.Bool, x)
			return nil
		}
	case time.Time:
		if t == value.Time {
			v.Time = append(v.Time, x)
			return nil
		}
	case []byte:
		if t == value.JSON {
			v.JSON = append(v.JSON, x)
			return nil
		}
	case string:
		if t == value.JSON {
			v.JSON = append(v.JSON, []byte(x))
			return nil
		}
	}
	return fmt.Errorf("%w: %T", errType, x)
}

// setNull marks row null, growing the bitmap as needed.
func setNull(v *value.Vector, row int) {
	if len(v.Nulls)*64 <= row {
		v.Nulls = append(v.Nulls, make(value.Bitmap, (row/64)+1-len(v.Nulls))...)
	}
	v.Nulls.Set(row)
}

// appendZero appends the slot of a null row.
func appendZero(v *value.Vector, t value.Type) error {
	switch t {
	case value.String, value.Enum, value.Ref:
		v.String = append(v.String, "")
	case value.Int, value.Epoch:
		v.Int64 = append(v.Int64, 0)
	case value.Decimal:
		v.Numeric = append(v.Numeric, "")
	case value.Float:
		v.Float64 = append(v.Float64, 0)
	case value.Bool:
		v.Bool = append(v.Bool, false)
	case value.Time:
		v.Time = append(v.Time, time.Time{})
	case value.JSON:
		v.JSON = append(v.JSON, nil)
	default:
		return fmt.Errorf("%w: invalid column type", errType)
	}
	return nil
}

// intOf reads an integer, or a time as epoch seconds when epoch is set.
func intOf(x any, epoch bool) (int64, error) {
	switch x := x.(type) {
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return 0, fmt.Errorf("%w: %d overflows int64", errType, x)
		}
		return int64(x), nil
	case time.Time:
		if epoch {
			return x.Unix(), nil
		}
	}
	return 0, fmt.Errorf("%w: %T", errType, x)
}

// numericOf reads a decimal from a keel.Decimal, a decimal string, a float or an integer.
func numericOf(x any) (keel.Decimal, error) {
	switch x := x.(type) {
	case keel.Decimal:
		return value.ParseDecimal(string(x))
	case string:
		return value.ParseDecimal(x)
	case float64:
		return value.DecimalFromFloat(x), nil
	case float32:
		return value.DecimalFromFloat(float64(x)), nil
	}
	i, err := intOf(x, false)
	if err != nil {
		return "", err
	}
	return value.DecimalFromInt(i), nil
}

// floatOf reads a float or an integer.
func floatOf(x any) (float64, error) {
	switch x := x.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	}
	i, err := intOf(x, false)
	return float64(i), err
}
