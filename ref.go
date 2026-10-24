package keel

import (
	"reflect"
	"sync"

	"github.com/emoss08/keel/schema"
)

// Table marks a struct as a keel model. Its `keel` tag names the table:
//
//	type Shipment struct {
//	    keel.Table `keel:"shipments"`
//	    ...
//	}
type Table struct{}

// Ref is one reference to a table inside statements. Generated code builds the base Ref of every
// model statically; relation paths and aliases derive further Refs on first use.
type Ref struct {
	Table *schema.Table

	parent   *Ref
	rel      int32
	alias    string
	numbered bool
}

// relation returns the relation that leads from the parent to r.
func (r *Ref) relation() *schema.Relation {
	return &r.parent.Table.Relations[r.rel]
}

func (r *Ref) isPath() bool { return r.parent != nil }

func (r *Ref) name() string {
	if r.alias != "" {
		return r.Table.Name + " as " + r.alias
	}
	if r.parent != nil {
		return r.parent.name() + "." + r.parent.Table.Relations[r.rel].Name
	}
	return r.Table.Name
}

// memoKey identifies a memoised value: the Ref of relation rel of ref (rel >= 0), the Ref of
// ref under alias (rel -1), or a handle of type typ built over ref (rel -2). Refs are memoised
// apart from handles, so every handle type built over one path or alias shares its Ref.
type memoKey struct {
	ref   *Ref
	rel   int32
	alias string
	typ   reflect.Type
}

var memo = struct {
	sync.RWMutex
	m map[memoKey]any
}{m: make(map[memoKey]any)}

// Path returns the handle for relation rel of the table referenced by parent, building it with
// build on first use. Generated relation methods call it. Every handle type built for one relation
// of one parent references the same Ref, so their columns resolve to the same join.
func Path[H any](parent *Ref, rel int32, build func(*Ref) H) H {
	ref := pathRef(parent, rel)
	return memoized(memoKey{ref: ref, rel: -2, typ: reflect.TypeFor[H]()}, func() H { return build(ref) })
}

// pathRef returns the Ref of relation rel of the table referenced by parent, the same one each
// time.
func pathRef(parent *Ref, rel int32) *Ref {
	return memoized(memoKey{ref: parent, rel: rel}, func() *Ref {
		target := parent.Table.Relations[rel].Target
		if target == nil {
			panic("keel: relation " + parent.Table.Relations[rel].Name + " of " + parent.Table.Name +
				" has no target table; is its generated package imported?")
		}
		return &Ref{Table: target, parent: parent, rel: rel}
	})
}

// Alias returns a handle that references the same table as ref under its own alias, for
// self-joins and correlated subqueries. Generated As methods call it. Aliases are memoised, so
// alias names should be constants.
func Alias[H any](ref *Ref, alias string, build func(*Ref) H) H {
	if !validAlias(alias) {
		panic("keel: alias " + alias + " must match [a-z_][a-z0-9_]*")
	}
	aref := memoized(memoKey{ref: ref, rel: -1, alias: alias}, func() *Ref {
		return &Ref{Table: ref.Table, alias: alias}
	})
	return memoized(memoKey{ref: aref, rel: -2, typ: reflect.TypeFor[H]()}, func() H { return build(aref) })
}

// memoized returns the value memoised under key, building it on first use. build must not call
// memoized.
func memoized[H any](key memoKey, build func() H) H {
	memo.RLock()
	v, ok := memo.m[key]
	memo.RUnlock()
	if ok {
		return v.(H)
	}
	memo.Lock()
	defer memo.Unlock()
	if v, ok := memo.m[key]; ok {
		return v.(H)
	}
	h := build()
	memo.m[key] = h
	return h
}

func validAlias(a string) bool {
	if a == "" || (a[0] >= '0' && a[0] <= '9') {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}
