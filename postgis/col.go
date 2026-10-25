package postgis

import "github.com/emoss08/keel"

// PointCol is a geometry(Point) or geography(Point) column. keel gen gives a *Point field this
// column kind.
type PointCol struct{ keel.AnyCol }

// DWithin reports whether the point is within meters of other.
func (c PointCol) DWithin(other, meters any) keel.DynExpr { return DWithin(c.Col, other, meters) }

// Distance is the distance in meters from the point to other.
func (c PointCol) Distance(other any) keel.DynExpr { return Distance(c.Col, other) }

// Within reports whether the point lies within geometry g.
func (c PointCol) Within(g any) keel.DynExpr { return Within(c.Col, g) }

// AsGeoJSON is the point as a GeoJSON geometry object.
func (c PointCol) AsGeoJSON() keel.DynExpr { return AsGeoJSON(c.Col) }

// SetExpr sets the column to e, a value built here: c.SetExpr(postgis.Pt(lon, lat)).
func (c PointCol) SetExpr(e keel.Expr) keel.Assignment { return c.ToExpr(e) }

// GeometryCol is a geometry or geography column of any shape. keel gen gives a *Geometry field
// this column kind.
type GeometryCol struct{ keel.AnyCol }

// Contains reports whether the geometry contains g.
func (c GeometryCol) Contains(g any) keel.DynExpr { return Contains(c.Col, g) }

// Intersects reports whether the geometry and g share any point.
func (c GeometryCol) Intersects(g any) keel.DynExpr { return Intersects(c.Col, g) }

// DWithin reports whether the geometry is within meters of g.
func (c GeometryCol) DWithin(g, meters any) keel.DynExpr { return DWithin(c.Col, g, meters) }

// Distance is the distance in meters from the geometry to g.
func (c GeometryCol) Distance(g any) keel.DynExpr { return Distance(c.Col, g) }

// Area is the geometry's area in square meters.
func (c GeometryCol) Area() keel.DynExpr { return Area(c.Col) }

// Centroid is the geometry's center of mass.
func (c GeometryCol) Centroid() keel.DynExpr { return Centroid(c.Col) }

// AsGeoJSON is the geometry as a GeoJSON geometry object.
func (c GeometryCol) AsGeoJSON() keel.DynExpr { return AsGeoJSON(c.Col) }

// SetExpr sets the column to e, a value built here: c.SetExpr(postgis.FromGeoJSON(json)).
func (c GeometryCol) SetExpr(e keel.Expr) keel.Assignment { return c.ToExpr(e) }
