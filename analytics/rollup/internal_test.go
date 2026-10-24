package rollup

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

func TestSplitKeyInvertsTenantKeys(t *testing.T) {
	for _, vals := range [][]string{{"org_1", "bu_2"}, {`a/b`, `c\d`}, {`\/`, ""}, {"x"}} {
		key := joinKey(vals)
		if got := splitKey(key); !slices.Equal(got, vals) {
			t.Errorf("splitKey(%q) = %q, want %q", key, got, vals)
		}
	}
	if splitKey("") != nil {
		t.Error("the empty key has values")
	}
}

func TestTruncateMatchesDateTrunc(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 3, 8, 22, 45, 10, 0, chicago)
	cases := map[ir.Bucket]string{
		ir.BucketHour:    "2026-03-08T22:00:00-05:00",
		ir.BucketDay:     "2026-03-08T00:00:00-06:00",
		ir.BucketWeek:    "2026-03-02T00:00:00-06:00",
		ir.BucketMonth:   "2026-03-01T00:00:00-06:00",
		ir.BucketQuarter: "2026-01-01T00:00:00-06:00",
		ir.BucketYear:    "2026-01-01T00:00:00-06:00",
	}
	for unit, want := range cases {
		if got := truncate(at, unit, chicago).Format(time.RFC3339); got != want {
			t.Errorf("truncate to %s = %s, want %s", unit, got, want)
		}
	}
}

func TestCalendarRange(t *testing.T) {
	now := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	cases := map[ir.Op][2]string{
		ir.OpToday:       {"2026-01-01", "2026-01-02"},
		ir.OpYesterday:   {"2025-12-31", "2026-01-01"},
		ir.OpTomorrow:    {"2026-01-02", "2026-01-03"},
		ir.OpThisWeek:    {"2025-12-29", "2026-01-05"},
		ir.OpLastWeek:    {"2025-12-22", "2025-12-29"},
		ir.OpLastMonth:   {"2025-12-01", "2026-01-01"},
		ir.OpThisQuarter: {"2026-01-01", "2026-04-01"},
		ir.OpLastQuarter: {"2025-10-01", "2026-01-01"},
		ir.OpLastYear:    {"2025-01-01", "2026-01-01"},
	}
	for op, want := range cases {
		lo, hi := calendarRange(op, now, time.UTC)
		if lo.Format(time.DateOnly) != want[0] || hi.Format(time.DateOnly) != want[1] {
			t.Errorf("%s = [%s, %s), want %v", op, lo.Format(time.DateOnly), hi.Format(time.DateOnly), want)
		}
	}
}

func TestInstant(t *testing.T) {
	want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, v := range []any{"2026-03-01", "2026-03-01T00:00:00Z", "2026-03-01 00:00:00", want} {
		if got, ok := instant(v, schema.ValueTime, time.UTC); !ok || !got.Equal(want) {
			t.Errorf("instant(%v) = %v %v", v, got, ok)
		}
	}
	for _, v := range []any{json.Number("1772323200"), float64(1772323200), int64(1772323200), 1772323200} {
		if got, ok := instant(v, schema.ValueEpoch, time.UTC); !ok || !got.Equal(want) {
			t.Errorf("instant(%v) = %v %v", v, got, ok)
		}
	}
	for _, v := range []any{1772323200, "yesterday", 1.5, json.Number("x")} {
		if _, ok := instant(v, schema.ValueTime, time.UTC); ok {
			t.Errorf("instant(%v) was read as a time for a timestamptz field", v)
		}
	}
	if _, ok := instant(1.5, schema.ValueEpoch, time.UTC); ok {
		t.Error("a fractional epoch was read")
	}
}

func TestCoarsens(t *testing.T) {
	want := map[ir.Bucket][]ir.Bucket{
		ir.BucketHour:    ir.Buckets(),
		ir.BucketDay:     {ir.BucketDay, ir.BucketWeek, ir.BucketMonth, ir.BucketQuarter, ir.BucketYear},
		ir.BucketWeek:    {ir.BucketWeek},
		ir.BucketMonth:   {ir.BucketMonth, ir.BucketQuarter, ir.BucketYear},
		ir.BucketQuarter: {ir.BucketQuarter, ir.BucketYear},
		ir.BucketYear:    {ir.BucketYear},
	}
	for g, qs := range want {
		for _, q := range ir.Buckets() {
			if coarsens(g, q) != slices.Contains(qs, q) {
				t.Errorf("coarsens(%s, %s) = %v", g, q, coarsens(g, q))
			}
		}
	}
}

func TestBucketAndRangeSQL(t *testing.T) {
	r := &Rollup{source: &schema.Table{Columns: []schema.Column{{Name: "on"}}}, unit: ir.BucketWeek, zoneName: "UTC"}
	cases := []struct {
		vt           schema.ValueType
		bucket, span string
	}{
		{schema.ValueTime, `date_trunc('week', s."on", 'UTC')`, `s."on" >= b AND s."on" < ((b AT TIME ZONE 'UTC') + interval '7 days') AT TIME ZONE 'UTC'`},
		{schema.ValueDate, `(date_trunc('week', s."on"::timestamp) AT TIME ZONE 'UTC')`, `s."on" >= (b AT TIME ZONE 'UTC')::date AND s."on" < ((((b AT TIME ZONE 'UTC') + interval '7 days') AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')::date`},
		{schema.ValueEpoch, `date_trunc('week', to_timestamp(s."on"), 'UTC')`, `s."on" >= extract(epoch FROM b)::int8 AND s."on" < extract(epoch FROM ((b AT TIME ZONE 'UTC') + interval '7 days') AT TIME ZONE 'UTC')::int8`},
	}
	for _, tc := range cases {
		r.grainType = tc.vt
		if got := r.bucketExpr("s"); got != tc.bucket {
			t.Errorf("bucket %v = %s", tc.vt, got)
		}
		if got := r.rangeExpr("s", "b"); got != tc.span {
			t.Errorf("range %v = %s", tc.vt, got)
		}
	}
}
