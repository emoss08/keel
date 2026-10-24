// Package geo holds models with PostGIS columns, read and written through a Go type with its own
// codec, as Trenova's postgis.Point is: a sql.Scanner and driver.Valuer on the pointer that decode
// EWKB and encode WKB.
package geo

import (
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

// Point is a longitude and latitude.
type Point struct {
	Lon float64
	Lat float64
}

// wkbPoint is the WKB geometry type of a point, and sridFlag the EWKB flag that says an SRID
// follows the type.
const (
	wkbPoint = 1
	sridFlag = 0x20000000
)

// Value encodes the point as little-endian WKB.
func (p *Point) Value() (driver.Value, error) {
	if p == nil {
		return nil, nil
	}
	b := make([]byte, 21)
	b[0] = 1
	binary.LittleEndian.PutUint32(b[1:], wkbPoint)
	binary.LittleEndian.PutUint64(b[5:], math.Float64bits(p.Lon))
	binary.LittleEndian.PutUint64(b[13:], math.Float64bits(p.Lat))
	return b, nil
}

// Scan decodes a point from WKB or EWKB, raw or hex-encoded.
func (p *Point) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case nil:
		*p = Point{}
		return nil
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("geo: cannot scan %T", src)
	}
	if len(b) > 0 && (b[0] == '0') {
		d, err := hex.DecodeString(string(b))
		if err != nil {
			return err
		}
		b = d
	}
	return p.decode(b)
}

// decode reads a point from WKB or EWKB.
func (p *Point) decode(b []byte) error {
	if len(b) < 5 {
		return errors.New("geo: short geometry")
	}
	var order binary.ByteOrder = binary.LittleEndian
	if b[0] == 0 {
		order = binary.BigEndian
	}
	typ := order.Uint32(b[1:])
	b = b[5:]
	if typ&sridFlag != 0 {
		b = b[4:]
		typ &^= sridFlag
	}
	if typ != wkbPoint || len(b) < 16 {
		return fmt.Errorf("geo: geometry type %d is not a point", typ)
	}
	p.Lon = math.Float64frombits(order.Uint64(b))
	p.Lat = math.Float64frombits(order.Uint64(b[8:]))
	return nil
}

// Place is a named location with PostGIS columns: a nullable geography point and a nullzero
// geometry point.
type Place struct {
	keel.Table `keel:"places"`

	ID             ids.ID `keel:"id,pk,autoid:plc_"`
	OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	Name           string `keel:"name"`
	Geom           *Point `keel:"geom"`
	Center         Point  `keel:"center,nullzero"`
	// Distance is the place's distance in meters from a point a query measures from.
	Distance float64 `keel:"distance,computed"`
}
