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
	FlagNullZero
	FlagComputed
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

	// Label is derived from the field's name, or set by the tag's label= option.
	Label string
	// Doc is the field's doc comment.
	Doc string
	// Enum lists the constants of the column's named Go type, in declaration order.
	Enum []EnumValue
	// Report is how reports present the column; nil when the model is not reportable.
	Report *ReportInfo
	// TSConfig is the text search configuration a tsvector column is searched with by default,
	// from the tag's tsvector=<config> option; empty means the database's default.
	TSConfig string
}

// Stored reports whether the column is a column of the table, as opposed to a computed field that
// queries fill with an expression.
func (c *Column) Stored() bool { return !c.Flags.Has(FlagComputed) }

// MayBeNull reports whether the column can hold NULL: its Go type is nullable (a pointer, a slice,
// a Null… scanner) or it is nullzero, which stores the zero value as NULL.
func (c *Column) MayBeNull() bool { return c.Flags&(FlagNullable|FlagNullZero) != 0 }

// RelKind is the shape of a relation.
type RelKind uint8

const (
	BelongsTo RelKind = iota + 1
	HasOne
	HasMany
	ManyToMany
	Pick
)

// ToMany reports whether the relation loads a slice.
func (k RelKind) ToMany() bool { return k == HasMany || k == ManyToMany }

// Relation links a model to another model. A Pick relation walks Path, one relation index per
// hop starting from the parent's relations, and picks the first row in Order (the last when Last
// is set); Local holds the parent's key, which the first hop starts from.
type Relation struct {
	Name   string
	Alias  string
	Kind   RelKind
	Target *Table
	Local  []int32
	Remote []int32

	Path  []int32
	Order []PickKey
	Last  bool

	// Label is derived from the field's name, or set by the tag's label= option.
	Label string
	// Doc is the field's doc comment.
	Doc string
	// NoReport hides the relation from reports.
	NoReport bool

	JoinTable   string
	JoinAlias   string
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
	Alias  string
	Model  string

	// Label and Plural are derived from the model's name, or set by the tag's label= and plural=
	// options.
	Label  string
	Plural string
	// Doc is the model's doc comment.
	Doc string
	// Report marks the model as a report's starting entity; nil when it is not reportable.
	Report *TableReport

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
		if c.Flags.Has(FlagComputed) {
			continue
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
	if t.Alias == "" {
		t.Alias = AliasFor(t.Name)
	}
	for i := range t.Relations {
		r := &t.Relations[i]
		if r.Alias == "" {
			r.Alias = AliasFor(Snake(r.Name))
		}
		if r.Kind != ManyToMany {
			continue
		}
		if r.JoinAlias == "" {
			r.JoinAlias = AliasFor(r.JoinTable)
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

// AliasFor derives a short alias from a snake_case name: the first letter of each word
// (shipment_moves → sm). An alias that is a reserved word gets a trailing underscore.
func AliasFor(name string) string {
	b := make([]byte, 0, 4)
	start := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' {
			start = true
			continue
		}
		if start {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if (c >= 'a' && c <= 'z') || (len(b) > 0 && c >= '0' && c <= '9') {
				b = append(b, c)
			}
			start = false
		}
	}
	if len(b) == 0 {
		b = append(b, 't')
	}
	a := string(b)
	if reserved[a] {
		a += "_"
	}
	return a
}

// Snake turns a Go identifier into snake_case (BillTo → bill_to, HTTPServer → http_server).
func Snake(name string) string {
	b := make([]byte, 0, len(name)+4)
	for i := 0; i < len(name); i++ {
		c := name[i]
		upper := c >= 'A' && c <= 'Z'
		if upper {
			prevLower := i > 0 && name[i-1] >= 'a' && name[i-1] <= 'z'
			nextLower := i+1 < len(name) && name[i+1] >= 'a' && name[i+1] <= 'z'
			prevUpper := i > 0 && name[i-1] >= 'A' && name[i-1] <= 'Z'
			if i > 0 && (prevLower || (prevUpper && nextLower)) {
				b = append(b, '_')
			}
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	return string(b)
}

// reserved holds the PostgreSQL reserved key words short enough to be derived aliases.
var reserved = map[string]bool{
	"all": true, "and": true, "any": true, "as": true, "asc": true, "by": true, "do": true,
	"end": true, "for": true, "in": true, "is": true, "not": true, "of": true, "off": true,
	"on": true, "or": true, "to": true, "add": true, "if": true, "at": true, "set": true,
}
