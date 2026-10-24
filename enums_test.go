package keel

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// manyEnums is the number of enum types TestEnumsRegisterInOneQuery creates, about as many as a
// large application's schema has.
const manyEnums = 700

// manyEnumsSQL creates manyEnums enum types in a schema of their own, one with a name that needs
// quoting, a domain over an enum and a table with an enum column.
func manyEnumsSQL() string {
	var b strings.Builder
	b.WriteString(`CREATE SCHEMA "Ops";`)
	for i := range manyEnums - 1 {
		b.WriteString(`CREATE TYPE "Ops".e` + strconv.Itoa(i) + ` AS ENUM ('a', 'b', 'c');`)
	}
	b.WriteString(`CREATE TYPE "Ops"."Stop Kind" AS ENUM ('pickup', 'de,livery');
		CREATE DOMAIN "Ops".first_kind AS "Ops"."Stop Kind";
		CREATE TABLE stops (id int PRIMARY KEY, kind "Ops"."Stop Kind" NOT NULL);
		INSERT INTO stops VALUES (1, 'pickup'), (2, 'de,livery'), (3, 'pickup');`)
	return b.String()
}

func TestEnumsRegisterInOneQuery(t *testing.T) {
	c := internalConn(t, manyEnumsSQL(), "Ops.first_kind")
	ctx := context.Background()
	m := c.Conn().TypeMap()

	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE t.typtype = 'e' AND n.nspname = 'Ops'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	same(t, n, manyEnums)
	for _, name := range []string{"e0", "Ops.e0", "Ops.e698", "Stop Kind", "Ops.Stop Kind"} {
		typ, ok := m.TypeForName(name)
		if !ok {
			t.Fatalf("enum %s is not registered", name)
		}
		if _, ok := typ.Codec.(*pgtype.EnumCodec); !ok {
			t.Fatalf("enum %s: codec %T, want *pgtype.EnumCodec", name, typ.Codec)
		}
	}
	for _, name := range []string{"_e0", "Ops._e698", "_Stop Kind"} {
		typ, ok := m.TypeForName(name)
		if !ok {
			t.Fatalf("enum array %s is not registered", name)
		}
		if _, ok := typ.Codec.(*arrayCodec); !ok {
			t.Fatalf("enum array %s: codec %T, want keel's arrayCodec", name, typ.Codec)
		}
	}
	if _, ok := m.TypeForOID(oidOf(t, c, `"Ops".first_kind`)); !ok {
		t.Fatal("the domain over an enum listed in LoadTypes is not registered")
	}

	same(t, roundTrip[[]string](t, c, textArray{"pickup", "de,livery"}, `"Ops"."Stop Kind"[]`),
		[]string{"pickup", "de,livery"})
	var count int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM stops WHERE kind = ANY($1)",
		textArray{"pickup"}).Scan(&count); err != nil {
		t.Fatal(err)
	}
	same(t, count, 2)
	rows, err := c.Query(ctx, "SELECT kind FROM stops ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	same(t, kinds, []string{"pickup", "de,livery", "pickup"})
	var arr []string
	if err := c.QueryRow(ctx, `SELECT array_agg(kind ORDER BY id) FROM stops`).Scan(&arr); err != nil {
		t.Fatal(err)
	}
	same(t, arr, []string{"pickup", "de,livery", "pickup"})

	pc, err := pgxpool.ParseConfig(c.Conn().Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	tracer := &queryCounter{}
	pc.ConnConfig.Tracer = tracer
	db, err := Open(ctx, Config{Pool: pc})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const conns = 5
	start := time.Now()
	for range conns {
		conn, err := db.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Conn().Close(ctx); err != nil {
			t.Fatal(err)
		}
		conn.Release()
	}
	t.Logf("connection setup with %d enums: %v", manyEnums, time.Since(start)/conns)
	same(t, int(tracer.n.Load()), conns)
}

// queryCounter counts the queries of the connections it traces.
type queryCounter struct{ n atomic.Int64 }

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	q.n.Add(1)
	return ctx
}

func (q *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
