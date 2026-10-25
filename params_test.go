package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestParameterSharing(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments

	other := fleet.Status("Other")
	sql, args, err := s.Pick(s.ID).Where(keel.Or(s.Status.Eq(other), s.ProNumber.Eq("Other"),
		keel.And(s.Status.Eq(other), s.Hazmat.IsTrue()))).ToSQL(ctx, d)
	ok(t, err)
	want := `WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND (s."status" = $3 OR ` +
		`s."pro_number" = $4 OR (s."status" = $3 AND s."hazmat" IS TRUE))`
	if !strings.HasSuffix(sql, want) {
		t.Fatalf("an equal value on the same column shares, another column does not:\n%s", sql)
	}
	eq(t, len(args), 4)

	sql, args, err = s.Pick(s.ID).Where(s.Priority.Eq(7), s.Weight.Eq(7), s.Priority.Gt(7)).ToSQL(ctx, d)
	ok(t, err)
	if !strings.HasSuffix(sql, `s."priority" = $3 AND s."weight" = $4 AND s."priority" > $5`) {
		t.Fatalf("columns and operators keep their own placeholders:\n%s", sql)
	}
	eq(t, len(args), 5)

	bucket := keel.Raw("CASE WHEN ? >= ? THEN ? ELSE ? END", s.Priority, 1, "high", "low")
	sql, args, err = s.Pick(bucket, keel.Count()).Group(bucket).Order(keel.Asc(bucket)).ToSQL(ctx, d)
	ok(t, err)
	expr := `CASE WHEN s."priority" >= $3 THEN $4 ELSE $5 END`
	if strings.Count(sql, expr) != 3 {
		t.Fatalf("a reused expression renders identically in every clause:\n%s", sql)
	}
	eq(t, len(args), 5)

	again := keel.Raw("CASE WHEN ? >= ? THEN ? ELSE ? END", s.Priority, 1, "high", "low")
	sql, _, err = s.Pick(bucket, keel.Count()).Group(again).ToSQL(ctx, d)
	ok(t, err)
	if strings.Count(sql, expr) != 1 {
		t.Fatalf("two Raw expressions keep their own placeholders:\n%s", sql)
	}

	v := keel.Val("x")
	sql, args, err = s.Pick(v, v, keel.Val("x")).ToSQL(ctx, d)
	ok(t, err)
	if !strings.HasPrefix(sql, `SELECT $3, $3, $4 FROM`) {
		t.Fatalf("a Val written twice binds once:\n%s", sql)
	}
	eq(t, len(args), 4)
}

func TestSharedParameterRuns(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	s := fleetdb.Shipments
	bucket := keel.Raw("CASE WHEN ? >= ? THEN ? ELSE ? END", s.Priority, 1, "high", "low")
	type row struct {
		Bucket string
		N      int64
	}
	rows := must(keel.Into[row](w.ctx, d, s.Pick(bucket, keel.Count()).Group(bucket).
		Order(keel.Asc(bucket))))(t)
	eq(t, len(rows), 2)
	eq(t, rows[0], row{"high", 13})
	eq(t, rows[1], row{"low", 7})
}
