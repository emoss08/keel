package bench

import (
	"strconv"
	"testing"

	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// --- bulk loads: binary COPY against INSERT … SELECT FROM unnest ---

func benchBulkLoad(b *testing.B, n int) {
	e := setup(b)
	customer := e.ship[0].CustomerID
	rows := func() []*fleet.Shipment {
		prefix := "LOAD-" + rid() + "-"
		out := make([]*fleet.Shipment, n)
		for i := range out {
			w := int64(i)
			out[i] = &fleet.Shipment{
				CustomerID: customer, ProNumber: prefix + strconv.Itoa(i), Status: fleet.StatusAssigned,
				Weight: &w, Total: fleet.Money{Cents: int64(i * 100)}, Labels: []string{"a", "b"},
				Attributes: map[string]any{"i": i},
			}
		}
		return out
	}
	reset := func(b *testing.B) {
		if _, err := e.pgx.Exec(e.ctx, "DELETE FROM shipments WHERE pro_number LIKE 'LOAD-%'"); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("CreateMany", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			reset(b)
			rs := rows()
			b.StartTimer()
			if err := fleetdb.Shipments.CreateMany(e.ctx, e.keel, rs); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Copy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			reset(b)
			rs := rows()
			b.StartTimer()
			if _, err := fleetdb.Shipments.Copy(e.ctx, e.keel, rs); err != nil {
				b.Fatal(err)
			}
		}
	})
	reset(b)
}

func BenchmarkBulkLoad1000(b *testing.B)  { benchBulkLoad(b, 1000) }
func BenchmarkBulkLoad20000(b *testing.B) { benchBulkLoad(b, 20000) }
