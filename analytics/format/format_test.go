package format_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/language"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/value"
)

func ptr[T any](v T) *T { return &v }

var tenth, fifth = 0.1, 0.2

type golden struct {
	name   string
	typ    value.Type
	hint   format.Hint
	spec   *format.Spec
	raw    *format.Resolved
	opts   format.Options
	values []any
	want   []string
	tones  []format.Tone
}

func (g golden) display() format.Resolved {
	if g.raw != nil {
		return *g.raw
	}
	return format.Resolve(g.typ, g.hint, g.spec)
}

func (g golden) run(t *testing.T) {
	t.Helper()
	f := format.New(g.typ, g.display(), g.opts)
	for i, v := range g.values {
		text, tone := f.Format(v)
		if text != g.want[i] {
			t.Errorf("%s: value %d (%v): got %q, want %q", g.name, i, v, text, g.want[i])
		}
		if g.tones != nil && tone != g.tones[i] {
			t.Errorf("%s: value %d (%v): tone %q, want %q", g.name, i, v, tone, g.tones[i])
		}
	}
}

func TestFormatGolden(t *testing.T) {
	for _, g := range []golden{
		{name: "money tames raw quotients", typ: value.Decimal, hint: format.Hint{Kind: format.Money},
			values: []any{"1082.0672643987443142", "-1082.0672643987443142", nil},
			want:   []string{"$1,082.07", "($1,082.07)", ""}},
		{name: "grouping off", typ: value.Decimal, spec: &format.Spec{Style: format.StyleNumber, Grouping: ptr(false)},
			values: []any{1234567.891}, want: []string{"1234567.89"}},
		{name: "zero places round half away from zero", typ: value.Decimal,
			spec:   &format.Spec{Style: format.StyleNumber, Decimals: ptr(0)},
			values: []any{1234.5}, want: []string{"1,235"}},
		{name: "compact", typ: value.Decimal,
			spec:   &format.Spec{Style: format.StyleNumber, Decimals: ptr(1), Notation: format.NotationCompact},
			values: []any{1284000, 940}, want: []string{"1.3M", "940.0"}},
		{name: "suffix", typ: value.Decimal, spec: &format.Spec{Style: format.StyleNumber, Decimals: ptr(1), Suffix: " mi"},
			values: []any{1284.44}, want: []string{"1,284.4 mi"}},
		{name: "percent", typ: value.Decimal, spec: &format.Spec{Style: format.StylePercent, Decimals: ptr(1)},
			values: []any{0.1834}, want: []string{"18.3%"}},
		{name: "minus instead of parentheses", typ: value.Decimal,
			spec:   &format.Spec{Style: format.StyleCurrency, Negative: format.NegativeMinus},
			values: []any{-12.5}, want: []string{"-$12.50"}},
		{name: "exact half rounds up where a float would not", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleNumber, Decimals: 2, Grouping: true},
			values: []any{1.005, 2.675, "1.005", -2.675, 0.125, 1234.565, " 3.115 "},
			want:   []string{"1.01", "2.68", "1.01", "-2.68", "0.13", "1,234.57", "3.12"}},
		{name: "a negative that rounds to zero loses its sign", typ: value.Decimal, hint: format.Hint{Kind: format.Money},
			values: []any{-0.001, 0}, want: []string{"$0.00", "$0.00"}},
		{name: "compact is chosen on the unrounded magnitude", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleNumber, Grouping: true, Notation: format.NotationCompact},
			values: []any{999999, 999.5, 1e15, -1284000}, want: []string{"1,000K", "1,000", "1,000T", "-1M"}},
		{name: "compact without grouping", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleNumber, Notation: format.NotationCompact},
			values: []any{999999}, want: []string{"1000K"}},
		{name: "compact percent", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StylePercent, Decimals: 1, Grouping: true, Notation: format.NotationCompact},
			values: []any{12345}, want: []string{"1.2M%"}},
		{name: "compact currency", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleCurrency, Decimals: 1, Currency: "EUR", Notation: format.NotationCompact},
			values: []any{2500000000}, want: []string{"€2.5B"}},
		{name: "decimals are clamped", typ: value.Decimal, raw: &format.Resolved{Style: format.StyleNumber, Decimals: 12},
			values: []any{1.23456789012345}, want: []string{"1.2345678901"}},
		{name: "negative decimals clamp to zero", typ: value.Decimal, raw: &format.Resolved{Style: format.StyleNumber, Decimals: -3},
			values: []any{1.5}, want: []string{"2"}},
		{name: "a symbol that is a code keeps off the digits", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleCurrency, Decimals: 2, Currency: "CHF"},
			values: []any{5}, want: []string{"CHF\u00a05.00"}},
		{name: "no currency is dollars", typ: value.Decimal, raw: &format.Resolved{Style: format.StyleCurrency, Decimals: 2},
			values: []any{5}, want: []string{"$5.00"}},
		{name: "yen", typ: value.Decimal, spec: &format.Spec{Style: format.StyleCurrency, Currency: "jpy"},
			values: []any{1500}, want: []string{"¥1,500"}},
		{name: "affixes inside the parentheses, minus before the prefix", typ: value.Decimal,
			raw: &format.Resolved{Style: format.StyleCurrency, Decimals: 2, Currency: "CAD",
				Negative: format.NegativeParentheses, Prefix: "~", Suffix: " net"},
			values: []any{-5}, want: []string{"(~CA$5.00 net)"}},
		{name: "minus before the prefix", typ: value.Decimal, raw: &format.Resolved{Style: format.StyleNumber, Decimals: 2, Prefix: "~"},
			values: []any{-5}, want: []string{"-~5.00"}},
		{name: "a number in a text column is still a number", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleText, Decimals: 2, Grouping: true},
			values: []any{1234.5}, want: []string{"1,234.50"}},
		{name: "a boolean in a numeric column", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleNumber, Decimals: 2, BoolStyle: format.BoolStyleOnOff},
			values: []any{true}, want: []string{"On"}},
		{name: "text takes the affixes, empty text the null text", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleNumber, Decimals: 2, Prefix: "<", Suffix: ">", NullText: "n/a"},
			values: []any{"pending", "", nil}, want: []string{"<pending>", "n/a", "n/a"}},
		{name: "a raw display writes values as they are", typ: value.Decimal, raw: &format.Resolved{},
			values: []any{"1.50", int64(7), true, "x", nil, 1234.5}, want: []string{"1.5", "7", "true", "x", "", "1234.5"}},
		{name: "hints: distance", typ: value.Decimal, hint: format.Hint{Kind: format.Distance},
			values: []any{1284.44}, want: []string{"1,284.4 mi"}},
		{name: "hints: weight", typ: value.Int, hint: format.Hint{Kind: format.Weight},
			values: []any{42000}, want: []string{"42,000 lbs"}},
		{name: "hints: count", typ: value.Int, hint: format.Hint{Kind: format.Count},
			values: []any{1204}, want: []string{"1,204"}},
		{name: "booleans", typ: value.Bool, values: []any{true, false}, want: []string{"Yes", "No"}},
		{name: "a bool column reads true and false strings as booleans", typ: value.Bool,
			spec:   &format.Spec{BoolStyle: format.BoolStyleOneZero},
			values: []any{"true", "false", "maybe"}, want: []string{"1", "0", "maybe"}},
		{name: "check booleans", typ: value.Bool, spec: &format.Spec{BoolStyle: format.BoolStyleCheck},
			values: []any{true, false}, want: []string{"✓", "✗"}},
		{name: "null text", typ: value.String, spec: &format.Spec{NullText: "—"},
			values: []any{nil, "", "ACME"}, want: []string{"—", "—", "ACME"}},
		{name: "enum labels", typ: value.Enum, hint: format.Hint{Labels: map[string]string{"InTransit": "In Transit"}},
			values: []any{"InTransit", "Delivered"}, want: []string{"In Transit", "Delivered"}},
		{name: "json is shown as written", typ: value.JSON, values: []any{[]byte(`{"a":1}`)}, want: []string{`{"a":1}`}},
	} {
		g.run(t)
	}
}

func TestFormatDurations(t *testing.T) {
	duration := func(style format.DurationStyle) *format.Spec {
		return &format.Spec{Style: format.StyleDuration, DurationUnit: format.DurationUnitSeconds,
			DurationStyle: style, Decimals: ptr(1)}
	}
	for _, g := range []golden{
		{name: "clock", typ: value.Decimal, spec: duration(format.DurationStyleClock), values: []any{94200}, want: []string{"26:10"}},
		{name: "compact", typ: value.Decimal, spec: duration(format.DurationStyleCompact), values: []any{94200},
			want: []string{"1d 2h 10m"}},
		{name: "verbose", typ: value.Decimal, spec: duration(format.DurationStyleVerbose), values: []any{94200},
			want: []string{"1 day 2 hours 10 minutes"}},
		{name: "decimal hours", typ: value.Decimal, spec: duration(format.DurationStyleDecimalHours), values: []any{94200},
			want: []string{"26.2 h"}},
		{name: "a negative keeps its sign", typ: value.Decimal,
			spec:   &format.Spec{Style: format.StyleDuration, DurationStyle: format.DurationStyleCompact},
			values: []any{-9000}, want: []string{"-2h 30m"}},
		{name: "minutes in decimal hours", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleDuration, DurationUnit: format.DurationUnitMinutes, DurationStyle: format.DurationStyleDecimalHours},
			values: []any{90}, want: []string{"1.5 h"}},
		{name: "clock truncates", typ: value.Decimal, raw: &format.Resolved{Style: format.StyleDuration, DurationStyle: format.DurationStyleClock},
			values: []any{59.9, 360000000, -59}, want: []string{"0:00", "100000:00", "0:00"}},
		{name: "verbose singulars and zero", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleDuration, DurationStyle: format.DurationStyleVerbose},
			values: []any{3660, 0, 59}, want: []string{"1 hour 1 minute", "0 minutes", "0 minutes"}},
		{name: "compact zero", typ: value.Decimal, raw: &format.Resolved{Style: format.StyleDuration, DurationStyle: format.DurationStyleCompact},
			values: []any{0}, want: []string{"0m"}},
		{name: "grouped decimal hours and text", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleDuration, DurationStyle: format.DurationStyleDecimalHours, Decimals: 1, Grouping: true},
			values: []any{36000000, "x"}, want: []string{"10,000.0 h", "x"}},
		{name: "days with parentheses", typ: value.Decimal,
			raw: &format.Resolved{Style: format.StyleDuration, DurationUnit: format.DurationUnitDays,
				DurationStyle: format.DurationStyleCompact, Prefix: "~", Negative: format.NegativeParentheses},
			values: []any{-1.5}, want: []string{"(~1d 12h)"}},
		{name: "hours round exactly", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleDuration, DurationUnit: format.DurationUnitHours, DurationStyle: format.DurationStyleDecimalHours, Decimals: 2},
			values: []any{1.005}, want: []string{"1.01 h"}},
		{name: "a duration kind is a clock", typ: value.Int, hint: format.Hint{Kind: format.Duration},
			values: []any{5400}, want: []string{"1:30"}},
	} {
		g.run(t)
	}
}

func TestFormatBandsAndTones(t *testing.T) {
	money := format.Hint{Kind: format.Money}
	for _, g := range []golden{
		{name: "explicit band, clamped below the first edge", typ: value.Decimal,
			hint:   format.Hint{Kind: format.Money, Band: &format.Band{Edges: []float64{0, 1000, 5000}}},
			values: []any{0, 1000, 5000, -250},
			want:   []string{"$0.00 – $1,000.00", "$1,000.00 – $5,000.00", "$5,000.00+", "$0.00 – $1,000.00"}},
		{name: "fixed-width bands are bounded", typ: value.Int, hint: format.Hint{Kind: format.Count, Band: &format.Band{Width: 5}},
			values: []any{0, 25, -10}, want: []string{"0 – 5", "25 – 30", "-10 – -5"}},
		{name: "band labels follow the display", typ: value.Int,
			hint:   format.Hint{Kind: format.Duration, Band: &format.Band{Edges: []float64{0, 7200, 14400}}},
			values: []any{0, 14400}, want: []string{"0:00 – 2:00", "4:00+"}},
		{name: "a trailing unit is written once", typ: value.Int, hint: format.Hint{Kind: format.Weight, Band: &format.Band{Width: 5000}},
			values: []any{10000}, want: []string{"10,000 – 15,000 lbs"}},
		{name: "band arithmetic is exact", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleNumber, Decimals: 1, Band: &format.Band{Width: 0.1}},
			values: []any{0.2}, want: []string{"0.2 – 0.3"}},
		{name: "a band that cannot describe a range is ignored", typ: value.Decimal,
			raw:    &format.Resolved{Style: format.StyleNumber, Decimals: 2, Grouping: true, Band: &format.Band{Edges: []float64{5, 5}}},
			values: []any{1000}, want: []string{"1,000.00"}},
		{name: "a banded date column shows numbers", typ: value.Int,
			hint: format.Hint{Band: &format.Band{Width: 10}}, spec: &format.Spec{Style: format.StyleDate},
			values: []any{5}, want: []string{"5 – 15"}},
		{name: "first matching rule wins", typ: value.Decimal, hint: money,
			spec: &format.Spec{Rules: []format.Rule{
				{Op: format.RuleGreater, Value: 100000, Tone: format.ToneNegative},
				{Op: format.RuleGreater, Value: 10000, Tone: format.ToneWarning}}},
			values: []any{250000, 50000, 500},
			want:   []string{"$250,000.00", "$50,000.00", "$500.00"},
			tones:  []format.Tone{format.ToneNegative, format.ToneWarning, format.ToneNone}},
		{name: "tones ignore nulls, text and booleans", typ: value.Decimal, hint: money,
			spec:   &format.Spec{Rules: []format.Rule{{Op: format.RuleGreater, Value: 0, Tone: format.ToneNegative}}},
			values: []any{nil, "ACME", true}, want: []string{"", "ACME", "Yes"},
			tones: []format.Tone{format.ToneNone, format.ToneNone, format.ToneNone}},
		{name: "percent rules compare on the shown scale", typ: value.Decimal, hint: format.Hint{Kind: format.Percent},
			spec:   &format.Spec{Rules: []format.Rule{{Op: format.RuleGreater, Value: 12, Tone: format.ToneNegative}}},
			values: []any{0.15, 0.08}, want: []string{"15.0%", "8.0%"},
			tones: []format.Tone{format.ToneNegative, format.ToneNone}},
		{name: "between is inclusive", typ: value.Int, hint: money,
			spec:   &format.Spec{Rules: []format.Rule{{Op: format.RuleBetween, Value: 10, Upper: 20, Tone: format.ToneWarning}}},
			values: []any{10, 20, 21}, want: []string{"$10.00", "$20.00", "$21.00"},
			tones: []format.Tone{format.ToneWarning, format.ToneWarning, format.ToneNone}},
		{name: "rules compare exactly", typ: value.Float,
			raw:    &format.Resolved{Style: format.StyleNumber, Decimals: 1, Rules: []format.Rule{{Op: format.RuleEqual, Value: 0.3, Tone: format.TonePositive}}},
			values: []any{0.3, tenth + fifth, "0.30"}, want: []string{"0.3", "0.3", "0.3"},
			tones: []format.Tone{format.TonePositive, format.ToneNone, format.TonePositive}},
		{name: "lte, lt and gte", typ: value.Int,
			raw: &format.Resolved{Style: format.StyleNumber, Rules: []format.Rule{
				{Op: format.RuleLess, Value: 0, Tone: format.ToneNegative},
				{Op: format.RuleLessEqual, Value: 10, Tone: format.ToneNeutral},
				{Op: format.RuleGreaterEqual, Value: 100, Tone: format.TonePositive}}},
			values: []any{-1, 10, 50, 100}, want: []string{"-1", "10", "50", "100"},
			tones: []format.Tone{format.ToneNegative, format.ToneNeutral, format.ToneNone, format.TonePositive}},
	} {
		g.run(t)
	}
}

func TestFormatDatesInTimeZones(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, time.March, 4, 1, 30, 0, 0, time.UTC)
	cases := []struct {
		style       format.DateStyle
		inChicago   string
		inTokyo     string
		chicagoTime string
	}{
		{format.DateStyleDate, "2026-03-03", "2026-03-04", ""},
		{format.DateStyleDateTime, "2026-03-03 19:30", "2026-03-04 10:30", ""},
		{format.DateStyleDateLong, "Mar 3, 2026", "Mar 4, 2026", ""},
		{format.DateStyleMonthYear, "Mar 2026", "Mar 2026", ""},
		{format.DateStyleQuarter, "Q1 2026", "Q1 2026", ""},
		{format.DateStyleYear, "2026", "2026", ""},
		{format.DateStyleTime, "19:30", "10:30", ""},
		{format.DateStyleWeekday, "Tue 2026-03-03", "Wed 2026-03-04", ""},
		{format.DateStyleISO, "2026-03-03T19:30:00-06:00", "2026-03-04T10:30:00+09:00", ""},
	}
	for _, c := range cases {
		for _, typ := range []value.Type{value.Epoch, value.Time} {
			d := format.Resolve(typ, format.Hint{}, &format.Spec{DateStyle: c.style})
			var v any = instant.Unix()
			if typ == value.Time {
				v = instant
			}
			if got, _ := format.New(typ, d, format.Options{Location: chicago}).Format(v); got != c.inChicago {
				t.Errorf("%s %s in Chicago: got %q, want %q", typ, c.style, got, c.inChicago)
			}
			if got, _ := format.New(typ, d, format.Options{Location: tokyo}).Format(v); got != c.inTokyo {
				t.Errorf("%s %s in Tokyo: got %q, want %q", typ, c.style, got, c.inTokyo)
			}
		}
	}
	decimalDate := format.Resolve(value.Decimal, format.Hint{}, &format.Spec{Style: format.StyleDate, DateStyle: format.DateStyleDate})
	if got, _ := format.New(value.Decimal, decimalDate, format.Options{}).Format("1772587800.5"); got != "2026-03-04" {
		t.Errorf("decimal epoch: got %q", got)
	}
	if got, _ := format.New(value.Time, format.Resolved{}, format.Options{Location: chicago}).Format(instant); got != "2026-03-04T01:30:00Z" {
		t.Errorf("raw instant: got %q", got)
	}
	month := format.Resolve(value.Time, format.Hint{Bucket: format.DateStyleMonthYear}, nil)
	if got, _ := format.New(value.Time, month, format.Options{Location: chicago}).Format(time.Date(2026, 3, 1, 6, 0, 0, 0, time.UTC)); got != "Mar 2026" {
		t.Errorf("month bucket starting at local midnight: got %q", got)
	}
}

func TestFormatLocales(t *testing.T) {
	money := func(code string) format.Resolved {
		return format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, &format.Spec{Currency: code})
	}
	number := format.Resolve(value.Decimal, format.Hint{}, &format.Spec{Decimals: ptr(1)})
	percent := format.Resolve(value.Decimal, format.Hint{Kind: format.Percent}, nil)
	cases := []struct {
		locale string
		d      format.Resolved
		value  any
		want   string
	}{
		{"en-US", money("USD"), -1234.5, "($1,234.50)"},
		{"en-CA", money("CAD"), 1234.5, "$1,234.50"},
		{"en-CA", money("USD"), 1234.5, "US$1,234.50"},
		{"de-DE", money("EUR"), 1234.5, "1.234,50\u00a0€"},
		{"de-CH", money("CHF"), 1234.5, "CHF\u00a01’234.50"},
		{"fr-FR", money("EUR"), 1234.5, "1\u00a0234,50\u00a0€"},
		{"nl-NL", money("EUR"), 1234.5, "€\u00a01.234,50"},
		{"es-MX", money("MXN"), 1234.5, "$1,234.50"},
		{"ja-JP", money("JPY"), 1500, "￥1,500"},
		{"en-US", money("BHD"), 1.5, "BHD\u00a01.500"},
		{"en-IN", number, 12345678.9, "1,23,45,678.9"},
		{"sv-SE", number, -1234.5, "−1\u00a0234,5"},
		{"tr-TR", percent, 0.5, "%50,0"},
		{"de-DE", percent, 0.5, "50,0\u00a0%"},
		{"ar", number, 1234.5, "١٬٢٣٤٫٥"},
	}
	for _, c := range cases {
		f := format.New(value.Decimal, c.d, format.Options{Locale: language.MustParse(c.locale)})
		if got, _ := f.Format(c.value); got != c.want {
			t.Errorf("%s %v: got %q, want %q", c.locale, c.value, got, c.want)
		}
	}
}

func TestResolve(t *testing.T) {
	baseline := format.Resolved{Style: format.StyleCurrency, Decimals: 2, Grouping: true, Currency: "USD",
		Negative: format.NegativeParentheses, Notation: format.NotationStandard, DateStyle: format.DateStyleDateTime,
		BoolStyle: format.BoolStyleYesNo, DurationUnit: format.DurationUnitSeconds, DurationStyle: format.DurationStyleClock}
	if got := format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, nil); !reflect.DeepEqual(got, baseline) {
		t.Errorf("money: got %+v", got)
	}
	check := func(name string, got format.Resolved, want func(format.Resolved) bool) {
		t.Helper()
		if !want(got) {
			t.Errorf("%s: got %+v", name, got)
		}
	}
	check("count", format.Resolve(value.Int, format.Hint{Kind: format.Count}, nil), func(d format.Resolved) bool {
		return d.Style == format.StyleNumber && d.Decimals == 0
	})
	check("float", format.Resolve(value.Float, format.Hint{}, nil), func(d format.Resolved) bool {
		return d.Style == format.StyleNumber && d.Decimals == 2
	})
	check("epoch", format.Resolve(value.Epoch, format.Hint{}, nil), func(d format.Resolved) bool { return d.Style == format.StyleDate })
	check("time", format.Resolve(value.Time, format.Hint{}, nil), func(d format.Resolved) bool { return d.Style == format.StyleDate })
	check("bool", format.Resolve(value.Bool, format.Hint{}, nil), func(d format.Resolved) bool { return d.Style == format.StyleBool })
	check("a kind on text is ignored", format.Resolve(value.Enum, format.Hint{Kind: format.Money}, nil), func(d format.Resolved) bool {
		return d.Style == format.StyleText && d.Decimals == 0
	})
	check("bucket on a date", format.Resolve(value.Epoch, format.Hint{Bucket: format.DateStyleMonthYear}, nil), func(d format.Resolved) bool {
		return d.DateStyle == format.DateStyleMonthYear
	})
	check("bucket on a number", format.Resolve(value.Decimal, format.Hint{Bucket: format.DateStyleMonthYear}, nil), func(d format.Resolved) bool {
		return d.DateStyle == format.DateStyleDateTime
	})
	check("money restyled as a number keeps an explicit suffix", format.Resolve(value.Decimal, format.Hint{Kind: format.Money},
		&format.Spec{Style: format.StyleNumber, Decimals: ptr(0), Suffix: " units"}), func(d format.Resolved) bool {
		return d.Style == format.StyleNumber && d.Decimals == 0 && d.Suffix == " units" && d.Negative == format.NegativeParentheses
	})
	check("distance restyled as the same style keeps its suffix", format.Resolve(value.Decimal, format.Hint{Kind: format.Distance},
		&format.Spec{Style: format.StyleNumber}), func(d format.Resolved) bool { return d.Suffix == " mi" && d.Decimals == 1 })
	check("every override", format.Resolve(value.Decimal, format.Hint{}, &format.Spec{Currency: "eur", Decimals: ptr(99),
		Grouping: ptr(false), Negative: format.NegativeParentheses, Notation: format.NotationCompact, Prefix: "~",
		BoolStyle: format.BoolStyleCheck, DurationUnit: format.DurationUnitHours, DurationStyle: format.DurationStyleVerbose,
		NullText: "—", DateStyle: format.DateStyleISO}), func(d format.Resolved) bool {
		return d.Currency == "EUR" && d.Decimals == 10 && !d.Grouping && d.Negative == format.NegativeParentheses &&
			d.Notation == format.NotationCompact && d.Prefix == "~" && d.BoolStyle == format.BoolStyleCheck &&
			d.DurationUnit == format.DurationUnitHours && d.DurationStyle == format.DurationStyleVerbose &&
			d.NullText == "—" && d.DateStyle == format.DateStyleISO
	})
	check("a currency sets its places", format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, &format.Spec{Currency: "JPY"}),
		func(d format.Resolved) bool { return d.Decimals == 0 })
	check("explicit places win over the currency's", format.Resolve(value.Decimal, format.Hint{Kind: format.Money},
		&format.Spec{Currency: "JPY", Decimals: ptr(2)}), func(d format.Resolved) bool { return d.Decimals == 2 })
	check("bands and rules are carried", format.Resolve(value.Int, format.Hint{Band: &format.Band{Width: 5000}},
		&format.Spec{Rules: []format.Rule{{Op: format.RuleBetween, Value: 1, Upper: 2.5, Tone: format.ToneWarning}}}),
		func(d format.Resolved) bool {
			return d.Band.Width == 5000 && len(d.Rules) == 1 && d.Rules[0].Upper == 2.5
		})
	check("an empty band is dropped", format.Resolve(value.Int, format.Hint{Band: &format.Band{}}, nil),
		func(d format.Resolved) bool { return d.Band == nil })
	check("a malformed band is kept for validation", format.Resolve(value.Int, format.Hint{Band: &format.Band{Edges: []float64{5}}}, nil),
		func(d format.Resolved) bool { return d.Band != nil && d.Band.Validate() != nil })
}

func TestValidate(t *testing.T) {
	cases := []struct {
		spec  *format.Spec
		typ   value.Type
		field string
		code  string
	}{
		{nil, value.Decimal, "", ""},
		{&format.Spec{Style: format.StyleCurrency}, value.Decimal, "", ""},
		{&format.Spec{Style: format.StyleCurrency}, value.String, "style", format.CodeStyleNotAllowed},
		{&format.Spec{Style: format.StyleDate}, value.Int, "", ""},
		{&format.Spec{Style: format.StyleNumber}, value.Time, "style", format.CodeStyleNotAllowed},
		{&format.Spec{Style: "wat"}, value.Decimal, "style", format.CodeUnknownStyle},
		{&format.Spec{Decimals: ptr(99)}, value.Decimal, "decimals", format.CodeDecimalsRange},
		{&format.Spec{Currency: "US"}, value.Decimal, "currency", format.CodeUnknownCurrency},
		{&format.Spec{Currency: "XYZ"}, value.Decimal, "currency", format.CodeUnknownCurrency},
		{&format.Spec{Currency: "eur"}, value.Decimal, "", ""},
		{&format.Spec{Prefix: strings.Repeat("x", 13)}, value.Decimal, "prefix", format.CodeAffixTooLong},
		{&format.Spec{NullText: strings.Repeat("—", 25)}, value.Decimal, "nullText", format.CodeNullTextTooLong},
		{&format.Spec{Negative: "red"}, value.Decimal, "negative", format.CodeUnknownNegative},
		{&format.Spec{DurationStyle: "long"}, value.Decimal, "durationStyle", format.CodeUnknownDurationStyle},
		{&format.Spec{Rules: []format.Rule{{Op: "nonsense", Tone: format.ToneNegative}}}, value.Decimal, "rules[0].op", format.CodeUnknownOp},
		{&format.Spec{Rules: []format.Rule{{Op: format.RuleGreater, Tone: "chartreuse"}}}, value.Decimal, "rules[0].tone", format.CodeUnknownTone},
		{&format.Spec{Rules: []format.Rule{{Op: format.RuleGreater, Value: 1, Tone: format.ToneWarning},
			{Op: format.RuleBetween, Value: 20, Upper: 10, Tone: format.ToneWarning}}}, value.Decimal, "rules[1].upper", format.CodeUpperBelowLower},
		{&format.Spec{Rules: make([]format.Rule, format.MaxRules+1)}, value.Decimal, "rules", format.CodeTooManyRules},
	}
	for i, c := range cases {
		err := c.spec.Validate(c.typ)
		if c.code == "" {
			if err != nil {
				t.Errorf("case %d: unexpected %v", i, err)
			}
			continue
		}
		var fe *format.Error
		if !errors.As(err, &fe) || fe.Field != c.field || fe.Code != c.code {
			t.Errorf("case %d: got %v, want %s %s", i, err, c.field, c.code)
		}
	}
}

func TestBandValidate(t *testing.T) {
	cases := map[string]*format.Band{
		"":                           nil,
		format.CodeBandWidthAndEdges: {Width: 5, Edges: []float64{0, 1}},
		format.CodeBandWidth:         {Width: -5},
		format.CodeBandTooFewEdges:   {Edges: []float64{500}},
		format.CodeBandNotAscending:  {Edges: []float64{5, 5}},
		format.CodeBandTooManyEdges:  {Edges: make([]float64, format.MaxBandEdges+1)},
	}
	for code, band := range cases {
		err := band.Validate()
		var fe *format.Error
		if code == "" {
			if err != nil {
				t.Errorf("empty band: %v", err)
			}
			continue
		}
		if !errors.As(err, &fe) || fe.Code != code {
			t.Errorf("%s: got %v", code, err)
		}
	}
	if !(&format.Band{Width: 5, Edges: nil}).IsWhole() || (&format.Band{Width: 0.5}).IsWhole() {
		t.Error("IsWhole")
	}
	if !(&format.Band{Edges: []float64{0, 10}}).Lowest(-3) || (&format.Band{Width: 5}).Lowest(-3) {
		t.Error("Lowest")
	}
}

func TestExcelFormats(t *testing.T) {
	berlin := format.Options{Locale: language.German}
	cases := []struct {
		typ    value.Type
		d      format.Resolved
		opts   format.Options
		code   string
		native bool
	}{
		{value.Decimal, format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, nil), format.Options{},
			`"$"#,##0.00;("$"#,##0.00)`, true},
		{value.Decimal, format.Resolve(value.Decimal, format.Hint{Kind: format.Percent}, nil), format.Options{}, "#,##0.0%", true},
		{value.Decimal, format.Resolve(value.Decimal, format.Hint{Kind: format.Percent}, nil), berlin, "#,##0.0\"\u00a0\"%", true},
		{value.Decimal, format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, &format.Spec{Currency: "EUR", Negative: format.NegativeMinus}),
			berlin, "#,##0.00\"\u00a0€\"", true},
		{value.Decimal, format.Resolve(value.Decimal, format.Hint{Kind: format.Duration}, nil), format.Options{}, "", false},
		{value.Epoch, format.Resolve(value.Epoch, format.Hint{}, nil), format.Options{}, "yyyy-mm-dd hh:mm", true},
		{value.Time, format.Resolve(value.Time, format.Hint{}, &format.Spec{DateStyle: format.DateStyleDateLong}), format.Options{}, `mmm d", "yyyy`, true},
		{value.Epoch, format.Resolve(value.Epoch, format.Hint{}, &format.Spec{DateStyle: format.DateStyleQuarter}), format.Options{}, "", false},
		{value.Int, format.Resolve(value.Int, format.Hint{Kind: format.Weight}, nil), format.Options{}, `#,##0" lbs"`, true},
		{value.Decimal, format.Resolve(value.Decimal, format.Hint{}, &format.Spec{Notation: format.NotationCompact}), format.Options{}, "", false},
		{value.Decimal, format.Resolve(value.Decimal, format.Hint{Band: &format.Band{Width: 5}}, nil), format.Options{}, "", false},
		{value.Float, format.Resolved{}, format.Options{}, "", true},
		{value.String, format.Resolve(value.String, format.Hint{}, nil), format.Options{}, "", false},
		{value.Bool, format.Resolve(value.Bool, format.Hint{}, nil), format.Options{}, "", false},
	}
	for i, c := range cases {
		code, native := format.New(c.typ, c.d, c.opts).Excel()
		if code != c.code || native != c.native {
			t.Errorf("case %d: got %q %v, want %q %v", i, code, native, c.code, c.native)
		}
	}
}

func TestRulesPublishesTheVocabulary(t *testing.T) {
	v := format.Rules()
	if len(v.Types) != len(value.Types()) {
		t.Fatalf("types: %d", len(v.Types))
	}
	for _, ts := range v.Types {
		for _, style := range ts.Styles {
			if err := (&format.Spec{Style: style}).Validate(ts.Type); err != nil {
				t.Errorf("%s allows %s but Validate refuses it: %v", ts.Type, style, err)
			}
		}
		if ts.Default != format.Resolve(ts.Type, format.Hint{}, nil).Style {
			t.Errorf("%s default %s", ts.Type, ts.Default)
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `{"type":"decimal","default":"number","styles":["number","currency","percent","duration","date","text"]}`) {
		t.Errorf("vocabulary JSON: %s", b)
	}
	v.Types[0].Styles[0] = "mutated"
	if format.Rules().Types[0].Styles[0] == "mutated" {
		t.Error("Rules shares its tables")
	}
}

func TestSpecJSON(t *testing.T) {
	in := `{"style":"currency","decimals":0,"grouping":false,"currency":"EUR","rules":[{"op":"between","value":1,"upper":2.5,"tone":"warning"}]}`
	var s format.Spec
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatal(err)
	}
	if *s.Decimals != 0 || *s.Grouping || s.Rules[0].Upper != 2.5 || s.IsEmpty() {
		t.Fatalf("decoded %+v", s)
	}
	out, err := json.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("round trip:\n got %s\nwant %s", out, in)
	}
	if !(&format.Spec{}).IsEmpty() || !(*format.Spec)(nil).IsEmpty() {
		t.Error("IsEmpty")
	}
}

func TestColumnAllocatesNothingOnceWarm(t *testing.T) {
	money := format.Resolve(value.Decimal, format.Hint{Kind: format.Money},
		&format.Spec{Rules: []format.Rule{{Op: format.RuleGreater, Value: 100, Tone: format.ToneWarning}}})
	vectors := map[value.Type]*value.Vector{
		value.Decimal: {Numeric: numerics("1082.07", "-12.5", "1000000.125")},
		value.Int:     {Int64: []int64{1, -2, 3000000}, Nulls: value.NewBitmap(3)},
		value.Float:   {Float64: []float64{1.5, -0.25, 12345.678}},
		value.Epoch:   {Int64: []int64{1772587800, 0, 1}},
		value.String:  {String: []string{"a", "b", ""}},
	}
	vectors[value.Int].Nulls.Set(1)
	for typ, v := range vectors {
		d := format.Resolve(typ, format.Hint{}, nil)
		if typ == value.Decimal {
			d = money
		}
		f := format.New(typ, d, format.Options{Locale: language.German})
		var c format.Cells
		f.Column(&c, v, 3)
		allocs := testing.AllocsPerRun(100, func() {
			c.Reset()
			f.Column(&c, v, 3)
		})
		if allocs != 0 {
			t.Errorf("%s: %v allocations per batch", typ, allocs)
		}
	}
}

func numerics(texts ...string) []keel.Decimal {
	out := make([]keel.Decimal, len(texts))
	for i, s := range texts {
		n, err := value.ParseDecimal(s)
		if err != nil {
			panic(err)
		}
		out[i] = n
	}
	return out
}
