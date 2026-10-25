package keel_test

import (
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestLiteralsTakeTheTypeOfTheirGoValueWhereNothingElseTypesThem(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	tests := []struct {
		name     string
		sql      func() (string, error)
		contains []string
		lacks    []string
	}{
		{
			name: "a picked literal",
			sql: func() (string, error) {
				q, _, err := s.Pick(s.ID, keel.DynLit(0), keel.DynLit("x"), keel.DynLit(true), keel.DynLit(1.5),
					keel.DynLit(time.Time{}), keel.DynLit([]string{"a"})).ToSQL(ctx, d)
				return q, err
			},
			contains: []string{
				`$3::bigint`, `$4::text`, `$5::boolean`, `$6::double precision`, `$7::timestamptz`, `$8::text[]`,
			},
		},
		{
			name: "CASE results that are all literals",
			sql: func() (string, error) {
				q, _, err := s.Pick(keel.DynCase().When(s.Hazmat.IsTrue(), 1).Else(0)).ToSQL(ctx, d)
				return q, err
			},
			contains: []string{`THEN $3::bigint ELSE $4::bigint END`},
		},
		{
			name: "CASE with an expression result types its literals after it",
			sql: func() (string, error) {
				q, _, err := s.Pick(keel.DynCase().When(s.Hazmat.IsTrue(), s.Status).Else(fleet.StatusNew)).ToSQL(ctx, d)
				return q, err
			},
			contains: []string{`THEN s."status" ELSE $3 END`},
			lacks:    []string{`::text`},
		},
		{
			name: "concat arguments",
			sql: func() (string, error) {
				q, _, err := s.Pick(keel.DynFunc("concat", s.ProNumber, "-", 7)).ToSQL(ctx, d)
				return q, err
			},
			contains: []string{`concat(s."pro_number", $3::text, $4::bigint)`},
		},
		{
			name: "comparisons leave the value to the other operand",
			sql: func() (string, error) {
				q, _, err := s.Pick(s.ID).Where(keel.DynOf(s.Weight).Add(s.Weight).Gt(10), keel.DynFunc("lower", s.ProNumber).Eq("x")).ToSQL(ctx, d)
				return q, err
			},
			contains: []string{`> $3`, `= $4`},
			lacks:    []string{`$3::`, `$4::`},
		},
		{
			name: "a type with methods is left to Postgres",
			sql: func() (string, error) {
				q, _, err := s.Pick(keel.DynLit(time.Second)).ToSQL(ctx, d)
				return q, err
			},
			lacks: []string{`::`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, err := tt.sql()
			ok(t, err)
			for _, c := range tt.contains {
				if !strings.Contains(sql, c) {
					t.Fatalf("%s\nlacks %s", sql, c)
				}
			}
			for _, c := range tt.lacks {
				if strings.Contains(sql, c) {
					t.Fatalf("%s\nhas %s", sql, c)
				}
			}
		})
	}
}

func TestGroupAndOrderNameAPickedExpressionByPosition(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	day := keel.DynFunc("date_trunc", "day", s.CreatedAt)
	total := keel.DynOf(s.Weight).Sum()
	sql, _, err := s.Pick(s.Status, day, total).Group(s.Status, day).Order(total.Desc(), s.Status.Asc()).ToSQL(ctx, d)
	ok(t, err)
	if !strings.Contains(sql, ` GROUP BY s."status", 2 ORDER BY 3 DESC, s."status" ASC`) {
		t.Fatalf("got %s", sql)
	}
	if strings.Count(sql, "date_trunc") != 1 {
		t.Fatalf("date_trunc written more than once: %s", sql)
	}

	sql, _, err = s.Group(s.Status, day).Pick(keel.Count()).Order(day.Asc()).ToSQL(ctx, d)
	ok(t, err)
	if strings.Count(sql, "date_trunc") != 2 {
		t.Fatalf("an expression the select list lacks is written out: %s", sql)
	}
}

func TestEnumConstantsAreWrittenIntoTheStatement(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	sql, args, err := s.Pick(s.ID).Where(
		s.Status.Eq(fleet.StatusNew),
		s.Status.NotIn(fleet.StatusCanceled, fleet.StatusDelivered),
		keel.DynOf(s.Priority).Gte(fleet.PriorityHigh),
		keel.DynOf(s.Status).In([]fleet.Status{fleet.StatusAssigned}),
		s.Status.Ne(fleet.Status("It's")),
	).ToSQL(ctx, d)
	ok(t, err)
	for _, want := range []string{
		`s."status" = 'New'`, `s."status" NOT IN ('Canceled', 'Delivered')`, `(s."priority" >= 1)`,
		`(s."status" IN ('Assigned'))`, `s."status" <> $3`,
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("%s\nlacks %s", sql, want)
		}
	}
	eq(t, len(args), 3)
}

func TestDistinctAggregatesArraysAndOrderedJSONAggregates(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	m := fleetdb.Moves
	sql, _, err := s.Pick(
		s.ID,
		keel.DynOf(keel.ArrayAgg(s.Status)).Distinct(),
		keel.DynFunc("jsonb_agg", s.ProNumber).OrderBy(s.CreatedAt.Desc()),
		keel.ArrayOf(m.Pick(m.Status).Where(m.ShipmentID.EqCol(s.ID)).Distinct()),
		keel.DynFunc("ln", s.Weight),
	).Group(s.ID).ToSQL(ctx, d)
	ok(t, err)
	for _, want := range []string{
		`array_agg(DISTINCT s."status")`, `jsonb_agg(s."pro_number" ORDER BY s."created_at" DESC)`,
		`ARRAY(SELECT DISTINCT m."status" FROM "moves" m`, `ln(s."weight")`,
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("%s\nlacks %s", sql, want)
		}
	}
}

func TestUpsertFromSQLRendersWithoutRunning(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	c := fleetdb.Contacts
	src := keel.Rows("src", []string{"id", "name", "score"}, keel.DynLit("con_1"), keel.DynLit("n"), keel.DynLit(1))
	sql, args, err := c.UpsertFromSQL(ctx, d, src,
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Name).Set(c.Score.Add(1)).UpdateWhere(c.Score.Lt(3)),
		c.ID, c.Name, c.Score)
	ok(t, err)
	if !strings.Contains(sql, `ON CONFLICT ("organization_id", "business_unit_id", "name") DO UPDATE SET "score" = c."score" + $`) ||
		!strings.Contains(sql, ` WHERE c."score" < $`) || len(args) == 0 {
		t.Fatalf("%s %v", sql, args)
	}
}
