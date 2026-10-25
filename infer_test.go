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
