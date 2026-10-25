// Package postgis maps PostGIS geometry and geography columns to Go values and builds spatial SQL
// with keel's expressions. Values are EWKB with SRID 4326 (WGS 84 longitude and latitude). Register
// the types on every connection with Config.AfterConnect: postgis.Register.
package postgis

import (
	"database/sql/driver"
	"fmt"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/ewkb"
)

// SRID is the spatial reference values are written in: WGS 84 longitude and latitude.
const SRID = 4326

// Point is a geometry(Point) or geography(Point) value: X is the longitude, Y the latitude.
type Point struct {
	orb.Point
}

// NewPoint returns the point at lon, lat.
func NewPoint(lon, lat float64) *Point {
	return &Point{Point: orb.Point{lon, lat}}
}

// Scan reads EWKB or its hex text form. NULL leaves the point unchanged.
func (p *Point) Scan(src any) error {
	if src == nil {
		return nil
	}
	g, err := decode(src)
	if err != nil || g == nil {
		return err
	}
	pt, ok := g.(orb.Point)
	if !ok {
		return fmt.Errorf("postgis: expected a Point, got %T", g)
	}
	p.Point = pt
	return nil
}

// Value writes the point as EWKB with SRID 4326; a nil point is NULL.
func (p *Point) Value() (driver.Value, error) {
	if p == nil {
		return nil, nil
	}
	return ewkb.Marshal(p.Point, SRID)
}

// Lon returns the longitude, 0 for a nil point.
func (p *Point) Lon() float64 {
	if p == nil {
		return 0
	}
	return p.Point.Lon()
}

// Lat returns the latitude, 0 for a nil point.
func (p *Point) Lat() float64 {
	if p == nil {
		return 0
	}
	return p.Point.Lat()
}

// IsValid reports whether the point is a longitude and latitude in range.
func (p *Point) IsValid() bool {
	if p == nil {
		return false
	}
	lon, lat := p.Lon(), p.Lat()
	return lon >= -180 && lon <= 180 && lat >= -90 && lat <= 90
}

// String formats the point as WKT.
func (p *Point) String() string {
	if p == nil {
		return "Point(nil)"
	}
	return fmt.Sprintf("POINT(%.6f %.6f)", p.Lon(), p.Lat())
}
