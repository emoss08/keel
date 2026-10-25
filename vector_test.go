package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/vector"
)

type halfVector []float32

func TestVectorDistancesAndCasts(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	cases := map[string]keel.DynExpr{
		"(($3)::halfvec(3) <=> ($4)::halfvec(3))": vector.CosineDistance(vector.HalfVec(halfVector{1}, 3), vector.HalfVec(halfVector{2}, 3)),
		"($3 <-> $4)":                 vector.L2Distance(halfVector{1}, halfVector{2}),
		"($3 <#> $4)":                 vector.NegativeInnerProduct(halfVector{1}, halfVector{2}),
		"((s.\"id\")::vector <+> $3)": vector.L1Distance(vector.Vec(s.ID, 0), halfVector{2}),
	}
	for want, e := range cases {
		sql, _, err := s.Pick(s.ID).Order(e.Asc()).ToSQL(ctx, d)
		ok(t, err)
		if !strings.Contains(sql, "ORDER BY "+want+" ASC") {
			t.Fatalf("got %s, want %s", sql, want)
		}
	}
}
