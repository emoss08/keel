package gen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
)

// source reads what keel gen needs beyond types from Go source: positions, doc comments and the
// literal a KeelFormat method returns. Files are parsed on first use, so it works the same for
// model packages and for packages loaded from export data.
type source struct {
	fset  *token.FileSet
	dir   string
	files map[string]*fileIndex
}

// fileIndex holds the declarations of one parsed file by line and name.
type fileIndex struct {
	docs  map[lineName]string
	funcs map[lineName]*ast.FuncDecl
}

// lineName identifies a declared name by the line it is declared on.
type lineName struct {
	line int
	name string
}

// newSource returns a source for positions in fset, printing file names relative to dir.
func newSource(fset *token.FileSet, dir string) *source {
	return &source{fset: fset, dir: dir, files: map[string]*fileIndex{}}
}

// pos formats p as file:line:col, the file relative to the source's directory when inside it.
func (s *source) pos(p token.Pos) string {
	at := s.fset.Position(p)
	if rel, err := filepath.Rel(s.dir, at.Filename); err == nil && !strings.HasPrefix(rel, "..") {
		at.Filename = rel
	}
	return at.String()
}

// before reports whether p comes before q: by file name, then by place in the file.
func (s *source) before(p, q token.Pos) bool {
	a, b := s.fset.Position(p), s.fset.Position(q)
	if a.Filename != b.Filename {
		return a.Filename < b.Filename
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Column < b.Column
}

// errorf returns an error prefixed with the position p.
func (s *source) errorf(p token.Pos, format string, args ...any) error {
	return fmt.Errorf("%s: %s", s.pos(p), fmt.Sprintf(format, args...))
}

// doc returns the normalized doc comment of the object declared at p under name.
func (s *source) doc(p token.Pos, name string) (string, error) {
	ix, at, err := s.index(p)
	if err != nil || ix == nil {
		return "", err
	}
	return ix.docs[lineName{at.Line, name}], nil
}

// formatOf returns the string literal the KeelFormat method fn returns.
func (s *source) formatOf(fn *types.Func) (string, error) {
	ix, at, err := s.index(fn.Pos())
	if err != nil {
		return "", err
	}
	if ix == nil {
		return "", s.errorf(fn.Pos(), "KeelFormat: source not found")
	}
	decl := ix.funcs[lineName{at.Line, fn.Name()}]
	if decl == nil || decl.Body == nil || len(decl.Body.List) != 1 {
		return "", s.errorf(fn.Pos(), "KeelFormat must be a single return of a string literal")
	}
	ret, ok := decl.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return "", s.errorf(fn.Pos(), "KeelFormat must be a single return of a string literal")
	}
	bl, ok := ret.Results[0].(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", s.errorf(fn.Pos(), "KeelFormat must return a string literal")
	}
	v, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", s.errorf(fn.Pos(), "KeelFormat: %v", err)
	}
	return v, nil
}

// index returns the parsed index of the file that holds p, or nil when p has no file.
func (s *source) index(p token.Pos) (*fileIndex, token.Position, error) {
	at := s.fset.Position(p)
	if at.Filename == "" {
		return nil, at, nil
	}
	if ix, ok := s.files[at.Filename]; ok {
		return ix, at, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, at.Filename, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, at, fmt.Errorf("parse %s: %w", at.Filename, err)
	}
	ix := indexFile(fset, f)
	s.files[at.Filename] = ix
	return ix, at, nil
}

// indexFile records the doc comments of a file's types, struct fields and constants, and its
// function declarations.
func indexFile(fset *token.FileSet, f *ast.File) *fileIndex {
	ix := &fileIndex{docs: map[lineName]string{}, funcs: map[lineName]*ast.FuncDecl{}}
	lineOf := func(n ast.Node) int { return fset.Position(n.Pos()).Line }
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			ix.funcs[lineName{lineOf(d.Name), d.Name.Name}] = d
		case *ast.GenDecl:
			ix.genDecl(d, lineOf)
		}
	}
	return ix
}

// genDecl records the docs of one type or const declaration. A declaration of one spec without
// parentheses carries its doc on the declaration.
func (ix *fileIndex) genDecl(d *ast.GenDecl, lineOf func(ast.Node) int) {
	single := !d.Lparen.IsValid()
	for _, spec := range d.Specs {
		switch sp := spec.(type) {
		case *ast.TypeSpec:
			ix.docs[lineName{lineOf(sp.Name), sp.Name.Name}] = docText(either(sp.Doc, single, d.Doc))
			if st, ok := sp.Type.(*ast.StructType); ok {
				ix.fields(st, lineOf)
			}
		case *ast.ValueSpec:
			for _, n := range sp.Names {
				ix.docs[lineName{lineOf(n), n.Name}] = docText(either(either(sp.Doc, single, d.Doc), true, sp.Comment))
			}
		}
	}
}

// fields records the docs of a struct's fields: the doc comment, else the line comment.
func (ix *fileIndex) fields(st *ast.StructType, lineOf func(ast.Node) int) {
	for _, fld := range st.Fields.List {
		doc := docText(either(fld.Doc, true, fld.Comment))
		for _, n := range fld.Names {
			ix.docs[lineName{lineOf(n), n.Name}] = doc
		}
	}
}

// either returns c, or alt when c is empty and use is set.
func either(c *ast.CommentGroup, use bool, alt *ast.CommentGroup) *ast.CommentGroup {
	if c == nil && use {
		return alt
	}
	return c
}

// docText normalizes a comment group: lines of a paragraph joined by spaces, paragraphs separated
// by a blank line, surrounding space trimmed.
func docText(c *ast.CommentGroup) string {
	if c == nil {
		return ""
	}
	var paras []string
	for _, p := range strings.Split(strings.TrimSpace(c.Text()), "\n\n") {
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			paras = append(paras, p)
		}
	}
	return strings.Join(paras, "\n\n")
}
