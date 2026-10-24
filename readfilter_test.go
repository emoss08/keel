package keel_test

import (
	"context"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/schema"
)

// rowRules is a read filter that hides customer C1, stops of kind "delivery" and moves after the
// first, recording the tables it was asked about.
func rowRules(asked map[string]int) keel.ReadFilter {
	return func(_ context.Context, t *schema.Table) ([]keel.Pred, error) {
		asked[t.Name]++
		switch t.Name {
		case "customers":
			return []keel.Pred{fleetdb.Customers.Code.Ne("C1")}, nil
		case "moves":
			return []keel.Pred{keel.DynFrom(t).Path("sequence").Lt(1)}, nil
		case "stops":
			return []keel.Pred{keel.DynFrom(t).Path("kind").Ne("delivery")}, nil
		}
		return nil, nil
	}
}

func TestReadFilterRendersOnEveryReference(t *testing.T) {
	d := renderDB(t)
	asked := map[string]int{}
	ctx := keel.WithReadFilter(tenantCtx("org_1", "bu_1"), rowRules(asked))
	q := keel.DynFrom(fleetdb.ShipmentsSchema())
	moves := q.Rel("Moves").PreAggregate(keel.PreAggCTE)
	q.Select(q.Path("pro_number"), q.Path("Customer", "code"), q.Path("BillTo", "code").As("bill to"),
		q.Path("OriginStop", "kind").As("origin"), moves.Measure(keel.DynCount()).As("moves")).
		Where(q.Rel("Moves", "Stops").Any(q.Rel("Moves", "Stops").Col("sequence").Eq(0)))
	sql, _, err := q.ToSQL(ctx, d)
	ok(t, err)
	for _, want := range []string{
		`LEFT JOIN "customers" c ON c."id" = s."customer_id" AND c."organization_id" = $1 AND c."business_unit_id" = $2 AND c."code" <> $`,
		`LEFT JOIN "customers" bt ON bt."id" = s."bill_to_id" AND bt."organization_id" = $1 AND bt."business_unit_id" = $2 AND bt."code" <> $`,
		`m."sequence" < $`,
		`s2."kind" <> $`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in\n%s", want, sql)
		}
	}
	if asked["customers"] < 2 || asked["moves"] < 3 || asked["stops"] < 2 {
		t.Errorf("tables asked: %v", asked)
	}
}

func TestReadFilterHidesRows(t *testing.T) {
	d := db(t)
	w := seed(t, 6)
	asked := map[string]int{}
	ctx := keel.WithReadFilter(w.ctx, rowRules(asked))
	q := keel.DynFrom(fleetdb.ShipmentsSchema())
	moves := q.Rel("Moves").PreAggregate(keel.PreAggCTE)
	stops := q.Rel("Moves", "Stops").PreAggregate(keel.PreAggLateral)
	pro := q.Path("pro_number")
	q.Select(pro, q.Path("Customer", "code"), moves.Measure(keel.DynCount()).As("moves"),
		stops.Measure(keel.DynCount()).As("stops")).Order(pro.Asc())
	res := must(q.Run(ctx, d))(t)
	eq(t, res.Rows, 6)
	codes := res.Columns[1]
	eq(t, codes.String[0], "C0")
	eq(t, codes.IsNull(1), true)
	eq(t, codes.String[2], "C2")
	eq(t, res.Columns[2].Int64[0], int64(1))
	eq(t, res.Columns[3].Int64[0], int64(1))
	n := must(keel.DynFrom(fleetdb.CustomersSchema()).Count(ctx, d))(t)
	eq(t, n, int64(2))
}

func TestExecAllReachesThePrimary(t *testing.T) {
	d := db(t)
	n, err := keel.ExecAll(context.Background(), d, "SELECT 1 FROM generate_series(1, 3)")
	ok(t, err)
	eq(t, n, int64(3))
}

func TestPreAggregationOfAllParents(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	build := func(all bool) string {
		q := keel.DynFrom(fleetdb.ShipmentsSchema())
		p := q.Rel("Moves").PreAggregate(keel.PreAggCTE)
		if all {
			p.AllParents()
		}
		q.Select(q.Path("pro_number"), p.Measure(keel.DynCount()).As("moves")).Where(q.Path("status").Eq("New"))
		sql, _, err := q.ToSQL(ctx, d)
		ok(t, err)
		return sql
	}
	if !strings.Contains(build(false), `) IN (SELECT`) {
		t.Fatal("a filtered pre-aggregation is not restricted to the selected parents")
	}
	if strings.Contains(build(true), `) IN (SELECT`) {
		t.Fatal("AllParents still restricts the parents")
	}
}
