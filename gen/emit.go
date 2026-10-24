package gen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/emoss08/keel/schema"
)

// imports tracks the imports of one generated file.
type imports struct {
	self   string
	byPath map[string]string
	names  map[string]string
}

func newImports(self string) *imports {
	return &imports{self: self, byPath: map[string]string{}, names: map[string]string{}}
}

func (im *imports) add(path, name string) string {
	if path == im.self {
		return ""
	}
	if a, ok := im.byPath[path]; ok {
		return a
	}
	alias := name
	for i := 2; ; i++ {
		if _, taken := im.names[alias]; !taken {
			break
		}
		alias = name + strconv.Itoa(i)
	}
	im.byPath[path] = alias
	im.names[alias] = path
	return alias
}

func (im *imports) qual(path, name, ident string) string {
	a := im.add(path, name)
	if a == "" {
		return ident
	}
	return a + "." + ident
}

func (im *imports) qualifier() types.Qualifier {
	return func(p *types.Package) string { return im.add(p.Path(), p.Name()) }
}

func (im *imports) write(b *bytes.Buffer) {
	paths := make([]string, 0, len(im.byPath))
	for p := range im.byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	b.WriteString("import (\n")
	for _, std := range []bool{true, false} {
		wrote := false
		for _, p := range paths {
			if isStd(p) != std {
				continue
			}
			alias := im.byPath[p]
			if alias == lastSegment(p) {
				fmt.Fprintf(b, "\t%q\n", p)
			} else {
				fmt.Fprintf(b, "\t%s %q\n", alias, p)
			}
			wrote = true
		}
		if std && wrote {
			b.WriteString("\n")
		}
	}
	b.WriteString(")\n\n")
}

func isStd(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".")
}

// lit returns s as a Go string literal, raw when that reads better.
func lit(s string) string {
	if strings.ContainsAny(s, "`\n") {
		return strconv.Quote(s)
	}
	return "`" + s + "`"
}

func lastSegment(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// wrapper is a generated column type for a named Go type.
type wrapper struct {
	named    *types.Named
	name     string
	outPath  string
	outName  string
	base     string
	basic    string
	scanner  bool
	valuePtr bool
}

// emitter writes the files of one generation run.
type emitter struct {
	pkgs     map[string]*pkgModels
	wrappers map[*types.Named]*wrapper
	kinds    *pkgModels
}

func (e *emitter) wrapperFor(c *column) *wrapper {
	if c.named == nil {
		return nil
	}
	return e.wrappers[c.named]
}

// planWrappers decides where each named column type's wrapper lives: with the models of the type's
// package, or in the shared kinds package.
func (e *emitter) planWrappers() {
	e.wrappers = map[*types.Named]*wrapper{}
	for _, p := range sortedPkgs(e.pkgs) {
		for _, m := range p.models {
			for _, c := range m.columns {
				if c.named == nil || e.wrappers[c.named] != nil {
					continue
				}
				obj := c.named.Obj()
				w := &wrapper{named: c.named, scanner: c.scanner, basic: c.basic, valuePtr: c.valuePtr}
				if owner, ok := e.pkgs[obj.Pkg().Path()]; ok {
					w.outPath, w.outName = owner.outPath, owner.outName
					w.name = obj.Name() + "Col"
				} else {
					w.outPath, w.outName = e.kinds.outPath, e.kinds.outName
					w.name = exportedName(obj.Pkg().Name()) + obj.Name() + "Col"
				}
				w.base = baseKind(c)
				e.wrappers[c.named] = w
			}
		}
	}
}

func baseKind(c *column) string {
	if c.scanner {
		return "AnyCol"
	}
	switch c.kind {
	case schema.KindString:
		return "StringCol"
	case schema.KindInt64:
		return "Int64Col"
	case schema.KindInt32:
		return "Int32Col"
	case schema.KindInt16:
		return "Int16Col"
	case schema.KindFloat64, schema.KindFloat32:
		return "Float64Col"
	case schema.KindBool:
		return "BoolCol"
	case schema.KindBytes:
		return "BytesCol"
	case schema.KindTime:
		return "TimeCol"
	case schema.KindJSON:
		return "JSONBCol"
	case schema.KindStringArray:
		return "StringArrayCol"
	case schema.KindInt64Array:
		return "Int64ArrayCol"
	case schema.KindTSVector:
		return "TSVectorCol"
	}
	return "AnyCol"
}

func kindName(k schema.Kind) string {
	return map[schema.Kind]string{
		schema.KindOther:       "KindOther",
		schema.KindString:      "KindString",
		schema.KindInt16:       "KindInt16",
		schema.KindInt32:       "KindInt32",
		schema.KindInt64:       "KindInt64",
		schema.KindFloat32:     "KindFloat32",
		schema.KindFloat64:     "KindFloat64",
		schema.KindBool:        "KindBool",
		schema.KindBytes:       "KindBytes",
		schema.KindTime:        "KindTime",
		schema.KindJSON:        "KindJSON",
		schema.KindStringArray: "KindStringArray",
		schema.KindInt64Array:  "KindInt64Array",
		schema.KindTSVector:    "KindTSVector",
	}[k]
}

func flagNames(f schema.ColFlag) string {
	names := []struct {
		f schema.ColFlag
		n string
	}{
		{schema.FlagPK, "FlagPK"}, {schema.FlagTenant, "FlagTenant"}, {schema.FlagVersion, "FlagVersion"},
		{schema.FlagAutoCreate, "FlagAutoCreate"}, {schema.FlagAutoUpdate, "FlagAutoUpdate"},
		{schema.FlagReadOnly, "FlagReadOnly"}, {schema.FlagNoSelect, "FlagNoSelect"},
		{schema.FlagNullable, "FlagNullable"}, {schema.FlagJSONB, "FlagJSONB"}, {schema.FlagAutoID, "FlagAutoID"},
		{schema.FlagNullZero, "FlagNullZero"}, {schema.FlagComputed, "FlagComputed"},
	}
	var parts []string
	for _, n := range names {
		if f.Has(n.f) {
			parts = append(parts, "schema."+n.n)
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, " | ")
}

func ints(xs []int32) string {
	if len(xs) == 0 {
		return "nil"
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(int(x))
	}
	return "[]int32{" + strings.Join(parts, ", ") + "}"
}

func strs(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = lit(x)
	}
	return "[]string{" + strings.Join(parts, ", ") + "}"
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// emitPackage writes the generated file of one model package.
func (e *emitter) emitPackage(p *pkgModels) ([]byte, error) {
	im := newImports(p.outPath)
	keel := im.add(keelPath, "keel")
	sch := im.add(keelPath+"/schema", "schema")
	unsafePkg := im.add("unsafe", "unsafe")
	q := im.qualifier()

	var body bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&body, format, args...) }
	sh := newShared(sch, p)

	for _, m := range p.models {
		model := types.TypeString(m.named, q)
		tblVar := lowerFirst(m.plural) + "Table"
		refVar := lowerFirst(m.plural) + "Ref"
		handle := m.plural + "Table"
		build := "Build" + m.plural
		d := m.derived()

		w("// %s is the schema of %s.\n", tblVar, model)
		w("var %s = %s.Table{\n", tblVar, sch)
		w("\tName: %q,\n\tQuoted: %s,\n\tAlias: %q,\n\tModel: %q,\n", m.table, lit(schema.Quote(m.table)),
			schema.AliasFor(m.table), m.pkg.name+"."+m.name)
		body.WriteString(sh.tableMeta(m))
		w("\tColumns: []%s.Column{\n", sch)
		for i, c := range m.columns {
			w("\t\t{Name: %q, Quoted: %s, Field: %q, Kind: %s.%s, Flags: %s%s},\n",
				c.tag.Name, lit(schema.Quote(c.tag.Name)), c.field, sch, kindName(c.kind),
				strings.ReplaceAll(flagNames(d.Columns[i].Flags), "schema.", sch+"."), sh.columnMeta(c))
		}
		w("\t},\n")
		if len(m.rels) > 0 {
			w("\tRelations: []%s.Relation{\n", sch)
			for _, r := range m.rels {
				w("\t\t{Name: %q, Alias: %q, Kind: %s.%s, Local: %s, Remote: %s", r.field,
					schema.AliasFor(schema.Snake(r.field)), sch, relKindName(r.kind), ints(r.local), ints(r.remote))
				if r.kind == schema.ManyToMany {
					w(", JoinAlias: %q", schema.AliasFor(r.tag.JoinTable))
					w(", JoinTable: %q, JoinLocal: %s, JoinRemote: %s, JoinQuoted: %s, JoinLocalQ: %s, JoinRemoteQ: %s",
						r.tag.JoinTable, strs([]string{r.tag.JoinLocal}), strs([]string{r.tag.JoinRemote}),
						lit(schema.Quote(r.tag.JoinTable)), strs([]string{schema.Quote(r.tag.JoinLocal)}),
						strs([]string{schema.Quote(r.tag.JoinRemote)}))
				}
				w("%s},\n", sh.relationMeta(r))
			}
			w("\t},\n")
		}
		w("\tPK: %s,\n\tTenant: %s,\n\tSelect: %s,\n\tInsert: %s,\n\tUpdate: %s,\n\tReturning: %s,\n",
			ints(d.PK), ints(d.Tenant), ints(d.Select), ints(d.Insert), ints(d.Update), ints(d.Returning))
		idPrefix := ""
		for _, c := range m.columns {
			if c.tag.Flags.Has(schema.FlagAutoID) {
				idPrefix = c.tag.IDPrefix
			}
		}
		w("\tVersion: %d,\n\tAutoID: %d,\n\tAutoCreate: %d,\n\tAutoUpdate: %d,\n\tIDPrefix: %q,\n",
			d.Version, d.AutoID, d.AutoCreate, d.AutoUpdate, idPrefix)
		w("\tNew: new%s,\n\tNewN: newN%s,\n\tSize: %s.Sizeof(%s{}),\n\tPtr: ptr%s,\n\tValue: value%s,\n",
			m.name, m.name, unsafePkg, model, m.name, m.name)
		if len(m.rels) > 0 {
			w("\tAttach: attach%s,\n", m.name)
		}
		for _, h := range []string{"BeforeCreate", "BeforeUpdate", "BeforeSave", "BeforeDelete", "AfterLoad"} {
			if m.hooks[h] {
				w("\t%s: %s%s,\n", h, lowerFirst(h), m.name)
			}
		}
		w("}\n\n")

		w("var %s = %s.Ref{Table: &%s}\n\n", refVar, keel, tblVar)

		w("// %s is the table of %s.\n", handle, model)
		w("type %s struct {\n\t%s.Handle[%s]\n", handle, keel, model)
		for _, c := range m.columns {
			w("\t%s %s\n", c.field, e.colType(im, c))
		}
		w("}\n\n")

		w("// %s references the %s table.\n", m.plural, m.table)
		w("var %s = &%s{\n\tHandle: %s.Handle[%s]{Ref: &%s},\n", m.plural, handle, keel, model, refVar)
		for _, c := range m.columns {
			w("\t%s: %s,\n", c.field, e.colLit(im, c, "&"+refVar))
		}
		w("}\n\n")

		w("// %s builds the handle of %s for ref. Generated relation and alias methods call it.\n",
			build, m.table)
		w("func %s(r *%s.Ref) *%s {\n\treturn &%s{\n\t\tHandle: %s.Handle[%s]{Ref: r},\n", build, keel,
			handle, handle, keel, model)
		for _, c := range m.columns {
			w("\t\t%s: %s,\n", c.field, e.colLit(im, c, "r"))
		}
		w("\t}\n}\n\n")

		w("// %sSchema returns the schema of %s.\n", m.plural, m.table)
		w("func %sSchema() *%s.Table { return &%s }\n\n", m.plural, sch, tblVar)

		w("// As returns a handle that references %s under its own alias.\n", m.table)
		w("func (t *%s) As(alias string) *%s { return %s.Alias(t.Ref, alias, %s) }\n\n", handle, handle,
			keel, build)

		for ri, r := range m.rels {
			tp := r.target.pkg
			th := r.target.plural + "Table"
			tb := "Build" + r.target.plural
			if tp.outPath != p.outPath {
				a := im.add(tp.outPath, tp.outName)
				th = a + "." + th
				tb = a + "." + tb
			}
			w("// %s is the %s relation.\n", r.field, relName(r.kind))
			w("func (t *%s) %s() *%s { return %s.Path(t.Ref, %d, %s) }\n\n", handle, r.field, th, keel, ri, tb)
		}

		if pk, err := m.singleKey(); err == nil && !pk.ptr {
			pt := types.TypeString(pk.typ, q)
			ctxPkg := im.add("context", "context")
			w("// Get returns the %s row with the given key, or keel.ErrNotFound.\n", m.table)
			w("func (t *%s) Get(ctx %s.Context, db *%s.DB, %s %s) (*%s, error) {\n", handle, ctxPkg, keel,
				"key", pt, model)
			w("\treturn t.Where(t.%s.Eq(key)).One(ctx, db)\n}\n\n", pk.field)
		}

		w("func new%s() %s.Pointer { return %s.Pointer(new(%s)) }\n\n", m.name, unsafePkg, unsafePkg, model)
		w("func newN%s(n int) %s.Pointer { return %s.Pointer(%s.SliceData(make([]%s, n))) }\n\n",
			m.name, unsafePkg, unsafePkg, unsafePkg, model)

		w("func ptr%s(p %s.Pointer, c int32) any {\n\tr := (*%s)(p)\n\tswitch c {\n", m.name, unsafePkg, model)
		for _, c := range m.columns {
			w("\tcase %d:\n\t\treturn %s\n", c.idx, e.ptrExpr(im, c, unsafePkg))
		}
		w("\t}\n\treturn nil\n}\n\n")

		w("func value%s(p %s.Pointer, c int32) any {\n\tr := (*%s)(p)\n\tswitch c {\n", m.name, unsafePkg, model)
		for _, c := range m.columns {
			w("\tcase %d:\n%s", c.idx, e.valueStmt(im, c, keel))
		}
		w("\t}\n\treturn nil\n}\n\n")

		if len(m.rels) > 0 {
			w("func attach%s(p %s.Pointer, rel int32, child %s.Pointer) {\n\tr := (*%s)(p)\n\tswitch rel {\n",
				m.name, unsafePkg, unsafePkg, model)
			for ri, r := range m.rels {
				tm := types.TypeString(r.named, q)
				switch {
				case r.slice && r.ptrEl:
					w("\tcase %d:\n\t\tr.%s = append(r.%s, (*%s)(child))\n", ri, r.field, r.field, tm)
				case r.slice:
					w("\tcase %d:\n\t\tr.%s = append(r.%s, *(*%s)(child))\n", ri, r.field, r.field, tm)
				default:
					w("\tcase %d:\n\t\tr.%s = (*%s)(child)\n", ri, r.field, tm)
				}
			}
			w("\t}\n}\n\n")
		}

		for _, h := range []string{"BeforeCreate", "BeforeUpdate", "BeforeSave", "BeforeDelete", "AfterLoad"} {
			if !m.hooks[h] {
				continue
			}
			ctxPkg := im.add("context", "context")
			w("func %s%s(ctx %s.Context, p %s.Pointer) error { return (*%s)(p).%s(ctx) }\n\n",
				lowerFirst(h), m.name, ctxPkg, unsafePkg, model, h)
		}
	}

	var inits []string
	for _, m := range p.models {
		for ri, r := range m.rels {
			tp := r.target.pkg
			fn := r.target.plural + "Schema()"
			if tp.outPath != p.outPath {
				fn = im.add(tp.outPath, tp.outName) + "." + fn
			}
			inits = append(inits, fmt.Sprintf("\t%sTable.Relations[%d].Target = %s\n",
				lowerFirst(m.plural), ri, fn))
		}
	}
	if len(inits) > 0 {
		w("func init() {\n")
		for _, s := range inits {
			body.WriteString(s)
		}
		w("}\n\n")
	}

	if p.hasWide() {
		w("// intIs64 fails the build where int is not 64 bits: int fields are read through *int64.\n")
		w("const intIs64 = %s.Sizeof(int(0)) - 8\n\n", unsafePkg)
	}

	sh.write(&body)
	for _, wr := range e.sortedWrappers() {
		if wr.outPath == p.outPath {
			e.emitWrapper(&body, im, wr)
		}
	}
	return finish(p.outName, im, body.Bytes())
}

func (e *emitter) sortedWrappers() []*wrapper {
	out := make([]*wrapper, 0, len(e.wrappers))
	for _, w := range e.wrappers {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].outPath != out[j].outPath {
			return out[i].outPath < out[j].outPath
		}
		return out[i].name < out[j].name
	})
	return out
}

// emitKinds writes the shared kinds package, or nil when it holds nothing.
func (e *emitter) emitKinds() ([]byte, error) {
	im := newImports(e.kinds.outPath)
	im.add(keelPath, "keel")
	var body bytes.Buffer
	n := 0
	for _, wr := range e.sortedWrappers() {
		if wr.outPath == e.kinds.outPath {
			e.emitWrapper(&body, im, wr)
			n++
		}
	}
	if n == 0 {
		return nil, nil
	}
	return finish(e.kinds.outName, im, body.Bytes())
}

// emitTables writes the package that lists every generated table in name order.
func (e *emitter) emitTables(outPath, outName string) ([]byte, error) {
	im := newImports(outPath)
	sch := im.add(keelPath+"/schema", "schema")
	type entry struct{ table, call string }
	var entries []entry
	for _, p := range sortedPkgs(e.pkgs) {
		pkg := im.add(p.outPath, p.outName)
		for _, m := range p.models {
			entries = append(entries, entry{table: m.table, call: pkg + "." + m.plural + "Schema()"})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].table < entries[j].table })
	var body bytes.Buffer
	body.WriteString("// All returns every table keel gen generated, in name order.\n")
	fmt.Fprintf(&body, "func All() []*%s.Table {\n\treturn []*%s.Table{\n", sch, sch)
	for _, en := range entries {
		fmt.Fprintf(&body, "\t\t%s,\n", en.call)
	}
	body.WriteString("\t}\n}\n")
	return finish(outName, im, body.Bytes())
}

func finish(pkgName string, im *imports, body []byte) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("// Code generated by keel gen. DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", pkgName)
	im.write(&b)
	b.Write(body)
	src, err := format.Source(b.Bytes())
	if err != nil {
		return b.Bytes(), fmt.Errorf("format generated %s: %w", pkgName, err)
	}
	return src, nil
}

func (e *emitter) emitWrapper(b *bytes.Buffer, im *imports, wr *wrapper) {
	keel := im.add(keelPath, "keel")
	t := types.TypeString(wr.named, im.qualifier())
	w := func(format string, args ...any) { fmt.Fprintf(b, format, args...) }
	w("// %s is a column of %s values.\n", wr.name, t)
	w("type %s struct{ %s.%s }\n\n", wr.name, keel, wr.base)
	conv := func(v string) string {
		switch {
		case wr.valuePtr:
			return "&" + v
		case wr.scanner:
			return v
		case wr.base == "Float64Col":
			return "float64(" + v + ")"
		}
		return wr.basic + "(" + v + ")"
	}
	for _, op := range []string{"Eq", "Ne", "Gt", "Gte", "Lt", "Lte", "IsDistinctFrom"} {
		if wr.base == "BoolCol" && op != "Eq" {
			continue
		}
		w("func (c %s) %s(v %s) %s.Pred { return c.%s.%s(%s) }\n", wr.name, op, t, keel, wr.base, op, conv("v"))
	}
	if wr.base != "BoolCol" {
		w("func (c %s) Between(lo, hi %s) %s.Pred { return c.%s.Between(%s, %s) }\n", wr.name, t, keel,
			wr.base, conv("lo"), conv("hi"))
	}
	w("func (c %s) To(v %s) %s.Assignment { return c.%s.To(%s) }\n", wr.name, t, keel, wr.base, conv("v"))
	switch wr.base {
	case "StringCol":
		w("func (c %s) In(vs ...%s) %s.Pred { return c.StringCol.In(%s.Strings(vs)...) }\n", wr.name, t, keel, keel)
		w("func (c %s) NotIn(vs ...%s) %s.Pred { return c.StringCol.NotIn(%s.Strings(vs)...) }\n", wr.name, t, keel, keel)
	case "Int64Col":
		w("func (c %s) In(vs ...%s) %s.Pred { return c.Int64Col.In(%s.Int64s(vs)...) }\n", wr.name, t, keel, keel)
		w("func (c %s) NotIn(vs ...%s) %s.Pred { return c.Int64Col.NotIn(%s.Int64s(vs)...) }\n", wr.name, t, keel, keel)
	case "Int32Col":
		w("func (c %s) In(vs ...%s) %s.Pred { return c.Int32Col.In(%s.Int32s(vs)...) }\n", wr.name, t, keel, keel)
		w("func (c %s) NotIn(vs ...%s) %s.Pred { return c.Int32Col.NotIn(%s.Int32s(vs)...) }\n", wr.name, t, keel, keel)
	case "Int16Col":
		w("func (c %s) In(vs ...%s) %s.Pred { return c.Int16Col.In(%s.Int16s(vs)...) }\n", wr.name, t, keel, keel)
		w("func (c %s) NotIn(vs ...%s) %s.Pred { return c.Int16Col.NotIn(%s.Int16s(vs)...) }\n", wr.name, t, keel, keel)
	case "AnyCol":
		list := "vs"
		if wr.valuePtr {
			list = keel + ".Pointers(vs)"
		}
		w("func (c %s) In(vs ...%s) %s.Pred { return c.AnyCol.In(%s) }\n", wr.name, t, keel, list)
		w("func (c %s) NotIn(vs ...%s) %s.Pred { return c.AnyCol.NotIn(%s) }\n", wr.name, t, keel, list)
	}
	w("\n")
}

// relKindName is the schema identifier of a relation kind.
func relKindName(k schema.RelKind) string {
	switch k {
	case schema.BelongsTo:
		return "BelongsTo"
	case schema.HasOne:
		return "HasOne"
	case schema.HasMany:
		return "HasMany"
	case schema.Pick:
		return "Pick"
	}
	return "ManyToMany"
}

func (e *emitter) colType(im *imports, c *column) string {
	if wr := e.wrapperFor(c); wr != nil {
		return im.qual(wr.outPath, wr.outName, wr.name)
	}
	return im.qual(keelPath, "keel", baseKind(c))
}

func (e *emitter) colLit(im *imports, c *column, ref string) string {
	keel := im.add(keelPath, "keel")
	inner := fmt.Sprintf("%s.%s{Col: %s.Col{Ref: %s, Index: %d}}", keel, baseKind(c), keel, ref, c.idx)
	if wr := e.wrapperFor(c); wr != nil {
		return fmt.Sprintf("%s{%s: %s}", im.qual(wr.outPath, wr.outName, wr.name), wr.base, inner)
	}
	return inner
}

func (e *emitter) ptrExpr(im *imports, c *column, unsafePkg string) string {
	switch {
	case c.converts():
		if c.ptr {
			return fmt.Sprintf("(**%s)(%s.Pointer(&r.%s))", c.basic, unsafePkg, c.field)
		}
		return fmt.Sprintf("(*%s)(%s.Pointer(&r.%s))", c.basic, unsafePkg, c.field)
	case c.elem != nil && (c.kind == schema.KindStringArray || c.kind == schema.KindInt64Array):
		elem := "string"
		if c.kind == schema.KindInt64Array {
			elem = "int64"
		}
		return fmt.Sprintf("(*[]%s)(%s.Pointer(&r.%s))", elem, unsafePkg, c.field)
	}
	return "&r." + c.field
}

func (e *emitter) valueStmt(im *imports, c *column, keel string) string {
	if c.zero != zeroNone {
		return fmt.Sprintf("\t\tif %s {\n\t\t\treturn nil\n\t\t}\n\t\treturn %s\n", e.zeroCond(im, c),
			e.plainValue(c, keel))
	}
	switch {
	case c.kind == schema.KindJSON && !c.ptr:
		switch c.typ.Underlying().(type) {
		case *types.Map:
			return fmt.Sprintf("\t\tif r.%s == nil {\n\t\t\treturn %s.EmptyJSONObject\n\t\t}\n\t\treturn r.%s\n",
				c.field, keel, c.field)
		case *types.Slice:
			return fmt.Sprintf("\t\tif r.%s == nil {\n\t\t\treturn %s.EmptyJSONArray\n\t\t}\n\t\treturn r.%s\n",
				c.field, keel, c.field)
		}
		return fmt.Sprintf("\t\treturn r.%s\n", c.field)
	case c.ptr && c.converts():
		return fmt.Sprintf("\t\tif r.%s == nil {\n\t\t\treturn nil\n\t\t}\n\t\treturn %s(*r.%s)\n", c.field,
			c.basic, c.field)
	case c.ptr && c.valuePtr:
		return fmt.Sprintf("\t\tif r.%s == nil {\n\t\t\treturn nil\n\t\t}\n\t\treturn r.%s\n", c.field, c.field)
	case c.valuePtr:
		return fmt.Sprintf("\t\treturn &r.%s\n", c.field)
	case c.ptr:
		return fmt.Sprintf("\t\tif r.%s == nil {\n\t\t\treturn nil\n\t\t}\n\t\treturn *r.%s\n", c.field, c.field)
	case c.converts():
		return fmt.Sprintf("\t\treturn %s(r.%s)\n", c.basic, c.field)
	case c.kind == schema.KindStringArray:
		v := "r." + c.field
		if c.elem != nil {
			v = keel + ".Strings(r." + c.field + ")"
		}
		return fmt.Sprintf("\t\tif r.%s == nil {\n\t\t\treturn []string{}\n\t\t}\n\t\treturn %s\n", c.field, v)
	case c.kind == schema.KindInt64Array:
		v := "r." + c.field
		if c.elem != nil {
			v = keel + ".Int64s(r." + c.field + ")"
		}
		return fmt.Sprintf("\t\tif r.%s == nil {\n\t\t\treturn []int64{}\n\t\t}\n\t\treturn %s\n", c.field, v)
	}
	return fmt.Sprintf("\t\treturn r.%s\n", c.field)
}

// zeroCond is the condition under which a nullzero field holds its zero value.
func (e *emitter) zeroCond(im *imports, c *column) string {
	f := "r." + c.field
	switch c.zero {
	case zeroString:
		return f + ` == ""`
	case zeroNumber:
		return f + " == 0"
	case zeroBool:
		return "!" + f
	case zeroNil:
		return f + " == nil"
	case zeroMethod:
		return f + ".IsZero()"
	}
	return fmt.Sprintf("%s == (%s{})", f, types.TypeString(c.typ, im.qualifier()))
}

// plainValue is the value a nullzero field binds when it is not zero.
func (e *emitter) plainValue(c *column, keel string) string {
	f := "r." + c.field
	switch {
	case c.valuePtr:
		return "&" + f
	case c.converts():
		return c.basic + "(" + f + ")"
	case c.elem != nil && c.kind == schema.KindStringArray:
		return keel + ".Strings(" + f + ")"
	case c.elem != nil && c.kind == schema.KindInt64Array:
		return keel + ".Int64s(" + f + ")"
	}
	return f
}
