package export

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// The Arrow format's constants (Schema.fbs, Message.fbs).
const (
	arrowV5          = 4
	arrowSchema      = 1
	arrowRecordBatch = 3
	arrowInt         = 2
	arrowFloat       = 3
	arrowUtf8        = 5
	arrowBool        = 6
	arrowDecimal     = 7
	arrowTimestamp   = 10
	arrowDouble      = 2
	arrowSecond      = 0
	arrowMicrosecond = 2
	decimalDigits    = 38
)

// ErrDecimalRange reports a decimal with more digits than an Arrow decimal128 holds.
var ErrDecimalRange = errors.New("export: decimal value exceeds Arrow's 38 digits")

// Arrow writes the result as an Arrow IPC stream, for notebooks and dataframes (pyarrow,
// pandas, polars). The stream is written with keel's own encoder, so exporting Arrow costs
// no dependency.
//
// Each column becomes a nullable field named by its ID, with its label and keel type in the
// field metadata ("keel.label", "keel.type"):
//
//	String, Enum, Ref  utf8
//	JSON               utf8 with the canonical arrow.json extension
//	Int                int64
//	Float              float64
//	Bool               bool
//	Decimal            decimal128(38, s)
//	Time               timestamp[us, UTC]
//	Epoch              timestamp[s, UTC]
//
// A decimal column's scale s is the largest scale in the first batch; a later value with more
// places is rounded half away from zero to it, and NaN and the infinities are written as null.
// Only detail rows are written: a dataframe computes its own totals. When the result was cut
// short, a final empty record batch carries "keel.truncated" ("rows" or "bytes") in its
// message metadata.
func Arrow(ctx context.Context, w io.Writer, r table.Reader, opts Options) (Stats, error) {
	aw := &arrowWriter{opts: opts, cols: r.Columns(), count: &counter{w: w}}
	aw.out = bufio.NewWriterSize(aw.count, 64<<10)
	err := aw.run(ctx, r)
	if err == nil {
		err = aw.out.Flush()
	}
	return Stats{Rows: aw.rows, Bytes: aw.count.n, Truncated: aw.truncated}, err
}

// arrowWriter is one Arrow call: the output, the decimal scales and buffers reused per batch.
type arrowWriter struct {
	opts      Options
	cols      []table.Column
	count     *counter
	out       *bufio.Writer
	scales    []int32
	fb        fbBuilder
	body      []byte
	nodes     []byte
	buffers   []byte
	sel       []int
	rows      int64
	truncated Truncation
}

// run writes the schema with the first batch, every batch, the truncation marker and the end of the
// stream.
func (aw *arrowWriter) run(ctx context.Context, r table.Reader) error {
	started := false
	for {
		b, err := r.Next(ctx)
		if errors.Is(err, io.EOF) {
			if r.Truncated() {
				aw.truncated = TruncatedRows
			}
			break
		}
		if err != nil {
			return err
		}
		if !started {
			started = true
			if err := aw.writeSchema(b); err != nil {
				return err
			}
		}
		if err := aw.writeBatch(b); err != nil {
			return err
		}
		if aw.opts.over(aw.count.n + int64(aw.out.Buffered())) {
			aw.truncated = TruncatedBytes
			break
		}
	}
	if !started {
		if err := aw.writeSchema(nil); err != nil {
			return err
		}
	}
	if aw.truncated != NotTruncated {
		if err := aw.writeMarker(); err != nil {
			return err
		}
	}
	_, err := aw.out.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0})
	return err
}

// writeSchema writes the schema message, reading each decimal column's scale from the first batch.
func (aw *arrowWriter) writeSchema(first *table.Batch) error {
	aw.scales = make([]int32, len(aw.cols))
	fields := make(fbTables, len(aw.cols))
	for i, col := range aw.cols {
		if col.Type == value.Decimal && first != nil {
			aw.scales[i] = decimalScale(&first.Columns[i], first.Len)
		}
		fields[i] = aw.field(col, aw.scales[i])
	}
	schema := fbTable{fbI16(0), fbRef(fields)}
	if aw.opts.Meta.Title != "" {
		schema = append(schema, fbRef(keyValues("keel.title", aw.opts.Meta.Title)))
	}
	return aw.message(fbTable{fbI16(arrowV5), fbU8(arrowSchema), fbRef(schema), fbI64(0)}, nil)
}

// field returns the Field table of a column.
func (aw *arrowWriter) field(col table.Column, scale int32) fbTable {
	meta := []string{"keel.label", col.Label, "keel.type", col.Type.String()}
	var typ uint8
	var spec fbTable
	switch col.Type {
	case value.Int:
		typ, spec = arrowInt, fbTable{fbI32(64), fbBool(true)}
	case value.Float:
		typ, spec = arrowFloat, fbTable{fbI16(arrowDouble)}
	case value.Bool:
		typ, spec = arrowBool, fbTable{}
	case value.Decimal:
		typ, spec = arrowDecimal, fbTable{fbI32(decimalDigits), fbI32(scale), fbI32(128)}
	case value.Time:
		typ, spec = arrowTimestamp, fbTable{fbI16(arrowMicrosecond), fbRef(fbString("UTC"))}
	case value.Epoch:
		typ, spec = arrowTimestamp, fbTable{fbI16(arrowSecond), fbRef(fbString("UTC"))}
	default:
		typ, spec = arrowUtf8, fbTable{}
		if col.Type == value.JSON {
			meta = append(meta, "ARROW:extension:name", "arrow.json", "ARROW:extension:metadata", "")
		}
	}
	return fbTable{fbRef(fbString(col.ID)), fbBool(true), fbU8(typ), fbRef(spec), {}, fbRef(fbTables{}),
		fbRef(keyValues(meta...))}
}

// keyValues returns a vector of KeyValue tables from alternating keys and values.
func keyValues(kv ...string) fbTables {
	out := make(fbTables, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, fbTable{fbRef(fbString(kv[i])), fbRef(fbString(kv[i+1]))})
	}
	return out
}

// decimalScale returns the largest scale of the column's finite values.
func decimalScale(v *value.Vector, n int) int32 {
	var scale int32
	for i := range n {
		x := v.Numeric[i]
		if !v.Null(i) && value.NumericFinite(x) && -x.Exp > scale {
			scale = -x.Exp
		}
	}
	return min(scale, decimalDigits)
}

// message writes an encapsulated message: the continuation marker, the metadata length, the
// metadata padded to 8 bytes, then the body.
func (aw *arrowWriter) message(msg fbTable, body []byte) error {
	meta := aw.fb.finish(msg)
	var prefix [8]byte
	binary.LittleEndian.PutUint32(prefix[:4], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(prefix[4:], uint32(len(meta)))
	if _, err := aw.out.Write(prefix[:]); err != nil {
		return err
	}
	if _, err := aw.out.Write(meta); err != nil {
		return err
	}
	_, err := aw.out.Write(body)
	return err
}

// writeBatch writes a batch's detail rows as one record batch.
func (aw *arrowWriter) writeBatch(b *table.Batch) error {
	aw.sel = aw.sel[:0]
	for i := range b.Len {
		if b.Level(i) == table.Detail {
			aw.sel = append(aw.sel, i)
		}
	}
	if len(aw.sel) == 0 {
		return nil
	}
	aw.body, aw.nodes, aw.buffers = aw.body[:0], aw.nodes[:0], aw.buffers[:0]
	for c, col := range aw.cols {
		if err := aw.column(col.Type, &b.Columns[c], aw.scales[c]); err != nil {
			return fmt.Errorf("export: column %q: %w", col.ID, err)
		}
	}
	aw.rows += int64(len(aw.sel))
	return aw.recordBatch(len(aw.sel), nil)
}

// recordBatch writes a record batch message of n rows from the nodes, buffers and body collected.
func (aw *arrowWriter) recordBatch(n int, meta fbTables) error {
	batch := fbTable{fbI64(int64(n)), fbRef(fbStructs{count: len(aw.nodes) / 16, data: aw.nodes}),
		fbRef(fbStructs{count: len(aw.buffers) / 16, data: aw.buffers})}
	msg := fbTable{fbI16(arrowV5), fbU8(arrowRecordBatch), fbRef(batch), fbI64(int64(len(aw.body)))}
	if meta != nil {
		msg = append(msg, fbRef(meta))
	}
	return aw.message(msg, aw.body)
}

// writeMarker writes an empty record batch whose message metadata says why the stream ends
// early.
func (aw *arrowWriter) writeMarker() error {
	aw.body, aw.nodes, aw.buffers = aw.body[:0], aw.nodes[:0], aw.buffers[:0]
	for _, col := range aw.cols {
		aw.node(0, 0)
		for range buffersOf(col.Type) {
			aw.buffer(0)
		}
	}
	return aw.recordBatch(0, keyValues("keel.truncated", aw.truncated.String()))
}

// buffersOf returns the number of buffers a column of type t has: validity and data, plus offsets for
// text.
func buffersOf(t value.Type) int {
	if t.Textual() || t == value.JSON {
		return 3
	}
	return 2
}

// node records a column's length and null count.
func (aw *arrowWriter) node(length, nulls int) {
	aw.nodes = binary.LittleEndian.AppendUint64(aw.nodes, uint64(length))
	aw.nodes = binary.LittleEndian.AppendUint64(aw.nodes, uint64(nulls))
}

// buffer records the body bytes appended since start as one buffer and pads the body to 8.
func (aw *arrowWriter) buffer(start int) {
	aw.buffers = binary.LittleEndian.AppendUint64(aw.buffers, uint64(start))
	aw.buffers = binary.LittleEndian.AppendUint64(aw.buffers, uint64(len(aw.body)-start))
	for len(aw.body)%8 != 0 {
		aw.body = append(aw.body, 0)
	}
}

// column appends a column's validity bitmap and value buffers to the body.
func (aw *arrowWriter) column(t value.Type, v *value.Vector, scale int32) error {
	nulls := 0
	for _, i := range aw.sel {
		if arrowNull(t, v, i) {
			nulls++
		}
	}
	aw.node(len(aw.sel), nulls)
	start := len(aw.body)
	if nulls > 0 {
		aw.body = appendBits(aw.body, aw.sel, func(i int) bool { return !arrowNull(t, v, i) })
	}
	aw.buffer(start)
	start = len(aw.body)
	switch t {
	case value.Int, value.Epoch:
		for _, i := range aw.sel {
			aw.body = binary.LittleEndian.AppendUint64(aw.body, uint64(v.Int64[i]))
		}
	case value.Float:
		for _, i := range aw.sel {
			aw.body = binary.LittleEndian.AppendUint64(aw.body, math.Float64bits(v.Float64[i]))
		}
	case value.Time:
		for _, i := range aw.sel {
			aw.body = binary.LittleEndian.AppendUint64(aw.body, uint64(v.Time[i].UnixMicro()))
		}
	case value.Bool:
		aw.body = appendBits(aw.body, aw.sel, func(i int) bool { return v.Bool[i] })
	case value.Decimal:
		if err := aw.decimals(v, scale); err != nil {
			return err
		}
	default:
		aw.strings(t, v)
		return nil
	}
	aw.buffer(start)
	return nil
}

// arrowNull reports whether row i is written as null; a decimal Arrow cannot hold is.
func arrowNull(t value.Type, v *value.Vector, i int) bool {
	return v.Null(i) || (t == value.Decimal && !value.NumericFinite(v.Numeric[i]))
}

// appendBits appends a bitmap, least significant bit first, of set over the selected rows.
func appendBits(dst []byte, sel []int, set func(int) bool) []byte {
	var cur byte
	for k, i := range sel {
		if set(i) {
			cur |= 1 << (k % 8)
		}
		if k%8 == 7 {
			dst = append(dst, cur)
			cur = 0
		}
	}
	if len(sel)%8 != 0 {
		dst = append(dst, cur)
	}
	return dst
}

// strings appends the offsets and data buffers of a text column.
func (aw *arrowWriter) strings(t value.Type, v *value.Vector) {
	start := len(aw.body)
	var size uint32
	aw.body = binary.LittleEndian.AppendUint32(aw.body, 0)
	for _, i := range aw.sel {
		if !v.Null(i) {
			size += uint32(textLen(t, v, i))
		}
		aw.body = binary.LittleEndian.AppendUint32(aw.body, size)
	}
	aw.buffer(start)
	start = len(aw.body)
	for _, i := range aw.sel {
		if v.Null(i) {
			continue
		}
		if t == value.JSON {
			aw.body = append(aw.body, v.JSON[i]...)
		} else {
			aw.body = append(aw.body, v.String[i]...)
		}
	}
	aw.buffer(start)
}

// textLen returns the byte length of a text value.
func textLen(t value.Type, v *value.Vector, i int) int {
	if t == value.JSON {
		return len(v.JSON[i])
	}
	return len(v.String[i])
}

// decimals appends the 16-byte little-endian two's-complement values of a decimal column at
// its scale.
func (aw *arrowWriter) decimals(v *value.Vector, scale int32) error {
	for _, i := range aw.sel {
		var lo, hi uint64
		if !arrowNull(value.Decimal, v, i) {
			var err error
			if lo, hi, err = decimal128(v.Numeric[i], scale); err != nil {
				return err
			}
		}
		aw.body = binary.LittleEndian.AppendUint64(aw.body, lo)
		aw.body = binary.LittleEndian.AppendUint64(aw.body, hi)
	}
	return nil
}

var (
	decimalLimit = new(big.Int).Exp(big.NewInt(10), big.NewInt(decimalDigits), nil)
	twoTo128     = new(big.Int).Lsh(big.NewInt(1), 128)
	mask64       = new(big.Int).SetUint64(math.MaxUint64)
)

// decimal128 returns n's unscaled value at scale as the low and high words of a 128-bit two's
// complement integer, rounding half away from zero when n has more places.
func decimal128(n pgtype.Numeric, scale int32) (uint64, uint64, error) {
	shift := int64(n.Exp) + int64(scale)
	if n.Int == nil {
		return 0, 0, nil
	}
	if n.Int.IsInt64() && shift >= 0 && shift <= 18 {
		if x, ok := mulPow10(n.Int.Int64(), int(shift)); ok {
			return uint64(x), uint64(x >> 63), nil
		}
	}
	x := new(big.Int).Set(n.Int)
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(abs64(shift)), nil)
	if shift >= 0 {
		x.Mul(x, pow)
	} else {
		roundQuo(x, pow)
	}
	if new(big.Int).Abs(x).Cmp(decimalLimit) >= 0 {
		return 0, 0, ErrDecimalRange
	}
	if x.Sign() < 0 {
		x.Add(x, twoTo128)
	}
	lo := new(big.Int).And(x, mask64).Uint64()
	hi := new(big.Int).Rsh(x, 64).Uint64()
	return lo, hi, nil
}

// roundQuo sets x to x/d rounded half away from zero.
func roundQuo(x, d *big.Int) {
	neg := x.Sign() < 0
	x.Abs(x)
	r := new(big.Int)
	x.QuoRem(x, d, r)
	if r.Lsh(r, 1).Cmp(d) >= 0 {
		x.Add(x, big.NewInt(1))
	}
	if neg {
		x.Neg(x)
	}
}

// mulPow10 returns x × 10^n and whether it fits an int64.
func mulPow10(x int64, n int) (int64, bool) {
	for range n {
		if x > math.MaxInt64/10 || x < math.MinInt64/10 {
			return 0, false
		}
		x *= 10
	}
	return x, true
}

// abs64 returns the absolute value of x.
func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
