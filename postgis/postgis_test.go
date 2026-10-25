package postgis_test

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/emoss08/keel/postgis"
	"github.com/paulmach/orb"
)

func TestPointRoundTripsThroughEWKBAndHex(t *testing.T) {
	p := postgis.NewPoint(-96.797, 32.7767)
	v, err := p.Value()
	if err != nil {
		t.Fatal(err)
	}
	raw := v.([]byte)
	for _, src := range []any{raw, hex.EncodeToString(raw), `\x` + hex.EncodeToString(raw), []byte(hex.EncodeToString(raw))} {
		var got postgis.Point
		if err := got.Scan(src); err != nil {
			t.Fatalf("%T: %v", src, err)
		}
		if got.Lon() != -96.797 || got.Lat() != 32.7767 {
			t.Fatalf("%T: %v", src, got.String())
		}
	}
	var none postgis.Point
	if err := none.Scan(nil); err != nil || none.Lon() != 0 {
		t.Fatalf("NULL scanned into %v, %v", none, err)
	}
	var nilPoint *postgis.Point
	if v, err := nilPoint.Value(); v != nil || err != nil {
		t.Fatalf("nil point valued %v, %v", v, err)
	}
	if nilPoint.IsValid() || postgis.NewPoint(200, 0).IsValid() || !p.IsValid() {
		t.Fatal("IsValid")
	}
}

func TestGeometryRoundTripsAndRefusesBadInput(t *testing.T) {
	poly := orb.Polygon{{{0, 0}, {1, 0}, {1, 1}, {0, 0}}}
	g := &postgis.Geometry{Geometry: poly}
	v, err := g.Value()
	if err != nil {
		t.Fatal(err)
	}
	var got postgis.Geometry
	if err := got.Scan(v); err != nil {
		t.Fatal(err)
	}
	if got.GeoJSONType() != "Polygon" {
		t.Fatalf("scanned %T", got.Geometry)
	}
	var pt postgis.Point
	if err := pt.Scan(v); err == nil {
		t.Fatal("a polygon scanned into a Point")
	}
	if err := got.Scan(42); err == nil {
		t.Fatal("an int scanned")
	}
	if err := got.Scan([]byte{1, 1, 0, 0, 0x20}); !errors.Is(err, postgis.ErrInvalidEWKB) {
		t.Fatalf("short EWKB: %v", err)
	}
	if err := got.Scan("zz"); err == nil {
		t.Fatal("bad hex scanned")
	}
	var empty postgis.Geometry
	if v, err := empty.Value(); v != nil || err != nil {
		t.Fatalf("empty geometry valued %v, %v", v, err)
	}
}
