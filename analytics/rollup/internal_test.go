package rollup

import (
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

func TestCamel(t *testing.T) {
	for in, want := range map[string]string{"ProNumber": "proNumber", "CustomerID": "customerId", "HTTPServer": "httpServer",
		"ID": "id", "BillToID": "billToId", "total_sum": "totalSum"} {
		if got := camel(in); got != want {
			t.Errorf("camel(%s) = %s, want %s", in, got, want)
		}
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
	r := &Rollup{source: &schema.Table{Columns: []schema.Column{{Name: "on"}}}, unit: ir.BucketWeek, zoneName: "UTC", zone: time.UTC}
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

func TestRangeInAZoneWithClockChanges(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	r := &Rollup{source: &schema.Table{Columns: []schema.Column{{Name: "at"}}}, unit: ir.BucketHour,
		zoneName: "America/Chicago", zone: chicago, grainType: schema.ValueTime}
	want := `s."at" >= (b - interval '3 hours') AND s."at" < (((b AT TIME ZONE 'America/Chicago') + interval '1 hour') AT TIME ZONE 'America/Chicago' + interval '3 hours') AND date_trunc('hour', s."at", 'America/Chicago') = b`
	if got := r.rangeExpr("s", "b"); got != want {
		t.Fatalf("range = %s\nwant    %s", got, want)
	}
}
