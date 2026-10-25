package keel_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/schema"
)

func registry(t testing.TB) *keel.Registry {
	t.Helper()
	return must(keel.NewRegistry(fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema(),
		fleetdb.MovesSchema(), fleetdb.StopsSchema(), fleetdb.TagsSchema(), fleetdb.LocationsSchema(),
		fleetdb.WorkersSchema(), fleetdb.PlantsSchema()))(t)
}

func strs(c *keel.DynColumn) []string { return slices.Clone(c.String) }

func TestDynamicRegistry(t *testing.T) {
	_, err := keel.NewRegistry(fleetdb.ShipmentsSchema(), fleetdb.ShipmentsSchema())
	if err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("duplicate table: %v", err)
	}
	_, err = keel.NewRegistry(nil)
	if err == nil {
		t.Fatal("nil table accepted")
	}
	reg := registry(t)
	tbl, found := reg.Table("customers")
	eq(t, found, true)
	eq(t, tbl, fleetdb.CustomersSchema())
	eq(t, len(reg.Tables()), 8)
	q := reg.Query("nope")
	if q.Err() == nil || !strings.Contains(q.Err().Error(), `unknown table "nope"`) {
		t.Fatalf("unknown table: %v", q.Err())
	}
	_, err = q.Run(newTenant(), db(t))
	if err == nil {
		t.Fatal("a query on an unknown table ran")
	}
	if keel.DynFrom(nil).Err() == nil {
		t.Fatal("DynFrom(nil) accepted")
	}
}

func TestDynamicSelectPathsAndFilters(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	reg := registry(t)
	q := reg.Query("shipments")
	pro := q.Path("pro_number")
	q.Select(pro, q.Path("customer", "code"), q.Path("Total"), q.Path("weight"), q.Path("hazmat"),
		q.Path("status"), q.Path("bill_to", "code").As("bill to")).
		Where(q.Path("status").In([]string{"New", "Assigned"}), pro.HasPrefix("PRO-00"),
			q.Path("priority").Between(0, 2)).
		Order(pro.Asc()).Limit(5)
	res := must(q.Run(w.ctx, d))(t)
	eq(t, res.Rows, 5)
	names := make([]string, len(res.Columns))
	types := make([]keel.ValueType, len(res.Columns))
	for i, c := range res.Columns {
		names[i], types[i] = c.Name, c.Type
	}
	eq(t, strings.Join(names, ","), "pro_number,customer.code,Total,weight,hazmat,status,bill to")
	eq(t, slices.Equal(types, []keel.ValueType{keel.TypeString, keel.TypeString, keel.TypeNumeric,
		keel.TypeInt64, keel.TypeBool, keel.TypeString, keel.TypeString}), true)
	eq(t, strings.Join(strs(&res.Columns[0]), ","), "PRO-0000,PRO-0001,PRO-0004,PRO-0005,PRO-0008")
	eq(t, res.Columns[1].String[2], "C1")
	eq(t, res.Columns[2].Numeric[1], keel.Decimal("101.0000"))
	weight := res.Columns[3]
	eq(t, weight.IsNull(0), true)
	eq(t, weight.Int64[1], int64(1010))
	eq(t, weight.IsNull(2), true)
	eq(t, weight.Value(0), any(nil))
	eq(t, weight.Value(1), any(int64(1010)))
	eq(t, res.Columns[4].Bool[0], true)
	eq(t, res.Columns[4].Bool[1], false)
	eq(t, res.Columns[5].String[1], "Assigned")
	billTo := res.Column("bill to")
	eq(t, billTo.IsNull(0), true)
	eq(t, billTo.String[1], "C0")
	eq(t, res.Column("missing") == nil, true)

	n := must(reg.Query("shipments").Where(reg.Query("shipments").Path("hazmat").IsTrue()).Count(w.ctx, d))(t)
	eq(t, n, int64(4))

	all := reg.Query("customers")
	res = must(all.Order(all.Path("code").Asc()).Run(w.ctx, d))(t)
	eq(t, res.Rows, 3)
	eq(t, len(res.Columns), len(fleetdb.CustomersSchema().Select))
	eq(t, res.Columns[0].Name, "id")
	eq(t, res.Column("code").String[2], "C2")
}

func TestDynamicExpressions(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	q := registry(t).Query("shipments")
	total := q.Path("total")
	q.Select(
		total.Mul(2).As("double"),
		q.Path("weight").Coalesce(0).Add(1).As("w1"),
		q.Path("pro_number").Concat("-x").As("tag"),
		keel.DynFunc("lower", q.Path("pro_number")).As("lower"),
		keel.DynCase().When(q.Path("priority").Gte(1), "high").Else("low").As("bucket"),
		keel.DynCase().When(q.Path("priority").Gt(5), "never").End().As("nothing"),
		total.Cast(keel.TypeFloat64).As("float"),
		keel.DynLit(int64(7)).Cast(keel.TypeInt64).As("seven"),
		q.Path("hazmat").Not().As("safe"),
		keel.DynFunc("round", total.Div(3), 2).As("third"),
		total.Sub(1).Neg().As("neg"),
		total.Mod(7).As("mod"),
		q.Path("priority").Eq(1).And(q.Path("hazmat").IsFalse()).As("and"),
		q.Path("priority").Eq(9).Or(q.Path("status").Ne("New")).As("or"),
		q.Path("weight").IsNotNull().As("weighed"),
		q.Path("bill_to_id").IsNull().As("own"),
		q.Path("pro_number").Like("PRO-%").As("like"),
		q.Path("pro_number").ILike("pro-%").As("ilike"),
		q.Path("pro_number").ContainsFold("o-00").As("fold"),
		q.Path("pro_number").HasSuffix("01").As("suffix"),
		q.Path("status").NotIn([]string{"New"}).As("notin"),
		q.Path("weight").IsDistinctFrom(nil).As("distinct"),
		total.NullIf(101).As("nullif"),
		keel.DynFunc("to_char", keel.DynFunc("to_timestamp", keel.DynLit(0)), "YYYY").As("year"),
		keel.DynOf(fleetdb.Shipments.Priority).As("typed"),
		keel.DynLit(nil).As("null"),
		keel.DynFunc("now").Gt(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)).As("now"),
	).Where(q.Path("pro_number").Eq("PRO-0001"))
	res := must(q.Run(w.ctx, d))(t)
	eq(t, res.Rows, 1)
	val := func(name string) any { return res.Column(name).Value(0) }
	eq(t, val("double"), any(keel.Decimal("202.0000")))
	eq(t, val("w1"), any(int64(1011)))
	eq(t, val("tag"), any("PRO-0001-x"))
	eq(t, val("lower"), any("pro-0001"))
	eq(t, val("bucket"), any("high"))
	eq(t, val("nothing"), any(nil))
	eq(t, val("float"), any(101.0))
	eq(t, val("seven"), any(int64(7)))
	eq(t, val("safe"), any(true))
	eq(t, val("third"), any(keel.Decimal("33.67")))
	eq(t, val("neg"), any(keel.Decimal("-100.0000")))
	eq(t, val("mod"), any(keel.Decimal("3.0000")))
	eq(t, val("and"), any(true))
	eq(t, val("or"), any(true))
	eq(t, val("weighed"), any(true))
	eq(t, val("own"), any(false))
	eq(t, val("like"), any(true))
	eq(t, val("ilike"), any(true))
	eq(t, val("fold"), any(true))
	eq(t, val("suffix"), any(true))
	eq(t, val("notin"), any(true))
	eq(t, val("distinct"), any(true))
	eq(t, val("nullif"), any(nil))
	eq(t, val("year"), any("1970"))
	eq(t, val("typed"), any(int64(1)))
	eq(t, val("null"), any(nil))
	eq(t, val("now"), any(true))
	f, err := res.Column("double").Numeric[0].Float64()
	ok(t, err)
	eq(t, f, 202.0)

	bad := []keel.DynExpr{
		keel.DynFunc("pg_sleep", 1),
		keel.DynFunc("lower"),
		keel.DynCase().End(),
		keel.DynCase().When(q.Path("nope").Eq(1), 1).End(),
		q.Path("total").Filter(q.Path("hazmat")),
		q.Path("total").Over(keel.Window{}),
		q.Path("total").Cast(keel.ValueType(99)),
		keel.DynOf(nil),
		keel.DynRow(1),
		keel.DynRow(1, keel.DynFunc("nope")),
		{},
	}
	for i, b := range bad {
		if b.Err() == nil {
			t.Fatalf("bad expression %d has no error", i)
		}
		if _, _, err := registry(t).Query("shipments").Select(b.As("x")).ToSQL(w.ctx, d); err == nil {
			t.Fatalf("bad expression %d rendered", i)
		}
	}
}

func TestDynRowKeysetInATypedQuery(t *testing.T) {
	d := db(t)
	w := seed(t, 12)
	s := fleetdb.Shipments
	all := must(s.Query().Order(s.Priority.Asc(), s.ProNumber.Asc()).All(w.ctx, d))(t)
	after := all[4]
	rows := must(s.Query().
		Where(keel.DynRow(s.Priority, s.ProNumber).Gt(keel.DynRow(after.Priority, after.ProNumber))).
		Order(s.Priority.Asc(), s.ProNumber.Asc()).
		All(w.ctx, d))(t)
	eq(t, len(rows), len(all)-5)
	for i, r := range rows {
		eq(t, r.ProNumber, all[i+5].ProNumber)
	}
	before := must(s.Query().
		Where(keel.DynRow(s.Priority, s.ProNumber).Lte(keel.DynRow(after.Priority, after.ProNumber))).
		All(w.ctx, d))(t)
	eq(t, len(before), 5)
}

func TestDynamicAggregatesWindowsAndGroupingSets(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	q := registry(t).Query("shipments")
	code := q.Path("customer", "code")
	revenue := q.Path("total").Sum()
	q.Select(code, keel.DynCount().As("n"),
		keel.DynCount().Filter(q.Path("hazmat").IsTrue()).As("hazmat"),
		revenue.As("revenue"),
		keel.DynOf(keel.Rank().OverWindow(keel.OrderBy(revenue.Desc()))).As("rank"),
		revenue.Sum().Over(keel.Window{}).As("all"),
		keel.DynFunc("string_agg", q.Path("pro_number"), ",").Filter(q.Path("priority").Eq(2)).As("p2"),
		q.Path("weight").Max().As("max"),
		q.Path("weight").Min().As("min"),
		q.Path("weight").Avg().As("avg"),
		q.Path("weight").Count().As("weighed"),
		q.Path("status").CountDistinct().As("statuses"),
	).Group(code).Order(code.Asc())
	res := must(q.Run(w.ctx, d))(t)
	eq(t, res.Rows, 3)
	eq(t, strings.Join(strs(res.Column("customer.code")), ","), "C0,C1,C2")
	eq(t, slices.Equal(res.Column("n").Int64, []int64{7, 7, 6}), true)
	eq(t, slices.Equal(res.Column("hazmat").Int64, []int64{2, 1, 1}), true)
	eq(t, slices.Equal(res.Column("revenue").Numeric, []keel.Decimal{"763.0000", "770.0000", "657.0000"}), true)
	eq(t, slices.Equal(res.Column("rank").Int64, []int64{2, 1, 3}), true)
	eq(t, res.Column("all").Numeric[2], keel.Decimal("2190.0000"))
	eq(t, res.Column("p2").IsNull(0), true)
	p2 := strings.Split(res.Column("p2").String[2], ",")
	slices.Sort(p2)
	eq(t, strings.Join(p2, ","), "PRO-0002,PRO-0005,PRO-0008,PRO-0011,PRO-0014,PRO-0017")
	eq(t, res.Column("max").Int64[0], int64(1180))
	eq(t, res.Column("weighed").Int64[0], int64(5))
	eq(t, res.Column("statuses").Int64[0], int64(4))
	eq(t, res.Column("avg").Type, keel.TypeNumeric)

	q = registry(t).Query("shipments")
	code = q.Path("customer", "code")
	st := q.Path("status")
	level := keel.DynOf(keel.Grouping(code, st))
	q.Select(code, st, keel.DynCount().As("n"), level.As("level")).
		Rollup(code, st).Having(level.Gt(0)).Order(level.Asc(), code.Asc())
	res = must(q.Run(w.ctx, d))(t)
	eq(t, res.Rows, 4)
	eq(t, slices.Equal(res.Column("n").Int64, []int64{7, 7, 6, 20}), true)
	eq(t, res.Column("customer.code").IsNull(3), true)

	q = registry(t).Query("shipments")
	code = q.Path("customer", "code")
	st = q.Path("status")
	q.Select(code, st, keel.DynCount().As("n")).GroupingSets([]keel.Expr{code}, []keel.Expr{st}, nil)
	eq(t, must(q.Count(w.ctx, d))(t), int64(8))
	q = registry(t).Query("shipments")
	q.Select(q.Path("hazmat"), q.Path("status"), keel.DynCount().As("n")).
		Group(q.Path("hazmat")).Cube(q.Path("status"))
	eq(t, must(q.Count(w.ctx, d))(t), int64(10))

	q = registry(t).Query("shipments")
	byPro := keel.OrderBy(q.Path("pro_number").Asc())
	q.Select(q.Path("pro_number"),
		q.Path("total").Sum().Over(byPro.Rows(keel.UnboundedPreceding, keel.CurrentRow)).As("running"),
		q.Path("total").Avg().Over(byPro.Rows(keel.Preceding(1), keel.Following(1))).As("moving"),
		keel.DynOf(keel.Lag(q.Path("total"), 1, 0)).Over(byPro).As("previous"),
		keel.DynOf(keel.RowNumber()).Over(keel.NamedWindow("w")).As("row"),
	).Window("w", keel.PartitionBy(q.Path("customer_id")).OrderBy(q.Path("pro_number").Asc())).
		Order(q.Path("pro_number").Asc()).Limit(4)
	res = must(q.Run(w.ctx, d))(t)
	eq(t, slices.Equal(res.Column("running").Numeric, []keel.Decimal{"100.0000", "201.0000", "303.0000", "406.0000"}), true)
	eq(t, res.Column("moving").Numeric[1], keel.Decimal("101.0000000000000000"))
	eq(t, res.Column("previous").Numeric[0], keel.Decimal("0"))
	eq(t, res.Column("previous").Numeric[3], keel.Decimal("102.0000"))
	eq(t, slices.Equal(res.Column("row").Int64, []int64{1, 1, 1, 2}), true)
}

func TestDynamicSharedParameters(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	q := registry(t).Query("shipments")
	bucket := keel.DynCase().When(q.Path("priority").Gte(1), "high").Else("low")
	q.Select(bucket.As("bucket"), keel.DynCount().As("n")).Group(bucket).Order(bucket.Asc())
	sql, args, err := q.ToSQL(w.ctx, d)
	ok(t, err)
	expr := `CASE WHEN (s."priority" >= $3) THEN $4 ELSE $5 END`
	if strings.Count(sql, expr) != 3 {
		t.Fatalf("the CASE renders identically in SELECT, GROUP BY and ORDER BY:\n%s", sql)
	}
	eq(t, len(args), 5)
	res := must(q.Run(w.ctx, d))(t)
	eq(t, strings.Join(strs(res.Column("bucket")), ","), "high,low")
	eq(t, slices.Equal(res.Column("n").Int64, []int64{13, 7}), true)

	q = registry(t).Query("shipments")
	q.Select(q.Path("pro_number")).Where(keel.Or(q.Path("status").Eq("New"), q.Path("status").Eq("New"),
		q.Path("pro_number").Eq("New")))
	sql, args, err = q.ToSQL(w.ctx, d)
	ok(t, err)
	if !strings.Contains(sql, `((s."status" = $3) OR (s."status" = $3) OR (s."pro_number" = $4))`) {
		t.Fatalf("equal values on one column share, on another they do not:\n%s", sql)
	}
	eq(t, len(args), 4)
	day := keel.DynLit("2026-01-01").Cast(keel.TypeDate)
	q = registry(t).Query("workers")
	q.Select(day.As("a"), keel.DynLit("2026-01-01").Cast(keel.TypeDate).As("b"),
		keel.DynLit("2026-01-01").Cast(keel.TypeTime).As("c"))
	sql, _, err = q.ToSQL(w.ctx, d)
	ok(t, err)
	if !strings.HasPrefix(sql, `SELECT $3::date AS c1, $3::date AS c2, $4::timestamptz AS c3`) {
		t.Fatalf("casts share by value and type:\n%s", sql)
	}
}

func TestDynamicToMany(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	reg := registry(t)
	count := func(p keel.DynExpr) int64 {
		t.Helper()
		q := reg.Query("shipments")
		return must(q.Where(p).Count(w.ctx, d))(t)
	}
	q := reg.Query("shipments")
	moves := q.Rel("moves")
	eq(t, count(moves.Any(moves.Col("status").Eq("New"))), int64(1))
	eq(t, count(moves.None()), int64(14))
	stops := q.Rel("moves", "stops")
	eq(t, count(stops.Any(stops.Col("kind").Eq("delivery"), stops.Col("location", "city").Eq("Dallas"))), int64(6))
	eq(t, count(stops.None(stops.Col("kind").Eq("pickup"))), int64(14))
	tags := q.Rel("tags")
	eq(t, count(tags.Any(tags.Col("name").Eq("rush"))), int64(2))
	cust := q.Rel("customer")
	eq(t, count(cust.Any(cust.Col("code").Eq("C1"))), int64(7))

	for _, s := range []keel.PreAggStrategy{keel.PreAggCTE, keel.PreAggLateral} {
		q := reg.Query("shipments")
		pro := q.Path("pro_number")
		m := q.Rel("moves").PreAggregate(s)
		nMoves := m.Measure(keel.DynCount().As("moves"))
		loaded := m.Measure(keel.DynCount().Filter(q.Rel("moves").Col("loaded").IsTrue()).As("loaded"))
		nStops := q.Rel("moves", "stops").PreAggregate(s).Measure(keel.DynCount().As("stops"))
		nTags := q.Rel("tags").PreAggregate(s).Measure(keel.DynCount().As("tags"))
		viaMoves := q.Rel("moves", "shipment", "tags").PreAggregate(s).
			Measure(keel.DynCount().As("move tags"))
		fresh := q.Rel("moves").PreAggregate(s).Where(q.Rel("moves").Col("status").Eq("New")).
			Measure(keel.DynCount().As("new"))
		q.Select(pro, nMoves, loaded, nStops, nTags, viaMoves, fresh).
			Where(pro.Lte("PRO-0007")).Order(pro.Asc())
		res := must(q.Run(w.ctx, d))(t)
		eq(t, res.Rows, 8)
		for i := range 8 {
			if i < 6 {
				eq(t, res.Column("moves").Int64[i], int64(2))
				eq(t, res.Column("loaded").Int64[i], int64(1))
				eq(t, res.Column("stops").Int64[i], int64(4))
			} else {
				eq(t, res.Column("moves").IsNull(i), true)
				eq(t, res.Column("stops").IsNull(i), true)
			}
		}
		eq(t, res.Column("tags").Int64[0], int64(2))
		eq(t, res.Column("tags").Int64[1], int64(1))
		eq(t, res.Column("tags").IsNull(2), true)
		eq(t, res.Column("move tags").Int64[0], int64(4))
		eq(t, res.Column("move tags").Int64[1], int64(2))
		eq(t, res.Column("new").Int64[0], int64(1))
		eq(t, res.Column("new").IsNull(1), true)

		g := reg.Query("shipments")
		code := g.Path("customer", "code")
		gm := g.Rel("moves").PreAggregate(s)
		perShipment := gm.Measure(keel.DynCount())
		g.Select(code, g.Path("total").Sum().As("revenue"), perShipment.Sum().As("moves")).
			Where(g.Path("hazmat").IsFalse()).
			Group(code).Order(code.Asc())
		res = must(g.Run(w.ctx, d))(t)
		eq(t, slices.Equal(res.Column("revenue").Numeric, []keel.Decimal{"548.0000", "660.0000", "552.0000"}), true)
		eq(t, slices.Equal(res.Column("moves").Numeric, []keel.Decimal{"2", "4", "2"}), true)
	}

	bad := reg.Query("shipments")
	if p := bad.Rel("customer").PreAggregate(keel.PreAggCTE); p.Err() == nil {
		t.Fatal("pre-aggregating a to-one path")
	}
	if p := bad.Rel("moves").PreAggregate(keel.PreAggStrategy(9)); p.Err() == nil {
		t.Fatal("unknown strategy")
	}
	if m := bad.Rel("moves").PreAggregate(keel.PreAggCTE).Measure(bad.Rel("moves").Col("sequence")); m.Err() == nil {
		t.Fatal("a measure that is not an aggregate")
	}
	if e := bad.Path("moves", "status"); e.Err() == nil || !strings.Contains(e.Err().Error(), "to-many") {
		t.Fatalf("a path through a to-many relation: %v", e.Err())
	}
	if r := bad.Rel("nope"); r.Err() == nil {
		t.Fatal("unknown relation")
	}
}

func TestDynamicCTEAndSetOperations(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	reg := registry(t)

	sub := reg.Query("shipments")
	sub.Select(sub.Path("customer_id").As("customer"), sub.Path("total").Sum().As("revenue")).
		Group(sub.Path("customer_id"))
	c := reg.Query("customers")
	totals := c.With(sub)
	c.Join(totals, totals.Col("customer").Eq(c.Path("id"))).
		Select(c.Path("code"), totals.Col("revenue")).
		Where(totals.Col("revenue").Gt(700)).Order(c.Path("code").Asc())
	res := must(c.Run(w.ctx, d))(t)
	eq(t, strings.Join(strs(res.Column("code")), ","), "C0,C1")
	eq(t, res.Column("revenue").Numeric[1], keel.Decimal("770.0000"))
	eq(t, slices.Equal(totals.Labels(), []string{"customer", "revenue"}), true)
	if totals.Col("nope").Err() == nil {
		t.Fatal("unknown CTE column")
	}
	other := reg.Query("customers")
	other.Join(totals)
	if other.Err() == nil {
		t.Fatal("joined a CTE declared on another query")
	}

	left := reg.Query("customers")
	lt := left.With(sub)
	left.LeftJoin(lt, lt.Col("customer").Eq(left.Path("id")), lt.Col("revenue").Gt(765)).
		Select(left.Path("code"), lt.Col("revenue")).Order(left.Path("code").Asc())
	res = must(left.Run(w.ctx, d))(t)
	eq(t, res.Rows, 3)
	eq(t, res.Column("revenue").IsNull(0), true)

	outer := keel.DynFromQuery(sub)
	outer.Select(keel.DynCount().As("customers"), outer.Path("revenue").Sum().As("revenue"))
	res = must(outer.Run(w.ctx, d))(t)
	eq(t, res.Column("customers").Int64[0], int64(3))
	eq(t, res.Column("revenue").Numeric[0], keel.Decimal("2190.0000"))
	if outer.Path("nope").Err() == nil || outer.Path("a", "b").Err() == nil {
		t.Fatal("unknown derived column")
	}

	names := func(table string) *keel.DynQuery {
		q := reg.Query(table)
		return q.Select(q.Path("name").As("name"))
	}
	u := keel.DynUnionAll(names("customers"), names("locations"), names("tags"))
	u.Order(u.Path("name").Asc())
	res = must(u.Run(w.ctx, d))(t)
	eq(t, res.Rows, 7)
	eq(t, res.Columns[0].Name, "name")
	eq(t, res.Column("name").String[0], "Customer C0")
	eq(t, must(keel.DynUnion(names("customers"), names("customers")).Count(w.ctx, d))(t), int64(3))
	eq(t, must(keel.DynIntersect(names("customers"), names("locations")).Count(w.ctx, d))(t), int64(0))
	ex := keel.DynExcept(names("customers"), reg.Query("customers").Select(
		reg.Query("customers").Path("name").As("name")).Where(reg.Query("customers").Path("code").Eq("C0")))
	eq(t, must(ex.Count(w.ctx, d))(t), int64(2))

	if keel.DynUnion(names("customers")).Err() == nil {
		t.Fatal("a set operation of one query")
	}
	two := reg.Query("customers")
	two.Select(two.Path("name"), two.Path("code"))
	if keel.DynUnion(names("customers"), two).Err() == nil {
		t.Fatal("a set operation of queries selecting different counts")
	}
	if keel.DynUnion(names("customers"), reg.Query("nope")).Err() == nil {
		t.Fatal("a set operation over an invalid part")
	}
}

func TestDynamicPage(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	reg := registry(t)
	q := reg.Query("shipments")
	pro := q.Path("pro_number")
	q.Select(pro, q.Path("customer", "code")).Where(q.Path("hazmat").IsFalse()).Order(pro.Desc())
	var got []string
	cursor := ""
	pages := 0
	for {
		res := must(q.Page(w.ctx, d, keel.After(cursor, 5)))(t)
		pages++
		got = append(got, res.Column("pro_number").String...)
		eq(t, len(res.Columns), 2)
		if res.Next == "" {
			break
		}
		cursor = res.Next
	}
	eq(t, pages, 4)
	eq(t, len(got), 16)
	eq(t, got[0], "PRO-0019")
	eq(t, slices.IsSortedFunc(got, func(a, b string) int { return strings.Compare(b, a) }), true)

	q = reg.Query("shipments")
	weight := q.Path("weight")
	q.Select(q.Path("pro_number")).Order(weight.Desc().NullsLast())
	var seen []string
	cursor = ""
	for {
		res := must(q.Page(w.ctx, d, keel.After(cursor, 6)))(t)
		seen = append(seen, res.Column("pro_number").String...)
		if res.Next == "" {
			break
		}
		cursor = res.Next
	}
	eq(t, len(seen), 20)
	eq(t, seen[0], "PRO-0019")
	eq(t, seen[14], "PRO-0001")
	tail := slices.Clone(seen[15:])
	slices.Sort(tail)
	eq(t, strings.Join(tail, ","), "PRO-0000,PRO-0004,PRO-0008,PRO-0012,PRO-0016")

	g := reg.Query("shipments")
	code := g.Path("customer", "code")
	g.Select(code, g.Path("total").Sum().As("revenue")).Group(code).Order(code.Asc())
	first := must(g.Page(w.ctx, d, keel.After("", 2)))(t)
	eq(t, strings.Join(strs(first.Column("customer.code")), ","), "C0,C1")
	second := must(g.Page(w.ctx, d, keel.After(first.Next, 2)))(t)
	eq(t, strings.Join(strs(second.Column("customer.code")), ","), "C2")
	eq(t, second.Next, "")

	var page *keel.DynResult
	var total int64
	ok(t, keel.Batch(w.ctx, d, keel.DynPageInto(q, keel.After("", 3), &page), keel.DynCountInto(q, &total)))
	eq(t, page.Rows, 3)
	eq(t, total, int64(20))

	_, err := reg.Query("shipments").Limit(3).Page(w.ctx, d, keel.After("", 2))
	if err == nil {
		t.Fatal("Page with a Limit")
	}
	_, err = q.Page(w.ctx, d, keel.After("", 0))
	if err == nil {
		t.Fatal("Page with size 0")
	}
	_, err = q.Page(w.ctx, d, keel.After("!!", 2))
	if err == nil {
		t.Fatal("invalid cursor")
	}
	_, err = g.Page(w.ctx, d, keel.After(first.Next+"AA", 2))
	if err == nil {
		t.Fatal("a cursor of another order")
	}
	u := keel.DynUnionAll(reg.Query("tags").Select(reg.Query("tags").Path("name").As("name")),
		reg.Query("tags").Select(reg.Query("tags").Path("name").As("name")))
	_, err = u.Page(w.ctx, d, keel.After("", 2))
	if err == nil {
		t.Fatal("Page of a derived query without an order")
	}
}

func TestDynamicTenancy(t *testing.T) {
	d := db(t)
	w := seed(t, 4)
	w2 := seed(t, 3)
	reg := registry(t)
	build := func() *keel.DynQuery {
		q := reg.Query("shipments")
		m := q.Rel("moves").PreAggregate(keel.PreAggCTE).Measure(keel.DynCount())
		return q.Select(q.Path("pro_number"), q.Path("customer", "code"), m.As("moves")).
			Where(q.Rel("tags").None(), q.Path("organization_id").In([]string{
				string(w.shipments[0].OrganizationID), string(w2.shipments[0].OrganizationID)}))
	}
	_, err := build().Run(context.Background(), d)
	if !errors.Is(err, keel.ErrNoScope) {
		t.Fatalf("no scope: %v", err)
	}
	sys := systemCtx("report")
	_, err = build().Run(sys, d)
	if !errors.Is(err, keel.ErrAllTenantsRequired) {
		t.Fatalf("system scope without AllTenants: %v", err)
	}
	_, err = build().AllTenants().Run(w.ctx, d)
	if !errors.Is(err, keel.ErrSystemScopeRequired) {
		t.Fatalf("AllTenants without a system scope: %v", err)
	}
	res := must(build().Run(w.ctx, d))(t)
	eq(t, res.Rows, 2)
	res = must(build().AllTenants().Run(sys, d))(t)
	eq(t, res.Rows, 3)
	moves := int64(0)
	for i := range res.Rows {
		if !res.Column("moves").IsNull(i) {
			moves += res.Column("moves").Int64[i]
		}
	}
	eq(t, moves, int64(6))

	sub := reg.Query("shipments")
	sub.Select(sub.Path("pro_number").As("pro"))
	_, err = keel.DynFromQuery(sub).AllTenants().Run(sys, d)
	if !errors.Is(err, keel.ErrAllTenantsRequired) {
		t.Fatalf("a derived query declares AllTenants itself: %v", err)
	}
	res = must(keel.DynFromQuery(sub.AllTenants()).AllTenants().Run(sys, d))(t)
	if res.Rows < 7 {
		t.Fatalf("all tenants: %d rows", res.Rows)
	}

	plants := reg.Query("plants")
	plants.Select(keel.DynCount().As("n"))
	_, err = plants.Run(context.Background(), d)
	ok(t, err)
}

func TestDynamicInjection(t *testing.T) {
	d := db(t)
	w := seed(t, 3)
	reg := registry(t)
	if reg.Query(`shipments"; DROP TABLE shipments; --`).Err() == nil {
		t.Fatal("a table name with SQL was accepted")
	}
	q := reg.Query("shipments")
	for _, e := range []keel.DynExpr{
		q.Path(`pro_number"; DROP TABLE shipments; --`),
		q.Path(`customer`, `name) FROM pg_user --`),
		q.Rel(`moves; SELECT 1`).Col("id"),
		keel.DynFunc(`lower); DROP TABLE shipments; --`, q.Path("pro_number")),
		keel.DynFunc("pg_read_file", "/etc/passwd"),
	} {
		if e.Err() == nil {
			t.Fatal("a name with SQL resolved")
		}
	}
	evil := `x' OR '1'='1`
	q = reg.Query("shipments")
	q.Select(q.Path("pro_number").As(`"; DROP TABLE shipments; --`)).
		Where(q.Path("pro_number").Eq(evil).Or(q.Path("status").Eq(evil)))
	sql, args, err := q.ToSQL(w.ctx, d)
	ok(t, err)
	if strings.Contains(sql, "DROP") || strings.Contains(sql, "'1'='1") {
		t.Fatalf("a label or value reached the SQL:\n%s", sql)
	}
	if !slices.Contains(args, any(evil)) {
		t.Fatalf("the value is not bound: %v", args)
	}
	_, err = q.Run(w.ctx, d)
	if err == nil || !strings.Contains(err.Error(), "invalid input value for enum") {
		t.Fatalf("an invalid enum value must fail as a value: %v", err)
	}
	q = reg.Query("shipments")
	q.Select(q.Path("pro_number")).Where(q.Path("pro_number").Eq(evil))
	res := must(q.Run(w.ctx, d))(t)
	eq(t, res.Rows, 0)
	eq(t, must(fleetdb.Shipments.Query().Count(w.ctx, d))(t), int64(3))

	_, _, err = q.Window(`w") AS (); DROP TABLE x; --`, keel.Window{}).ToSQL(w.ctx, d)
	if err == nil {
		t.Fatal("a window name with SQL rendered")
	}
}

func TestDynamicColumnarTypes(t *testing.T) {
	d := db(t)
	w := seed(t, 2)
	q := registry(t).Query("shipments")
	at := time.Date(2026, 3, 4, 5, 6, 7, 891000000, time.UTC)
	lit := func(v any, typ keel.ValueType) keel.DynExpr { return keel.DynLit(v).Cast(typ) }
	null := func(typ keel.ValueType) keel.DynExpr { return keel.DynLit(nil).Cast(typ) }
	q.Select(
		lit("héllo", keel.TypeString).As("string"), null(keel.TypeString).As("string null"),
		lit(int64(-42), keel.TypeInt64).As("int64"), null(keel.TypeInt64).As("int64 null"),
		q.Path("priority").As("int2"),
		lit(1.25, keel.TypeFloat64).As("float64"), null(keel.TypeFloat64).As("float64 null"),
		lit("-1234567890123456789012345.000100", keel.TypeNumeric).As("numeric"),
		lit("0.000012", keel.TypeNumeric).As("small"),
		lit("NaN", keel.TypeNumeric).As("nan"),
		lit("Infinity", keel.TypeNumeric).As("inf"),
		lit("10000", keel.TypeNumeric).As("ten thousand"),
		lit("0", keel.TypeNumeric).As("zero"),
		null(keel.TypeNumeric).As("numeric null"),
		lit(true, keel.TypeBool).As("bool"), null(keel.TypeBool).As("bool null"),
		lit(at, keel.TypeTime).As("time"), null(keel.TypeTime).As("time null"),
		lit("1999-12-31", keel.TypeDate).As("date"), null(keel.TypeDate).As("date null"),
		lit([]byte{0, 1, 0xfe}, keel.TypeBytes).As("bytes"), null(keel.TypeBytes).As("bytes null"),
		lit(`{"a": [1, 2]}`, keel.TypeJSON).As("json"), null(keel.TypeJSON).As("json null"),
		q.Path("attributes"),
		q.Path("labels"),
		q.Path("status"),
		keel.DynFunc("md5", "x").As("md5"),
		keel.DynOf(keel.Raw("'7b2d3e50-1c1c-4b5a-9a0e-3d2c1b0a9f8e'::uuid")).As("uuid"),
		keel.DynOf(keel.Raw("'1 day 02:00:00'::interval")).As("interval"),
		keel.DynOf(keel.Raw("'2026-01-02 03:04:05'::timestamp")).As("timestamp"),
		keel.DynOf(keel.Raw("'lit'::varchar(5)")).As("varchar"),
		keel.DynOf(keel.Raw("3::real")).As("real"),
		keel.DynOf(keel.Raw("'a:1'::tsvector")).As("tsvector"),
	).Where(q.Path("pro_number").Eq("PRO-0000"))
	res := must(q.Run(w.ctx, d))(t)
	eq(t, res.Rows, 1)
	check := func(name string, typ keel.ValueType, want any) {
		t.Helper()
		c := res.Column(name)
		if c == nil {
			t.Fatalf("no column %s", name)
		}
		eq(t, c.Type, typ)
		got := c.Value(0)
		if b, isBytes := got.([]byte); isBytes {
			got = string(b)
		}
		if tm, isTime := got.(time.Time); isTime {
			if !tm.Equal(want.(time.Time)) || tm.Location() != time.UTC {
				t.Fatalf("%s: got %v, want %v", name, tm, want)
			}
			return
		}
		if got != want {
			t.Fatalf("%s: got %#v, want %#v", name, got, want)
		}
	}
	check("string", keel.TypeString, "héllo")
	check("string null", keel.TypeString, nil)
	check("int64", keel.TypeInt64, int64(-42))
	check("int64 null", keel.TypeInt64, nil)
	check("int2", keel.TypeInt64, int64(0))
	check("float64", keel.TypeFloat64, 1.25)
	check("float64 null", keel.TypeFloat64, nil)
	check("numeric", keel.TypeNumeric, keel.Decimal("-1234567890123456789012345.000100"))
	check("small", keel.TypeNumeric, keel.Decimal("0.000012"))
	check("nan", keel.TypeNumeric, keel.Decimal("NaN"))
	check("inf", keel.TypeNumeric, keel.Decimal("Infinity"))
	check("ten thousand", keel.TypeNumeric, keel.Decimal("10000"))
	check("zero", keel.TypeNumeric, keel.Decimal("0"))
	check("numeric null", keel.TypeNumeric, nil)
	check("bool", keel.TypeBool, true)
	check("bool null", keel.TypeBool, nil)
	check("time", keel.TypeTime, at)
	check("time null", keel.TypeTime, nil)
	check("date", keel.TypeDate, time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC))
	check("date null", keel.TypeDate, nil)
	check("bytes", keel.TypeBytes, "\x00\x01\xfe")
	check("bytes null", keel.TypeBytes, nil)
	check("json", keel.TypeJSON, `{"a": [1, 2]}`)
	check("json null", keel.TypeJSON, nil)
	check("attributes", keel.TypeJSON, `{"temp": true, "index": 0}`)
	check("labels", keel.TypeJSON, `["lane-0"]`)
	check("status", keel.TypeString, "New")
	check("md5", keel.TypeString, "9dd4e461268c8034f5c8564e155c67a6")
	check("uuid", keel.TypeString, "7b2d3e50-1c1c-4b5a-9a0e-3d2c1b0a9f8e")
	check("timestamp", keel.TypeTime, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	check("varchar", keel.TypeString, "lit")
	check("real", keel.TypeFloat64, 3.0)
	eq(t, res.Column("interval").Type, keel.TypeJSON)
	if !strings.Contains(string(res.Column("interval").JSON[0]), "Days") {
		t.Fatalf("interval as JSON: %s", res.Column("interval").JSON[0])
	}
	eq(t, res.Column("tsvector").Type, keel.TypeJSON)
	eq(t, res.Column("time").OID, uint32(1184))

	nulls := registry(t).Query("shipments")
	weight := nulls.Path("weight")
	nulls.Select(weight).Order(nulls.Path("pro_number").Asc())
	many := seed(t, 130)
	res = must(nulls.Run(many.ctx, d))(t)
	eq(t, res.Rows, 130)
	for i := range 130 {
		eq(t, res.Columns[0].IsNull(i), i%4 == 0)
		if i%4 != 0 {
			eq(t, res.Columns[0].Int64[i], int64(1000+i*10))
		}
	}
	eq(t, len(res.Columns[0].Nulls), 3)
	eq(t, keel.TypeNumeric.String(), "numeric")
	eq(t, keel.ValueType(0).String(), "invalid")
}

func TestDynamicStream(t *testing.T) {
	d := db(t)
	w := seed(t, 10)
	q := registry(t).Query("shipments")
	pro := q.Path("pro_number")
	q.Select(pro, q.Path("weight")).Order(pro.Asc())
	var sizes []int
	var all []string
	for batch, err := range q.Stream(w.ctx, d, 3) {
		ok(t, err)
		sizes = append(sizes, batch.Rows)
		all = append(all, batch.Column("pro_number").String...)
	}
	eq(t, slices.Equal(sizes, []int{3, 3, 3, 1}), true)
	eq(t, len(all), 10)
	eq(t, all[9], "PRO-0009")

	n := 0
	for batch, err := range q.Stream(w.ctx, d, 4) {
		ok(t, err)
		n += batch.Rows
		break
	}
	eq(t, n, 4)
	ok(t, keel.Read(w.ctx, d, func(ctx context.Context) error {
		for _, err := range q.Stream(ctx, d, 2) {
			if err != nil {
				return err
			}
			break
		}
		_, err := fleetdb.Shipments.Query().Count(ctx, d)
		return err
	}))

	empty := registry(t).Query("shipments")
	empty.Select(empty.Path("pro_number")).Where(empty.Path("pro_number").Eq("none"))
	batches := 0
	for batch, err := range empty.Stream(w.ctx, d, 5) {
		ok(t, err)
		batches++
		eq(t, batch.Rows, 0)
		eq(t, batch.Columns[0].Name, "pro_number")
	}
	eq(t, batches, 1)

	for _, err := range registry(t).Query("shipments").Stream(context.Background(), d, 5) {
		if !errors.Is(err, keel.ErrNoScope) {
			t.Fatalf("stream without scope: %v", err)
		}
	}
	for _, err := range q.Stream(w.ctx, d, 0) {
		if err == nil {
			t.Fatal("stream with size 0")
		}
	}
}

func TestDynamicInterceptorsAndBatch(t *testing.T) {
	d := openDB(t, nil)
	w := seed(t, 20)
	s, m := fleetdb.Shipments, fleetdb.Moves
	d.Intercept(s, func(_ context.Context, q *keel.Intercepted) error {
		q.Where(s.Hazmat.IsFalse())
		return nil
	})
	d.Intercept(m, func(_ context.Context, q *keel.Intercepted) error {
		q.Where(m.Loaded.IsTrue())
		return nil
	})
	reg := registry(t)
	q := reg.Query("shipments")
	pro := q.Path("pro_number")
	moves := q.Rel("moves").PreAggregate(keel.PreAggCTE).Measure(keel.DynCount().As("moves"))
	q.Select(pro, moves).Where(pro.Lte("PRO-0005")).Order(pro.Asc())
	var res *keel.DynResult
	var n int64
	ok(t, keel.Batch(w.ctx, d, keel.DynInto(q, &res), keel.DynCountInto(q, &n)))
	eq(t, strings.Join(strs(res.Column("pro_number")), ","), "PRO-0001,PRO-0002,PRO-0003,PRO-0004")
	eq(t, slices.Equal(res.Column("moves").Int64, []int64{1, 1, 1, 1}), true)
	eq(t, n, int64(4))

	any := reg.Query("shipments")
	eq(t, must(any.Where(any.Rel("moves").Any(any.Rel("moves").Col("loaded").IsFalse())).Count(w.ctx, d))(t), int64(0))

	err := keel.Batch(w.ctx, d, keel.DynInto(q, nil))
	if err == nil {
		t.Fatal("DynInto without a destination")
	}
	err = keel.Batch(w.ctx, d, keel.DynCountInto(nil, &n))
	if err == nil {
		t.Fatal("DynCountInto without a query")
	}
}

func TestDynamicRefusesUnknownColumnsAtRun(t *testing.T) {
	d := db(t)
	w := seed(t, 1)
	q := registry(t).Query("shipments")
	q.Select(q.Path("nope"))
	_, err := q.Run(w.ctx, d)
	if err == nil || !strings.Contains(err.Error(), `table shipments has no column "nope"`) {
		t.Fatalf("unknown column: %v", err)
	}
	q = registry(t).Query("shipments")
	q.Select(q.Path("pro_number"), q.Path("pro_number"))
	if q.Err() == nil {
		t.Fatal("a label selected twice")
	}
	tbl := &schema.Table{Name: "ghost", Columns: []schema.Column{{Name: "id"}},
		Relations: []schema.Relation{{Name: "Other", Kind: schema.BelongsTo, Local: []int32{0}, Remote: []int32{0}}}}
	schema.Derive(tbl)
	if keel.DynFrom(tbl).Path("other", "id").Err() == nil {
		t.Fatal("a relation without a target resolved")
	}
}

func TestDynamicCTENamesArePerStatement(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	reg := registry(t)

	perCustomer := func() *keel.DynQuery {
		q := reg.Query("shipments")
		return q.Select(q.Path("customer_id").As("customer"), q.Path("total").Sum().As("revenue")).
			Group(q.Path("customer_id"))
	}
	inner := reg.Query("customers")
	totals := inner.With(perCustomer())
	inner.Join(totals, totals.Col("customer").Eq(inner.Path("id"))).
		Select(inner.Path("id").As("id"), totals.Col("revenue").As("revenue"))
	outer := reg.Query("customers")
	nested := outer.With(inner)
	again := outer.With(perCustomer())
	outer.Join(nested, nested.Col("id").Eq(outer.Path("id"))).
		Join(again, again.Col("customer").Eq(outer.Path("id"))).
		Select(outer.Path("code"), nested.Col("revenue").As("nested"), again.Col("revenue").As("direct")).
		Order(outer.Path("code").Asc())
	sql, _, err := outer.ToSQL(w.ctx, d)
	ok(t, err)
	for _, name := range []string{`"keel_cte"`, `"keel_cte_2"`, `"keel_cte_3"`} {
		if strings.Count(sql, name+" (") != 1 {
			t.Fatalf("%s is not declared exactly once:\n%s", name, sql)
		}
	}
	if strings.Contains(sql, `"keel_cte_4"`) {
		t.Fatalf("more CTEs than the statement declares:\n%s", sql)
	}
	res := must(outer.Run(w.ctx, d))(t)
	eq(t, res.Rows, 3)
	for i := range res.Rows {
		eq(t, res.Column("nested").Numeric[i], res.Column("direct").Numeric[i])
	}
	derived := keel.DynFromQuery(keel.DynFromQuery(perCustomer()))
	derived.Select(derived.Path("revenue").Sum().As("revenue"))
	sql, _, err = derived.ToSQL(w.ctx, d)
	ok(t, err)
	if strings.Count(sql, `"keel_sub" (`) != 1 || strings.Count(sql, `"keel_sub_2" (`) != 1 {
		t.Fatalf("nested derived tables share a name:\n%s", sql)
	}
	eq(t, must(derived.Run(w.ctx, d))(t).Column("revenue").Numeric[0], keel.Decimal("2190.0000"))
}

func TestDynamicExists(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	w := seed(t, 6)
	other := seed(t, 6)
	reg := registry(t)
	q := func() *keel.DynQuery { return reg.Query("shipments") }

	hazmat := q()
	eq(t, must(hazmat.Where(hazmat.Path("hazmat").IsTrue()).Exists(w.ctx, d))(t), true)
	missing := q()
	eq(t, must(missing.Where(missing.Path("pro_number").Eq("nope")).Exists(w.ctx, d))(t), false)
	rel := q()
	eq(t, must(rel.Where(rel.Path("customer", "code").Eq("C2")).Exists(w.ctx, d))(t), true)
	moves := q()
	mv := moves.Rel("moves")
	eq(t, must(moves.Where(mv.Any(mv.Col("sequence").Eq(1))).Exists(w.ctx, d))(t), true)
	eq(t, must(q().Offset(5).Exists(w.ctx, d))(t), true)
	eq(t, must(q().Offset(6).Exists(w.ctx, d))(t), false)

	mine := q()
	eq(t, must(mine.Where(mine.Path("id").Eq(other.shipments[0].ID)).Exists(w.ctx, d))(t), false)

	rec.mu.Lock()
	last := rec.events[len(rec.events)-1].SQL
	rec.mu.Unlock()
	if !strings.HasPrefix(last, "SELECT EXISTS (SELECT ") || !strings.Contains(last, `"organization_id" = $`) {
		t.Fatalf("exists statement: %s", last)
	}

	d.Intercept(fleetdb.Shipments, func(_ context.Context, ic *keel.Intercepted) error {
		ic.Where(fleetdb.Shipments.Hazmat.IsFalse())
		return nil
	})
	intercepted := q()
	eq(t, must(intercepted.Where(intercepted.Path("hazmat").IsTrue()).Exists(w.ctx, d))(t), false)

	var a, b bool
	ok(t, keel.Batch(w.ctx, d, keel.DynExistsInto(q(), &a), keel.DynExistsInto(reg.Query("tags"), &b)))
	eq(t, a && b, true)

	if _, err := q().Exists(systemCtx("exists"), d); !errors.Is(err, keel.ErrAllTenantsRequired) {
		t.Fatalf("system scope without AllTenants: %v", err)
	}
	if _, err := q().Exists(context.Background(), d); !errors.Is(err, keel.ErrNoScope) {
		t.Fatalf("no scope: %v", err)
	}
	if _, err := reg.Query("nope").Exists(w.ctx, d); err == nil {
		t.Fatal("unknown table")
	}
}

func TestDynExpressionsInTypedQueries(t *testing.T) {
	d := db(t)
	w := seed(t, 12)
	s := fleetdb.Shipments
	type row struct {
		Pro       string
		Konst     int64
		Arr       []string
		First     string
		Ranked    string
		Covers    bool
		Path      map[string]any
		Temp      bool
		Collated  string
		Replaced  string
		Bucket    int64
		Built     map[string]any
		Appended  []string
		Positions int64
	}
	rows := must(keel.Into[row](w.ctx, d, s.Pick(
		s.ProNumber,
		keel.DynConst(7),
		keel.DynArray(s.ProNumber, keel.DynConst("x")),
		keel.DynOf(s.Labels).At(1),
		keel.DynFunc("upper", s.ProNumber).Collate("C"),
		keel.DynOf(s.Labels).Covers(keel.DynArray(keel.DynConst("lane-0"))),
		s.Attributes.Path(),
		s.Attributes.Path("temp"),
		keel.DynOf(s.ProNumber).Collate("C"),
		keel.DynFunc("regexp_replace", s.ProNumber, "^PRO-", ""),
		keel.DynFunc("width_bucket", keel.DynLit(5).Cast(keel.TypeFloat64), 0.0, 10.0, 2),
		keel.DynFunc("jsonb_build_object", keel.DynLit("k").Cast(keel.TypeString), s.ProNumber),
		keel.DynFunc("array_append", s.Labels, keel.DynLit("z").Cast(keel.TypeString)),
		keel.DynFunc("array_position", s.Labels, keel.DynLit("lane-1").Cast(keel.TypeString)).Coalesce(0),
	).Where(
		keel.DynRow(s.ProNumber, s.Priority).InUnnest([]string{"PRO-0000", "PRO-0001"}, []int16{0, 1}),
	).Order(s.ProNumber.Asc())))(t)
	eq(t, len(rows), 2)
	eq(t, rows[0].Pro, "PRO-0000")
	eq(t, rows[0].Konst, int64(7))
	eq(t, strings.Join(rows[0].Arr, ","), "PRO-0000,x")
	eq(t, rows[0].First, "lane-0")
	eq(t, rows[0].Ranked, "PRO-0000")
	eq(t, rows[0].Covers, true)
	eq(t, rows[1].Covers, false)
	eq(t, rows[0].Path["temp"], any(true))
	eq(t, rows[0].Temp, true)
	eq(t, rows[0].Replaced, "0000")
	eq(t, rows[0].Bucket, int64(2))
	eq(t, rows[0].Built["k"], any("PRO-0000"))
	eq(t, strings.Join(rows[0].Appended, ","), "lane-0,z")
	eq(t, rows[0].Positions, int64(0))
	eq(t, rows[1].Positions, int64(1))

	type stats struct {
		Median   float64
		Disc     int64
		Mode     string
		Pros     []string
		Joined   string
		Contains bool
	}
	got := must(keel.IntoOne[stats](w.ctx, d, s.Pick(
		keel.PercentileCont(0.5, s.Weight.Asc()),
		keel.PercentileDisc(0.5, s.Weight.Asc()).Filter(s.Weight.IsNotNull()),
		keel.Mode(s.Status.Asc()),
		keel.ArrayAgg(s.ProNumber).OrderBy(s.ProNumber.Desc()),
		keel.StringAgg(s.ProNumber, ",").OrderBy(s.ProNumber.Asc()).Filter(s.ProNumber.Lte("PRO-0001")),
		keel.DynFunc("bool_or", s.Attributes.Contains(keel.DynFunc("jsonb_build_object",
			keel.DynLit("index").Cast(keel.TypeString), keel.DynConst(3)))),
	)))(t)
	eq(t, got.Median, 1060.0)
	eq(t, got.Disc, int64(1060))
	eq(t, got.Pros[0], "PRO-0011")
	eq(t, got.Joined, "PRO-0000,PRO-0001")
	eq(t, got.Contains, true)
	if got.Mode == "" {
		t.Fatal("mode returned nothing")
	}

	tag := w.shipments[0]
	_, err := s.Where(s.ID.Eq(tag.ID)).Update(w.ctx, d, s.Labels.Remove("lane-0"))
	ok(t, err)
	after := must(s.Where(s.ID.Eq(tag.ID)).One(w.ctx, d))(t)
	eq(t, len(after.Labels), 0)

	for i, b := range []keel.DynExpr{
		keel.DynConst(math.NaN()),
		keel.DynConst("a\x00b"),
		keel.DynConst([]int{1}),
		keel.DynArray(),
		keel.DynOf(s.ProNumber).Collate(""),
		keel.DynOf(s.ProNumber).Collate(`C"`),
		keel.DynRow(s.ID, s.ProNumber).InUnnest(),
	} {
		if b.Err() == nil {
			t.Fatalf("bad expression %d has no error", i)
		}
	}
	eq(t, must(s.Where(keel.DynOf(s.ProNumber).Eq(keel.DynConst("it's"))).Count(w.ctx, d))(t), int64(0))
}
