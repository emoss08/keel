package keel_test

import (
	"context"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// renderDB is a DB that is never connected: pgxpool dials lazily, so it renders statements
// without a server.
func renderDB(t *testing.T) *keel.DB {
	t.Helper()
	d, err := keel.Open(context.Background(), testConfig("postgres://keel:keel@127.0.0.1:1/none"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func TestRenderedSQL(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	m := s.Moves()
	cases := []struct {
		name string
		q    interface {
			ToSQL(context.Context, *keel.DB) (string, []any, error)
		}
		want string
	}{
		{
			"filter and order",
			s.Pick(s.ID, s.ProNumber).Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned)).
				Order(s.CreatedAt.Desc()).Limit(20),
			`SELECT s."id", s."pro_number", s."organization_id", s."business_unit_id" FROM "shipments" s ` +
				`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND s."status" = ANY($3) ` +
				`ORDER BY s."created_at" DESC LIMIT 20`,
		},
		{
			"relation path joins by relation name",
			s.Pick(s.ID).Where(s.Customer().Code.Eq("C1"), s.BillTo().Code.Eq("C2")),
			`SELECT s."id", s."organization_id", s."business_unit_id" FROM "shipments" s ` +
				`LEFT JOIN "customers" c ON c."id" = s."customer_id" AND c."organization_id" = $1 AND c."business_unit_id" = $2 ` +
				`LEFT JOIN "customers" bt ON bt."id" = s."bill_to_id" AND bt."organization_id" = $1 AND bt."business_unit_id" = $2 ` +
				`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND c."code" = $3 AND bt."code" = $4`,
		},
		{
			"exists over a to-many path",
			s.Pick(s.ID).Where(m.Any(m.Loaded.IsTrue())),
			`SELECT s."id", s."organization_id", s."business_unit_id" FROM "shipments" s ` +
				`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND EXISTS (SELECT 1 FROM "moves" m ` +
				`WHERE m."organization_id" = $1 AND m."business_unit_id" = $2 AND m."shipment_id" = s."id" AND m."loaded" IS TRUE)`,
		},
		{
			"self join with an As name",
			fleetdb.Workers.Pick(fleetdb.Workers.ID).Join(fleetdb.Workers.As("mgr"),
				fleetdb.Workers.As("mgr").ID.EqCol(fleetdb.Workers.ManagerID)),
			`SELECT w."id", w."organization_id", w."business_unit_id" FROM "workers" w ` +
				`JOIN "workers" mgr ON mgr."organization_id" = $1 AND mgr."business_unit_id" = $2 AND mgr."id" = w."manager_id" ` +
				`WHERE w."organization_id" = $1 AND w."business_unit_id" = $2`,
		},
		{
			"same table twice gets a numbered alias",
			s.Pick(s.ID).Where(keel.Exists(s.As("s").Where(s.As("s").ProNumber.Eq("x")))),
			`SELECT s."id", s."organization_id", s."business_unit_id" FROM "shipments" s ` +
				`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND EXISTS (SELECT 1 FROM "shipments" s2 ` +
				`WHERE s2."organization_id" = $1 AND s2."business_unit_id" = $2 AND s2."pro_number" = $3)`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := c.q.ToSQL(ctx, d)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}
