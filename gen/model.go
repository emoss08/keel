// Package gen writes the typed data-access packages for keel models.
package gen

import (
	"cmp"
	"fmt"
	"go/token"
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
	pos     token.Pos
	table   string
	plural  string
	columns []*column
	rels    []*relation

	label       string
	pluralLabel string
	doc         string
	report      *schema.TableReport

	hooks map[string]bool
}

// column is one stored field.
type column struct {
	field    string
	pos      token.Pos
	tag      schema.FieldTag
	typ      types.Type
	kind     schema.Kind
	nullable bool
	ptr      bool
	named    *types.Named
	elem     *types.Named
	basic    string
	scanner  bool
	valuePtr bool
	wide     bool
	idx      int32

	zero zeroTest

	label   string
	doc     string
	json    string
	enum    *types.Named
	values  []schema.EnumValue
	format  string
	key     bool
	exposed bool
	report  *schema.ReportInfo
}

// converts reports whether generated code converts the field to its basic type: a named basic
// type other than a scanner, or int, which is read and written as int64.
func (c *column) converts() bool { return (c.named != nil && !c.scanner) || c.wide }

// hasWide reports whether a model of the package has an int field.
func (p *pkgModels) hasWide() bool {
	for _, m := range p.models {
		for _, c := range m.columns {
			if c.wide {
				return true
			}
		}
	}
	return false
}

// relation is one relation field.
type relation struct {
	named  *types.Named
	field  string
	pos    token.Pos
	tag    schema.FieldTag
	target *model
	slice  bool
	ptrEl  bool
	kind   schema.RelKind
	local  []int32
	remote []int32
	path   []int32
	order  []schema.PickKey
	filter []schema.PickFilter

	label string
	doc   string
	json  string
}

// isKeelTable reports whether t is the keel.Table marker.
func isKeelTable(t types.Type) bool {
	n, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := n.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == keelPath && obj.Name() == "Table"
}

// reader reads models with the source, labels and per-type facts of one generation run.
type reader struct {
	src     *source
	labels  *labeler
	enums   map[*types.Named][]schema.EnumValue
	formats map[*types.Named]string
}

// readModel reads the model named obj, or returns nil when it is not a model.
func (rd *reader) readModel(p *pkgModels, obj *types.TypeName) (*model, error) {
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return nil, nil
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, nil
	}
	m := &model{pkg: p, named: named, name: obj.Name(), pos: obj.Pos(), hooks: map[string]bool{}}
	isModel, err := rd.readTable(m, st)
	if err != nil || !isModel {
		return nil, err
	}
	if !obj.Exported() {
		return nil, rd.src.errorf(obj.Pos(), "%s: models must be exported; the generated package refers to them", m.name)
	}
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Embedded() && isKeelTable(f.Type()) {
			continue
		}
		if err := rd.readField(m, f, st.Tag(i)); err != nil {
			return nil, rd.src.errorf(f.Pos(), "%s.%s: %v", m.name, f.Name(), err)
		}
	}
	if err := rd.readHooks(m); err != nil {
		return nil, err
	}
	return m, nil
}

// readTable reads the keel.Table marker of st into m and reports whether st is a model.
func (rd *reader) readTable(m *model, st *types.Struct) (bool, error) {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if !f.Embedded() || !isKeelTable(f.Type()) {
			continue
		}
		tt, err := schema.ParseTableTag(lookupTag(st.Tag(i), "keel"))
		if err != nil {
			return false, rd.src.errorf(f.Pos(), "%s: %v", m.name, err)
		}
		doc, err := rd.src.doc(m.pos, m.name)
		if err != nil {
			return false, err
		}
		m.table, m.doc, m.plural = tt.Name, doc, plural(m.name)
		m.label = cmp.Or(tt.Label, rd.labels.label(m.name))
		m.pluralLabel = cmp.Or(tt.Plural, pluralLabel(m.label))
		if tt.Report {
			m.report = &schema.TableReport{Category: tt.Category}
		}
		return true, nil
	}
	return false, nil
}

// readField reads one field of a model, whose struct tag is tag: a column, a relation, or nothing
// when it is not stored.
func (rd *reader) readField(m *model, f *types.Var, tag string) error {
	ft, ok, err := schema.ParseFieldTag(lookupTag(tag, "keel"))
	if err != nil || !ok {
		return err
	}
	if !f.Exported() {
		return fmt.Errorf("stored fields must be exported")
	}
	doc, err := rd.src.doc(f.Pos(), f.Name())
	if err != nil {
		return err
	}
	label := cmp.Or(ft.Label, rd.labels.label(f.Name()))
	if ft.IsRelation() {
		r, err := readRelation(f, ft)
		if err != nil {
			return err
		}
		r.pos, r.doc, r.label, r.json = f.Pos(), doc, label, jsonName(f.Name(), lookupTag(tag, "json"))
		m.rels = append(m.rels, r)
		return nil
	}
	c, err := readColumn(f, ft)
	if err != nil {
		return err
	}
	c.pos, c.doc, c.label, c.idx = f.Pos(), doc, label, int32(len(m.columns))
	c.json = jsonName(f.Name(), lookupTag(tag, "json"))
	if err := rd.readTypeFacts(c); err != nil {
		return err
	}
	m.columns = append(m.columns, c)
	return nil
}

// readHooks records the model hooks m declares, refusing a hook name with the wrong signature.
func (rd *reader) readHooks(m *model) error {
	ms := types.NewMethodSet(types.NewPointer(m.named))
	for _, h := range []string{"BeforeCreate", "BeforeUpdate", "BeforeSave", "BeforeDelete", "AfterLoad"} {
		sel := ms.Lookup(m.pkg.pkg, h)
		if sel == nil {
			continue
		}
		if !isHookSig(sel.Type().(*types.Signature)) {
			return rd.src.errorf(sel.Obj().Pos(), "%s.%s must be func(context.Context) error", m.name, h)
		}
		m.hooks[h] = true
	}
	return nil
}

// isHookSig reports whether sig is func(context.Context) error.
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

// readRelation reads a relation field.
func readRelation(f *types.Var, ft schema.FieldTag) (*relation, error) {
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
		return nil, fmt.Errorf("relation must point to a model")
	}
	r.named = n
	if ft.Rel.ToMany() != r.slice {
		return nil, fmt.Errorf("%s relations need a %s field", relName(ft.Rel),
			map[bool]string{true: "slice", false: "pointer"}[ft.Rel.ToMany()])
	}
	if !r.slice && !r.ptrEl {
		return nil, fmt.Errorf("to-one relations need a pointer field")
	}
	return r, nil
}

// relName is the tag name of a relation kind.
func relName(k schema.RelKind) string {
	switch k {
	case schema.BelongsTo:
		return "belongs"
	case schema.HasOne:
		return "one"
	case schema.HasMany:
		return "many"
	case schema.Pick:
		return "pick"
	}
	return "m2m"
}

// scannerIface is sql.Scanner.
var scannerIface = func() *types.Interface {
	any := types.Universe.Lookup("any").Type()
	errT := types.Universe.Lookup("error").Type()
	sig := types.NewSignatureType(nil, nil, nil,
		types.NewTuple(types.NewVar(0, nil, "src", any)),
		types.NewTuple(types.NewVar(0, nil, "", errT)), false)
	fn := types.NewFunc(0, nil, "Scan", sig)
	return types.NewInterfaceType([]*types.Func{fn}, nil).Complete()
}()

// readColumn reads a column field: its kind, nullability and Go type.
func readColumn(f *types.Var, ft schema.FieldTag) (*column, error) {
	c := &column{field: f.Name(), tag: ft, typ: f.Type()}
	t := f.Type()
	if p, ok := t.(*types.Pointer); ok {
		c.ptr = true
		c.nullable = true
		t = p.Elem()
	}
	if ft.Flags.Has(schema.FlagJSONB) {
		c.kind = schema.KindJSON
		return c, checkNullZero(c)
	}
	if n, ok := t.(*types.Named); ok {
		obj := n.Obj()
		_, isBasic := n.Underlying().(*types.Basic)
		switch {
		case obj.Pkg() != nil && obj.Pkg().Path() == "time" && obj.Name() == "Time":
			c.kind = schema.KindTime
			return c, checkNullZero(c)
		case isBasic:
			c.named = n
		case types.Implements(types.NewPointer(n), scannerIface):
			c.kind = schema.KindOther
			c.scanner = true
			c.named = n
			c.valuePtr = valuerOnPointer(n)
			if strings.HasPrefix(obj.Name(), "Null") {
				c.nullable = true
			}
			return c, checkNullZero(c)
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
			return nil, fmt.Errorf("unsupported type %s", f.Type())
		}
		if u.Kind() == types.Int {
			c.wide = true
		}
	case *types.Slice:
		if c.ptr {
			return nil, fmt.Errorf("pointer to slice is not supported")
		}
		c.nullable = true
		eb, ok := u.Elem().Underlying().(*types.Basic)
		if !ok {
			return nil, fmt.Errorf("slices of %s need the jsonb option", u.Elem())
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
		case types.Int, types.Int32, types.Int16, types.Float64, types.Float32, types.Bool:
			if c.elem != nil {
				return nil, fmt.Errorf("unsupported slice type %s; use []%s", f.Type(), eb.Name())
			}
			c.kind = schema.KindOther
		default:
			return nil, fmt.Errorf("unsupported slice type %s", f.Type())
		}
	default:
		return nil, fmt.Errorf("%s needs the jsonb option", f.Type())
	}
	return c, checkNullZero(c)
}

// valuerOnPointer reports whether only *n, not n, has a method Value() (driver.Value, error), so
// the generated Value accessor hands the driver a pointer to the field.
func valuerOnPointer(n *types.Named) bool {
	return !hasValuer(types.NewMethodSet(n)) && hasValuer(types.NewMethodSet(types.NewPointer(n)))
}

// hasValuer reports whether ms has a method Value with no parameters returning a value and an
// error.
func hasValuer(ms *types.MethodSet) bool {
	for i := range ms.Len() {
		fn, ok := ms.At(i).Obj().(*types.Func)
		if !ok || fn.Name() != "Value" {
			continue
		}
		sig := fn.Type().(*types.Signature)
		return sig.Params().Len() == 0 && sig.Results().Len() == 2 &&
			types.Identical(sig.Results().At(1).Type(), types.Universe.Lookup("error").Type())
	}
	return false
}

// zeroTest is how generated code tests a nullzero field for its zero value.
type zeroTest uint8

const (
	zeroNone zeroTest = iota
	zeroString
	zeroNumber
	zeroBool
	zeroNil
	zeroMethod
	zeroStruct
)

// resolveZero chooses how generated code tests c for its zero value: a literal for basic types,
// nil for maps, slices, pointers and interfaces, an IsZero() bool method (time.Time, decimals),
// or a comparison with the composite zero value of a comparable struct or array.
func resolveZero(c *column) error {
	if !c.tag.Flags.Has(schema.FlagNullZero) {
		return nil
	}
	t := c.typ
	if hasIsZero(t) {
		c.zero = zeroMethod
		return nil
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		switch {
		case u.Info()&types.IsString != 0:
			c.zero = zeroString
		case u.Info()&types.IsBoolean != 0:
			c.zero = zeroBool
		default:
			c.zero = zeroNumber
		}
	case *types.Map, *types.Slice, *types.Pointer, *types.Interface:
		c.zero = zeroNil
	case *types.Struct, *types.Array:
		if !types.Comparable(t) {
			return fmt.Errorf("nullzero needs a comparable type or an IsZero() bool method; %s has neither", t)
		}
		c.zero = zeroStruct
	default:
		return fmt.Errorf("nullzero does not apply to %s", t)
	}
	return nil
}

// hasIsZero reports whether t or *t has a method IsZero() bool.
func hasIsZero(t types.Type) bool {
	obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(t), true, nil, "IsZero")
	fn, ok := obj.(*types.Func)
	if !ok {
		return false
	}
	sig := fn.Type().(*types.Signature)
	return sig.Params().Len() == 0 && sig.Results().Len() == 1 &&
		types.Identical(sig.Results().At(0).Type(), types.Typ[types.Bool])
}

// checkNullZero refuses nullzero on a column whose Go type already stores nil as NULL: a pointer
// or a Null… scanner.
func checkNullZero(c *column) error {
	if !c.tag.Flags.Has(schema.FlagNullZero) {
		return nil
	}
	switch {
	case c.ptr:
		return fmt.Errorf("nullzero needs a non-pointer field; a pointer already stores nil as NULL")
	case c.scanner && c.nullable:
		return fmt.Errorf("nullzero needs a non-nullable type; %s already stores NULL", c.named.Obj().Name())
	}
	return resolveZero(c)
}

// jsonName returns the name encoding/json gives the field named field whose json tag is tag: the
// tag's name, the field's name when the tag names none (",omitempty"), and "" when the field has
// no json tag or encoding/json skips it ("-").
func jsonName(field, tag string) string {
	if tag == "" || tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field
	}
	return name
}

// lookupTag returns the value of key in a struct tag, as reflect.StructTag.Get does.
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

// column returns the column named name, or nil.
func (m *model) column(name string) *column {
	for _, c := range m.columns {
		if c.tag.Name == name {
			return c
		}
	}
	return nil
}

// singleKey returns the primary key column relations reference by default: the one primary key
// column besides the tenant columns, or, when the key has several, the one named id.
func (m *model) singleKey() (*column, error) {
	var keys []*column
	for _, c := range m.columns {
		if c.tag.Flags.Has(schema.FlagPK) && !c.tag.Flags.Has(schema.FlagTenant) {
			keys = append(keys, c)
		}
	}
	if len(keys) == 1 {
		return keys[0], nil
	}
	for _, c := range keys {
		if c.tag.Name == "id" {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%s needs one primary key column besides its tenant columns, or one named id", m.name)
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

// sortedPkgs returns the packages ordered by import path.
func sortedPkgs(m map[string]*pkgModels) []*pkgModels {
	out := make([]*pkgModels, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// exportedName returns s with its first letter in upper case.
func exportedName(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
