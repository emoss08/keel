package postgis

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Register teaches conn the geometry and geography types and their arrays, as EWKB bytes, so values
// bind and scan in binary and bulk inserts send them as arrays. Use it as Config.AfterConnect, or
// call it from one. A database without PostGIS registers nothing.
func Register(ctx context.Context, conn *pgx.Conn) error {
	rows, err := conn.Query(ctx,
		`SELECT typname, oid, typarray FROM pg_type WHERE typname IN ('geometry', 'geography')`)
	if err != nil {
		return fmt.Errorf("postgis: read types: %w", err)
	}
	var types []*pgtype.Type
	for rows.Next() {
		var name string
		var oid, array uint32
		if err := rows.Scan(&name, &oid, &array); err != nil {
			rows.Close()
			return fmt.Errorf("postgis: read types: %w", err)
		}
		elem := &pgtype.Type{Name: name, OID: oid, Codec: pgtype.ByteaCodec{}}
		types = append(types, elem, &pgtype.Type{Name: "_" + name, OID: array,
			Codec: &pgtype.ArrayCodec{ElementType: elem}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgis: read types: %w", err)
	}
	for _, t := range types {
		conn.TypeMap().RegisterType(t)
	}
	return nil
}
