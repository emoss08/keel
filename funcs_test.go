package keel_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// sqlRenderer is a statement that renders without running.
type sqlRenderer interface {
	ToSQL(context.Context, *keel.DB) (string, []any, error)
}

// renderOf renders q for tenant org_1/bu_1, whose two tenant values bind $1 and $2.
func renderOf(t *testing.T, d *keel.DB, q sqlRenderer) (string, []any) {
	t.Helper()
	sql, args, err := q.ToSQL(tenantCtx("org_1", "bu_1"), d)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return sql, args
}

// selects reports whether sql's select list starts with want as a whole expression.
func selects(sql, want string) bool {
	return strings.Contains(sql, "SELECT "+want+", ") || strings.Contains(sql, "SELECT "+want+" FROM")
}

func TestTypedFunctionsRender(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	c := fleetdb.Contacts
	slugify := keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("public.slugify")
	nextNumber := keel.DeclareFunc0[keel.Int64Expr]("next_number")
	pad := keel.DeclareFunc3[keel.StringExpr, keel.StringValue, keel.Int32Value, keel.StringValue]("lpad")
	cases := []struct {
		name string
		q    sqlRenderer
		want string
		args map[int]any
	}{
		{"lower", s.Pick(s.ProNumber.Lower()), `lower(s."pro_number")`, nil},
		{"upper then trim", s.Pick(s.ProNumber.Upper().Trim()), `btrim(upper(s."pro_number"))`, nil},
		{"trim chars", s.Pick(s.ProNumber.TrimChars("-")), `btrim(s."pro_number", $3)`, map[int]any{2: "-"}},
		{"ltrim", s.Pick(s.ProNumber.LTrim()), `ltrim(s."pro_number")`, nil},
		{"ltrim chars", s.Pick(s.ProNumber.LTrimChars("0")), `ltrim(s."pro_number", $3)`, map[int]any{2: "0"}},
		{"rtrim", s.Pick(s.ProNumber.RTrim()), `rtrim(s."pro_number")`, nil},
		{"rtrim chars", s.Pick(s.ProNumber.RTrimChars(" .")), `rtrim(s."pro_number", $3)`, nil},
		{"length", s.Pick(s.ProNumber.Length()), `length(s."pro_number")`, nil},
		{"left", s.Pick(s.ProNumber.Left(3)), `left(s."pro_number", $3)`, map[int]any{2: int64(3)}},
		{"right", s.Pick(s.ProNumber.Right(2)), `right(s."pro_number", $3)`, map[int]any{2: int64(2)}},
		{"substr", s.Pick(s.ProNumber.Substr(1, 26)), `substr(s."pro_number", $3, $4)`,
			map[int]any{2: int64(1), 3: int64(26)}},
		{"substr from", s.Pick(s.ProNumber.SubstrFrom(5)), `substr(s."pro_number", $3)`, nil},
		{"replace", s.Pick(s.ProNumber.Replace("-", "")), `replace(s."pro_number", $3, $4)`,
			map[int]any{2: "-", 3: ""}},
		{"split part", s.Pick(s.ProNumber.SplitPart("-", 2)), `split_part(s."pro_number", $3, $4)`,
			map[int]any{2: "-", 3: int64(2)}},
		{"md5", s.Pick(s.ProNumber.MD5()), `md5(s."pro_number")`, nil},
		{"regexp replace with flags", s.Pick(s.ProNumber.RegexpReplace("[^0-9]", "", "g")),
			`regexp_replace(s."pro_number", $3, $4, $5)`, map[int]any{4: "g"}},
		{"regexp replace without flags", s.Pick(s.ProNumber.RegexpReplace("^PRO-", "", "")),
			`regexp_replace(s."pro_number", $3, $4)`, nil},
		{"an enum column is cast to text", s.Pick(s.Status.Lower()), `lower(CAST(s."status" AS text))`, nil},
		{"string coalesce", s.Pick(s.ProNumber.Coalesce("")), `coalesce(s."pro_number", $3)`, nil},
		{"string nullif", s.Pick(s.ProNumber.NullIf("")), `nullif(s."pro_number", $3)`, nil},
		{"string greatest", s.Pick(s.ProNumber.Greatest(s.ID)), `greatest(s."pro_number", s."id")`, nil},
		{"string least of a literal", s.Pick(s.ProNumber.Least(keel.Text("m"))),
			`least(s."pro_number", $3::text)`, nil},
		{"concat types its literals", s.Pick(keel.Concat(s.ProNumber, "-", 7)),
			`concat(s."pro_number", $3::text, $4::bigint)`, nil},
		{"concat_ws", s.Pick(keel.ConcatWS(" ", s.ProNumber, s.Status)),
			`concat_ws($3::text, s."pro_number", s."status")`, map[int]any{2: " "}},
		{"random uuid", s.Pick(keel.GenRandomUUID()), `CAST(gen_random_uuid() AS text)`, nil},
		{"cast text", s.Pick(keel.CastText(s.Status)), `CAST(s."status" AS text)`, nil},
		{"abs", s.Pick(s.Weight.Abs()), `abs(s."weight")`, nil},
		{"int coalesce", s.Pick(s.Weight.Coalesce(0)), `coalesce(s."weight", $3)`, map[int]any{2: 0}},
		{"int nullif", s.Pick(s.Weight.NullIf(0)), `nullif(s."weight", $3)`, map[int]any{2: int64(0)}},
		{"int greatest", s.Pick(s.CreatedAt.Greatest(s.UpdatedAt)), `greatest(s."created_at", s."updated_at")`, nil},
		{"int least of a literal", s.Pick(s.CreatedAt.Least(keel.Int64(5))), `least(s."created_at", $3::bigint)`,
			map[int]any{2: int64(5)}},
		{"round to places", s.Pick(keel.CastNumeric(s.Weight).RoundTo(2)), `round(CAST(s."weight" AS numeric), $3)`,
			map[int]any{2: int64(2)}},
		{"floor", s.Pick(keel.CastFloat64(s.Weight).Floor()), `floor(CAST(s."weight" AS double precision))`, nil},
		{"ceil then ln", s.Pick(keel.CastFloat64(s.Weight).Ceil().Ln()),
			`ln(ceil(CAST(s."weight" AS double precision)))`, nil},
		{"round cast back", s.Pick(keel.CastInt64(keel.CastFloat64(s.Weight).Round())),
			`CAST(round(CAST(s."weight" AS double precision)) AS bigint)`, nil},
		{"numeric view", s.Pick(keel.NumericOf(s.Total).Abs()), `abs(s."total")`, nil},
		{"float column round", c.Pick(c.Rating.Round()), `round(c."rating")`, nil},
		{"int32 column abs", c.Pick(c.Visits.Abs()), `abs(c."visits")`, nil},
		{"time greatest", c.Pick(c.LastSeenAt.Greatest(keel.Now())), `greatest(c."last_seen_at", now())`, nil},
		{"time to_char", c.Pick(c.LastSeenAt.ToChar("YYYY")), `to_char(c."last_seen_at", $3)`,
			map[int]any{2: "YYYY"}},
		{"now", s.Pick(keel.Now()), `now()`, nil},
		{"now epoch", s.Pick(keel.NowEpoch()), `CAST(extract(epoch FROM now()) AS bigint)`, nil},
		{"to_timestamp", s.Pick(keel.ToTimestamp(s.CreatedAt)), `to_timestamp(s."created_at")`, nil},
		{"date_part writes its field", s.Pick(keel.DatePart("HOUR", keel.ToTimestamp(s.CreatedAt))),
			`date_part('hour', to_timestamp(s."created_at"))`, nil},
		{"date_trunc in a zone",
			s.Pick(keel.DateTrunc("month", keel.ToTimestamp(s.CreatedAt).AtTimeZone(keel.Text("UTC")))),
			`date_trunc('month', timezone($3::text, to_timestamp(s."created_at")))`, map[int]any{2: "UTC"}},
		{"epoch to_char", s.Pick(keel.ToTimestamp(s.CreatedAt).ToChar("YYYY-MM-DD")),
			`to_char(to_timestamp(s."created_at"), $3)`, nil},
		{"jsonb_build_object", s.Pick(keel.JSONBuildObject(keel.Pair("pro", s.ProNumber), keel.Pair("n", 1))),
			`jsonb_build_object($3::text, s."pro_number", $4::text, $5::bigint)`,
			map[int]any{2: "pro", 3: "n", 4: 1}},
		{"empty jsonb_build_array", s.Pick(keel.JSONBuildArray()), `jsonb_build_array()`, nil},
		{"jsonb_build_array", s.Pick(keel.JSONBuildArray(s.ProNumber, true)),
			`jsonb_build_array(s."pro_number", $3::boolean)`, nil},
		{"jsonb_typeof", s.Pick(s.Attributes.TypeOf()), `jsonb_typeof(s."attributes")`, nil},
		{"jsonb_set", s.Pick(s.Attributes.Set([]string{"temp"}, keel.JSONB(true))),
			`jsonb_set(s."attributes", $3, $4::jsonb)`, map[int]any{3: "true"}},
		{"jsonb concat", s.Pick(s.Attributes.Concat(keel.JSONBuildObject())),
			`(s."attributes" || jsonb_build_object())`, nil},
		{"jsonb coalesce encodes its value", s.Pick(s.Attributes.Coalesce(map[string]any{})),
			`coalesce(s."attributes", $3::jsonb)`, map[int]any{2: "{}"}},
		{"array elements", s.Pick(keel.JSONBOf(s.Attributes).ArrayElementsText()),
			`jsonb_array_elements_text(s."attributes")`, nil},
		{"cardinality", s.Pick(s.Labels.Len()), `cardinality(s."labels")`, nil},
		{"unnest", s.Pick(s.Labels.Unnest()), `unnest(s."labels")`, nil},
		{"array_position", s.Pick(keel.ArrayPosition([]string{"a", "b"}, s.ProNumber)),
			`array_position($3::text[], s."pro_number")`, nil},
		{"width_bucket over bounds", s.Pick(keel.WidthBucket(s.CreatedAt, []int64{1, 2})),
			`width_bucket(s."created_at", $3::bigint[])`, nil},
		{"width_bucket over a range", s.Pick(keel.WidthBucketRange(keel.CastFloat64(s.Weight), 0, 10, 5)),
			`width_bucket(CAST(s."weight" AS double precision), $3::double precision, $4::double precision, ` +
				`$5::integer)`, map[int]any{2: 0.0, 3: 10.0, 4: int64(5)}},
		{"generate_series shares equal literals", s.Pick(keel.GenerateSeries(keel.Int64(1), keel.Int64(1))),
			`generate_series($3::bigint, $3::bigint)`, nil},
		{"generate_series by", s.Pick(keel.GenerateSeriesBy(keel.Int64(0), keel.Int64(10), keel.Int64(5))),
			`generate_series($3::bigint, $4::bigint, $5::bigint)`, nil},
		{"time series", s.Pick(keel.GenerateTimeSeries(keel.Now(), keel.Now(), "1 day")),
			`generate_series(now(), now(), $3::interval)`, map[int]any{2: "1 day"}},
		{"bool_or", s.Pick(keel.BoolOr(s.Hazmat.IsTrue())), `bool_or(s."hazmat" IS TRUE)`, nil},
		{"bool_and", s.Pick(keel.BoolAnd(s.Weight.Gt(5))), `bool_and(s."weight" > $3)`, nil},
		{"jsonb_agg", s.Pick(keel.JSONBAgg(s.ProNumber)), `jsonb_agg(s."pro_number")`, nil},
		{"bool coalesce", s.Pick(s.Hazmat.Coalesce(false)), `coalesce(s."hazmat", $3)`, nil},
		{"declared function", s.Pick(slugify.Call(s.ProNumber)), `"public"."slugify"(s."pro_number")`, nil},
		{"declared function of no arguments", s.Pick(nextNumber.Call()), `"next_number"()`, nil},
		{"declared function of three arguments", s.Pick(pad.Call(s.ProNumber, keel.Int32(10), keel.Text("0"))),
			`"lpad"(s."pro_number", $3::integer, $4::text)`, nil},
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
}

func TestTypedFunctionPredicatesRender(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	cases := []struct {
		name string
		p    keel.Pred
		want string
	}{
		{"value comparison", s.ProNumber.Lower().Eq("pro-1"), `lower(s."pro_number") = $3`},
		{"expression comparison", s.ProNumber.Lower().EqExpr(keel.Text("PRO-1").Lower()),
			`lower(s."pro_number") = lower($3::text)`},
		{"length", s.ProNumber.Length().Gte(4), `length(s."pro_number") >= $3`},
		{"abs", s.Weight.Abs().Gt(5), `abs(s."weight") > $3`},
		{"epoch now", keel.Int64Of(s.CreatedAt).GtExpr(keel.NowEpoch()),
			`s."created_at" > CAST(extract(epoch FROM now()) AS bigint)`},
		{"greatest", s.CreatedAt.Greatest(s.UpdatedAt).Lt(10), `greatest(s."created_at", s."updated_at") < $3`},
		{"range", keel.Int8Range(keel.Int64(1), keel.Int64(10)).Covers(s.CreatedAt),
			`int8range($3::bigint, $4::bigint) @> s."created_at"`},
		{"jsonb_typeof", keel.JSONBOf(s.Attributes).TypeOf().Eq("object"), `jsonb_typeof(s."attributes") = $3`},
		{"in", s.ProNumber.Trim().In("a", "b"), `btrim(s."pro_number") = ANY($3)`},
		{"a boolean expression is a predicate", keel.BoolOf(keel.Raw("TRUE")), `TRUE`},
		{"between", s.ProNumber.Length().Between(2, 8), `length(s."pro_number") BETWEEN $3 AND $4`},
		{"null test", s.ProNumber.NullIf("").IsNull(), `nullif(s."pro_number", $3) IS NULL`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, _ := renderOf(t, d, s.Pick(s.ID).Where(tc.p))
			if !strings.HasSuffix(sql, " AND "+tc.want) {
				t.Fatalf("%s\ndoes not end with AND %s", sql, tc.want)
			}
		})
	}

	ids := []string{"b", "a"}
	sql, _ := renderOf(t, d, s.Pick(s.ID).Order(keel.ArrayPosition(ids, s.ID).Asc()))
	if !strings.HasSuffix(sql, ` ORDER BY array_position($3::text[], s."id") ASC`) {
		t.Fatalf("order by position: %s", sql)
	}

	left := s.ProNumber.Left(3)
	sql, args := renderOf(t, d, s.Pick(left, keel.Count()).Group(left))
	if strings.Count(sql, "left(s.\"pro_number\", $3)") != 2 || len(args) != 3 {
		t.Fatalf("an expression written in Pick and Group binds its value once: %s %v", sql, args)
	}
}

func TestTypedFunctionErrors(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	c := fleetdb.Contacts
	for name, q := range map[string]sqlRenderer{
		"unknown date_part field":        s.Pick(keel.DatePart("fortnight", keel.Now())),
		"unknown date_trunc field":       s.Pick(keel.DateTrunc("dow", keel.Now())),
		"typed view of nil":              s.Pick(keel.StringOf(nil)),
		"cast of nil":                    s.Pick(keel.CastInt64(nil)),
		"computed field of another kind": c.Compute(c.Loads, c.Name.Upper()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := q.ToSQL(tenantCtx("org_1", "bu_1"), d); err == nil {
				t.Fatal("rendered")
			}
		})
	}
	if _, _, err := c.Compute(c.CustomerName, c.Name.Upper()).ToSQL(tenantCtx("org_1", "bu_1"), d); err != nil {
		t.Fatalf("a text expression fills a text field: %v", err)
	}
	if _, _, err := c.Compute(c.Loads, c.Visits.Abs()).ToSQL(tenantCtx("org_1", "bu_1"), d); err != nil {
		t.Fatalf("an integer expression fills a bigint field: %v", err)
	}
	for _, name := range []string{"", "a.b.c", ".fn", "fn.", `a"b`, "a\x00b"} {
		t.Run("declare "+strconv.Quote(name), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("DeclareFunc0(%q) did not panic", name)
				}
			}()
			keel.DeclareFunc0[keel.Int64Expr](name)
		})
	}
}

func TestTypedFunctionsRun(t *testing.T) {
	d := db(t)
	w := seed(t, 4)
	s := fleetdb.Shipments
	type row struct {
		CreatedAt int64
		Lower     string
		Upper     string
		Left      string
		Part      string
		Len       int32
		Replaced  string
		Joined    string
		Status    string
		Weight    int64
		Abs       int64
		Rounded   float64
		Year      float64
		YearText  string
		Kind      string
		Built     map[string]any
		Labels    int32
		Digest    string
	}
	rows := must(keel.Into[row](w.ctx, d, s.Pick(
		s.CreatedAt,
		s.ProNumber.Lower(),
		s.ProNumber.Upper(),
		s.ProNumber.Left(3),
		s.ProNumber.SplitPart("-", 2),
		s.ProNumber.Length(),
		s.ProNumber.Replace("PRO-", ""),
		keel.ConcatWS("/", s.ProNumber, s.Status),
		s.Status.Lower(),
		s.Weight.Coalesce(0),
		s.Weight.Coalesce(0).Abs(),
		keel.CastFloat64(s.Weight.Coalesce(0)).Round(),
		keel.DatePart("year", keel.ToTimestamp(s.CreatedAt).AtTimeZone(keel.Text("UTC"))),
		keel.ToTimestamp(s.CreatedAt).AtTimeZone(keel.Text("UTC")).ToChar("YYYY"),
		s.Attributes.TypeOf(),
		keel.JSONBuildObject(keel.Pair("pro", s.ProNumber), keel.Pair("n", 1)),
		s.Labels.Len(),
		s.ProNumber.MD5(),
	).Order(s.ProNumber.Asc())))(t)
	eq(t, len(rows), 4)
	r := rows[1]
	eq(t, r.Lower, "pro-0001")
	eq(t, r.Upper, "PRO-0001")
	eq(t, r.Left, "PRO")
	eq(t, r.Part, "0001")
	eq(t, r.Len, int32(8))
	eq(t, r.Replaced, "0001")
	eq(t, r.Joined, "PRO-0001/Assigned")
	eq(t, r.Status, "assigned")
	eq(t, r.Weight, int64(1010))
	eq(t, r.Abs, int64(1010))
	eq(t, r.Rounded, 1010.0)
	year := time.Unix(r.CreatedAt, 0).UTC().Year()
	eq(t, r.Year, float64(year))
	eq(t, r.YearText, strconv.Itoa(year))
	eq(t, r.Kind, "object")
	eq(t, r.Built["pro"], any("PRO-0001"))
	eq(t, fmt.Sprint(r.Built["n"]), "1")
	eq(t, r.Labels, int32(1))
	eq(t, len(r.Digest), 32)
	eq(t, rows[0].Weight, int64(0))

	count := func(p keel.Pred) int64 { return must(s.Where(p).Count(w.ctx, d))(t) }
	eq(t, count(s.ProNumber.Lower().Eq("pro-0002")), int64(1))
	eq(t, count(s.ProNumber.Lower().EqExpr(keel.Text("PRO-0002").Lower())), int64(1))
	eq(t, count(s.Weight.Coalesce(0).Gt(0)), int64(3))
	eq(t, count(keel.Int64Of(s.CreatedAt).LteExpr(keel.NowEpoch())), int64(4))
	eq(t, count(keel.Int8Range(keel.Int64(1005), keel.Int64(1025)).Covers(s.Weight)), int64(2))
	eq(t, count(keel.ConcatWS(" ", s.ProNumber, s.Status).HasSuffix(" New")), int64(1))
	eq(t, count(s.ProNumber.LTrimChars("PRO-0").Eq("3")), int64(1))
	eq(t, count(keel.Now().GtExpr(keel.ToTimestamp(s.CreatedAt).AtTimeZone(keel.Text("UTC")).
		AtTimeZone(keel.Text("UTC")))), int64(4))

	type flags struct {
		Any bool
		All bool
	}
	got := must(keel.IntoOne[flags](w.ctx, d, s.Pick(keel.BoolOr(s.Hazmat.IsTrue()),
		keel.BoolAnd(s.Weight.IsNotNull()))))(t)
	eq(t, got.Any, true)
	eq(t, got.All, false)

	order := []string{"PRO-0002", "PRO-0000", "PRO-0003", "PRO-0001"}
	pros := must(keel.Into[string](w.ctx, d, s.Pick(s.ProNumber).
		Order(keel.ArrayPosition(order, s.ProNumber).Asc())))(t)
	eq(t, strings.Join(pros, ","), strings.Join(order, ","))

	n := keel.Rows("n", []string{"i"}, keel.GenerateSeries(keel.Int64(1), keel.Int64(3)))
	eq(t, must(keel.IntoOne[int64](w.ctx, d, s.JoinLateral(n).Pick(keel.Count())))(t), int64(12))
	lbl := keel.Rows("lbl", []string{"label"}, s.Labels.Unnest())
	labels := must(keel.Into[string](w.ctx, d, s.JoinLateral(lbl).Pick(lbl.Col("label")).
		Where(s.ProNumber.Eq("PRO-0001"))))(t)
	eq(t, strings.Join(labels, ","), "lane-1")

	first := w.shipments[0]
	_, err := s.Where(s.ID.Eq(first.ID)).Update(w.ctx, d,
		s.Labels.Append("z"),
		s.Attributes.ToExpr(s.Attributes.Set([]string{"temp"}, keel.JSONB(false))))
	ok(t, err)
	after := must(s.Where(s.ID.Eq(first.ID)).One(w.ctx, d))(t)
	eq(t, strings.Join(after.Labels, ","), "lane-0,z")
	eq(t, after.Attributes["temp"], any(false))

	lower := keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("pg_catalog.lower")
	low := must(keel.IntoOne[string](w.ctx, d, s.Pick(lower.Call(s.ProNumber)).Where(s.ProNumber.Eq("PRO-0003"))))(t)
	eq(t, low, "pro-0003")
}

func TestVerifyFuncs(t *testing.T) {
	d := db(t)
	ctx := context.Background()
	good := []keel.DeclaredFunc{
		keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("pg_catalog.lower"),
		keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("lower"),
		keel.DeclareFunc2[keel.StringExpr, keel.StringValue, keel.Int32Value]("left"),
		keel.DeclareFunc3[keel.StringExpr, keel.StringValue, keel.StringValue, keel.StringValue]("concat_ws"),
		keel.DeclareFunc3[keel.JSONBExpr, keel.JSONBValue, keel.Expr, keel.JSONBValue]("jsonb_set"),
		keel.DeclareFunc0[keel.TimeExpr]("now"),
		keel.DeclareFunc1[keel.Float64Expr, keel.Float64Value]("ln"),
		nil,
	}
	drifts := must(keel.VerifyFuncs(ctx, d, good...))(t)
	if len(drifts) != 0 {
		t.Fatalf("drifts for functions that exist: %v", drifts)
	}

	missing := keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("keel_no_such_function")
	qualified := keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("pg_catalog.keel_no_such_function")
	arity := keel.DeclareFunc2[keel.StringExpr, keel.StringValue, keel.StringValue]("lower")
	result := keel.DeclareFunc1[keel.Int64Expr, keel.StringValue]("md5")
	drifts = must(keel.VerifyFuncs(ctx, d, missing, qualified, arity, result, missing))(t)
	if len(drifts) != 4 {
		t.Fatalf("drifts: %v", drifts)
	}
	want := []struct {
		kind  keel.DriftKind
		table string
	}{
		{keel.DriftMissingFunction, "keel_no_such_function"},
		{keel.DriftMissingFunction, "pg_catalog.keel_no_such_function"},
		{keel.DriftFunctionArity, "lower"},
		{keel.DriftFunctionResult, "md5"},
	}
	for i, w := range want {
		if drifts[i].Kind != w.kind || drifts[i].Table != w.table {
			t.Fatalf("drift %d: %v, want %s of %s", i, drifts[i], w.kind, w.table)
		}
	}
	if !strings.Contains(drifts[2].Detail, "takes 2 arguments") || !strings.Contains(drifts[3].Detail, "text") {
		t.Fatalf("details: %v", drifts)
	}
	none := must(keel.VerifyFuncs(ctx, d))(t)
	eq(t, len(none), 0)
}
