package keel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// internalConn returns a connection of a keel DB on a fresh database prepared by setup. The database is
// dropped when the test ends. Tests skip when Postgres is unreachable and KEEL_TEST_REQUIRE is not
// set.
func internalConn(t *testing.T, setup string, loadTypes ...string) *pgxpool.Conn {
	t.Helper()
	ctx := context.Background()
	admin := os.Getenv("KEEL_TEST_DSN")
	if admin == "" {
		admin = "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
	}
	a, err := pgx.Connect(ctx, admin)
	if err != nil {
		if os.Getenv("KEEL_TEST_REQUIRE") != "" {
			t.Fatalf("postgres: %v", err)
		}
		t.Skipf("postgres unavailable: %v", err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "keel_internal_" + hex.EncodeToString(b)
	if _, err := a.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg := a.Config()
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	t.Cleanup(func() {
		_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = a.Close(ctx)
	})
	if setup != "" {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Exec(ctx, setup)
		_ = c.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := Open(ctx, Config{DSN: dsn, LoadTypes: loadTypes})
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Release()
		db.Close()
	})
	return c
}

// arrayConn returns a connection whose database has an enum, a domain over text and its array
// registered.
func arrayConn(t *testing.T) *pgxpool.Conn {
	return internalConn(t, `CREATE TYPE mood AS ENUM ('happy', 'sad', 'it''s "complicated"');
		CREATE DOMAIN label AS text`, "label", "_label")
}

// oidOf returns the OID of a type name.
func oidOf(t *testing.T, c *pgxpool.Conn, typ string) uint32 {
	t.Helper()
	var oid uint32
	if err := c.QueryRow(context.Background(), "SELECT $1::regtype::oid", typ).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	return oid
}

// roundTrip sends v as a parameter of type typ, checks that keel's plan encodes it, and decodes
// the array Postgres returns into a T.
func roundTrip[T any](t *testing.T, c *pgxpool.Conn, v any, typ string) T {
	t.Helper()
	plan := c.Conn().TypeMap().PlanEncode(oidOf(t, c, typ), pgtype.BinaryFormatCode, v)
	if _, ok := plan.(arrayPlan); !ok {
		t.Fatalf("%T as %s: plan %T, want keel's arrayPlan", v, typ, plan)
	}
	var out T
	if err := c.QueryRow(context.Background(), "SELECT $1::"+typ, v).Scan(&out); err != nil {
		t.Fatalf("%T as %s: %v", v, typ, err)
	}
	return out
}

// same fails the test when got and want differ.
func same(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// ptr returns a pointer to a copy of v.
func ptr[V any](v V) *V { return &v }

// trickyStrings need quoting or escaping in an array literal, or are not ASCII.
var trickyStrings = []string{`plain`, `with "quotes"`, `back\slash`, `comma,brace}`, `{}`, ``, `NULL`,
	` padded `, "ünïcødé ✓ 日本語", "new\nline", strings.Repeat("x", 3000)}

func TestArrayParamsText(t *testing.T) {
	c := arrayConn(t)
	for _, typ := range []string{"text[]", "varchar[]", "name[]", "label[]"} {
		if typ == "name[]" {
			same(t, roundTrip[[]string](t, c, textArray(trickyStrings[:10]), typ), trickyStrings[:10])
			continue
		}
		same(t, roundTrip[[]string](t, c, textArray(trickyStrings), typ), trickyStrings)
	}
	same(t, roundTrip[[]*string](t, c, nullTextArray{ptr(`a"b`), nil, ptr(""), ptr(`c\d`)}, "text[]"),
		[]*string{ptr(`a"b`), nil, ptr(""), ptr(`c\d`)})
	same(t, roundTrip[[]*string](t, c, nullTextArray{nil}, "text[]"), []*string{nil})
	same(t, roundTrip[[]string](t, c, textArray{"happy", `it's "complicated"`}, "mood[]"),
		[]string{"happy", `it's "complicated"`})
	if err := c.QueryRow(context.Background(), "SELECT $1::mood[]", textArray{"angry"}).Scan(new([]string)); err == nil {
		t.Fatal("an unknown enum label was accepted")
	}
}

func TestArrayParamsEmpty(t *testing.T) {
	c := arrayConn(t)
	cases := []struct {
		v   any
		typ string
	}{
		{textArray{}, "text[]"}, {nullTextArray{}, "text[]"},
		{int8Array{}, "int8[]"}, {nullInt8Array{}, "int8[]"}, {int4Array{}, "int4[]"},
		{nullInt4Array{}, "int4[]"}, {int2Array{}, "int2[]"}, {nullInt2Array{}, "int2[]"},
		{float8Array{}, "float8[]"}, {nullFloat8Array{}, "float8[]"}, {float4Array{}, "float4[]"},
		{nullFloat4Array{}, "float4[]"}, {boolArray{}, "bool[]"}, {nullBoolArray{}, "bool[]"},
		{timestamptzArray{}, "timestamptz[]"}, {nullTimestamptzArray{}, "timestamptz[]"},
		{byteaArray{}, "bytea[]"}, {textArray{}, "mood[]"},
	}
	for _, tc := range cases {
		var n *int32
		var empty bool
		err := c.QueryRow(context.Background(), "SELECT cardinality($1::"+tc.typ+"), $1::"+tc.typ+" = '{}'",
			tc.v).Scan(&n, &empty)
		if err != nil {
			t.Fatalf("%T as %s: %v", tc.v, tc.typ, err)
		}
		if n == nil || *n != 0 || !empty {
			t.Fatalf("%T as %s: not an empty array", tc.v, tc.typ)
		}
	}
	var null bool
	err := c.QueryRow(context.Background(), "SELECT $1::text[] IS NULL", textArray(nil)).Scan(&null)
	if err != nil || !null {
		t.Fatalf("a nil slice binds NULL, like every nil slice in pgx: %v %v", null, err)
	}
}

func TestArrayParamsIntegers(t *testing.T) {
	c := arrayConn(t)
	same(t, roundTrip[[]int64](t, c, int8Array{0, -1, 1, math.MaxInt64, math.MinInt64}, "int8[]"),
		[]int64{0, -1, 1, math.MaxInt64, math.MinInt64})
	same(t, roundTrip[[]int32](t, c, int8Array{math.MaxInt32, math.MinInt32, 7}, "int4[]"),
		[]int32{math.MaxInt32, math.MinInt32, 7})
	same(t, roundTrip[[]int16](t, c, int8Array{math.MaxInt16, math.MinInt16}, "int2[]"),
		[]int16{math.MaxInt16, math.MinInt16})
	same(t, roundTrip[[]*int64](t, c, nullInt8Array{ptr(int64(5)), nil}, "int8[]"), []*int64{ptr(int64(5)), nil})
	same(t, roundTrip[[]int32](t, c, int4Array{math.MaxInt32, math.MinInt32}, "int4[]"),
		[]int32{math.MaxInt32, math.MinInt32})
	same(t, roundTrip[[]int64](t, c, int4Array{-3}, "int8[]"), []int64{-3})
	same(t, roundTrip[[]*int32](t, c, nullInt4Array{nil, ptr(int32(-9))}, "int4[]"), []*int32{nil, ptr(int32(-9))})
	same(t, roundTrip[[]int16](t, c, int2Array{math.MaxInt16, math.MinInt16, 0}, "int2[]"),
		[]int16{math.MaxInt16, math.MinInt16, 0})
	same(t, roundTrip[[]*int16](t, c, nullInt2Array{ptr(int16(4)), nil}, "int2[]"), []*int16{ptr(int16(4)), nil})
	ctx := context.Background()
	for _, tc := range []struct {
		v   any
		typ string
	}{
		{int8Array{math.MaxInt32 + 1}, "int4[]"}, {int8Array{math.MinInt32 - 1}, "int4[]"},
		{int4Array{math.MaxInt16 + 1}, "int2[]"}, {nullInt8Array{nil, ptr(int64(math.MinInt16 - 1))}, "int2[]"},
	} {
		err := c.QueryRow(ctx, "SELECT $1::"+tc.typ, tc.v).Scan(new([]*int64))
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("%v as %s: err %v, want out of range", tc.v, tc.typ, err)
		}
	}
}

func TestArrayParamsFloatsAndBools(t *testing.T) {
	c := arrayConn(t)
	f8 := []float64{0, -1.5, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1)}
	same(t, roundTrip[[]float64](t, c, float8Array(f8), "float8[]"), f8)
	same(t, roundTrip[[]*float64](t, c, nullFloat8Array{nil, ptr(2.25)}, "float8[]"), []*float64{nil, ptr(2.25)})
	f4 := []float32{0, -1.5, math.MaxFloat32, math.SmallestNonzeroFloat32, float32(math.Inf(1))}
	same(t, roundTrip[[]float32](t, c, float4Array(f4), "float4[]"), f4)
	same(t, roundTrip[[]*float32](t, c, nullFloat4Array{ptr(float32(0.5)), nil}, "float4[]"),
		[]*float32{ptr(float32(0.5)), nil})
	var nan []float64
	if err := c.QueryRow(context.Background(), "SELECT $1::float8[]", float8Array{math.NaN()}).Scan(&nan); err != nil ||
		len(nan) != 1 || !math.IsNaN(nan[0]) {
		t.Fatalf("NaN: %v %v", nan, err)
	}
	same(t, roundTrip[[]bool](t, c, boolArray{true, false, true}, "bool[]"), []bool{true, false, true})
	same(t, roundTrip[[]*bool](t, c, nullBoolArray{nil, ptr(false), ptr(true)}, "bool[]"),
		[]*bool{nil, ptr(false), ptr(true)})
}

func TestArrayParamsTimes(t *testing.T) {
	c := arrayConn(t)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.FixedZone("EST", -5*3600)
	}
	in := []time.Time{
		time.Date(2026, 10, 23, 14, 5, 6, 123456000, time.UTC),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1999, 12, 31, 23, 59, 59, 999999000, time.UTC),
		time.Date(1969, 7, 20, 20, 17, 40, 1000, time.UTC),
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 8, 3, 30, 0, 0, ny),
	}
	check := func(got []time.Time) {
		t.Helper()
		if len(got) != len(in) {
			t.Fatalf("got %d times", len(got))
		}
		for i := range in {
			if !got[i].Equal(in[i]) {
				t.Fatalf("time %d: got %v, want %v", i, got[i], in[i])
			}
		}
	}
	check(roundTrip[[]time.Time](t, c, timestamptzArray(in), "timestamptz[]"))
	got := roundTrip[[]*time.Time](t, c, nullTimestamptzArray{nil, &in[0], nil}, "timestamptz[]")
	if len(got) != 3 || got[0] != nil || got[2] != nil || !got[1].Equal(in[0]) {
		t.Fatalf("nullable times %v", got)
	}
	var truncated time.Time
	err = c.QueryRow(context.Background(), "SELECT ($1::timestamptz[])[1]",
		timestamptzArray{time.Date(2026, 1, 2, 3, 4, 5, 6789, time.UTC)}).Scan(&truncated)
	if err != nil || !truncated.Equal(time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)) {
		t.Fatalf("sub-microsecond time: %v %v", truncated, err)
	}
}

func TestArrayParamsBytea(t *testing.T) {
	c := arrayConn(t)
	in := byteaArray{{0, 1, 2, 255}, nil, {}, []byte(`"quoted" \ ,`)}
	got := roundTrip[[][]byte](t, c, in, "bytea[]")
	if len(got) != len(in) {
		t.Fatalf("got %d values", len(got))
	}
	for i := range in {
		if (got[i] == nil) != (in[i] == nil) || !bytes.Equal(got[i], in[i]) {
			t.Fatalf("value %d: got %v, want %v", i, got[i], in[i])
		}
	}
}

func TestArrayParamsAnyAndUnnest(t *testing.T) {
	c := internalConn(t, `CREATE TYPE mood AS ENUM ('happy', 'sad');
		CREATE TABLE items (s text, v varchar(20), i2 int2, i4 int4, i8 int8, m mood);`)
	ctx := context.Background()
	_, err := c.Exec(ctx, `INSERT INTO items (s, v, i2, i4, i8, m)
		SELECT u.s, u.v, u.i2, u.i4, u.i8, u.m::mood FROM unnest($1::text[], $2::text[], $3::int2[], $4::int4[],
		$5::int8[], $6::text[]) AS u(s, v, i2, i4, i8, m)`,
		nullTextArray{ptr(`a,"b"`), ptr(`ü`), nil}, textArray{"x", "y", "z"},
		nullInt2Array{ptr(int16(1)), nil, ptr(int16(3))}, int4Array{10, 20, 30},
		nullInt8Array{ptr(int64(100)), ptr(int64(200)), nil}, textArray{"happy", "sad", "happy"})
	if err != nil {
		t.Fatal(err)
	}
	count := func(where string, arg any) int {
		t.Helper()
		var n int
		if err := c.QueryRow(ctx, "SELECT count(*) FROM items WHERE "+where+" = ANY($1)", arg).Scan(&n); err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		return n
	}
	same(t, count("s", textArray{`a,"b"`, `ü`, `nope`}), 2)
	same(t, count("v", textArray{"x", "z"}), 2)
	same(t, count("i2", int8Array{1, 3}), 2)
	same(t, count("i4", int8Array{20, 30, 40}), 2)
	same(t, count("i8", int8Array{100}), 1)
	same(t, count("m", textArray{"happy"}), 2)
	same(t, count("s", textArray{}), 0)

	rows, err := c.Query(ctx, `SELECT k.k FROM unnest($1::text[]) AS k(k) ORDER BY k.k COLLATE "C"`,
		textArray{"b", `a"`, "c"})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	same(t, keys, []string{`a"`, "b", "c"})
	var sum int64
	if err := c.QueryRow(ctx, `SELECT sum(k) FROM unnest($1::int8[]) AS k(k)`, int8Array{1, 2, 3}).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	same(t, sum, int64(6))
}

func TestArrayParamsFallBackToPgx(t *testing.T) {
	c := arrayConn(t)
	m := c.Conn().TypeMap()
	numeric := oidOf(t, c, "numeric[]")
	if _, ok := m.PlanEncode(numeric, pgtype.BinaryFormatCode, int8Array{1}).(arrayPlan); ok {
		t.Fatal("keel planned an array type it does not encode")
	}
	var nums []string
	if err := c.QueryRow(context.Background(), "SELECT $1::numeric[]::text[]", int8Array{1, -2}).Scan(&nums); err != nil {
		t.Fatal(err)
	}
	same(t, nums, []string{"1", "-2"})
	var uuids []string
	id := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	if err := c.QueryRow(context.Background(), "SELECT $1::uuid[]::text[]", textArray{id}).Scan(&uuids); err != nil {
		t.Fatal(err)
	}
	same(t, uuids, []string{id})
	if _, ok := m.PlanEncode(pgtype.Int8ArrayOID, pgtype.BinaryFormatCode, float8Array{1}).(arrayPlan); ok {
		t.Fatal("keel planned float elements as integers")
	}
	text, err := m.Encode(pgtype.TextArrayOID, pgtype.TextFormatCode, textArray{"a b", `c"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	same(t, string(text), `{"a b","c\""}`)
	var plain []string
	if err := c.QueryRow(context.Background(), "SELECT $1::text[]", []string{"plain", "slice"}).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	same(t, plain, []string{"plain", "slice"})
}

func TestArrayEncoding(t *testing.T) {
	m := pgtype.NewMap()
	registerArrays(m, nil)
	empty, err := m.Encode(pgtype.TextArrayOID, pgtype.BinaryFormatCode, textArray{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	same(t, empty, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 25})
	one, err := m.Encode(pgtype.Int4ArrayOID, pgtype.BinaryFormatCode, nullInt8Array{ptr(int64(-2)), nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	same(t, one, []byte{0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 23, 0, 0, 0, 2, 0, 0, 0, 1,
		0, 0, 0, 4, 0xff, 0xff, 0xff, 0xfe, 0xff, 0xff, 0xff, 0xff})
}

func TestArrayEncodingAllocs(t *testing.T) {
	m := pgtype.NewMap()
	registerArrays(m, nil)
	strs := make(textArray, 1000)
	ints := make(nullInt8Array, 1000)
	times := make(timestamptzArray, 1000)
	for i := range strs {
		strs[i] = fmt.Sprintf("value %d", i)
		if i%3 != 0 {
			ints[i] = ptr(int64(i) << 40)
		}
		times[i] = time.Unix(int64(i)*1000, 0)
	}
	for _, tc := range []struct {
		oid uint32
		v   any
	}{{pgtype.TextArrayOID, strs}, {pgtype.Int8ArrayOID, ints}, {pgtype.TimestamptzArrayOID, times}} {
		buf, err := m.Encode(tc.oid, pgtype.BinaryFormatCode, tc.v, nil)
		if err != nil {
			t.Fatal(err)
		}
		allocs := testing.AllocsPerRun(50, func() {
			buf, err = m.Encode(tc.oid, pgtype.BinaryFormatCode, tc.v, buf[:0])
		})
		if err != nil || allocs != 0 {
			t.Fatalf("%T: %v allocations per encode, err %v", tc.v, allocs, err)
		}
	}
}
