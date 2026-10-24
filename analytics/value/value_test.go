package value_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/emoss08/keel/analytics/value"
)

func TestTypeText(t *testing.T) {
	for _, typ := range value.Types() {
		b, err := json.Marshal(typ)
		if err != nil {
			t.Fatal(err)
		}
		var back value.Type
		if err := json.Unmarshal(b, &back); err != nil || back != typ {
			t.Errorf("%s: %s -> %v %v", typ, b, back, err)
		}
	}
	if _, err := value.ParseType("money"); err == nil {
		t.Error("unknown type accepted")
	}
	if _, err := json.Marshal(value.Type(0)); err == nil {
		t.Error("the zero type marshals")
	}
	if !value.Decimal.Numeric() || value.Epoch.Numeric() || !value.Epoch.Instant() || !value.Ref.Textual() {
		t.Error("type classes")
	}
}

func TestBitmap(t *testing.T) {
	b := value.NewBitmap(130)
	b.Set(0)
	b.Set(64)
	b.Set(129)
	b.Clear(64)
	for i, want := range map[int]bool{0: true, 1: false, 64: false, 129: true, 500: false} {
		if b.Has(i) != want {
			t.Errorf("row %d", i)
		}
	}
	var none value.Bitmap
	if none.Has(3) {
		t.Error("a nil bitmap is empty")
	}
	v := value.Vector{Numeric: []pgtype.Numeric{{Valid: false}, value.NumericFromInt(1)}}
	if !v.Null(0) || v.Null(1) {
		t.Error("an invalid numeric is null")
	}
}

func TestNumericText(t *testing.T) {
	cases := map[string]string{
		"1.50": "1.50", "-0.001": "-0.001", "1.2e3": "1200", "12e-4": "0.0012", " 3.115 ": "3.115",
		"0": "0", "-0": "0", ".5": "0.5", "-.5": "-0.5", "1e-1": "0.1",
		"NaN": "NaN", "Infinity": "Infinity", "-Infinity": "-Infinity",
		"123456789012345678901234567890.123": "123456789012345678901234567890.123",
	}
	for in, want := range cases {
		n, err := value.ParseNumeric(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := string(value.AppendNumeric(nil, n)); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "-", "1,000", "1.2.3", "abc", "1e", "--1", "1-2"} {
		if _, err := value.ParseNumeric(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := string(value.AppendNumeric(nil, pgtype.Numeric{})); got != "" {
		t.Errorf("null appends %q", got)
	}
}

func TestNumericFromFloat(t *testing.T) {
	for f, want := range map[float64]string{0.1: "0.1", 1.005: "1.005", -2.5e-7: "-0.00000025", 1e21: "1000000000000000000000"} {
		if got := string(value.AppendNumeric(nil, value.NumericFromFloat(f))); got != want {
			t.Errorf("%v: got %q, want %q", f, got, want)
		}
	}
	if !value.NumericFromFloat(math.NaN()).NaN || value.NumericFinite(value.NumericFromFloat(math.Inf(-1))) {
		t.Error("non-finite floats")
	}
}

func TestNumericArithmetic(t *testing.T) {
	n := func(s string) pgtype.Numeric {
		x, err := value.ParseNumeric(s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	text := func(x pgtype.Numeric) string { return string(value.AppendNumeric(nil, value.NormalizeNumeric(x))) }
	sum, _ := value.AddNumeric(n("0.1"), n("0.2"))
	diff, _ := value.SubNumeric(n("0.30"), n("0.10"))
	if text(sum) != "0.3" || text(diff) != "0.2" {
		t.Errorf("sum %s, diff %s", text(sum), text(diff))
	}
	if c, _ := value.CompareNumeric(n("1.50"), n("1.5")); c != 0 {
		t.Error("1.50 equals 1.5")
	}
	if c, _ := value.CompareNumeric(n("-2"), n("1e-9")); c != -1 {
		t.Error("ordering")
	}
	if _, err := value.AddNumeric(n("NaN"), n("1")); !errors.Is(err, value.ErrNotFinite) {
		t.Errorf("NaN arithmetic: %v", err)
	}
	if got := text(n("1200")); got != "1200" || n("1200").Exp != 0 {
		t.Errorf("normalized 1200: %s", got)
	}
	if norm := value.NormalizeNumeric(n("1200")); norm.Exp != 2 {
		t.Errorf("1200 normalizes to exponent %d", norm.Exp)
	}
}
