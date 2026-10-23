package keel

import (
	"encoding/binary"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// elemKind is the binary encoding of an array type's elements, for the element types keel encodes
// itself.
type elemKind uint8

const (
	elemNone elemKind = iota
	elemText
	elemInt2
	elemInt4
	elemInt8
	elemFloat4
	elemFloat8
	elemBool
	elemTimestamptz
	elemBytea
)

// elemKindOf returns the element kind of an array whose elements use codec c, elemNone when keel
// leaves the array to pgx. Domains share their base type's codec, enums send their label as text.
func elemKindOf(c pgtype.Codec) elemKind {
	switch c.(type) {
	case pgtype.TextCodec, *pgtype.EnumCodec:
		return elemText
	case pgtype.Int2Codec:
		return elemInt2
	case pgtype.Int4Codec:
		return elemInt4
	case pgtype.Int8Codec:
		return elemInt8
	case pgtype.Float4Codec:
		return elemFloat4
	case pgtype.Float8Codec:
		return elemFloat8
	case pgtype.BoolCodec:
		return elemBool
	case *pgtype.TimestamptzCodec:
		return elemTimestamptz
	case pgtype.ByteaCodec:
		return elemBytea
	}
	return elemNone
}

// integer reports the integer element kinds.
func (k elemKind) integer() bool { return k == elemInt2 || k == elemInt4 || k == elemInt8 }

// width is the encoded size of one element of a fixed-width kind, 0 for variable-width kinds.
func (k elemKind) width() int {
	switch k {
	case elemBool:
		return 1
	case elemInt2:
		return 2
	case elemInt4, elemFloat4:
		return 4
	case elemInt8, elemFloat8, elemTimestamptz:
		return 8
	}
	return 0
}

// builtinArrayOIDs are the array types keel encodes on every connection; loaded enum and domain
// array types are added to them.
var builtinArrayOIDs = []uint32{
	pgtype.TextArrayOID, pgtype.VarcharArrayOID, pgtype.BPCharArrayOID, pgtype.NameArrayOID,
	pgtype.Int2ArrayOID, pgtype.Int4ArrayOID, pgtype.Int8ArrayOID,
	pgtype.Float4ArrayOID, pgtype.Float8ArrayOID, pgtype.BoolArrayOID,
	pgtype.TimestamptzArrayOID, pgtype.ByteaArrayOID,
}

// registerArrays replaces the codecs of the builtin array types and of the loaded types that are
// arrays with keel's arrayCodec, so keel's array parameters encode without boxing their elements.
func registerArrays(m *pgtype.Map, loaded []*pgtype.Type) {
	for _, oid := range builtinArrayOIDs {
		if t, ok := m.TypeForOID(oid); ok {
			registerArray(m, t)
		}
	}
	for _, t := range loaded {
		registerArray(m, t)
	}
}

// registerArray registers t again with an arrayCodec when t is an array whose elements keel
// encodes.
func registerArray(m *pgtype.Map, t *pgtype.Type) {
	ac, ok := t.Codec.(*pgtype.ArrayCodec)
	if !ok {
		return
	}
	kind := elemKindOf(ac.ElementType.Codec)
	if kind == elemNone {
		return
	}
	m.RegisterType(&pgtype.Type{Name: t.Name, OID: t.OID, Codec: &arrayCodec{ArrayCodec: ac, kind: kind}})
}

// arrayCodec is pgx's codec of an array type that encodes keel's array parameters itself, in the
// binary format, and leaves every other value and all decoding to pgx.
type arrayCodec struct {
	*pgtype.ArrayCodec
	kind elemKind
}

// PlanEncode returns keel's plan for a keel array parameter whose elements encode as the array's
// element type, and pgx's plan otherwise.
func (c *arrayCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if a, ok := value.(arrayParam); ok && format == pgtype.BinaryFormatCode && fits(a, c.kind) {
		return arrayPlan{kind: c.kind, oid: c.ElementType.OID}
	}
	return c.ArrayCodec.PlanEncode(m, oid, format, value)
}

// arrayPlan encodes a keel array parameter as a binary array of element type oid.
type arrayPlan struct {
	kind elemKind
	oid  uint32
}

// Encode appends the array to buf.
func (p arrayPlan) Encode(value any, buf []byte) ([]byte, error) {
	return value.(arrayParam).appendBinary(buf, p.kind, p.oid)
}

// arrayParam is an array parameter that encodes itself in Postgres's binary array format. keel
// binds these instead of plain slices: pgx boxes every element of a plain slice. A nil slice binds
// NULL, as pgx binds every nil slice before planning.
type arrayParam interface {
	// appendBinary appends the array with elements of kind k and type oid.
	appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error)
}

// fits reports whether the elements of a keel array parameter encode as elements of kind k.
func fits(a arrayParam, k elemKind) bool {
	switch a.(type) {
	case textArray, nullTextArray:
		return k == elemText
	case int8Array, nullInt8Array, int4Array, nullInt4Array, int2Array, nullInt2Array:
		return k.integer()
	case float8Array, nullFloat8Array:
		return k == elemFloat8
	case float4Array, nullFloat4Array:
		return k == elemFloat4
	case boolArray, nullBoolArray:
		return k == elemBool
	case timestamptzArray, nullTimestamptzArray:
		return k == elemTimestamptz
	case byteaArray:
		return k == elemBytea
	}
	return false
}

// elemEncoder appends one non-NULL element, its length first, as an element of kind k.
type elemEncoder[V any] func(buf []byte, k elemKind, v V) ([]byte, error)

// textArray binds a text array; enum and domain arrays over text take it too.
type textArray []string

// nullTextArray binds a text array whose nil elements are NULL.
type nullTextArray []*string

// int8Array binds an integer array; it encodes as int2[], int4[] or int8[], checking the range.
type int8Array []int64

// nullInt8Array binds an integer array whose nil elements are NULL.
type nullInt8Array []*int64

// int4Array binds an integer array; it encodes as int2[], int4[] or int8[], checking the range.
type int4Array []int32

// nullInt4Array binds an integer array whose nil elements are NULL.
type nullInt4Array []*int32

// int2Array binds an integer array; it encodes as int2[], int4[] or int8[].
type int2Array []int16

// nullInt2Array binds an integer array whose nil elements are NULL.
type nullInt2Array []*int16

// float8Array binds a float8 array.
type float8Array []float64

// nullFloat8Array binds a float8 array whose nil elements are NULL.
type nullFloat8Array []*float64

// float4Array binds a float4 array.
type float4Array []float32

// nullFloat4Array binds a float4 array whose nil elements are NULL.
type nullFloat4Array []*float32

// boolArray binds a bool array.
type boolArray []bool

// nullBoolArray binds a bool array whose nil elements are NULL.
type nullBoolArray []*bool

// timestamptzArray binds a timestamptz array.
type timestamptzArray []time.Time

// nullTimestamptzArray binds a timestamptz array whose nil elements are NULL.
type nullTimestamptzArray []*time.Time

// byteaArray binds a bytea array; nil elements are NULL.
type byteaArray [][]byte

// appendBinary appends a as an array of kind k.
func (a textArray) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	size := 0
	for _, s := range a {
		size += 4 + len(s)
	}
	return appendValues(buf, k, oid, a, size, appendText)
}

// appendBinary appends a as an array of kind k.
func (a nullTextArray) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	size := 0
	for _, s := range a {
		size += 4
		if s != nil {
			size += len(*s)
		}
	}
	return appendPointers(buf, k, oid, a, size, appendText)
}

// appendBinary appends a as an array of kind k.
func (a int8Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendValues(buf, k, oid, a, fixedSize(len(a), k), appendInt[int64])
}

// appendBinary appends a as an array of kind k.
func (a nullInt8Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendPointers(buf, k, oid, a, fixedSize(len(a), k), appendInt[int64])
}

// appendBinary appends a as an array of kind k.
func (a int4Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendValues(buf, k, oid, a, fixedSize(len(a), k), appendInt[int32])
}

// appendBinary appends a as an array of kind k.
func (a nullInt4Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendPointers(buf, k, oid, a, fixedSize(len(a), k), appendInt[int32])
}

// appendBinary appends a as an array of kind k.
func (a int2Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendValues(buf, k, oid, a, fixedSize(len(a), k), appendInt[int16])
}

// appendBinary appends a as an array of kind k.
func (a nullInt2Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendPointers(buf, k, oid, a, fixedSize(len(a), k), appendInt[int16])
}

// appendBinary appends a as an array of kind k.
func (a float8Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendValues(buf, k, oid, a, fixedSize(len(a), k), appendFloat8)
}

// appendBinary appends a as an array of kind k.
func (a nullFloat8Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendPointers(buf, k, oid, a, fixedSize(len(a), k), appendFloat8)
}

// appendBinary appends a as an array of kind k.
func (a float4Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendValues(buf, k, oid, a, fixedSize(len(a), k), appendFloat4)
}

// appendBinary appends a as an array of kind k.
func (a nullFloat4Array) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendPointers(buf, k, oid, a, fixedSize(len(a), k), appendFloat4)
}

// appendBinary appends a as an array of kind k.
func (a boolArray) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendValues(buf, k, oid, a, fixedSize(len(a), k), appendBool)
}

// appendBinary appends a as an array of kind k.
func (a nullBoolArray) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendPointers(buf, k, oid, a, fixedSize(len(a), k), appendBool)
}

// appendBinary appends a as an array of kind k.
func (a timestamptzArray) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendValues(buf, k, oid, a, fixedSize(len(a), k), appendTimestamptz)
}

// appendBinary appends a as an array of kind k.
func (a nullTimestamptzArray) appendBinary(buf []byte, k elemKind, oid uint32) ([]byte, error) {
	return appendPointers(buf, k, oid, a, fixedSize(len(a), k), appendTimestamptz)
}

// appendBinary appends a as a bytea array.
func (a byteaArray) appendBinary(buf []byte, _ elemKind, oid uint32) ([]byte, error) {
	size := 0
	nulls := false
	for _, b := range a {
		size += 4 + len(b)
		nulls = nulls || b == nil
	}
	buf = appendArrayHeader(slices.Grow(buf, arrayHeaderSize+size), len(a), oid, nulls)
	for _, b := range a {
		if b == nil {
			buf = appendNull(buf)
			continue
		}
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(b)))
		buf = append(buf, b...)
	}
	return buf, nil
}

// arrayHeaderSize is the size of a one-dimensional array's header.
const arrayHeaderSize = 20

// fixedSize is the encoded size of n elements of a fixed-width kind, lengths included.
func fixedSize(n int, k elemKind) int { return n * (4 + k.width()) }

// appendArrayHeader appends the header of a one-dimensional array of n elements of type oid: the
// number of dimensions (0 for an empty array), the has-nulls flag, the element type and, for a
// non-empty array, its length and lower bound 1.
func appendArrayHeader(buf []byte, n int, oid uint32, nulls bool) []byte {
	var flag uint32
	if nulls {
		flag = 1
	}
	if n == 0 {
		buf = binary.BigEndian.AppendUint32(buf, 0)
		buf = binary.BigEndian.AppendUint32(buf, flag)
		return binary.BigEndian.AppendUint32(buf, oid)
	}
	buf = binary.BigEndian.AppendUint32(buf, 1)
	buf = binary.BigEndian.AppendUint32(buf, flag)
	buf = binary.BigEndian.AppendUint32(buf, oid)
	buf = binary.BigEndian.AppendUint32(buf, uint32(n))
	return binary.BigEndian.AppendUint32(buf, 1)
}

// appendValues appends an array of non-NULL elements; size is the encoded size of the elements.
func appendValues[V any](buf []byte, k elemKind, oid uint32, xs []V, size int,
	enc elemEncoder[V]) ([]byte, error) {
	buf = appendArrayHeader(slices.Grow(buf, arrayHeaderSize+size), len(xs), oid, false)
	var err error
	for _, x := range xs {
		if buf, err = enc(buf, k, x); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// appendPointers appends an array whose nil elements are NULL; size bounds the encoded size of
// the elements.
func appendPointers[V any](buf []byte, k elemKind, oid uint32, xs []*V, size int,
	enc elemEncoder[V]) ([]byte, error) {
	buf = appendArrayHeader(slices.Grow(buf, arrayHeaderSize+size), len(xs), oid, slices.Contains(xs, nil))
	var err error
	for _, x := range xs {
		if x == nil {
			buf = appendNull(buf)
			continue
		}
		if buf, err = enc(buf, k, *x); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// appendNull appends a NULL element: the length -1.
func appendNull(buf []byte) []byte { return append(buf, 0xff, 0xff, 0xff, 0xff) }

// appendText appends a string as a text element: its UTF-8 bytes.
func appendText(buf []byte, _ elemKind, s string) ([]byte, error) {
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(s)))
	return append(buf, s...), nil
}

// appendInt appends an integer as an element of the integer kind k, refusing values out of its
// range.
func appendInt[V int16 | int32 | int64](buf []byte, k elemKind, v V) ([]byte, error) {
	n := int64(v)
	switch k {
	case elemInt2:
		if n < math.MinInt16 || n > math.MaxInt16 {
			return nil, errf("%d is out of range for smallint", n)
		}
		buf = append(buf, 0, 0, 0, 2)
		return binary.BigEndian.AppendUint16(buf, uint16(n)), nil
	case elemInt4:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return nil, errf("%d is out of range for integer", n)
		}
		buf = append(buf, 0, 0, 0, 4)
		return binary.BigEndian.AppendUint32(buf, uint32(n)), nil
	}
	buf = append(buf, 0, 0, 0, 8)
	return binary.BigEndian.AppendUint64(buf, uint64(n)), nil
}

// appendFloat8 appends a float8 element: the IEEE 754 bits, big endian.
func appendFloat8(buf []byte, _ elemKind, v float64) ([]byte, error) {
	buf = append(buf, 0, 0, 0, 8)
	return binary.BigEndian.AppendUint64(buf, math.Float64bits(v)), nil
}

// appendFloat4 appends a float4 element: the IEEE 754 bits, big endian.
func appendFloat4(buf []byte, _ elemKind, v float32) ([]byte, error) {
	buf = append(buf, 0, 0, 0, 4)
	return binary.BigEndian.AppendUint32(buf, math.Float32bits(v)), nil
}

// appendBool appends a bool element: one byte, 1 for true.
func appendBool(buf []byte, _ elemKind, v bool) ([]byte, error) {
	if v {
		return append(buf, 0, 0, 0, 1, 1), nil
	}
	return append(buf, 0, 0, 0, 1, 0), nil
}

// microsecFromUnixEpochToY2K is the number of microseconds from 1970-01-01 to 2000-01-01, the
// epoch of Postgres timestamps.
const microsecFromUnixEpochToY2K = 946_684_800 * 1_000_000

// appendTimestamptz appends a timestamptz element: microseconds since 2000-01-01 UTC.
func appendTimestamptz(buf []byte, _ elemKind, t time.Time) ([]byte, error) {
	us := t.Unix()*1_000_000 + int64(t.Nanosecond())/1000 - microsecFromUnixEpochToY2K
	buf = append(buf, 0, 0, 0, 8)
	return binary.BigEndian.AppendUint64(buf, uint64(us)), nil
}
