package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestGroupingSets(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	s := fleetdb.Shipments
	code := s.Customer().Code
	type row struct {
		Customer *string
		Status   *fleet.Status
		N        int64
		Level    int32
	}
	level := keel.Grouping(code, s.Status)

	rows := must(keel.Into[row](w.ctx, d, s.Pick(code, s.Status, keel.Count(), level).
		Rollup(code, s.Status).Order(level.Asc(), code.Asc(), s.Status.Asc())))(t)
	eq(t, len(rows), 16)
	detail, subtotal := 0, int64(0)
	for _, r := range rows {
		switch r.Level {
		case 0:
			detail++
		case 1:
			if r.Customer == nil || r.Status != nil {
				t.Fatalf("subtotal row %+v", r)
			}
			subtotal += r.N
		case 3:
			if r.Customer != nil || r.N != 20 {
				t.Fatalf("total row %+v", r)
			}
		default:
			t.Fatalf("unexpected level %d", r.Level)
		}
	}
	eq(t, detail, 12)
	eq(t, subtotal, int64(20))
	eq(t, *rows[12].Customer, "C0")
	eq(t, rows[12].N, int64(7))

	rows = must(keel.Into[row](w.ctx, d, s.Pick(code, s.Status, keel.Count(), level).
		GroupingSets([]keel.Expr{s.Status}, []keel.Expr{code}, nil).
		Order(level.Asc(), code.Asc(), s.Status.Asc())))(t)
	eq(t, len(rows), 8)
	eq(t, rows[7].N, int64(20))
	eq(t, rows[7].Level, int32(3))

	rows = must(keel.Into[row](w.ctx, d, s.Pick(code, s.Status, keel.Count(), level).
		Cube(code, s.Status)))(t)
	eq(t, len(rows), 20)

	rows = must(keel.Into[row](w.ctx, d, s.Pick(code, s.Status, keel.Count(), level).
		Rollup(code, s.Status).Having(level.Gt(0)).Order(level.Desc())))(t)
	eq(t, len(rows), 4)
	eq(t, rows[0].Level, int32(3))

	type mixed struct {
		Hazmat bool
		Status *fleet.Status
		N      int64
	}
	mixedRows := must(keel.Into[mixed](w.ctx, d, s.Pick(s.Hazmat, s.Status, keel.Count()).
		Group(s.Hazmat).Rollup(s.Status).Order(s.Hazmat.Asc(), s.Status.Asc())))(t)
	eq(t, len(mixedRows), 10)
	eq(t, mixedRows[4].Status == nil, true)
	eq(t, mixedRows[4].N, int64(16))

	bucket := keel.Raw("CASE WHEN ? >= ? THEN ? ELSE ? END", s.Priority, 1, "high", "low")
	type bucketRow struct {
		Bucket *string
		N      int64
		Level  int32
	}
	buckets := must(keel.Into[bucketRow](w.ctx, d, s.Pick(bucket, keel.Count(), keel.Grouping(bucket)).
		Rollup(bucket).Order(keel.Grouping(bucket).Asc(), keel.Asc(bucket))))(t)
	eq(t, len(buckets), 3)
	eq(t, buckets[2].N, int64(20))
	eq(t, buckets[2].Level, int32(1))

	n := must(s.Rollup(s.Status).Count(w.ctx, d))(t)
	eq(t, n, int64(5))

	_, _, err := s.Pick(keel.Count()).Rollup().ToSQL(w.ctx, d)
	if err == nil || !strings.Contains(err.Error(), "ROLLUP needs at least one expression") {
		t.Fatalf("empty rollup: %v", err)
	}
	_, _, err = s.Pick(keel.Grouping()).Group(s.Status).ToSQL(w.ctx, d)
	if err == nil {
		t.Fatal("Grouping without expressions rendered")
	}
}
