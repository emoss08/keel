package keel

import (
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// jsonNumberKey names, in a connection's CustomData, the decoder of jsonnumber columns:
// Config.JSON.UnmarshalNumber, stored by the pool's AfterConnect.
const jsonNumberKey = "keel.jsonnumber"

// jsonNumberPlan decodes a jsonnumber column (schema.FlagJSONNumber) with a decoder that keeps
// numbers as json.Number. NULL stores the field's zero value, as the jsonb codec does for a nil map
// or slice and as nullzero asks; a jsonb value in the binary format drops its version byte first.
type jsonNumberPlan struct {
	unmarshal func(data []byte, v any) error
	binary    bool
}

// Scan decodes src into target, a pointer to the field.
func (p jsonNumberPlan) Scan(src []byte, target any) error {
	if src == nil {
		reflect.ValueOf(target).Elem().SetZero()
		return nil
	}
	if p.binary {
		if len(src) == 0 || src[0] != 1 {
			return errors.New("keel: unknown jsonb binary format version")
		}
		src = src[1:]
	}
	return p.unmarshal(src, target)
}

// planJSONNumber returns the plan of a jsonnumber column of type oid read on conn in format: the
// connection's Config.JSON.UnmarshalNumber, or the default's on a connection keel did not set
// up. It returns nil for a column of another type, which decodes as usual.
func planJSONNumber(conn *pgx.Conn, oid uint32, format int16) pgtype.ScanPlan {
	if oid != pgtype.JSONOID && oid != pgtype.JSONBOID {
		return nil
	}
	fn := defaultJSON.UnmarshalNumber
	if f, ok := conn.PgConn().CustomData()[jsonNumberKey].(func([]byte, any) error); ok {
		fn = f
	}
	return jsonNumberPlan{unmarshal: fn, binary: oid == pgtype.JSONBOID && format == pgtype.BinaryFormatCode}
}
