package keel

import (
	"testing"
	"unsafe"

	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5/pgtype"
)

var testEnum = []schema.EnumValue{{Value: "New"}, {Value: "Assigned"}, {Value: "InTransit"}}

func TestEnumScanPlanStoresTheConstantsString(t *testing.T) {
	m := pgtype.NewMap()
	var s string
	p := newEnumScanPlan(testEnum, &s, planScan(m, pgtype.TextOID, pgtype.TextFormatCode, &s))
	if _, ok := p.(*enumScanPlan); !ok {
		t.Fatalf("a *string field got %T", p)
	}
	src := []byte("Assigned")
	if err := p.Scan(src, &s); err != nil {
		t.Fatal(err)
	}
	if s != "Assigned" || unsafe.StringData(s) != unsafe.StringData(testEnum[1].Value) {
		t.Fatalf("decoded %q into its own string", s)
	}
	if n := testing.AllocsPerRun(100, func() { _ = p.Scan(src, &s) }); n != 0 {
		t.Fatalf("decoding a constant allocates %v times", n)
	}

	other := []byte("Lost")
	if err := p.Scan(other, &s); err != nil || s != "Lost" {
		t.Fatalf("a value outside the constants decoded as %q, %v", s, err)
	}
	other[0] = 'X'
	if s != "Lost" {
		t.Fatal("a value outside the constants shares the driver's buffer")
	}
	if err := p.Scan(nil, &s); err == nil {
		t.Fatal("NULL decoded into a field that cannot hold it")
	}
}

func TestEnumScanPlanOfNullableAndNullzeroFields(t *testing.T) {
	m := pgtype.NewMap()
	var ps *string
	p := newEnumScanPlan(testEnum, &ps, planScan(m, pgtype.TextOID, pgtype.TextFormatCode, &ps))
	if _, ok := p.(*enumPtrScanPlan); !ok {
		t.Fatalf("a **string field got %T", p)
	}
	if err := p.Scan([]byte("InTransit"), &ps); err != nil {
		t.Fatal(err)
	}
	if ps == nil || *ps != "InTransit" || unsafe.StringData(*ps) != unsafe.StringData(testEnum[2].Value) {
		t.Fatalf("decoded %v", ps)
	}
	if err := p.Scan(nil, &ps); err != nil || ps != nil {
		t.Fatalf("NULL decoded as %v, %v", ps, err)
	}

	z := "New"
	zp := newEnumScanPlan(testEnum, &z, newZeroScanPlan(m, pgtype.TextOID, pgtype.TextFormatCode, &z))
	if err := zp.Scan(nil, &z); err != nil || z != "" {
		t.Fatalf("NULL in a nullzero field decoded as %q, %v", z, err)
	}

	var n int64
	elem := planScan(m, pgtype.Int8OID, pgtype.BinaryFormatCode, &n)
	switch got := newEnumScanPlan(testEnum, &n, elem).(type) {
	case *enumScanPlan, *enumPtrScanPlan:
		t.Fatalf("a field that is not a string got %T", got)
	}
}

func TestEnumBytes(t *testing.T) {
	m := pgtype.NewMap()
	const moodOID = 900001
	m.RegisterType(&pgtype.Type{Name: "mood", OID: moodOID, Codec: &pgtype.EnumCodec{}})
	for _, tc := range []struct {
		oid    uint32
		format int16
		want   bool
	}{
		{pgtype.TextOID, pgtype.BinaryFormatCode, true},
		{pgtype.VarcharOID, pgtype.BinaryFormatCode, true},
		{pgtype.Int8OID, pgtype.TextFormatCode, true},
		{pgtype.Int8OID, pgtype.BinaryFormatCode, false},
		{pgtype.UUIDOID, pgtype.BinaryFormatCode, false},
		{moodOID, pgtype.BinaryFormatCode, true},
		{moodOID + 1, pgtype.BinaryFormatCode, false},
	} {
		if got := enumBytes(m, tc.oid, tc.format); got != tc.want {
			t.Fatalf("enumBytes(%d, %d) = %v", tc.oid, tc.format, got)
		}
	}
}
