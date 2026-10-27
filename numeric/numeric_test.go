package numeric_test

import (
	"context"
	"os"
	"testing"

	"github.com/emoss08/keel/numeric"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
)

var values = []string{
	"0", "0.0000", "1", "-1", "1.5000", "-0.0001", "12345.6789", "100", "1e6", "0.000000000000000000001",
	"99999999999999.9999", "-123456789012345678901234567890.123456789", "1000000000000000000000",
}

// connect opens a connection with Register, and one without it that scans decimals through their
// sql.Scanner, as they do without the codec.
func connect(t *testing.T) (codec, text *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN is not set")
	}
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	text, err = pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = text.Close(ctx) })
	codec, err = pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = codec.Close(ctx) })
	if err := numeric.Register(ctx, codec); err != nil {
		t.Fatal(err)
	}
	return codec, text
}

// same reports whether a and b have the same coefficient and exponent, as String shows them.
func same(a, b decimal.Decimal) bool {
	return a.Coefficient().Cmp(b.Coefficient()) == 0 && a.Exponent() == b.Exponent()
}

func TestScanMatchesText(t *testing.T) {
	codec, text := connect(t)
	ctx := context.Background()
	for _, v := range values {
		for _, typ := range []string{"numeric", "numeric(40,4)", "numeric(60,25)"} {
			q := "SELECT $1::text::" + typ
			var got, want decimal.Decimal
			if err := codec.QueryRow(ctx, q, v).Scan(&got); err != nil {
				if typ != "numeric" {
					continue
				}
				t.Fatalf("%s as %s: %v", v, typ, err)
			}
			if err := text.QueryRow(ctx, q, v).Scan(&want); err != nil {
				t.Fatal(err)
			}
			if !same(got, want) {
				t.Fatalf("%s as %s: scanned %s (exp %d), text gives %s (exp %d)",
					v, typ, got, got.Exponent(), want, want.Exponent())
			}
			var null decimal.NullDecimal
			if err := codec.QueryRow(ctx, q, v).Scan(&null); err != nil || !null.Valid || !same(null.Decimal, want) {
				t.Fatalf("%s as %s into NullDecimal: %v, %v", v, typ, null, err)
			}
		}
	}
}

func TestScanNullAndSpecialValues(t *testing.T) {
	codec, text := connect(t)
	ctx := context.Background()
	null := decimal.NullDecimal{Decimal: decimal.NewFromInt(7), Valid: true}
	if err := codec.QueryRow(ctx, "SELECT NULL::numeric").Scan(&null); err != nil || null.Valid {
		t.Fatalf("NULL into NullDecimal: %v, %v", null, err)
	}
	var d decimal.Decimal
	if err := codec.QueryRow(ctx, "SELECT NULL::numeric").Scan(&d); err == nil {
		t.Fatal("NULL into Decimal was accepted")
	}
	if err := text.QueryRow(ctx, "SELECT NULL::numeric").Scan(&d); err == nil {
		t.Fatal("the text path accepts NULL into Decimal, so the codec should too")
	}
	for _, v := range []string{"NaN", "Infinity", "-Infinity"} {
		if err := codec.QueryRow(ctx, "SELECT $1::text::numeric", v).Scan(&d); err == nil {
			t.Fatalf("%s into Decimal was accepted", v)
		}
	}
}

func TestBindRoundTrips(t *testing.T) {
	codec, _ := connect(t)
	ctx := context.Background()
	for _, v := range values {
		d := decimal.RequireFromString(v)
		var got decimal.Decimal
		if err := codec.QueryRow(ctx, "SELECT $1::numeric", d).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !got.Equal(d) {
			t.Fatalf("%s bound and read back as %s", d, got)
		}
		var viaText string
		if err := codec.QueryRow(ctx, "SELECT ($1::numeric = $2::text::numeric)::text", &d, v).Scan(&viaText); err != nil || viaText != "true" {
			t.Fatalf("%s bound as a pointer compares %s, %v", d, viaText, err)
		}
	}
	var isNull bool
	if err := codec.QueryRow(ctx, "SELECT $1::numeric IS NULL", decimal.NullDecimal{}).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("an invalid NullDecimal bound as %v, %v", isNull, err)
	}
}

func TestArrays(t *testing.T) {
	codec, _ := connect(t)
	ctx := context.Background()
	in := []decimal.Decimal{decimal.RequireFromString("1.25"), decimal.RequireFromString("-3"), decimal.Zero}
	var out []decimal.Decimal
	if err := codec.QueryRow(ctx, "SELECT $1::numeric[]", in).Scan(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("read back %v", out)
	}
	for i := range in {
		if !out[i].Equal(in[i]) {
			t.Fatalf("read back %v", out)
		}
	}
	var nulls []decimal.NullDecimal
	if err := codec.QueryRow(ctx, "SELECT ARRAY[1.5, NULL]::numeric[]").Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if len(nulls) != 2 || !nulls[0].Valid || nulls[1].Valid || !nulls[0].Decimal.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("read back %v", nulls)
	}
	var n int
	if err := codec.QueryRow(ctx, "SELECT count(*) FROM unnest($1::numeric[]) x WHERE x > 0", in).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unnest counted %d, %v", n, err)
	}
}

func TestOtherTypesReadAsBefore(t *testing.T) {
	codec, _ := connect(t)
	ctx := context.Background()
	var v any
	if err := codec.QueryRow(ctx, "SELECT 1.5::numeric").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(pgtype.Numeric); !ok {
		t.Fatalf("numeric into any is %T, want pgtype.Numeric", v)
	}
	var f float64
	var s string
	if err := codec.QueryRow(ctx, "SELECT 1.5::numeric, 2.25::numeric").Scan(&f, &s); err != nil || f != 1.5 || s != "2.25" {
		t.Fatalf("float64 %v, string %q, %v", f, s, err)
	}
}

func BenchmarkScan(b *testing.B) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		b.Skip("KEEL_TEST_DSN is not set")
	}
	for _, register := range []bool{false, true} {
		name := "text"
		if register {
			name = "codec"
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close(ctx)
			if register {
				if err := numeric.Register(ctx, conn); err != nil {
					b.Fatal(err)
				}
			}
			const q = `SELECT g * 1.2345, g * 123.4567, CASE WHEN g % 2 = 0 THEN g * 0.5 END
				FROM generate_series(1, 1000) g`
			b.ReportAllocs()
			for b.Loop() {
				rows, _ := conn.Query(ctx, q)
				var a, c decimal.Decimal
				var d decimal.NullDecimal
				for rows.Next() {
					if err := rows.Scan(&a, &c, &d); err != nil {
						b.Fatal(err)
					}
				}
				if rows.Err() != nil {
					b.Fatal(rows.Err())
				}
			}
		})
	}
}

func BenchmarkBind(b *testing.B) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		b.Skip("KEEL_TEST_DSN is not set")
	}
	d := decimal.RequireFromString("12345.6789")
	in := make([]decimal.Decimal, 1000)
	for i := range in {
		in[i] = d.Add(decimal.NewFromInt(int64(i)))
	}
	for _, register := range []bool{false, true} {
		name := "text"
		if register {
			name = "codec"
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close(ctx)
			if register {
				if err := numeric.Register(ctx, conn); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				var n int
				if err := conn.QueryRow(ctx, "SELECT cardinality($1::numeric[])", in).Scan(&n); err != nil || n != 1000 {
					b.Fatal(n, err)
				}
			}
		})
	}
}
