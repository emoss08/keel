package keel_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestTypedOperatorsRender(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	c := fleetdb.Contacts
	n := int64(42)
	cases := []struct {
		name string
		q    sqlRenderer
		want string
		args map[int]any
	}{
		{"bigint add", s.Pick(s.Weight.Add(keel.Int64(3))), `(s."weight" + $3::bigint)`, map[int]any{2: int64(3)}},
		{"bigint chain", s.Pick(s.Weight.Sub(s.CreatedAt).Div(keel.Int64(86400))),
			`((s."weight" - s."created_at") / $3::bigint)`, nil},
		{"bigint mul of a literal", s.Pick(keel.Int64(5).Mul(s.CreatedAt)), `($3::bigint * s."created_at")`, nil},
		{"bigint mod", s.Pick(s.Weight.Mod(keel.Int64(7))), `(s."weight" % $3::bigint)`, nil},
		{"bigint neg", s.Pick(s.Weight.Neg()), `(-s."weight")`, nil},
		{"equal literals share", s.Pick(s.Weight.Mul(keel.Int64(60)).Add(keel.Int64(60))),
			`((s."weight" * $3::bigint) + $3::bigint)`, nil},
		{"integer add", c.Pick(c.Visits.Add(keel.Int32(1))), `(c."visits" + $3::integer)`, nil},
		{"integer widened", c.Pick(keel.CastInt64(c.Visits).Mul(keel.Int64(86400))),
			`(CAST(c."visits" AS bigint) * $3::bigint)`, nil},
		{"integer neg", c.Pick(c.Visits.Neg()), `(-c."visits")`, nil},
		{"float div", c.Pick(c.Rating.Div(keel.Float64(2))), `(c."rating" / $3::double precision)`, nil},
		{"float neg", c.Pick(c.Rating.Neg()), `(-c."rating")`, nil},
		{"numeric sub abs", s.Pick(s.Total.Sub(keel.Numeric("0.5")).Abs()), `abs((s."total" - $3::numeric))`,
			map[int]any{2: "0.5"}},
		{"numeric mod", s.Pick(keel.NumericOf(s.Total).Mod(keel.CastNumeric(s.Weight))),
			`(s."total" % CAST(s."weight" AS numeric))`, nil},
		{"time plus interval", c.Pick(c.LastSeenAt.Add(keel.Interval("1 day"))),
			`(c."last_seen_at" + $3::interval)`, map[int]any{2: "1 day"}},
		{"time minus duration", c.Pick(keel.Now().Sub(keel.Duration(90 * time.Minute))),
			`(now() - $3::interval)`, map[int]any{2: "5400000000 microseconds"}},
		{"since in seconds", c.Pick(keel.Now().Since(c.LastSeenAt).Seconds()),
			`date_part('epoch', (now() - c."last_seen_at"))`, nil},
		{"interval times a count", c.Pick(keel.Interval("1 day").Mul(c.Visits)),
			`($3::interval * c."visits")`, nil},
		{"cat", s.Pick(keel.Text("MC ").Cat(s.ProNumber.NullIf(""))), `($3::text || nullif(s."pro_number", $4))`,
			map[int]any{2: "MC ", 3: ""}},
		{"cat chain", s.Pick(s.ProNumber.Cat(keel.ConstText("@"), s.ID)), `((s."pro_number" || '@') || s."id")`, nil},
		{"cat of nothing", s.Pick(s.ProNumber.Cat()), `s."pro_number"`, nil},
		{"collate", s.Pick(s.ID.Collate("C")), `(s."id" COLLATE "C")`, nil},
		{"collate an expression", s.Pick(s.ProNumber.Lower().Collate("en-US-x-icu")),
			`(lower(s."pro_number") COLLATE "en-US-x-icu")`, nil},
		{"string case", s.Pick(keel.CaseString().
			When(s.Weight.Gt(10), keel.Text("heavy")).
			Else(keel.Text("light"))),
			`CASE WHEN s."weight" > $3 THEN $4::text ELSE $5::text END`, map[int]any{3: "heavy", 4: "light"}},
		{"case skips a nil condition", s.Pick(keel.CaseInt64().
			When(keel.When(false, s.Hazmat.IsTrue()), keel.Int64(1)).
			When(s.Hazmat.IsTrue(), keel.Int64(2)).
			End()),
			`CASE WHEN s."hazmat" IS TRUE THEN $3::bigint END`, map[int]any{2: int64(2)}},
		{"case of no when is its else", s.Pick(keel.CaseInt64().Else(keel.Int64(0))), `$3::bigint`, nil},
		{"case nil value", s.Pick(keel.CaseString().When(s.Hazmat.IsTrue(), nil).Else(s.ProNumber)),
			`CASE WHEN s."hazmat" IS TRUE THEN NULL ELSE s."pro_number" END`, nil},
		{"numeric case summed", s.Pick(keel.CaseNumeric().When(s.Hazmat.IsTrue(), s.Total).
			Else(keel.ConstNumeric("0")).Sum()),
			`sum(CASE WHEN s."hazmat" IS TRUE THEN s."total" ELSE '0'::numeric END)`, nil},
		{"time case", c.Pick(keel.CaseTime().When(c.VIP.IsTrue(), keel.Now()).Else(c.LastSeenAt)),
			`CASE WHEN c."vip" IS TRUE THEN now() ELSE c."last_seen_at" END`, nil},
		{"bool case", c.Pick(keel.CaseBool().When(c.VIP.IsTrue(), keel.ConstBool(true)).End()),
			`CASE WHEN c."vip" IS TRUE THEN TRUE END`, nil},
		{"jsonb case", c.Pick(keel.CaseJSONB().When(c.VIP.IsTrue(), c.Prefs).Else(keel.JSONBuildObject())),
			`CASE WHEN c."vip" IS TRUE THEN c."prefs" ELSE jsonb_build_object() END`, nil},
		{"text array literal", s.Pick(keel.TextArray([]string{"a", "b"})), `$3::text[]`, nil},
		{"bigint array unnest", s.Pick(keel.Int64Array([]int64{1, 2}).Unnest()), `unnest($3::bigint[])`, nil},
		{"array constructor", s.Pick(keel.ArrayOfInt64(keel.Int64Of(keel.Count().Filter(s.Hazmat.IsTrue())),
			keel.Int64Of(keel.Count()))),
			`ARRAY[count(*) FILTER (WHERE s."hazmat" IS TRUE), count(*)]`, nil},
		{"empty array constructor", s.Pick(keel.ArrayOfString()), `ARRAY[]::text[]`, nil},
		{"array of text values", s.Pick(keel.ArrayOfString(keel.CastText(s.Total), keel.Text("x"))),
			`ARRAY[CAST(s."total" AS text), $3::text]`, nil},
		{"element", s.Pick(s.Labels.At(1)), `(s."labels")[1]`, nil},
		{"without", s.Pick(s.Labels.Without(keel.Text("x"))), `array_remove(s."labels", $3::text)`, nil},
		{"without then len", s.Pick(s.Labels.Without(keel.Text("x")).Len()),
			`cardinality(array_remove(s."labels", $3::text))`, nil},
		{"array view", s.Pick(keel.Int64ArrayOf(keel.Raw("ARRAY[1, 2]")).At(2)), `(ARRAY[1, 2])[2]`, nil},
		{"array coalesce", s.Pick(keel.StringArrayOf(s.Labels).Coalesce(keel.ArrayOfString())),
			`coalesce(s."labels", ARRAY[]::text[])`, nil},
		{"time array", s.Pick(keel.TimeArray(nil).Len()), `cardinality($3::timestamptz[])`, nil},
		{"null text", s.Pick(keel.NullText()), `NULL::text`, nil},
		{"null bigint", s.Pick(keel.NullInt64()), `NULL::bigint`, nil},
		{"null jsonb", s.Pick(keel.NullJSONB()), `NULL::jsonb`, nil},
		{"nil pointer", s.Pick(keel.Int64Ptr(nil)), `$3::bigint`, map[int]any{2: nil}},
		{"pointer", s.Pick(keel.Int64Ptr(&n)), `$3::bigint`, map[int]any{2: int64(42)}},
		{"text pointer", s.Pick(keel.TextPtr(nil)), `$3::text`, map[int]any{2: nil}},
		{"const text", s.Pick(keel.ConstText("it's")), `'it''s'`, nil},
		{"const bigint", s.Pick(keel.ConstInt64(768)), `768`, nil},
		{"negative const", s.Pick(keel.ConstInt64(-1).Neg()), `(-(-1))`, nil},
		{"const float", s.Pick(keel.ConstFloat64(0.5)), `'0.5'::double precision`, nil},
		{"const numeric", s.Pick(keel.ConstNumeric("-12.50")), `'-12.50'::numeric`, nil},
		{"const in coalesce", s.Pick(s.ProNumber.Coalesce(keel.ConstText(""))), `coalesce(s."pro_number", '')`, nil},
		{"cast text to bigint", s.Pick(keel.CastInt64(s.ProNumber.SplitPart("-", 2))),
			`CAST(split_part(s."pro_number", $3, $4) AS bigint)`, nil},
		{"cast text to integer", s.Pick(keel.CastInt32(s.ProNumber.Right(4))),
			`CAST(right(s."pro_number", $3) AS integer)`, nil},
		{"cast to numeric", s.Pick(keel.CastNumeric(s.Attributes.Text("rate"))),
			`CAST((s."attributes" #>> $3) AS numeric)`, nil},
		{"cast to boolean", s.Pick(keel.CastBool(s.Attributes.Text("temp")).Coalesce(false)),
			`coalesce(CAST((s."attributes" #>> $3) AS boolean), $4)`, nil},
		{"cast to jsonb", s.Pick(keel.CastJSONB(keel.Text("{}"))), `CAST($3::text AS jsonb)`, nil},
		{"cast to timestamptz", s.Pick(keel.CastTime(keel.Text("2026-01-01"))), `CAST($3::text AS timestamptz)`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := renderOf(t, d, tc.q)
			if !selects(sql, tc.want) {
				t.Fatalf("%s\ndoes not select %s", sql, tc.want)
			}
			for i, want := range tc.args {
				if i >= len(args) || args[i] != want {
					t.Fatalf("argument %d of %v, want %v", i+1, args, want)
				}
			}
		})
	}

	sql, args := renderOf(t, d, s.Pick(keel.TextArray(nil)))
	if got, isSlice := args[2].([]string); !isSlice || got == nil || len(got) != 0 || !selects(sql, `$3::text[]`) {
		t.Fatalf("a nil slice binds the empty array: %s %#v", sql, args)
	}
	sql, _ = renderOf(t, d, s.Pick(s.ID).Order(s.ID.Collate("C").Asc()))
	if !strings.HasSuffix(sql, ` ORDER BY (s."id" COLLATE "C") ASC`) {
		t.Fatalf("collated order: %s", sql)
	}
	sql, _ = renderOf(t, d, s.Pick(s.ID).Where(s.Weight.Neg().Lt(-5)))
	if !strings.HasSuffix(sql, ` AND (-s."weight") < $3`) {
		t.Fatalf("negated comparison: %s", sql)
	}
}

func TestTypedOperatorPredicatesRender(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	c := fleetdb.Contacts
	cases := []struct {
		name string
		q    sqlRenderer
		want string
	}{
		{"arithmetic compared", s.Pick(s.ID).Where(s.Weight.Sub(s.CreatedAt).Abs().Lte(100)),
			`abs((s."weight" - s."created_at")) <= $3`},
		{"arithmetic against an expression", s.Pick(s.ID).Where(
			keel.Int64(1000).Sub(s.Weight).GtExpr(s.CreatedAt.Div(keel.Int64(86400)))),
			`($3::bigint - s."weight") > (s."created_at" / $4::bigint)`},
		{"distinct from an expression", s.Pick(s.ID).Where(s.Weight.IsDistinctFromExpr(s.CreatedAt)),
			`s."weight" IS DISTINCT FROM s."created_at"`},
		{"not distinct from an expression", s.Pick(s.ID).Where(s.ProNumber.Lower().IsNotDistinctFromExpr(s.ID)),
			`lower(s."pro_number") IS NOT DISTINCT FROM s."id"`},
		{"not distinct from a value", s.Pick(s.ID).Where(s.Hazmat.IsNotDistinctFrom(true)),
			`s."hazmat" IS NOT DISTINCT FROM $3`},
		{"bool distinct", s.Pick(s.ID).Where(s.Hazmat.IsDistinctFrom(false)), `s."hazmat" IS DISTINCT FROM $3`},
		{"bool distinct from an expression", s.Pick(s.ID).Where(keel.BoolOf(s.Hazmat).IsDistinctFromExpr(keel.NullBool())),
			`s."hazmat" IS DISTINCT FROM NULL::boolean`},
		{"time distinct from an expression", c.Pick(c.ID).Where(c.LastSeenAt.IsDistinctFromExpr(keel.NullTime())),
			`c."last_seen_at" IS DISTINCT FROM NULL::timestamptz`},
		{"nullzero not distinct from zero", c.Pick(c.ID).Where(c.Phone.IsNotDistinctFrom("")), `c."phone" IS NULL`},
		{"jsonb distinct", c.Pick(c.ID).Where(c.Prefs.IsDistinctFromExpr(keel.JSONB(map[string]any{}))),
			`c."prefs" IS DISTINCT FROM $3::jsonb`},
		{"interval compared", c.Pick(c.ID).Where(keel.Now().Since(c.LastSeenAt).GtExpr(keel.Interval("1 hour"))),
			`(now() - c."last_seen_at") > $3::interval`},
		{"in array", s.Pick(s.ID).Where(s.ID.InArray(keel.TextArray([]string{"a"}))), `(s."id" = ANY($3::text[]))`},
		{"not in array", s.Pick(s.ID).Where(s.Weight.NotInArray(keel.Int64Array([]int64{1}))),
			`(s."weight" <> ALL($3::bigint[]))`},
		{"in an array column", s.Pick(s.ID).Where(s.ProNumber.Lower().InArray(s.Labels)),
			`(lower(s."pro_number") = ANY(s."labels"))`},
		{"in a viewed array", s.Pick(s.ID).Where(s.ID.InArray(keel.StringArrayOf(keel.Raw("ARRAY['a']")))),
			`(s."id" = ANY(ARRAY['a']))`},
		{"array contains", s.Pick(s.ID).Where(s.Labels.ContainsAllExpr(keel.ArrayOfString(s.ProNumber))),
			`(s."labels" @> ARRAY[s."pro_number"])`},
		{"array contained by", s.Pick(s.ID).Where(keel.StringArrayOf(s.Labels).ContainedByExpr(keel.TextArray([]string{"x"}))),
			`(s."labels" <@ $3::text[])`},
		{"array overlaps", s.Pick(s.ID).Where(s.Labels.OverlapsExpr(keel.TextArray([]string{"x"}))),
			`(s."labels" && $3::text[])`},
		{"row keyset", s.Pick(s.ID).Where(keel.Tuple(s.CreatedAt, s.ID).Gt(keel.Tuple(int64(5), "x"))),
			`(ROW(s."created_at", s."id") > ROW($3, $4))`},
		{"row keyset descending", s.Pick(s.ID).Where(keel.Tuple(s.Hazmat, s.CreatedAt, s.ID).Lt(
			keel.Tuple(true, keel.Int64(5), nil))),
			`(ROW(s."hazmat", s."created_at", s."id") < ROW($3, $4::bigint, NULL))`},
		{"row lte and gte", s.Pick(s.ID).Where(keel.Tuple(s.CreatedAt, s.ID).Lte(keel.Tuple(int64(1), "a")),
			keel.Tuple(s.CreatedAt, s.ID).Gte(keel.Tuple(int64(0), "a"))),
			`(ROW(s."created_at", s."id") <= ROW($3, $4)) AND (ROW(s."created_at", s."id") >= ROW($5, $6))`},
		{"row eq", s.Pick(s.ID).Where(keel.Tuple(s.CreatedAt, s.ID).Eq(keel.Tuple(int64(1), "a"))),
			`(ROW(s."created_at", s."id") = ROW($3, $4))`},
		{"row distinct", s.Pick(s.ID).Where(keel.Tuple(s.ProNumber, s.Weight).IsDistinctFrom(keel.Tuple(s.ID, s.CreatedAt))),
			`(ROW(s."pro_number", s."weight") IS DISTINCT FROM ROW(s."id", s."created_at"))`},
		{"row in unnest", s.Pick(s.ID).Where(keel.Tuple(s.ProNumber, s.Weight).InUnnest(
			keel.TextArray([]string{"a"}), keel.Int64Array([]int64{1}))),
			`(ROW(s."pro_number", s."weight") IN (SELECT * FROM unnest($3::text[], $4::bigint[])))`},
		{"bool case predicate", c.Pick(c.ID).Where(keel.CaseBool().When(c.VIP.IsTrue(), keel.ConstBool(true)).
			Else(keel.BoolOf(c.Rating.Gt(4)))),
			`CASE WHEN c."vip" IS TRUE THEN TRUE ELSE c."rating" > $3 END`},
		{"like an expression", s.Pick(s.ID).Where(s.ProNumber.LikeExpr(keel.ConstText("%").Cat(s.ID))),
			`(s."pro_number" LIKE ('%' || s."id"))`},
		{"ilike an expression", s.Pick(s.ID).Where(s.ProNumber.Lower().ILikeExpr(keel.ConstText("p%"))),
			`(lower(s."pro_number") ILIKE 'p%')`},
		{"coalesce of several", s.Pick(s.ID).Where(s.ProNumber.Coalesce(s.ID, "none").Eq("x")),
			`coalesce(s."pro_number", s."id", $3) = $4`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, _ := renderOf(t, d, tc.q)
			if !strings.HasSuffix(sql, " AND "+tc.want) {
				t.Fatalf("%s\ndoes not end with AND %s", sql, tc.want)
			}
		})
	}
}

func TestTypedOperatorErrors(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	for name, q := range map[string]sqlRenderer{
		"arithmetic with nil":         s.Pick(s.Weight.Add(nil)),
		"cat with nil":                s.Pick(s.ProNumber.Cat(nil)),
		"empty collation":             s.Pick(s.ProNumber.Collate("")),
		"quoted collation":            s.Pick(s.ProNumber.Collate(`C" ; DROP`)),
		"long collation":              s.Pick(s.ProNumber.Collate(strings.Repeat("c", 64))),
		"row of one value":            s.Pick(s.ID).Where(keel.Tuple(s.ID).Eq(keel.Tuple(s.ID, s.ID))),
		"in unnest of nothing":        s.Pick(s.ID).Where(keel.Tuple(s.ID, s.ProNumber).InUnnest()),
		"const text with NUL":         s.Pick(keel.ConstText("a\x00b")),
		"const NaN":                   s.Pick(keel.ConstFloat64(math.NaN())),
		"const numeric not a decimal": s.Pick(keel.ConstNumeric("1e5")),
		"const numeric injection":     s.Pick(keel.ConstNumeric("1); DROP TABLE x; --")),
		"in array of nil":             s.Pick(s.ID).Where(s.ID.InArray(nil)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := q.ToSQL(tenantCtx("org_1", "bu_1"), d); err == nil {
				t.Fatal("rendered")
			}
		})
	}
}

func TestTypedOperatorsRun(t *testing.T) {
	d := db(t)
	w := seed(t, 4)
	s := fleetdb.Shipments
	type row struct {
		Pro     string
		Doubled int64
		Days    int64
		Rem     int64
		Neg     int64
		Total   string
		Label   string
		Tagged  *string
		First   string
		Nothing *string
		Seconds float64
	}
	weight := s.Weight.Coalesce(0)
	rows := must(keel.Into[row](w.ctx, d, s.Pick(
		s.ProNumber,
		weight.Mul(keel.Int64(2)),
		keel.Int64(3*86400).Div(keel.Int64(86400)),
		weight.Mod(keel.Int64(7)),
		weight.Neg(),
		keel.CastText(s.Total.Add(keel.Numeric("0.5"))),
		keel.CaseString().
			When(s.Weight.IsNull(), keel.Text("none")).
			When(s.Weight.Gt(1015), keel.Text("heavy")).
			Else(keel.Text("light")),
		keel.Text("W").Cat(keel.CastText(s.Weight)),
		s.Labels.At(1),
		keel.NullText(),
		keel.Duration(90*time.Minute).Seconds(),
	).Order(s.ProNumber.Asc())))(t)
	eq(t, len(rows), 4)
	r := rows[1]
	eq(t, r.Pro, "PRO-0001")
	eq(t, r.Doubled, int64(2020))
	eq(t, r.Days, int64(3))
	eq(t, r.Rem, int64(2))
	eq(t, r.Neg, int64(-1010))
	eq(t, r.Total, "101.5000")
	eq(t, r.Label, "light")
	if r.Tagged == nil || *r.Tagged != "W1010" {
		t.Fatalf("cat: %v", r.Tagged)
	}
	if rows[0].Tagged != nil {
		t.Fatalf("|| of NULL is NULL: %q", *rows[0].Tagged)
	}
	eq(t, r.First, "lane-1")
	if r.Nothing != nil {
		t.Fatalf("typed NULL: %q", *r.Nothing)
	}
	eq(t, r.Seconds, 5400.0)
	eq(t, rows[0].Label, "none")
	eq(t, rows[2].Label, "heavy")

	count := func(p keel.Pred) int64 { return must(s.Where(p).Count(w.ctx, d))(t) }
	eq(t, count(keel.Tuple(s.Weight, s.ProNumber).Gt(keel.Tuple(int64(1010), "PRO-0001"))), int64(2))
	eq(t, count(keel.Tuple(s.Weight, s.ProNumber).Lt(keel.Tuple(int64(1030), "PRO-0003"))), int64(2))
	eq(t, count(keel.Tuple(s.Weight, s.ProNumber).Gte(keel.Tuple(keel.Int64(1020), keel.Text("PRO-0002")))), int64(2))
	eq(t, count(keel.Tuple(s.ProNumber, s.Weight).InUnnest(
		keel.TextArray([]string{"PRO-0001", "PRO-0002"}), keel.Int64Array([]int64{1010, 1}))), int64(1))
	eq(t, count(s.ProNumber.InArray(keel.TextArray([]string{"PRO-0001", "PRO-0003"}))), int64(2))
	eq(t, count(s.ProNumber.NotInArray(keel.TextArray([]string{"PRO-0001", "PRO-0003"}))), int64(2))
	eq(t, count(keel.Text("lane-0").InArray(s.Labels)), int64(2))
	eq(t, count(s.Labels.ContainsAllExpr(keel.ArrayOfString(keel.Text("lane-1")))), int64(2))
	eq(t, count(s.Weight.IsNotDistinctFromExpr(keel.Int64Ptr(nil))), int64(1))
	heavy := int64(1020)
	eq(t, count(s.Weight.IsNotDistinctFromExpr(keel.Int64Ptr(&heavy))), int64(1))
	eq(t, count(s.Weight.IsDistinctFromExpr(keel.NullInt64())), int64(3))
	eq(t, count(weight.GtExpr(keel.ConstInt64(1015))), int64(2))
	eq(t, count(s.Total.Sub(keel.Numeric("101")).Abs().Lte(1)), int64(3))
	eq(t, count(keel.Now().Add(keel.Interval("1 day")).GtExpr(keel.Now())), int64(4))
	eq(t, count(keel.ToTimestamp(s.CreatedAt).Add(keel.Duration(time.Hour)).GtExpr(keel.Now())), int64(4))
	eq(t, count(keel.CaseBool().When(s.Hazmat.IsTrue(), keel.ConstBool(true)).Else(keel.ConstBool(false))), int64(1))

	pros := must(keel.Into[string](w.ctx, d, s.Pick(s.ProNumber).Order(s.ProNumber.Collate("C").Desc())))(t)
	eq(t, strings.Join(pros, ","), "PRO-0003,PRO-0002,PRO-0001,PRO-0000")

	counts := must(keel.IntoOne[[]int64](w.ctx, d, s.Pick(keel.ArrayOfInt64(
		keel.Int64Of(keel.Count().Filter(s.Hazmat.IsTrue())), keel.Int64Of(keel.Count())))))(t)
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 4 {
		t.Fatalf("filtered counts in one array: %v", counts)
	}
	left := must(keel.IntoOne[int32](w.ctx, d, s.Pick(s.Labels.Without(keel.Text("lane-1")).Len()).
		Where(s.ProNumber.Eq("PRO-0001"))))(t)
	eq(t, left, int32(0))

	_, err := s.Where(s.ProNumber.Eq("PRO-0001")).Update(w.ctx, d, s.Weight.ToExpr(s.Weight.Add(keel.Int64(5))))
	ok(t, err)
	_, err = s.Where(s.ProNumber.Eq("PRO-0002")).Update(w.ctx, d, s.Weight.Increment(5))
	ok(t, err)
	got := must(keel.Into[int64](w.ctx, d, s.Pick(s.Weight).
		Where(s.ProNumber.In("PRO-0001", "PRO-0002")).Order(s.ProNumber.Asc())))(t)
	if len(got) != 2 || got[0] != 1015 || got[1] != 1025 {
		t.Fatalf("arithmetic assigned and incremented: %v", got)
	}
}

func TestTypedFormsRenderAsDynamic(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	pairs := []struct {
		name       string
		typed, dyn keel.Expr
	}{
		{"arithmetic", s.Weight.Sub(s.CreatedAt).Div(keel.Int64(86400)),
			keel.DynOf(s.Weight).Sub(s.CreatedAt).Div(86400)},
		{"literal minus a column", keel.Int64(7).Sub(s.CreatedAt), keel.DynLit(int64(7)).Cast(keel.TypeInt64).Sub(s.CreatedAt)},
		{"negation", s.Weight.Neg(), keel.DynOf(s.Weight).Neg()},
		{"concatenation", s.ProNumber.Cat(s.ID), keel.DynOf(s.ProNumber).Concat(s.ID)},
		{"collation", s.ID.Collate("C"), keel.DynOf(s.ID).Collate("C")},
		{"element", s.Labels.At(2), keel.DynOf(s.Labels).At(2)},
		{"case", keel.CaseString().When(s.Hazmat.IsTrue(), keel.Text("h")).Else(keel.Text("n")),
			keel.DynCase().When(s.Hazmat.IsTrue(), "h").Else("n")},
		{"row", keel.Tuple(s.CreatedAt, s.ID).Gt(keel.Tuple(int64(1), "a")),
			keel.DynRow(s.CreatedAt, s.ID).Gt(keel.DynRow(int64(1), "a"))},
		{"array membership", s.ID.InArray(s.Labels), keel.DynOf(s.ID).In(s.Labels)},
		{"array constructor", keel.ArrayOfString(s.ProNumber, s.ID), keel.DynArray(s.ProNumber, s.ID)},
		{"constant", keel.ConstInt64(768), keel.DynConst(768)},
		{"text constant", keel.ConstText("o'k"), keel.DynConst("o'k")},
		{"cast", keel.CastInt64(s.ProNumber), keel.DynOf(s.ProNumber).Cast(keel.TypeInt64)},
		{"cast to jsonb", keel.CastJSONB(s.ProNumber), keel.DynOf(s.ProNumber).Cast(keel.TypeJSON)},
		{"row in unnest", keel.Tuple(s.ProNumber, s.Weight).InUnnest(keel.TextArray([]string{"a"}),
			keel.Int64Array([]int64{1})),
			keel.DynRow(s.ProNumber, s.Weight).InUnnest([]string{"a"}, []int64{1})},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			typed, _ := renderOf(t, d, s.Pick(p.typed))
			dyn, _ := renderOf(t, d, s.Pick(p.dyn))
			if typed != dyn {
				t.Fatalf("typed %s\ndynamic %s", typed, dyn)
			}
		})
	}
}

func TestTypedOutputsGroupAndOrderByPosition(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	prefix := func() keel.StringExpr { return s.ProNumber.Left(3).Coalesce("none") }
	sql, args := renderOf(t, d, s.Pick(prefix(), keel.Count()).Group(prefix()).Order(prefix().Asc()))
	if !strings.Contains(sql, " GROUP BY 1") || !strings.Contains(sql, " ORDER BY 1") {
		t.Fatalf("typed outputs not named by position: %s", sql)
	}
	if strings.Count(sql, "coalesce(") != 1 {
		t.Fatalf("the grouped expression repeats: %s", sql)
	}
	eq(t, len(args), 3)

	other, _ := renderOf(t, d, s.Pick(s.ProNumber.Left(3).Coalesce("none"), keel.Count()).
		Group(s.ProNumber.Left(3).Coalesce("other")))
	if strings.Contains(other, " GROUP BY 1") {
		t.Fatalf("a term with other values was named by position: %s", other)
	}
}
