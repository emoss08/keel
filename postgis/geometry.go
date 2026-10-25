package postgis

import (
	"database/sql/driver"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/ewkb"
)

// Geometry is a geometry or geography value of any shape.
type Geometry struct {
	orb.Geometry
}

// Scan reads EWKB or its hex text form. NULL leaves the geometry unchanged.
func (g *Geometry) Scan(src any) error {
	if src == nil {
		return nil
	}
	geom, err := decode(src)
	if err != nil || geom == nil {
		return err
	}
	g.Geometry = geom
	return nil
}

// Value writes the geometry as EWKB with SRID 4326; a nil or empty geometry is NULL.
func (g *Geometry) Value() (driver.Value, error) {
	if g == nil || g.Geometry == nil {
		return nil, nil
	}
	return ewkb.Marshal(g.Geometry, SRID)
}
