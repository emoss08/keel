package keel_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/geo"
	"github.com/emoss08/keel/internal/testmodels/geo/geodb"
	"github.com/emoss08/keel/postgis"
	"github.com/jackc/pgx/v5"
)

// postgisDSN is the PostGIS server the geometry tests run against. They skip when it is
// unreachable, since keel's own server has no PostGIS.
var postgisDSN = envOr("KEEL_TEST_POSTGIS_DSN", "postgres://keel:keel@localhost:55541/keel?sslmode=disable")

// placesDDL creates the table of geo.Place.
const placesDDL = `CREATE EXTENSION IF NOT EXISTS postgis;
CREATE TABLE places (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    geom geography(Point, 4326),
    center geometry(Point),
    CONSTRAINT pk_places PRIMARY KEY (id, organization_id, business_unit_id),
    CONSTRAINT uq_places_name UNIQUE (organization_id, business_unit_id, name)
);
CREATE TABLE zones (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    center geography(Point, 4326),
    area geometry(Geometry, 4326),
    CONSTRAINT pk_zones PRIMARY KEY (id, organization_id, business_unit_id)
);
CREATE INDEX idx_zones_center ON zones USING gist (center)`

// postgisDB opens a fresh database on the PostGIS server with the places table, dropped after the
// test, with the PostGIS codecs registered when codecs is set and observers seeing its statements.
func postgisDB(t *testing.T, codecs bool, observers ...keel.Observer) (*keel.DB, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, postgisDSN)
	if err != nil {
		t.Skipf("PostGIS unavailable at %s: %v", postgisDSN, err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "keel_geo_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if a, err := pgx.Connect(ctx, postgisDSN); err == nil {
			_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = a.Close(ctx)
		}
	})
	u, err := url.Parse(postgisDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	setupConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setupConn.Exec(ctx, placesDDL); err != nil {
		t.Fatal(err)
	}
	_ = setupConn.Close(ctx)
	cfg := testConfig(dsn)
	cfg.Observers = observers
	if codecs {
		cfg.AfterConnect = postgis.Register
	}
	d, err := keel.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d, dsn
}

// near reports whether two points are the same to a micro-degree.
func near(a, b geo.Point) bool {
	return math.Abs(a.Lon-b.Lon) < 1e-6 && math.Abs(a.Lat-b.Lat) < 1e-6
}

var (
	dallas  = geo.Point{Lon: -96.797, Lat: 32.7767}
	austin  = geo.Point{Lon: -97.7431, Lat: 30.2672}
	chicago = geo.Point{Lon: -87.6298, Lat: 41.8781}
)

func TestPostGISCodecColumns(t *testing.T) {
	tr := &tracer{}
	d, _ := postgisDB(t, true, tr)
	ctx := newTenant()
	p := geodb.Places

	one := &geo.Place{Name: "dallas", Geom: &dallas, Center: dallas}
	ok(t, p.Create(ctx, d, one))
	none := &geo.Place{Name: "nowhere"}
	ok(t, p.Create(ctx, d, none))
	ok(t, p.CreateMany(ctx, d, []*geo.Place{
		{Name: "austin", Geom: &austin, Center: austin}, {Name: "blank"},
	}))
	unnested := false
	for _, sp := range tr.spans {
		unnested = unnested || strings.Contains(sp.sql, "$5::geography[], $6::geometry[]")
	}
	if !unnested {
		t.Fatal("the bulk insert did not send the PostGIS columns through their codec")
	}
	n := must(p.Copy(ctx, d, []*geo.Place{{Name: "chicago", Geom: &chicago, Center: chicago}, {Name: "void"}}))(t)
	eq(t, n, 2)

	nulls := must(keel.QueryValue[int64](ctx, d,
		`SELECT count(*) FROM places WHERE geom IS NULL AND center IS NULL`))(t)
	eq(t, nulls, 3)

	got := must(p.Get(ctx, d, one.ID))(t)
	if got.Geom == nil || !near(*got.Geom, dallas) || !near(got.Center, dallas) {
		t.Fatalf("read back %+v", got)
	}
	empty := must(p.Get(ctx, d, none.ID))(t)
	if empty.Geom != nil || empty.Center != (geo.Point{}) {
		t.Fatalf("NULL read back as %+v", empty)
	}
	rows := must(p.Where(p.Geom.IsNotNull()).Order(p.Name.Asc()).All(ctx, d))(t)
	eq(t, len(rows), 3)
	if !near(*rows[0].Geom, austin) || !near(rows[1].Center, chicago) {
		t.Fatalf("bulk rows %+v %+v", rows[0], rows[1])
	}
	eq(t, must(p.Where(p.Center.Eq(geo.Point{})).Count(ctx, d))(t), 3)

	from := geo.Point{Lon: -96.8, Lat: 32.78}
	ranked := must(p.Compute(p.Distance, keel.Raw("ST_Distance(?, ?::geography)", p.Geom, &from)).
		Where(keel.Raw("ST_DWithin(?, ?::geography, 400000)", p.Geom, &from)).
		Order(p.Distance.Asc()).All(ctx, d))(t)
	eq(t, len(ranked), 2)
	eq(t, ranked[0].Name, "dallas")
	if ranked[0].Distance <= 0 || ranked[0].Distance > ranked[1].Distance {
		t.Fatalf("distances %v %v", ranked[0].Distance, ranked[1].Distance)
	}

	got.Geom = &austin
	got.Center = geo.Point{}
	ok(t, p.Save(ctx, d, got))
	again := must(p.Get(ctx, d, one.ID))(t)
	if !near(*again.Geom, austin) || again.Center != (geo.Point{}) {
		t.Fatalf("saved %+v", again)
	}
	moved := must(p.Where(p.Name.Eq("nowhere")).Update(ctx, d, p.Center.To(chicago)))(t)
	eq(t, moved, 1)
	eq(t, must(p.Where(p.Center.IsNotNull(), p.Name.Eq("nowhere")).Count(ctx, d))(t), 1)
	_, err := p.Upsert(ctx, d, []*geo.Place{{Name: "nowhere", Geom: &chicago}, {Name: "new", Center: austin}},
		keel.OnConflict(p.OrganizationID, p.BusinessUnitID, p.Name).Update(p.Geom))
	ok(t, err)
	up := must(p.Where(p.Name.In("nowhere", "new")).Order(p.Name.Asc()).All(ctx, d))(t)
	if !near(up[0].Center, austin) || !near(*up[1].Geom, chicago) || !near(up[1].Center, chicago) {
		t.Fatalf("upserted %+v %+v", up[0], up[1])
	}
}

func TestPostGISScannerWithoutCodec(t *testing.T) {
	d, dsn := postgisDB(t, true)
	ctx := newTenant()
	p := geodb.Places
	ok(t, p.CreateMany(ctx, d, []*geo.Place{{Name: "dallas", Geom: &dallas, Center: dallas}, {Name: "none"}}))

	cfg := testConfig(dsn)
	plain, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	rows := must(p.Order(p.Name.Asc()).All(ctx, plain))(t)
	if rows[0].Geom == nil || !near(*rows[0].Geom, dallas) || !near(rows[0].Center, dallas) || rows[1].Geom != nil {
		t.Fatalf("text-format scan %+v %+v", rows[0], rows[1])
	}
}

func TestPostGISPackageBuildsSpatialQueries(t *testing.T) {
	d, _ := postgisDB(t, true)
	ctx := newTenant()
	z := geodb.Zones
	dal := &geo.Zone{Name: "dallas", Center: postgis.NewPoint(-96.797, 32.7767)}
	aus := &geo.Zone{Name: "austin", Center: postgis.NewPoint(-97.7431, 30.2672)}
	ok(t, z.CreateMany(ctx, d, []*geo.Zone{dal, aus}))

	got := must(z.Where(z.ID.Eq(dal.ID)).One(ctx, d))(t)
	eq(t, got.Center.Lon(), -96.797)
	eq(t, got.Center.Lat(), 32.7767)

	_, err := z.Where(z.ID.Eq(dal.ID)).Update(ctx, d,
		z.Area.SetExpr(postgis.Buffer(postgis.Pt(-96.797, 32.7767), 5000)))
	ok(t, err)
	square := `{"type":"Polygon","coordinates":[[[-98,30],[-97,30],[-97,31],[-98,31],[-98,30]]]}`
	_, err = z.Where(z.ID.Eq(aus.ID)).Update(ctx, d, z.Area.SetExpr(postgis.FromGeoJSON(square)))
	ok(t, err)

	near := must(z.Where(z.Center.DWithin(postgis.Pt(-96.8, 32.78), 2000)).All(ctx, d))(t)
	eq(t, len(near), 1)
	eq(t, near[0].Name, "dallas")

	type measured struct {
		Name     string
		Meters   float64
		Inside   bool
		Area     float64
		GeoJSON  string
		Polygon  *postgis.Geometry
		Centroid *postgis.Point
	}
	rows := must(keel.Into[measured](ctx, d, z.Pick(
		z.Name,
		z.Center.Distance(postgis.NewPoint(-96.797, 32.7767)),
		z.Area.Contains(postgis.Pt(-96.797, 32.7767)),
		z.Area.Area(),
		z.Center.AsGeoJSON(),
		z.Area,
		z.Area.Centroid(),
	).Order(z.Name.Asc())))(t)
	eq(t, len(rows), 2)
	eq(t, rows[0].Name, "austin")
	if rows[0].Meters < 280_000 || rows[0].Meters > 300_000 {
		t.Fatalf("austin is %f m from dallas", rows[0].Meters)
	}
	eq(t, rows[1].Meters, 0.0)
	eq(t, rows[1].Inside, true)
	eq(t, rows[0].Inside, false)
	if rows[1].Area < 7.7e7 || rows[1].Area > 7.9e7 {
		t.Fatalf("a 5 km buffer covers %f m2", rows[1].Area)
	}
	if !strings.Contains(rows[0].GeoJSON, `"Point"`) {
		t.Fatalf("GeoJSON %s", rows[0].GeoJSON)
	}
	if rows[0].Polygon == nil || rows[0].Polygon.GeoJSONType() != "Polygon" {
		t.Fatalf("polygon %v", rows[0].Polygon)
	}
	if !rows[0].Centroid.IsValid() || math.Abs(rows[0].Centroid.Lon()+97.5) > 1e-6 {
		t.Fatalf("centroid %v", rows[0].Centroid)
	}

	within := must(z.Where(z.Center.Within(postgis.FromGeoJSON(square))).All(ctx, d))(t)
	eq(t, len(within), 1)
	eq(t, within[0].Name, "austin")
	intersects := must(z.Where(z.Area.Intersects(postgis.Buffer(postgis.Pt(-97.5, 30.5), 1000))).All(ctx, d))(t)
	eq(t, len(intersects), 1)
	sphere := must(keel.IntoOne[float64](ctx, d, z.Pick(
		postgis.DistanceSphere(postgis.Pt(-96.797, 32.7767), postgis.Pt(-97.7431, 30.2672)).Max())))(t)
	if sphere < 280_000 || sphere > 300_000 {
		t.Fatalf("sphere distance %f", sphere)
	}
}
