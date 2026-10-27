package keel

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/emoss08/keel/numeric"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestAppendNumeric(t *testing.T) {
	num := func(weight int16, sign, dscale uint16, digits ...uint16) []byte {
		b := binary.BigEndian.AppendUint16(nil, uint16(len(digits)))
		b = binary.BigEndian.AppendUint16(b, uint16(weight))
		b = binary.BigEndian.AppendUint16(b, sign)
		b = binary.BigEndian.AppendUint16(b, dscale)
		for _, d := range digits {
			b = binary.BigEndian.AppendUint16(b, d)
		}
		return b
	}
	cases := []struct {
		src  []byte
		want string
	}{
		{num(0, 0, 0), "0"},
		{num(0, 0, 2), "0.00"},
		{num(0, 0, 0, 7), "7"},
		{num(1, 0, 0, 1), "10000"},
		{num(1, 0x4000, 4, 12, 3456, 7800), "-123456.7800"},
		{num(-1, 0, 4, 5000), "0.5000"},
		{num(-2, 0, 6, 1200), "0.000012"},
		{num(-2, 0, 6, 12), "0.000000"},
		{num(0, 0, 3, 1, 2345), "1.234"},
		{num(2, 0, 1, 1, 0, 0), "100000000.0"},
		{num(0, 0xC000, 0), "NaN"},
		{num(0, 0xD000, 0), "Infinity"},
		{num(0, 0xF000, 0), "-Infinity"},
	}
	for _, c := range cases {
		got, err := appendNumeric(nil, c.src)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Fatalf("got %s, want %s", got, c.want)
		}
	}
	if _, err := appendNumeric(nil, []byte{0, 1}); err == nil {
		t.Fatal("short numeric decoded")
	}
	if _, err := appendNumeric(nil, num(0, 0, 0, 1)[:9]); err == nil {
		t.Fatal("truncated digits decoded")
	}
}

func TestDynamicDecodeTextAndBinary(t *testing.T) {
	c := internalConn(t, `CREATE TYPE mood AS ENUM ('happy', 'sad');
		CREATE DOMAIN label AS text`, "label", "mood")
	ctx := context.Background()
	sql := `SELECT 'héllo'::text, 'sad'::mood, 'x'::label, -7::int2, 9000000000::int8, 1.5::float4,
		-2.25::float8, '-1234.5600'::numeric, 'NaN'::numeric, true, false,
		'2026-03-04 05:06:07.5+02'::timestamptz, '2026-03-04 05:06:07'::timestamp, '1999-12-31'::date,
		'\x00ff'::bytea, '{"a": 1}'::json, '{"b": [true]}'::jsonb,
		'7b2d3e50-1c1c-4b5a-9a0e-3d2c1b0a9f8e'::uuid, NULL::text, NULL::int8, NULL::numeric,
		NULL::timestamptz, NULL::bytea, NULL::jsonb, NULL::bool, NULL::float8, NULL::date`
	read := func(mode pgx.QueryExecMode) *DynResult {
		rows, err := c.Query(ctx, sql, mode)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		rd := &dynReader{db: &DB{}, hint: 1}
		if err := rd.start(rows); err != nil {
			t.Fatal(err)
		}
		res := rd.result(rows.FieldDescriptions())
		for rows.Next() {
			if err := res.add(rows.RawValues()); err != nil {
				t.Fatal(err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return res
	}
	binaryRes := read(pgx.QueryExecModeCacheStatement)
	textRes := read(pgx.QueryExecModeSimpleProtocol)
	if len(binaryRes.Columns) != len(textRes.Columns) {
		t.Fatal("column counts differ")
	}
	want := []ValueType{TypeString, TypeString, TypeString, TypeInt64, TypeInt64, TypeFloat64,
		TypeFloat64, TypeNumeric, TypeNumeric, TypeBool, TypeBool, TypeTime, TypeTime, TypeDate,
		TypeBytes, TypeJSON, TypeJSON, TypeString, TypeString, TypeInt64, TypeNumeric, TypeTime,
		TypeBytes, TypeJSON, TypeBool, TypeFloat64, TypeDate}
	for i := range binaryRes.Columns {
		b, x := &binaryRes.Columns[i], &textRes.Columns[i]
		if b.Type != want[i] || x.Type != want[i] {
			t.Fatalf("column %d: types %s and %s, want %s", i+1, b.Type, x.Type, want[i])
		}
		bv, xv := b.Value(0), x.Value(0)
		if bb, ok := bv.([]byte); ok {
			if xb, _ := xv.([]byte); !slices.Equal(bb, xb) && b.Type != TypeJSON {
				t.Fatalf("column %d: %q and %q", i+1, bb, xb)
			}
			continue
		}
		if bv != xv {
			t.Fatalf("column %d: binary %#v, text %#v", i+1, bv, xv)
		}
	}
	eq := func(got, want any) {
		t.Helper()
		if got != want {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
	eq(binaryRes.Columns[7].Value(0), any(Decimal("-1234.5600")))
	eq(binaryRes.Columns[11].Time[0].Unix(), int64(1772593567))
	eq(binaryRes.Columns[11].Time[0].Nanosecond(), 500000000)
	eq(string(binaryRes.Columns[14].Bytes[0]), "\x00\xff")
	eq(string(binaryRes.Columns[16].JSON[0]), `{"b": [true]}`)
	for i := 18; i < len(binaryRes.Columns); i++ {
		if !binaryRes.Columns[i].IsNull(0) || !textRes.Columns[i].IsNull(0) {
			t.Fatalf("column %d is not NULL", i+1)
		}
	}
}

func TestDynamicDecodeRegisteredNumericCodec(t *testing.T) {
	m := pgtype.NewMap()
	m.RegisterType(&pgtype.Type{Name: "numeric", OID: pgtype.NumericOID, Codec: numeric.Codec{}})
	for _, format := range []int16{pgtype.TextFormatCode, pgtype.BinaryFormatCode} {
		fd := pgconn.FieldDescription{Name: "n", DataTypeOID: pgtype.NumericOID, Format: format}
		if got, _ := dynDecoderFor(&DB{}, m, fd); got != TypeNumeric {
			t.Fatalf("format %d: numeric decodes as %s, want %s", format, got, TypeNumeric)
		}
	}
}

func TestDynColumnTruncate(t *testing.T) {
	c := DynColumn{Type: TypeInt64}
	for i := range 130 {
		if i%3 == 0 {
			c.null(i)
		} else {
			c.Int64 = append(c.Int64, int64(i))
		}
	}
	c.truncate(66)
	if len(c.Int64) != 66 || len(c.Nulls) != 2 || !c.IsNull(63) || c.IsNull(64) || c.IsNull(66) {
		t.Fatalf("truncate: %d values, %d words", len(c.Int64), len(c.Nulls))
	}
	if c.Nulls[1] != 0 {
		t.Fatalf("bits past the end kept: %b", c.Nulls[1])
	}
}
