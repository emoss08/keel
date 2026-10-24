package value_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/emoss08/keel"
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
	v := value.Vector{Numeric: []keel.Decimal{"", value.DecimalFromInt(1)}}
	if !v.Null(0) || v.Null(1) {
		t.Error("an empty decimal is null")
	}
}

func TestDecimalText(t *testing.T) {
	cases := map[string]string{
		"1.50": "1.50", "-0.001": "-0.001", "1.2e3": "1200", "12e-4": "0.0012", " 3.115 ": "3.115",
		"0": "0", "-0": "0", ".5": "0.5", "-.5": "-0.5", "1e-1": "0.1", "+7": "7", "007.10": "7.10",
		"NaN": "NaN", "Infinity": "Infinity", "-Infinity": "-Infinity",
		"123456789012345678901234567890.123": "123456789012345678901234567890.123",
	}
	for in, want := range cases {
		d, err := value.ParseDecimal(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if string(d) != want {
			t.Errorf("%q: got %q, want %q", in, d, want)
		}
	}
	for _, bad := range []string{"", "-", "1,000", "1.2.3", "abc", "1e", "--1", "1-2"} {
		if _, err := value.ParseDecimal(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if value.DecimalFinite("") || value.DecimalFinite("NaN") || !value.DecimalFinite("-1.5") {
		t.Error("finiteness")
	}
	neg, whole, frac, ok := value.SplitDecimal("-12.340")
	if !neg || whole != "12" || frac != "340" || !ok {
		t.Errorf("split: %v %q %q %v", neg, whole, frac, ok)
	}
}

func TestDecimalFromFloat(t *testing.T) {
	for f, want := range map[float64]keel.Decimal{0.1: "0.1", 1.005: "1.005", -2.5e-7: "-0.00000025", 1e21: "1000000000000000000000"} {
		if got := value.DecimalFromFloat(f); got != want {
			t.Errorf("%v: got %q, want %q", f, got, want)
		}
	}
	if value.DecimalFromFloat(math.NaN()) != "NaN" || value.DecimalFinite(value.DecimalFromFloat(math.Inf(-1))) {
		t.Error("non-finite floats")
	}
	if value.DecimalFromInt(-42) != "-42" {
		t.Error("from int")
	}
}

func TestDecimalArithmetic(t *testing.T) {
	sum, _ := value.AddDecimal("0.1", "0.2")
	diff, _ := value.SubDecimal("0.30", "0.10")
	if value.NormalizeDecimal(sum) != "0.3" || value.NormalizeDecimal(diff) != "0.2" {
		t.Errorf("sum %s, diff %s", sum, diff)
	}
	if c, _ := value.CompareDecimal("1.50", "1.5"); c != 0 {
		t.Error("1.50 equals 1.5")
	}
	if c, _ := value.CompareDecimal("-2", "0.000000001"); c != -1 {
		t.Error("ordering")
	}
	if _, err := value.AddDecimal("NaN", "1"); !errors.Is(err, value.ErrNotFinite) {
		t.Errorf("NaN arithmetic: %v", err)
	}
	if got := value.NormalizeDecimal("1200"); got != "1200" {
		t.Errorf("normalized 1200: %s", got)
	}
	if got := value.NormalizeDecimal("-0.000"); got != "0" {
		t.Errorf("normalized -0.000: %s", got)
	}
	big := keel.Decimal("9223372036854775807.5")
	twice, _ := value.AddDecimal(big, big)
	if twice != "18446744073709551615.0" {
		t.Errorf("overflowing sum: %s", twice)
	}
	back, _ := value.SubDecimal(twice, big)
	if c, _ := value.CompareDecimal(back, big); c != 0 {
		t.Errorf("big difference: %s", back)
	}
	if c, _ := value.CompareDecimal("123456789012345678901234567890", "123456789012345678901234567891"); c != -1 {
		t.Error("big comparison")
	}
	neg, _ := value.SubDecimal("1", "-2.25")
	if neg != "3.25" {
		t.Errorf("1 - -2.25 = %s", neg)
	}
}
