package postgis

import "github.com/emoss08/keel"

// Operands below are keel expressions (a column, a DynExpr, another builder here) written in place,
// or values bound as parameters: a *Point or *Geometry binds as EWKB. Distances and radii are in
// meters, measured on the WGS 84 spheroid through geography.

func expr(sql string, args ...any) keel.DynExpr { return keel.DynOf(keel.Raw(sql, args...)) }

// Pt is the point at lon, lat with SRID 4326: ST_SetSRID(ST_MakePoint(lon, lat), 4326). lon and lat
// are numbers or numeric expressions, such as two columns.
func Pt(lon, lat any) keel.DynExpr {
	return expr("ST_SetSRID(ST_MakePoint(?, ?), 4326)", lon, lat)
}

// FromGeoJSON is the geometry of a GeoJSON geometry object with SRID 4326.
func FromGeoJSON(geojson any) keel.DynExpr {
	return expr("ST_SetSRID(ST_GeomFromGeoJSON(?), 4326)", geojson)
}

// ToGeography is g as geography, for measures in meters and the geography GiST indexes.
func ToGeography(g any) keel.DynExpr { return expr("(?)::geography", g) }

// ToGeometry is g as geometry.
func ToGeometry(g any) keel.DynExpr { return expr("(?)::geometry", g) }

// Buffer is the geometry within meters of g: ST_Buffer(g::geography, meters)::geometry, a circle
// polygon around a point.
func Buffer(g, meters any) keel.DynExpr {
	return expr("ST_Buffer((?)::geography, ?)::geometry", g, meters)
}

// DWithin reports whether a and b are within meters of each other, as geography, which a GiST
// index on a geography column serves.
func DWithin(a, b, meters any) keel.DynExpr {
	return expr("ST_DWithin((?)::geography, (?)::geography, ?)", a, b, meters)
}

// Distance is the distance in meters between a and b on the spheroid.
func Distance(a, b any) keel.DynExpr {
	return expr("ST_Distance((?)::geography, (?)::geography)", a, b)
}

// DistanceSphere is the distance in meters between two points on a sphere, cheaper than Distance.
func DistanceSphere(a, b any) keel.DynExpr {
	return expr("ST_DistanceSphere((?)::geometry, (?)::geometry)", a, b)
}

// Contains reports whether geometry a contains geometry b.
func Contains(a, b any) keel.DynExpr {
	return expr("ST_Contains((?)::geometry, (?)::geometry)", a, b)
}

// Within reports whether geometry a lies within geometry b.
func Within(a, b any) keel.DynExpr {
	return expr("ST_Within((?)::geometry, (?)::geometry)", a, b)
}

// Intersects reports whether a and b share any point.
func Intersects(a, b any) keel.DynExpr {
	return expr("ST_Intersects((?)::geometry, (?)::geometry)", a, b)
}

// Area is g's area in square meters.
func Area(g any) keel.DynExpr { return expr("ST_Area((?)::geography)", g) }

// Centroid is g's center of mass, a point.
func Centroid(g any) keel.DynExpr { return expr("ST_Centroid((?)::geometry)", g) }

// AsGeoJSON is g as a GeoJSON geometry object, text.
func AsGeoJSON(g any) keel.DynExpr { return expr("ST_AsGeoJSON((?)::geometry)", g) }
