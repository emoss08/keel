package keel

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// nullCase checks one nullable kind: the plan planScan picks for a **V field and the values it
// decodes.
type nullCase struct {
	typ   string
	value string
	check func(t *testing.T, m *pgtype.Map, null, value []byte, oid uint32, format int16)
}

// checkNull plans a **V field, decodes value into a nil field and NULL into a field that holds a
// value, and compares the decoded value with want.
func checkNull[V any](want V, equal func(a, b V) bool) func(*testing.T, *pgtype.Map, []byte, []byte, uint32, int16) {
	return func(t *testing.T, m *pgtype.Map, null, value []byte, oid uint32, format int16) {
		t.Helper()
		var field *V
		plan := planScan(m, oid, format, &field)
		if _, ok := plan.(*nullScanPlan[V]); !ok {
			t.Fatalf("%T: plan %T, want nullScanPlan", field, plan)
		}
		if err := plan.Scan(value, &field); err != nil {
			t.Fatal(err)
		}
		if field == nil || !equal(*field, want) {
			t.Fatalf("%T: decoded %v, want %v", field, field, want)
		}
		first := field
		if err := plan.Scan(value, &field); err != nil {
			t.Fatal(err)
		}
		if field == first {
			t.Fatalf("%T: two rows share one value", field)
		}
		if err := plan.Scan(null, &field); err != nil {
			t.Fatal(err)
		}
		if field != nil {
			t.Fatalf("%T: NULL decoded to %v", field, *field)
		}
	}
}

// equalValues compares two comparable values.
func equalValues[V comparable](a, b V) bool { return a == b }

func TestNullableScanPlans(t *testing.T) {
	c := internalConn(t, `CREATE TYPE mood AS ENUM ('happy', 'sad')`)
	at := time.Date(2026, 10, 23, 14, 5, 6, 123456000, time.UTC)
	cases := []nullCase{
		{"text", "'it''s'", checkNull("it's", equalValues)},
		{"varchar(10)", "'ü'", checkNull("ü", equalValues)},
		{"mood", "'sad'", checkNull("sad", equalValues)},
		{"int8", "-9000000000", checkNull(int64(-9_000_000_000), equalValues)},
		{"int4", "-7", checkNull(int32(-7), equalValues)},
		{"int2", "12", checkNull(int16(12), equalValues)},
		{"float8", "2.5", checkNull(2.5, equalValues)},
		{"float4", "-0.25", checkNull(float32(-0.25), equalValues)},
		{"bool", "true", checkNull(true, equalValues)},
		{"bool", "false", checkNull(false, equalValues)},
		{"timestamptz", "'2026-10-23 14:05:06.123456+00'", checkNull(at, time.Time.Equal)},
	}
	ctx := context.Background()
	for _, tc := range cases {
		rows, err := c.Query(ctx, "SELECT NULL::"+tc.typ+", "+tc.value+"::"+tc.typ)
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatalf("%s: no row: %v", tc.typ, rows.Err())
		}
		fd := rows.FieldDescriptions()[1]
		raw := rows.RawValues()
		tc.check(t, rows.Conn().TypeMap(), raw[0], raw[1], fd.DataTypeOID, fd.Format)
		rows.Close()
	}
}

func TestPlanScanLeavesOtherTargetsToPgx(t *testing.T) {
	m := pgtype.NewMap()
	var s string
	var labels *[]string
	for _, dst := range []any{&s, &labels} {
		if got, want := reflect.TypeOf(planScan(m, pgtype.TextArrayOID, pgtype.BinaryFormatCode, dst)),
			reflect.TypeOf(m.PlanScan(pgtype.TextArrayOID, pgtype.BinaryFormatCode, dst)); got != want {
			t.Fatalf("%T: plan %v, want pgx's %v", dst, got, want)
		}
	}
}

// BenchmarkNullableScan compares keel's plan for a nullable int8 field with pgx's
// pointer-to-pointer plan.
func BenchmarkNullableScan(b *testing.B) {
	m := pgtype.NewMap()
	src := []byte{0, 0, 0, 0, 0, 0, 0, 42}
	for name, plan := range map[string]func(dst any) pgtype.ScanPlan{
		"keel": func(dst any) pgtype.ScanPlan { return planScan(m, pgtype.Int8OID, pgtype.BinaryFormatCode, dst) },
		"pgx":  func(dst any) pgtype.ScanPlan { return m.PlanScan(pgtype.Int8OID, pgtype.BinaryFormatCode, dst) },
	} {
		b.Run(name, func(b *testing.B) {
			var field *int64
			p := plan(&field)
			b.ReportAllocs()
			for b.Loop() {
				if err := p.Scan(src, &field); err != nil {
					b.Fatal(err)
				}
				if err := p.Scan(nil, &field); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
