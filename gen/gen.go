package gen

import (
	"bytes"
	"errors"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/emoss08/keel/schema"
	"golang.org/x/tools/go/packages"
)

// Options configures a generation run.
type Options struct {
	// Dir is the directory patterns are resolved from. Defaults to the current directory.
	Dir string
	// Patterns select the packages that hold models, as for go build.
	Patterns []string
	// Out is the directory of each generated package, relative to the module root. {dir} is the
	// model package's directory relative to the module root and {name} its package name. Defaults
	// to "{dir}/{name}db".
	Out string
	// Kinds is the directory, relative to the module root, of the package that holds column types
	// for named types declared outside model packages. Defaults to "keelkinds".
	Kinds string
	// Tables is the directory, relative to the module root, of the package that lists every
	// generated table, for code that works over all of them (dynamic queries, the analytics
	// catalog). Defaults to "keeltables".
	Tables string
	// Tags are build tags used to load the packages.
	Tags []string
	// Acronyms are the words labels write in capitals (PRO, BOL, SCAC). Defaults to ID, URL, API,
	// JSON and UUID.
	Acronyms []string
}

// File is one generated file.
type File struct {
	Path    string
	Content []byte
}

// Generate loads the packages and returns the files to write.
func Generate(o Options) ([]File, error) {
	if o.Out == "" {
		o.Out = "{dir}/{name}db"
	}
	if o.Kinds == "" {
		o.Kinds = "keelkinds"
	}
	if o.Tables == "" {
		o.Tables = "keeltables"
	}
	labels, err := newLabeler(o.Acronyms)
	if err != nil {
		return nil, err
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedModule | packages.NeedFiles,
		Dir:  o.Dir,
	}
	if len(o.Tags) > 0 {
		cfg.BuildFlags = []string{"-tags=" + strings.Join(o.Tags, ",")}
	}
	pkgs, err := packages.Load(cfg, o.Patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages match %s in %s", strings.Join(o.Patterns, " "), o.Dir)
	}
	var loadErrs []error
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if isGenerated(p) {
			return
		}
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, fmt.Errorf("%s: %s", p.PkgPath, e.Msg))
		}
	})
	if len(loadErrs) > 0 {
		return nil, errors.Join(loadErrs...)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	src := newSource(pkgs[0].Fset, cwd)
	rd := &reader{src: src, labels: labels, enums: map[*types.Named][]schema.EnumValue{},
		formats: map[*types.Named]string{}}
	byPath := map[string]*pkgModels{}
	all := map[*types.Named]*model{}
	var modRoot, modPath string
	for _, p := range pkgs {
		if p.Module == nil {
			return nil, fmt.Errorf("%s is not in a module", p.PkgPath)
		}
		if modRoot == "" {
			modRoot, modPath = p.Module.Dir, p.Module.Path
		}
		if strings.HasSuffix(p.Name, "db") && strings.Contains(o.Out, "{name}db") && isGenerated(p) {
			continue
		}
		pm := &pkgModels{path: p.PkgPath, name: p.Name, pkg: p.Types}
		scope := p.Types.Scope()
		names := scope.Names()
		sort.Strings(names)
		for _, n := range names {
			tn, ok := scope.Lookup(n).(*types.TypeName)
			if !ok {
				continue
			}
			m, err := rd.readModel(pm, tn)
			if err != nil {
				return nil, err
			}
			if m != nil {
				pm.models = append(pm.models, m)
				all[m.named] = m
			}
		}
		if len(pm.models) == 0 {
			continue
		}
		dir, err := pkgDir(p, modRoot)
		if err != nil {
			return nil, err
		}
		rel := strings.NewReplacer("{dir}", dir, "{name}", p.Name).Replace(o.Out)
		rel = filepath.ToSlash(filepath.Clean(rel))
		pm.outDir = filepath.Join(modRoot, rel)
		pm.outPath = modPath + "/" + rel
		pm.outName = filepath.Base(rel)
		byPath[p.PkgPath] = pm
	}
	if len(byPath) == 0 {
		return nil, fmt.Errorf("no models found in %s", strings.Join(o.Patterns, " "))
	}
	var models []*model
	for _, p := range sortedPkgs(byPath) {
		models = append(models, p.models...)
	}
	if err := resolve(src, all, models); err != nil {
		return nil, err
	}
	if err := resolveReports(src, models); err != nil {
		return nil, err
	}

	kindsRel := filepath.ToSlash(filepath.Clean(o.Kinds))
	e := &emitter{pkgs: byPath, kinds: &pkgModels{
		outPath: modPath + "/" + kindsRel,
		outName: filepath.Base(kindsRel),
		outDir:  filepath.Join(modRoot, kindsRel),
	}}
	e.planWrappers()

	var files []File
	for _, p := range sortedPkgs(byPath) {
		content, err := e.emitPackage(p)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: filepath.Join(p.outDir, "keel_gen.go"), Content: content})
	}
	kinds, err := e.emitKinds()
	if err != nil {
		return nil, err
	}
	if kinds != nil {
		files = append(files, File{Path: filepath.Join(e.kinds.outDir, "keel_gen.go"), Content: kinds})
	}
	tablesRel := filepath.ToSlash(filepath.Clean(o.Tables))
	tables, err := e.emitTables(modPath+"/"+tablesRel, filepath.Base(tablesRel))
	if err != nil {
		return nil, err
	}
	files = append(files, File{Path: filepath.Join(modRoot, tablesRel, "keel_gen.go"), Content: tables})
	return files, nil
}

// Write writes files, creating directories as needed, and reports the files that changed.
func Write(files []File) ([]string, error) {
	var changed []string
	for _, f := range files {
		old, err := os.ReadFile(f.Path)
		if err == nil && bytes.Equal(old, f.Content) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(f.Path, f.Content, 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, f.Path)
	}
	return changed, nil
}

// Stale reports the files whose content on disk differs from files.
func Stale(files []File) []string {
	var stale []string
	for _, f := range files {
		old, err := os.ReadFile(f.Path)
		if err != nil || !bytes.Equal(old, f.Content) {
			stale = append(stale, f.Path)
		}
	}
	return stale
}

// pkgDir returns the directory of p relative to the module root.
func pkgDir(p *packages.Package, modRoot string) (string, error) {
	if len(p.GoFiles) == 0 {
		return "", fmt.Errorf("%s has no Go files", p.PkgPath)
	}
	rel, err := filepath.Rel(modRoot, filepath.Dir(p.GoFiles[0]))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// isGenerated reports whether p is a package keel gen wrote.
func isGenerated(p *packages.Package) bool {
	for _, f := range p.GoFiles {
		if filepath.Base(f) == "keel_gen.go" {
			return true
		}
	}
	return false
}
