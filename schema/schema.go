// Package schema holds the runtime description of keel models. The generator writes these values
// as static literals; application code reads them only through keel.
package schema

import (
	"context"
	"unsafe"
)

// Kind is the Go storage kind of a column, used to choose operators and value handling.
type Kind uint8

const (
	KindOther Kind = iota
	KindString
	KindInt16
	KindInt32
	KindInt64
	KindFloat32
	KindFloat64
	KindBool
	KindBytes
	KindTime
	KindJSON
	KindStringArray
	KindInt64Array
	KindTSVector
)

// ColFlag describes how keel treats a column.
type ColFlag uint16

const (
	FlagPK ColFlag = 1 << iota
	FlagTenant
	FlagVersion
	FlagAutoCreate
	FlagAutoUpdate
	FlagReadOnly
	FlagNoSelect
	FlagNullable
	FlagJSONB
	FlagAutoID
)

// Has reports whether every flag in f2 is set.
func (f ColFlag) Has(f2 ColFlag) bool { return f&f2 == f2 }

// Column is one stored field of a model.
type Column struct {
	Name   string
	Quoted string
	Field  string
	Kind   Kind
	Flags  ColFlag
}

// RelKind is the shape of a relation.
type RelKind uint8

const (
	BelongsTo RelKind = iota + 1
	HasOne
	HasMany
	ManyToMany
)

// ToMany reports whether the relation loads a slice.
func (k RelKind) ToMany() bool { return k == HasMany || k == ManyToMany }

// Relation links a model to another model.
type Relation struct {
	Name   string
	Kind   RelKind
	Target *Table
	Local  []int32
	Remote []int32

	JoinTable   string
	JoinLocal   []string
	JoinRemote  []string
	JoinQuoted  string
	JoinLocalQ  []string
	JoinRemoteQ []string
}

// Hook is a model hook trampoline written by the generator.
type Hook func(ctx context.Context, row unsafe.Pointer) error

// Table describes one model.
type Table struct {
	Name   string
	Quoted string
	Model  string

	Columns   []Column
	Relations []Relation

	PK        []int32
	Tenant    []int32
	Select    []int32
	Insert    []int32
	Update    []int32
	Returning []int32

	Version    int32
	AutoID     int32
	AutoCreate int32
	AutoUpdate int32
	IDPrefix   string

	New    func() unsafe.Pointer
	NewN   func(n int) unsafe.Pointer
	Size   uintptr
	Ptr    func(row unsafe.Pointer, col int32) any
	Value  func(row unsafe.Pointer, col int32) any
	Attach func(row unsafe.Pointer, rel int32, child unsafe.Pointer)

	BeforeCreate Hook
	BeforeUpdate Hook
	BeforeSave   Hook
	BeforeDelete Hook
	AfterLoad    Hook
}

// Column returns the index of the column named name, or -1.
func (t *Table) Column(name string) int32 {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return int32(i)
		}
	}
	return -1
}

// Relation returns the index of the relation named name, or -1.
func (t *Table) Relation(name string) int32 {
	for i := range t.Relations {
		if t.Relations[i].Name == name {
			return int32(i)
		}
	}
	return -1
}

// HasTenant reports whether the table has tenant columns.
func (t *Table) HasTenant() bool { return len(t.Tenant) > 0 }

// Derive fills the precomputed column sets and special column indexes from the columns'
// flags. The generator writes the same values as literals; Derive exists for hand-written tables
// and to check the generator.
func Derive(t *Table) {
	t.PK, t.Tenant, t.Select, t.Insert, t.Update, t.Returning = nil, nil, nil, nil, nil, nil
	t.Version, t.AutoID, t.AutoCreate, t.AutoUpdate = -1, -1, -1, -1
	for i := range t.Columns {
		c := &t.Columns[i]
		idx := int32(i)
		if c.Quoted == "" {
			c.Quoted = Quote(c.Name)
		}
		if c.Flags.Has(FlagPK) {
			t.PK = append(t.PK, idx)
		}
		if c.Flags.Has(FlagTenant) {
			t.Tenant = append(t.Tenant, idx)
		}
		if c.Flags.Has(FlagVersion) {
			t.Version = idx
		}
		if c.Flags.Has(FlagAutoID) {
			t.AutoID = idx
		}
		if c.Flags.Has(FlagAutoCreate) {
			t.AutoCreate = idx
		}
		if c.Flags.Has(FlagAutoUpdate) {
			t.AutoUpdate = idx
		}
		if !c.Flags.Has(FlagNoSelect) {
			t.Select = append(t.Select, idx)
		}
		if c.Flags.Has(FlagReadOnly) {
			if !c.Flags.Has(FlagNoSelect) {
				t.Returning = append(t.Returning, idx)
			}
			continue
		}
		t.Insert = append(t.Insert, idx)
		if !c.Flags.Has(FlagPK) && !c.Flags.Has(FlagTenant) && !c.Flags.Has(FlagAutoCreate) &&
			!c.Flags.Has(FlagVersion) {
			t.Update = append(t.Update, idx)
		}
	}
	if t.Quoted == "" {
		t.Quoted = Quote(t.Name)
	}
	for i := range t.Relations {
		r := &t.Relations[i]
		if r.Kind != ManyToMany {
			continue
		}
		r.JoinQuoted = Quote(r.JoinTable)
		r.JoinLocalQ = quoteAll(r.JoinLocal)
		r.JoinRemoteQ = quoteAll(r.JoinRemote)
	}
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = Quote(n)
	}
	return out
}

// Quote returns name as a quoted SQL identifier.
func Quote(name string) string {
	b := make([]byte, 0, len(name)+2)
	b = append(b, '"')
	for i := 0; i < len(name); i++ {
		if name[i] == '"' {
			b = append(b, '"')
		}
		b = append(b, name[i])
	}
	b = append(b, '"')
	return string(b)
}
