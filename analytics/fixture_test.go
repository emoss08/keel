package analytics_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// world is a tenant's report data: customers, locations, shipments with moves and stops, at
// instants chosen around a month boundary west of UTC and a daylight saving change.
type world struct {
	ctx context.Context
	org string
	bu  string
}

// shipmentRow is one fixture shipment.
type shipmentRow struct {
	pro, customer, status string
	priority              int
	weight                any
	total                 string
	hazmat                bool
	labels                string
	created               string
	moves                 [][]string
}

// fixture is the shipments of the main tenant. Created instants: P02 is January everywhere west
// of UTC and in UTC; P03 is February in UTC and January in Chicago; P06 is March in UTC and
// February in Chicago; P07 and P08 are on either side of New York's spring-forward on 8 March;
// P09 is 9 March in UTC and 8 March in New York.
var fixture = []shipmentRow{
	{"P01", "A", "New", 0, 1000, "100.50", false, "{x}", "2026-01-15T12:00:00Z", [][]string{{"Austin", "Dallas"}, {"Dallas", "Denver"}}},
	{"P02", "A", "Assigned", 1, 2000, "200.25", true, "{y}", "2026-01-31T23:30:00Z", [][]string{{"Austin", "Denver"}}},
	{"P03", "A", "InTransit", 2, nil, "300.00", false, "{}", "2026-02-01T03:00:00Z", [][]string{{"Dallas", "Austin"}}},
	{"P04", "G", "Delivered", 0, 4000, "400.00", false, "{x,y}", "2026-02-01T07:00:00Z", [][]string{{"Austin", "Dallas"}, {"Dallas", "Austin"}, {"Austin", "Denver"}}},
	{"P05", "G", "Delivered", 1, 500, "50.75", true, "{x}", "2026-02-14T10:00:00Z", nil},
	{"P06", "G", "Canceled", 0, nil, "0.00", false, "{}", "2026-03-01T05:30:00Z", [][]string{{"Denver", "Denver"}}},
	{"P07", "I", "New", 2, 7000, "700.00", false, "{z}", "2026-03-08T06:30:00Z", [][]string{{"Austin", "Dallas"}}},
	{"P08", "I", "Assigned", 0, 800, "80.00", false, "{}", "2026-03-08T07:30:00Z", [][]string{{"Dallas", "Dallas"}}},
	{"P09", "I", "InTransit", 1, 900, "90.10", true, "{}", "2026-03-09T03:30:00Z", [][]string{{"Austin", "Austin"}, {"Austin", "Dallas"}}},
	{"P10", "A", "Delivered", 0, 1100, "110.00", false, "{}", "2026-04-02T12:00:00Z", [][]string{{"Denver", "Austin"}}},
	{"P11", "G", "Delivered", 2, 1200, "120.00", false, "{}", "2026-04-20T12:00:00Z", [][]string{{"Dallas", "Denver"}}},
	{"P12", "I", "New", 0, nil, "12.40", false, "{}", "2026-05-05T12:00:00Z", nil},
}

// customerNames are the fixture customers by code.
var customerNames = map[string]string{"A": "Acme", "G": "Globex", "I": "Initech"}

// seedWorld creates a tenant with the fixture's rows.
func seedWorld(t testing.TB) *world {
	t.Helper()
	return seedRows(t, fixture)
}

// seedRows creates a tenant with the given shipments.
func seedRows(t testing.TB, rows []shipmentRow) *world {
	t.Helper()
	d := db(t)
	ctx := newTenant()
	tn := tenantOf(ctx)
	w := &world{ctx: ctx, org: string(tn.org), bu: string(tn.bu)}
	var stmts []string
	add := func(format string, args ...any) { stmts = append(stmts, fmt.Sprintf(format, args...)) }
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	tenantCols := q(w.org) + ", " + q(w.bu)
	for code, name := range customerNames {
		add(`INSERT INTO customers (id, organization_id, business_unit_id, name, code, created_at, updated_at)
			VALUES (%s, %s, %s, %s, 0, 0)`, q("cus_"+code), tenantCols, q(name), q(code))
	}
	for _, city := range []string{"Austin", "Dallas", "Denver"} {
		add(`INSERT INTO locations (id, organization_id, business_unit_id, name, city)
			VALUES (%s, %s, %s, %s)`, q("loc_"+city), tenantCols, q(city+" Dock"), q(city))
	}
	for _, s := range rows {
		created, err := time.Parse(time.RFC3339, s.created)
		if err != nil {
			t.Fatal(err)
		}
		weight := "NULL"
		if s.weight != nil {
			weight = fmt.Sprint(s.weight)
		}
		add(`INSERT INTO shipments (id, organization_id, business_unit_id, customer_id, pro_number, status,
			priority, weight, total, hazmat, labels, created_at, updated_at)
			VALUES (%s, %s, %s, %s, %s, %d, %s, %s, %t, %s, %d, %d)`,
			q("shp_"+s.pro), tenantCols, q("cus_"+s.customer), q(s.pro), q(s.status), s.priority, weight,
			s.total, s.hazmat, q(s.labels), created.Unix(), created.Unix())
		for mi, stops := range s.moves {
			mid := fmt.Sprintf("mov_%s_%d", s.pro, mi+1)
			add(`INSERT INTO moves (id, organization_id, business_unit_id, shipment_id, sequence, status, loaded, created_at)
				VALUES (%s, %s, %s, %d, %s, %t, %d)`, q(mid), tenantCols, q("shp_"+s.pro), mi+1, q(s.status), mi == 0,
				created.Unix())
			for si, city := range stops {
				kind := "pickup"
				if si > 0 {
					kind = "delivery"
				}
				arrived := created.Add(time.Duration(mi*10+si+1) * time.Hour)
				add(`INSERT INTO stops (id, organization_id, business_unit_id, move_id, location_id, sequence, kind, arrived_at)
					VALUES (%s, %s, %s, %s, %d, %s, %s)`, q(fmt.Sprintf("stp_%s_%d_%d", s.pro, mi+1, si+1)), tenantCols,
					q(mid), q("loc_"+city), si+1, q(kind), q(arrived.Format(time.RFC3339)))
			}
		}
	}
	if _, err := d.Pool().Exec(context.Background(), strings.Join(stmts, ";\n")); err != nil {
		t.Fatal(err)
	}
	return w
}
