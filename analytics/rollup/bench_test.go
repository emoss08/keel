package rollup_test

import (
	"context"
	"testing"

	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// BenchmarkCreate measures a one-row create with the two shipment rollups' triggers marking slices
// and with the triggers disabled, so the difference is what dirty tracking costs a write.
func BenchmarkCreate(b *testing.B) {
	clk := &clock{}
	d := openDB(b, clk)
	f := newFixture(b, d, clk)
	for _, mode := range []string{"triggers", "no triggers"} {
		b.Run(mode, func(b *testing.B) {
			if mode == "no triggers" {
				_, err := d.Pool().Exec(context.Background(), "ALTER TABLE shipments DISABLE TRIGGER USER")
				ok(b, err)
				b.Cleanup(func() { _, _ = d.Pool().Exec(context.Background(), "ALTER TABLE shipments ENABLE TRIGGER USER") })
			}
			b.ReportAllocs()
			for b.Loop() {
				sh := &fleet.Shipment{CustomerID: f.customers[0].ID, ProNumber: "B" + randHex(), Status: fleet.StatusNew,
					Labels: []string{}, Attributes: map[string]any{}}
				ok(b, fleetdb.Shipments.Create(f.ctx, d, sh))
			}
		})
	}
}

// BenchmarkWriteAndRefreshSlice measures a create followed by the refresh of the day it dirtied,
// which holds 200 shipments of one tenant.
func BenchmarkWriteAndRefreshSlice(b *testing.B) {
	clk := &clock{}
	d := openDB(b, clk)
	f := newFixture(b, d, clk)
	for i := range 200 {
		f.shipment(0, i%3, fleet.StatusNew, int64(i), nil)
	}
	ctx := context.Background()
	for b.Loop() {
		f.shipment(0, 0, fleet.StatusNew, 1, nil)
		_, err := rollup.Refresh(ctx, d, daily)
		ok(b, err)
	}
}
