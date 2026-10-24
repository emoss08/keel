package keel

import (
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

type memoKey struct {
	ref   *Ref
	rel   int32
	alias string
}

var memo = struct {
	sync.RWMutex
	m map[memoKey]any
}{m: make(map[memoKey]any)}

// Path returns the handle for relation rel of the table referenced by parent, building it with
// build on first use. Generated relation methods call it.
func Path[H any](parent *Ref, rel int32, build func(*Ref) H) H {
	return memoized(memoKey{ref: parent, rel: rel}, func() H {
		target := parent.Table.Relations[rel].Target
		if target == nil {
			panic("keel: relation " + parent.Table.Relations[rel].Name + " of " + parent.Table.Name +
				" has no target table; is its generated package imported?")
		}
		return build(&Ref{Table: target, parent: parent, rel: rel})
	})
}

// Alias returns a handle that references the same table as ref under its own alias, for
// self-joins and correlated subqueries. Generated As methods call it. Aliases are memoised, so
// alias names should be constants.
func Alias[H any](ref *Ref, alias string, build func(*Ref) H) H {
	if !validAlias(alias) {
		panic("keel: alias " + alias + " must match [a-z_][a-z0-9_]*")
	}
	return memoized(memoKey{ref: ref, rel: -1, alias: alias}, func() H {
		return build(&Ref{Table: ref.Table, alias: alias})
	})
}

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
