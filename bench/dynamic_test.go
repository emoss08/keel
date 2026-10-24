package bench

import (
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// BenchmarkDynamic1000 reads 1000 rows of six columns through a dynamic query built by name into a
// columnar result, beside the same projection decoded into structs with keel.Into.
func BenchmarkDynamic1000(b *testing.B) {
	e := setup(b)
	reg, err := keel.NewRegistry(fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema())
	if err != nil {
		b.Fatal(err)
	}
	b.Run("dynamic", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			q := reg.Query("shipments")
			pro := q.Path("pro_number")
			q.Select(pro, q.Path("status"), q.Path("weight"), q.Path("total"), q.Path("hazmat"),
				q.Path("created_at")).
				Where(q.Path("status").Eq("Assigned")).Order(pro.Asc()).Limit(1000)
			res, err := q.Run(e.ctx, e.keel)
			if err != nil || res.Rows != 1000 {
				b.Fatal(err)
			}
		}
	})
	b.Run("into", func(b *testing.B) {
		type row struct {
			Pro     string
			Status  fleet.Status
			Weight  *int64
			Total   fleet.Money
			Hazmat  bool
			Created int64
		}
		s := fleetdb.Shipments
		b.ReportAllocs()
		for b.Loop() {
			rows, err := keel.Into[row](e.ctx, e.keel, s.Pick(s.ProNumber, s.Status, s.Weight, s.Total,
				s.Hazmat, s.CreatedAt).Where(s.Status.Eq(fleet.StatusAssigned)).Order(s.ProNumber.Asc()).
				Limit(1000))
			if err != nil || len(rows) != 1000 {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkDynamicReport runs a grouped report with a pre-aggregated to-many measure, a window
// and subtotals.
func BenchmarkDynamicReport(b *testing.B) {
	e := setup(b)
	reg, err := keel.NewRegistry(fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema(), fleetdb.MovesSchema())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		q := reg.Query("shipments")
		code := q.Path("customer", "code")
		moves := q.Rel("moves").PreAggregate(keel.PreAggCTE).Measure(keel.DynCount())
		revenue := q.Path("total").Sum()
		q.Select(code, revenue.As("revenue"), moves.Sum().As("moves"),
			keel.DynOf(keel.Grouping(code)).As("level"),
			keel.DynOf(keel.Rank().OverWindow(keel.OrderBy(revenue.Desc()))).As("rank")).
			Where(q.Path("created_at").Gte(time.Now().Add(-24 * time.Hour).Unix())).
			Rollup(code).Order(code.Asc())
		if _, err := q.Run(e.ctx, e.keel); err != nil {
			b.Fatal(err)
		}
	}
}
