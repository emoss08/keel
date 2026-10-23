package keel_test

import (
	"testing"

	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func BenchmarkGetByIDLocal(b *testing.B) {
	d := db(b)
	w := seed(b, 4)
	id := w.shipments[1].ID
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := fleetdb.Shipments.Get(w.ctx, d, id); err != nil {
			b.Fatal(err)
		}
	}
}
