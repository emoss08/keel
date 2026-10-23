// Package gen writes the typed data-access packages for keel models.
package gen

import (
	"fmt"
	"go/types"
	"sort"
	"strings"
	"unicode"

	"github.com/emoss08/keel/schema"
)

const keelPath = "github.com/emoss08/keel"

// pkgModels is one package of models.
type pkgModels struct {
	path   string
	name   string
	pkg    *types.Package
	models []*model

	outPath string
	outName string
	outDir  string
}

// model is one struct embedding keel.Table.
type model struct {
	pkg     *pkgModels
	named   *types.Named
	name    string
	table   string
	plural  string
	columns []*column
	rels    []*relation

	hooks map[string]bool
}

// column is one stored field.
type column struct {
	field    string
	tag      schema.FieldTag
	typ      types.Type
	kind     schema.Kind
	nullable bool
	ptr      bool
	named    *types.Named
	elem     *types.Named
	basic    string
	scanner  bool
	idx      int32
}

// relation is one relation field.
type relation struct {
	named  *types.Named
	field  string
	tag    schema.FieldTag
	target *model
	slice  bool
	ptrEl  bool
	kind   schema.RelKind
	local  []int32
	remote []int32
}

func isKeelTable(t types.Type) bool {
	n, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := n.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == keelPath && obj.Name() == "Table"
}

// readModel reads the model named obj, or returns nil when it is not a model.
func readModel(p *pkgModels, obj *types.TypeName) (*model, error) {
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return nil, nil
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, nil
	}
	m := &model{pkg: p, named: named, name: obj.Name(), hooks: map[string]bool{}}
	isModel := false
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Embedded() && isKeelTable(f.Type()) {
			tag := lookupTag(st.Tag(i), "db")
			tt, err := schema.ParseTableTag(tag)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", p.path, obj.Name(), err)
			}
			m.table = tt.Name
			isModel = true
		}
	}
	if !isModel {
		return nil, nil
	}
	m.plural = plural(m.name)
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Embedded() && isKeelTable(f.Type()) {
			continue
		}
		raw := lookupTag(st.Tag(i), "db")
		ft, ok, err := schema.ParseFieldTag(raw)
		if err != nil {
			return nil, fmt.Errorf("%s.%s.%s: %w", p.path, m.name, f.Name(), err)
		}
		if !ok {
			continue
		}
		if !f.Exported() {
			return nil, fmt.Errorf("%s.%s.%s: stored fields must be exported", p.path, m.name, f.Name())
		}
		if ft.IsRelation() {
			r, err := readRelation(m, f, ft)
			if err != nil {
				return nil, err
			}
			m.rels = append(m.rels, r)
			continue
		}
		c, err := readColumn(m, f, ft)
		if err != nil {
			return nil, err
		}
		c.idx = int32(len(m.columns))
		m.columns = append(m.columns, c)
	}
	ptr := types.NewPointer(named)
	ms := types.NewMethodSet(ptr)
	for _, h := range []string{"BeforeCreate", "BeforeUpdate", "BeforeSave", "BeforeDelete", "AfterLoad"} {
		sel := ms.Lookup(p.pkg, h)
		if sel == nil {
			continue
		}
		sig := sel.Type().(*types.Signature)
		if !isHookSig(sig) {
			return nil, fmt.Errorf("%s.%s.%s must be func(context.Context) error", p.path, m.name, h)
		}
		m.hooks[h] = true
	}
	return m, nil
}

func isHookSig(sig *types.Signature) bool {
	if sig.Params().Len() != 1 || sig.Results().Len() != 1 {
		return false
	}
	pn, ok := sig.Params().At(0).Type().(*types.Named)
	if !ok || pn.Obj().Pkg() == nil || pn.Obj().Pkg().Path() != "context" || pn.Obj().Name() != "Context" {
		return false
	}
	return types.Identical(sig.Results().At(0).Type(), types.Universe.Lookup("error").Type())
}

func readRelation(m *model, f *types.Var, ft schema.FieldTag) (*relation, error) {
	r := &relation{field: f.Name(), tag: ft, kind: ft.Rel}
	t := f.Type()
	if s, ok := t.(*types.Slice); ok {
		r.slice = true
		t = s.Elem()
	}
	if p, ok := t.(*types.Pointer); ok {
		r.ptrEl = true
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok {
		return nil, fmt.Errorf("%s.%s: relation must point to a model", m.name, f.Name())
	}
	r.named = n
	if ft.Rel.ToMany() != r.slice {
		return nil, fmt.Errorf("%s.%s: %s relations need a %s field", m.name, f.Name(),
			relName(ft.Rel), map[bool]string{true: "slice", false: "pointer"}[ft.Rel.ToMany()])
	}
	if !r.slice && !r.ptrEl {
		return nil, fmt.Errorf("%s.%s: to-one relations need a pointer field", m.name, f.Name())
	}
	return r, nil
}

func relName(k schema.RelKind) string {
	switch k {
	case schema.BelongsTo:
		return "belongs"
	case schema.HasOne:
		return "one"
	case schema.HasMany:
		return "many"
	}
	return "m2m"
}

var scannerIface = func() *types.Interface {
	any := types.Universe.Lookup("any").Type()
	errT := types.Universe.Lookup("error").Type()
	sig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(types.NewVar(0, nil, "src", any)),
		types.NewTuple(types.NewVar(0, nil, "", errT)), false)
	fn := types.NewFunc(0, nil, "Scan", sig)
	return types.NewInterfaceType([]*types.Func{fn}, nil).Complete()
}()

func readColumn(m *model, f *types.Var, ft schema.FieldTag) (*column, error) {
	c := &column{field: f.Name(), tag: ft, typ: f.Type()}
	t := f.Type()
	if p, ok := t.(*types.Pointer); ok {
		c.ptr = true
		c.nullable = true
		t = p.Elem()
	}
	if ft.Flags.Has(schema.FlagJSONB) {
		c.kind = schema.KindJSON
		return c, nil
	}
	if n, ok := t.(*types.Named); ok {
		obj := n.Obj()
		if obj.Pkg() != nil && obj.Pkg().Path() == "time" && obj.Name() == "Time" {
			c.kind = schema.KindTime
			return c, nil
		}
		if types.Implements(types.NewPointer(n), scannerIface) {
			c.kind = schema.KindOther
			c.scanner = true
			c.named = n
			if strings.HasPrefix(obj.Name(), "Null") {
				c.nullable = true
			}
			return c, nil
		}
		if _, isBasic := n.Underlying().(*types.Basic); isBasic {
			c.named = n
		}
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		c.basic = u.Name()
		switch u.Kind() {
		case types.String:
			c.kind = schema.KindString
			if ft.TSVector {
				c.kind = schema.KindTSVector
			}
		case types.Int64, types.Int:
			c.kind = schema.KindInt64
			c.basic = "int64"
		case types.Int32:
			c.kind = schema.KindInt32
		case types.Int16:
			c.kind = schema.KindInt16
		case types.Float64:
			c.kind = schema.KindFloat64
		case types.Float32:
			c.kind = schema.KindFloat32
		case types.Bool:
			c.kind = schema.KindBool
		default:
			return nil, fmt.Errorf("%s.%s: unsupported type %s", m.name, f.Name(), f.Type())
		}
		if u.Kind() == types.Int && c.named != nil {
			return nil, fmt.Errorf("%s.%s: use int64, not int, for named integer types", m.name, f.Name())
		}
		if u.Kind() == types.Int {
			return nil, fmt.Errorf("%s.%s: use int64, not int", m.name, f.Name())
		}
	case *types.Slice:
		if c.ptr {
			return nil, fmt.Errorf("%s.%s: pointer to slice is not supported", m.name, f.Name())
		}
		c.nullable = true
		eb, ok := u.Elem().Underlying().(*types.Basic)
		if !ok {
			return nil, fmt.Errorf("%s.%s: slices of %s need the jsonb option", m.name, f.Name(), u.Elem())
		}
		if en, ok := u.Elem().(*types.Named); ok {
			c.elem = en
		}
		switch eb.Kind() {
		case types.Byte:
			c.kind = schema.KindBytes
		case types.String:
			c.kind = schema.KindStringArray
		case types.Int64:
			c.kind = schema.KindInt64Array
		default:
			return nil, fmt.Errorf("%s.%s: unsupported slice type %s", m.name, f.Name(), f.Type())
		}
	default:
		return nil, fmt.Errorf("%s.%s: %s needs the jsonb option", m.name, f.Name(), f.Type())
	}
	return c, nil
}

func lookupTag(tag, key string) string {
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		value := tag[1:i]
		tag = tag[i+1:]
		if name == key {
			return value
		}
	}
	return ""
}

// plural turns a model name into the name of its table handle.
func plural(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "s"), strings.HasSuffix(lower, "x"), strings.HasSuffix(lower, "z"),
		strings.HasSuffix(lower, "ch"), strings.HasSuffix(lower, "sh"):
		return name + "es"
	case strings.HasSuffix(lower, "y") && len(lower) > 1 && !strings.ContainsRune("aeiou", rune(lower[len(lower)-2])):
		return name[:len(name)-1] + "ies"
	}
	return name + "s"
}

// resolve links relations to their target models and computes their keys.
func resolve(all map[*types.Named]*model, models []*model) error {
	for _, m := range models {
		if err := checkColumns(m); err != nil {
			return err
		}
	}
	for _, m := range models {
		for _, r := range m.rels {
			n := relTarget(m, r)
			target, ok := all[n]
			if !ok {
				return fmt.Errorf("%s.%s: %s is not a model in the generated packages", m.name, r.field, n)
			}
			r.target = target
			switch r.kind {
			case schema.BelongsTo:
				fk := m.column(r.tag.RelKey)
				if fk == nil {
					return fmt.Errorf("%s.%s: %s has no column %s", m.name, r.field, m.name, r.tag.RelKey)
				}
				pk, err := target.singleKey()
				if err != nil {
					return fmt.Errorf("%s.%s: %w", m.name, r.field, err)
				}
				r.local, r.remote = []int32{fk.idx}, []int32{pk.idx}
			case schema.HasOne, schema.HasMany:
				fk := target.column(r.tag.RelKey)
				if fk == nil {
					return fmt.Errorf("%s.%s: %s has no column %s", m.name, r.field, target.name, r.tag.RelKey)
				}
				pk, err := m.singleKey()
				if err != nil {
					return fmt.Errorf("%s.%s: %w", m.name, r.field, err)
				}
				r.local, r.remote = []int32{pk.idx}, []int32{fk.idx}
			case schema.ManyToMany:
				pk, err := m.singleKey()
				if err != nil {
					return fmt.Errorf("%s.%s: %w", m.name, r.field, err)
				}
				tpk, err := target.singleKey()
				if err != nil {
					return fmt.Errorf("%s.%s: %w", m.name, r.field, err)
				}
				r.local, r.remote = []int32{pk.idx}, []int32{tpk.idx}
			}
		}
	}
	return nil
}

func relTarget(_ *model, r *relation) *types.Named { return r.named }

func checkColumns(m *model) error {
	seen := map[string]bool{}
	special := map[schema.ColFlag]string{}
	for _, c := range m.columns {
		if seen[c.tag.Name] {
			return fmt.Errorf("%s: column %s is declared twice", m.name, c.tag.Name)
		}
		seen[c.tag.Name] = true
		for _, f := range []schema.ColFlag{schema.FlagVersion, schema.FlagAutoID, schema.FlagAutoCreate,
			schema.FlagAutoUpdate} {
			if c.tag.Flags.Has(f) {
				if prev, ok := special[f]; ok {
					return fmt.Errorf("%s: %s and %s both carry the same option", m.name, prev, c.field)
				}
				special[f] = c.field
			}
		}
		if c.tag.Flags.Has(schema.FlagVersion) && (c.kind != schema.KindInt64 || c.ptr) {
			return fmt.Errorf("%s.%s: version columns must be int64", m.name, c.field)
		}
		if c.tag.Flags.Has(schema.FlagAutoID) && (c.kind != schema.KindString || c.ptr) {
			return fmt.Errorf("%s.%s: autoid columns must be strings", m.name, c.field)
		}
		for _, f := range []schema.ColFlag{schema.FlagAutoCreate, schema.FlagAutoUpdate} {
			if c.tag.Flags.Has(f) && c.kind != schema.KindInt64 && c.kind != schema.KindTime {
				return fmt.Errorf("%s.%s: timestamp columns must be int64 (Unix seconds) or time.Time",
					m.name, c.field)
			}
		}
		if c.tag.Flags.Has(schema.FlagAutoCreate) && c.named != nil {
			return fmt.Errorf("%s.%s: timestamp columns must be int64 or time.Time", m.name, c.field)
		}
		if c.tag.Flags.Has(schema.FlagAutoUpdate) && c.named != nil {
			return fmt.Errorf("%s.%s: timestamp columns must be int64 or time.Time", m.name, c.field)
		}
	}
	return nil
}

func (m *model) column(name string) *column {
	for _, c := range m.columns {
		if c.tag.Name == name {
			return c
		}
	}
	return nil
}

func (m *model) singleKey() (*column, error) {
	var keys []*column
	for _, c := range m.columns {
		if c.tag.Flags.Has(schema.FlagPK) && !c.tag.Flags.Has(schema.FlagTenant) {
			keys = append(keys, c)
		}
	}
	if len(keys) != 1 {
		return nil, fmt.Errorf("%s needs exactly one primary key column besides its tenant columns", m.name)
	}
	return keys[0], nil
}

// derived computes the precomputed column sets the same way schema.Derive does.
func (m *model) derived() *schema.Table {
	t := &schema.Table{Name: m.table}
	for _, c := range m.columns {
		flags := c.tag.Flags
		if c.nullable {
			flags |= schema.FlagNullable
		}
		t.Columns = append(t.Columns, schema.Column{Name: c.tag.Name, Kind: c.kind, Flags: flags})
	}
	schema.Derive(t)
	return t
}

func sortedPkgs(m map[string]*pkgModels) []*pkgModels {
	out := make([]*pkgModels, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func exportedName(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
