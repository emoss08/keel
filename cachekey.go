package keel

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/fnv"
	"math"
	"reflect"
	"time"

	"github.com/emoss08/keel/schema"
)

// cacheFormat names the layout of result cache keys and entries. It changes whenever either
// changes, or the statements keel renders for a query change in a way its first round trip does
// not show, so processes of different keel versions sharing a cache never read each other's
// entries.
const cacheFormat = "keel.cache.1"

// keyHasher builds a result cache key: a SHA-256 digest of length-prefixed fields, so no two
// different sequences of fields hash the same input.
type keyHasher struct {
	h   hash.Hash
	buf []byte
}

// newKeyHasher returns a hasher that starts with the cache format.
func newKeyHasher() *keyHasher {
	k := &keyHasher{h: sha256.New(), buf: make([]byte, 0, 64)}
	k.str(cacheFormat)
	return k
}

// tag writes a one-byte type tag.
func (k *keyHasher) tag(b byte) {
	k.buf = append(k.buf[:0], b)
	k.h.Write(k.buf)
}

// u64 writes an unsigned integer.
func (k *keyHasher) u64(v uint64) {
	k.buf = binary.AppendUvarint(k.buf[:0], v)
	k.h.Write(k.buf)
}

// i64 writes a signed integer.
func (k *keyHasher) i64(v int64) {
	k.buf = binary.AppendVarint(k.buf[:0], v)
	k.h.Write(k.buf)
}

// str writes a string, prefixed with its length.
func (k *keyHasher) str(s string) {
	k.u64(uint64(len(s)))
	k.h.Write([]byte(s))
}

// bytes writes a byte slice, prefixed with its length.
func (k *keyHasher) bytes(b []byte) {
	k.u64(uint64(len(b)))
	k.h.Write(b)
}

// sum returns the key: the digest in hex.
func (k *keyHasher) sum() string {
	return hex.EncodeToString(k.h.Sum(nil))
}

// cacheKey returns the result cache key of a terminal's first round trip: the cache format, the
// tenant key, the key material Config.Cache.Key returns, the shape of the reads (which of them
// load levels after the first round trip) and every statement's text and arguments. The
// statements of later round trips (relation loads per level) follow from these and the rows they
// return, and the versions of every table read are checked against the entry, so the key need
// not hold them. It reports false when an argument cannot be keyed.
func cacheKey(tenant, extra, shape string, stmts []*stmt) (string, bool) {
	k := newKeyHasher()
	k.str(tenant)
	k.str(extra)
	k.str(shape)
	k.u64(uint64(len(stmts)))
	for _, s := range stmts {
		k.str(s.sql)
		k.u64(uint64(len(s.args)))
		for _, a := range s.args {
			if !k.arg(a) {
				return "", false
			}
		}
	}
	return k.sum(), true
}

// arg writes a bound value so that two values that bind differently never write the same bytes:
// common types by value, a driver.Valuer by the value it returns, other values through reflection
// by type and value. It reports false for a value it cannot key (a function, a channel).
func (k *keyHasher) arg(v any) bool {
	switch x := v.(type) {
	case nil:
		k.tag('n')
	case string:
		k.tag('s')
		k.str(x)
	case []byte:
		if x == nil {
			k.tag('n')
			return true
		}
		k.tag('b')
		k.bytes(x)
	case bool:
		k.tag('t')
		if x {
			k.u64(1)
		} else {
			k.u64(0)
		}
	case int64:
		k.tag('i')
		k.i64(x)
	case int:
		k.tag('i')
		k.i64(int64(x))
	case int32:
		k.tag('i')
		k.i64(int64(x))
	case int16:
		k.tag('i')
		k.i64(int64(x))
	case float64:
		k.tag('f')
		k.u64(math.Float64bits(x))
	case float32:
		k.tag('g')
		k.u64(uint64(math.Float32bits(x)))
	case time.Time:
		k.tag('T')
		k.str(x.Format(time.RFC3339Nano))
	case []string:
		k.tag('S')
		k.u64(uint64(len(x)))
		for _, s := range x {
			k.str(s)
		}
	case []int64:
		k.tag('I')
		k.u64(uint64(len(x)))
		for _, n := range x {
			k.i64(n)
		}
	case []int32:
		k.tag('J')
		k.u64(uint64(len(x)))
		for _, n := range x {
			k.i64(int64(n))
		}
	case driver.Valuer:
		return k.valuer(x)
	default:
		return k.reflected(reflect.ValueOf(v))
	}
	return true
}

// valuer writes a driver.Valuer as its type and the value it returns.
func (k *keyHasher) valuer(x driver.Valuer) bool {
	rv := reflect.ValueOf(x)
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		k.tag('n')
		return true
	}
	dv, err := x.Value()
	if err != nil {
		return false
	}
	k.tag('V')
	k.str(rv.Type().String())
	return k.arg(dv)
}

// reflected writes a value of a type arg has no case for, by its type and its value.
func (k *keyHasher) reflected(rv reflect.Value) bool {
	k.tag('R')
	k.str(rv.Type().String())
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			k.tag('n')
			return true
		}
		return k.arg(rv.Elem().Interface())
	case reflect.String:
		k.str(rv.String())
	case reflect.Bool:
		if rv.Bool() {
			k.u64(1)
		} else {
			k.u64(0)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		k.i64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		k.u64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		k.u64(math.Float64bits(rv.Float()))
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			k.tag('n')
			return true
		}
		k.u64(uint64(rv.Len()))
		for i := range rv.Len() {
			if !k.arg(rv.Index(i).Interface()) {
				return false
			}
		}
	case reflect.Map, reflect.Struct:
		k.str(fmt.Sprintf("%#v", rv.Interface()))
	default:
		return false
	}
	return true
}

// sqlHash is the stable hash of a statement's text an entry keeps for each statement, so a replay
// checks that it feeds each statement its own result.
func sqlHash(sql string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sql))
	return h.Sum64()
}

// readSetKey holds a terminal's readSet on its context.
type readSetKey struct{}

// readSet collects, in order of first use, the names of the tables the statements of one
// terminal read: every render on the terminal's context adds the base table of each query level
// (the query, subqueries, Any and None, CTE bodies, relation loads), every joined relation and
// every table named in FROM or JOIN.
type readSet struct {
	names []string
}

// readSetFrom returns the readSet on ctx, or nil.
func readSetFrom(ctx context.Context) *readSet {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(readSetKey{}).(*readSet)
	return s
}

// add records that a statement reads t. It does nothing on a nil set.
func (s *readSet) add(t *schema.Table) {
	if s == nil || t == nil {
		return
	}
	s.addName(t.Name)
}

// addName records that a statement reads the table name.
func (s *readSet) addName(name string) {
	for _, n := range s.names {
		if n == name {
			return
		}
	}
	s.names = append(s.names, name)
}
