package postgis

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/wkb"
)

// ErrInvalidEWKB is returned for EWKB too short to hold the SRID its header declares.
var ErrInvalidEWKB = errors.New("postgis: invalid EWKB: too short for its SRID")

// decode turns a scanned value, binary EWKB or its hex text form, into a geometry. It returns nil for
// an empty value.
func decode(src any) (orb.Geometry, error) {
	var data []byte
	var err error
	switch v := src.(type) {
	case []byte:
		data, err = decodeHexBytes(v)
	case string:
		data, err = decodeHexString(v)
	default:
		return nil, fmt.Errorf("postgis: cannot scan %T", src)
	}
	if err != nil || len(data) == 0 {
		return nil, err
	}
	plain, err := stripSRID(data)
	if err != nil {
		return nil, err
	}
	g, err := wkb.Unmarshal(plain)
	if err != nil {
		return nil, fmt.Errorf("postgis: unmarshal WKB: %w", err)
	}
	return g, nil
}

// stripSRID removes the SRID an EWKB header carries, leaving WKB.
func stripSRID(data []byte) ([]byte, error) {
	if len(data) < 5 {
		return nil, fmt.Errorf("postgis: invalid EWKB: %d bytes", len(data))
	}
	order := data[0]
	if order != 0 && order != 1 {
		return nil, fmt.Errorf("postgis: invalid EWKB byte order %d", order)
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if order == 0 {
		bo = binary.BigEndian
	}
	typ := bo.Uint32(data[1:5])
	if typ&0x20000000 == 0 {
		return data, nil
	}
	if len(data) < 9 {
		return nil, ErrInvalidEWKB
	}
	out := make([]byte, len(data)-4)
	out[0] = order
	bo.PutUint32(out[1:5], typ&^0x20000000)
	copy(out[5:], data[9:])
	return out, nil
}

func decodeHexString(src string) ([]byte, error) {
	if len(src) >= 2 && src[0] == '\\' && src[1] == 'x' {
		src = src[2:]
	}
	data, err := hex.DecodeString(src)
	if err != nil {
		return nil, fmt.Errorf("postgis: decode hex: %w", err)
	}
	return data, nil
}

func decodeHexBytes(src []byte) ([]byte, error) {
	trimmed := src
	if len(trimmed) >= 2 && trimmed[0] == '\\' && trimmed[1] == 'x' {
		trimmed = trimmed[2:]
	}
	if !isHex(trimmed) {
		return src, nil
	}
	data, err := hex.DecodeString(string(trimmed))
	if err != nil {
		return nil, fmt.Errorf("postgis: decode hex: %w", err)
	}
	return data, nil
}

func isHex(src []byte) bool {
	if len(src) == 0 || len(src)%2 != 0 {
		return false
	}
	for _, b := range src {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
			return false
		}
	}
	return true
}
