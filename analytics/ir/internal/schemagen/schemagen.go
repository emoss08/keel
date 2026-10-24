// Package schemagen generates the JSON Schema of analytics/ir definitions from the Go types, with
// descriptions taken from their doc comments.
package schemagen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
)

// SchemaID is the $id of the generated schema.
const SchemaID = "https://github.com/emoss08/keel/analytics/ir/definition.schema.json"

type object = map[string]any

type generator struct {
	docs  map[string]string
	defs  object
	enums map[reflect.Type][]string
}

func enumNames[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

func opNames() []string {
	var out []string
	for _, o := range ir.Ops() {
		out = append(out, string(o.Op))
	}
	return out
}

// Build returns the schema, reading doc comments from the source directories of analytics/ir and
// analytics/formula.
func Build(irDir, formulaDir string) ([]byte, error) {
	g := &generator{docs: map[string]string{}, defs: object{}, enums: map[reflect.Type][]string{
		reflect.TypeFor[ir.Agg]():         enumNames(ir.Aggs()),
		reflect.TypeFor[ir.Bucket]():      enumNames(ir.Buckets()),
		reflect.TypeFor[ir.WindowFn]():    enumNames(ir.WindowFns()),
		reflect.TypeFor[ir.TransformOp](): enumNames(ir.TransformOps()),
		reflect.TypeFor[ir.ChartType]():   enumNames(ir.ChartTypes()),
		reflect.TypeFor[ir.Op]():          opNames(),
		reflect.TypeFor[formula.Type]():   enumNames(formula.Types()),
	}}
	if err := g.readDocs(irDir, ""); err != nil {
		return nil, err
	}
	if err := g.readDocs(formulaDir, "formula."); err != nil {
		return nil, err
	}
	for _, t := range []reflect.Type{reflect.TypeFor[ir.Definition](), reflect.TypeFor[ir.DashboardFilter]()} {
		if _, err := g.schema(t); err != nil {
			return nil, err
		}
	}
	root := object{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         SchemaID,
		"title":       "Definition",
		"description": g.docs["Definition"],
		"$ref":        "#/$defs/Definition",
		"$defs":       g.defs,
	}
	return Marshal(root)
}

// Marshal encodes v as indented JSON with a trailing newline, without escaping <, > and &.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func (g *generator) readDocs(dir, prefix string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return err
		}
		g.fileDocs(file, prefix)
	}
	return nil
}

func (g *generator) fileDocs(file *ast.File, prefix string) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts := spec.(*ast.TypeSpec)
			doc := ts.Doc
			if doc == nil {
				doc = gen.Doc
			}
			g.docs[prefix+ts.Name.Name] = clean(doc)
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, f := range st.Fields.List {
				for _, n := range f.Names {
					g.docs[prefix+ts.Name.Name+"."+n.Name] = clean(f.Doc)
				}
			}
		}
	}
}

func clean(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.Join(strings.Fields(cg.Text()), " ")
}

func (g *generator) ref(name string) object { return object{"$ref": "#/$defs/" + name} }

func (g *generator) schema(t reflect.Type) (object, error) {
	if t.Kind() == reflect.Pointer {
		return g.schema(t.Elem())
	}
	if s, ok := g.special(t); ok {
		return s, nil
	}
	switch t.Kind() {
	case reflect.String:
		return g.str(t)
	case reflect.Bool:
		return object{"type": "boolean"}, nil
	case reflect.Int, reflect.Int64:
		return object{"type": "integer"}, nil
	case reflect.Float64:
		return object{"type": "number"}, nil
	case reflect.Interface:
		return object{}, nil
	case reflect.Slice:
		items, err := g.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return object{"type": "array", "items": items}, nil
	case reflect.Struct:
		return g.structRef(t)
	default:
		return nil, fmt.Errorf("schemagen: no schema for %s", t)
	}
}

func (g *generator) str(t reflect.Type) (object, error) {
	if t.Name() == "string" {
		return object{"type": "string"}, nil
	}
	values, ok := g.enums[t]
	if !ok {
		return nil, fmt.Errorf("schemagen: %s is a named string type with no registered values", t)
	}
	return object{"type": "string", "enum": values}, nil
}

func (g *generator) structRef(t reflect.Type) (object, error) {
	name := t.Name()
	if _, done := g.defs[name]; done {
		return g.ref(name), nil
	}
	g.defs[name] = object{}
	props, required := object{}, []string{}
	for i := range t.NumField() {
		f := t.Field(i)
		jsonName, optional := jsonField(f)
		if jsonName == "" {
			continue
		}
		s, err := g.schema(f.Type)
		if err != nil {
			return nil, err
		}
		props[jsonName] = g.describe(s, name+"."+f.Name)
		if !optional {
			required = append(required, jsonName)
		}
	}
	if name == "Definition" {
		props["version"] = g.describe(object{"const": ir.Version}, "Definition.Version")
	}
	def := object{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		def["required"] = required
	}
	g.defs[name] = g.describe(def, name)
	return g.ref(name), nil
}

func (g *generator) describe(s object, key string) object {
	if doc := g.docs[key]; doc != "" {
		out := object{"description": doc}
		for k, v := range s {
			out[k] = v
		}
		return out
	}
	return s
}

func jsonField(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" || !f.IsExported() {
		return "", false
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
}

func lowerFirst(s string) string {
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

func paramRef() object {
	return object{
		"type":                 "object",
		"properties":           object{"param": object{"type": "string", "description": "The name of the parameter that supplies the value."}},
		"required":             []string{"param"},
		"additionalProperties": false,
	}
}

func (g *generator) special(t reflect.Type) (object, bool) {
	switch t {
	case reflect.TypeFor[json.RawMessage]():
		return object{"type": "object"}, true
	case reflect.TypeFor[ir.Count]():
		return g.named("Count", object{"oneOf": []any{object{"type": "integer", "minimum": 0}, paramRef()}}), true
	case reflect.TypeFor[ir.Grain]():
		return g.named("Grain", object{"oneOf": []any{object{"type": "string", "enum": g.enums[reflect.TypeFor[ir.Bucket]()]}, paramRef()}}), true
	case reflect.TypeFor[ir.Values]():
		return g.named("Values", object{"oneOf": []any{object{"type": "array", "items": object{"type": "string"}}, paramRef()}}), true
	case reflect.TypeFor[formula.Expr]():
		g.formulaNode()
		return g.named("Formula", object{"oneOf": []any{object{"type": "string", "description": "Formula text."}, g.ref("FormulaNode")}}), true
	case reflect.TypeFor[ir.Filter]():
		g.filter()
		return g.ref("Filter"), true
	}
	return nil, false
}

func (g *generator) named(name string, s object) object {
	if _, done := g.defs[name]; !done {
		key := name
		if name == "Formula" {
			key = "formula.Expr"
		}
		g.defs[name] = g.describe(s, key)
	}
	return g.ref(name)
}

func (g *generator) filter() {
	if _, done := g.defs["Filter"]; done {
		return
	}
	g.defs["Filter"] = object{}
	t := reflect.TypeFor[ir.Filter]()
	props := object{}
	for i := range t.NumField() {
		f := t.Field(i)
		s, _ := g.schema(f.Type)
		props[lowerFirst(f.Name)] = g.describe(s, "Filter."+f.Name)
	}
	group := func(key string) object { return object{"required": []string{key}, "maxProperties": 1} }
	leaf := func(key string) object {
		return object{"required": []string{key}, "not": object{"anyOf": []any{object{"required": []string{"and"}}, object{"required": []string{"or"}}}}}
	}
	g.defs["Filter"] = g.describe(object{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
		"oneOf":                []any{group("and"), group("or"), leaf("field"), leaf("calc"), leaf("column")},
	}, "Filter")
}

func (g *generator) formulaNode() {
	if _, done := g.defs["FormulaNode"]; done {
		return
	}
	node := g.ref("FormulaNode")
	nodes := object{"type": "array", "items": node}
	str := object{"type": "string"}
	variant := func(key string, value any, extra object) object {
		props := object{key: value}
		required := []string{key}
		for k, v := range extra {
			props[k] = v
			required = append(required, k)
		}
		sort.Strings(required)
		return object{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	var ops []string
	for _, o := range formula.Operators() {
		ops = append(ops, o.Symbol)
	}
	ops = dedupe(ops)
	var fns []string
	for _, f := range formula.Functions() {
		if f.Syntax == "" {
			fns = append(fns, f.Name)
		}
	}
	when := object{"type": "object", "properties": object{"if": node, "then": node}, "required": []string{"if", "then"}, "additionalProperties": false}
	caseBody := object{"type": "object", "properties": object{"subject": node, "when": object{"type": "array", "items": when, "minItems": 1}, "else": node}, "required": []string{"when"}, "additionalProperties": false}
	g.defs["FormulaNode"] = g.describe(object{"oneOf": []any{
		variant("number", object{"type": "string", "pattern": `^(0|[1-9][0-9]*)(\.[0-9]+)?$`}, nil),
		variant("string", str, nil),
		variant("bool", object{"type": "boolean"}, nil),
		variant("null", object{"const": true}, nil),
		variant("date", object{"type": "string", "format": "date"}, nil),
		variant("timestamp", object{"type": "string", "format": "date-time"}, nil),
		variant("column", object{"type": "string", "pattern": "^[A-Za-z0-9_]+$"}, nil),
		variant("field", object{"type": "string", "pattern": `^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`}, nil),
		variant("param", object{"type": "string", "pattern": "^[A-Za-z_][A-Za-z0-9_]*$"}, nil),
		variant("op", object{"type": "string", "enum": ops}, object{"args": object{"type": "array", "items": node, "minItems": 1, "maxItems": 2}}),
		variant("call", object{"type": "string", "enum": fns}, object{"args": nodes}),
		variant("case", caseBody, nil),
	}}, "formula.Node")
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
