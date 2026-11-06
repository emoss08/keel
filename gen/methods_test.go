package gen

import (
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// keelTypes loads the types of this repository's keel package.
func keelTypes(t *testing.T) *types.Package {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedTypes,
		Dir: filepath.Dir(filepath.Dir(file)), Env: loaderEnv(os.Environ())}, keelPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("load keel: %d packages", len(pkgs))
	}
	if len(pkgs[0].Errors) > 0 || pkgs[0].Types == nil {
		t.Fatalf("load keel: %v", pkgs[0].Errors)
	}
	return pkgs[0].Types
}

// TestMethodSpecsMatchKeel checks the method specs against keel: every method keel gen writes is
// a method of keel.Handle or keel.Query, every method of those interfaces is written or promoted
// from keel.TableBase or keel.QueryCore, the functions the bodies call exist, and tableBaseMembers
// lists what keel.TableBase promotes. The generated compile-time assertions check the signatures.
func TestMethodSpecsMatchKeel(t *testing.T) {
	keel := keelTypes(t)
	scope := keel.Scope()
	typeOf := func(name string) types.Type {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok {
			t.Fatalf("keel has no type %s", name)
		}
		return tn.Type()
	}
	iface := func(name string) *types.Interface {
		it, ok := typeOf(name).Underlying().(*types.Interface)
		if !ok {
			t.Fatalf("keel.%s is not an interface", name)
		}
		return it
	}
	check := func(name string, generated []string, embedded string) {
		it := iface(name)
		written := map[string]bool{}
		for _, n := range generated {
			if written[n] {
				t.Errorf("keel gen writes %s twice on the %s type", n, name)
			}
			written[n] = true
			if obj, _, _ := types.LookupFieldOrMethod(it, false, keel, n); obj == nil {
				t.Errorf("keel gen writes %s, which keel.%s does not have", n, name)
			}
		}
		base := types.NewPointer(typeOf(embedded))
		for i := range it.NumMethods() {
			m := it.Method(i)
			if written[m.Name()] {
				continue
			}
			if obj, _, _ := types.LookupFieldOrMethod(base, false, m.Pkg(), m.Name()); obj == nil {
				t.Errorf("keel.%s.%s is neither written by keel gen nor promoted from keel.%s", name, m.Name(),
					embedded)
			}
		}
	}
	check("Handle", tableMethodNames(), "TableBase")
	check("Query", queryMethodNames(), "QueryCore")

	called := regexp.MustCompile(`\{keel\}\.(\w+)\(`)
	pointerFn := func(s methodSpec) {
		m := called.FindStringSubmatch(s.body)
		if m == nil {
			t.Errorf("the body of %s calls no keel function", s.name)
			return
		}
		f, ok := scope.Lookup(m[1]).(*types.Func)
		if !ok {
			t.Errorf("keel has no function %s", m[1])
			return
		}
		if f.Type().(*types.Signature).TypeParams().Len() != 0 {
			t.Errorf("keel.%s, which %s calls, is generic: every generated package would compile it", m[1], s.name)
		}
		generic, ok := scope.Lookup(s.name).(*types.Func)
		if s.name == "Page" {
			generic, ok = scope.Lookup("PageOf").(*types.Func)
		}
		if !ok || generic.Type().(*types.Signature).TypeParams().Len() != 1 {
			t.Errorf("keel has no generic %s for models keel gen does not cover", s.name)
		}
	}
	for _, s := range tableWrites {
		pointerFn(s)
	}
	for _, s := range queryTerminals {
		pointerFn(s)
	}
	if _, ok := scope.Lookup(newQueryCore).(*types.Func); !ok {
		t.Errorf("keel has no function %s", newQueryCore)
	}
	typeOf(lateralSource)

	base := typeOf("TableBase")
	listed := map[string]bool{}
	for _, n := range tableBaseMembers {
		listed[n] = true
		if obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(base), false, keel, n); obj == nil {
			t.Errorf("keel.TableBase has no %s", n)
		}
	}
	ms := types.NewMethodSet(types.NewPointer(base))
	for i := range ms.Len() {
		if n := ms.At(i).Obj().Name(); token.IsExported(n) && !listed[n] {
			t.Errorf("tableBaseMembers lacks keel.TableBase.%s", n)
		}
	}
	st := base.Underlying().(*types.Struct)
	for i := range st.NumFields() {
		if f := st.Field(i); f.Exported() && !listed[f.Name()] {
			t.Errorf("tableBaseMembers lacks keel.TableBase.%s", f.Name())
		}
	}
}

// TestGeneratedCodeCompiles writes the generated packages of outputModels into their module and
// builds it, which runs the compile-time assertions that the table and query types implement
// keel.Handle and keel.Query.
func TestGeneratedCodeCompiles(t *testing.T) {
	dir := moduleFiles(t, outputModels, nil)
	files, err := Generate(Options{Dir: dir, Patterns: []string{"./..."}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(files); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

func TestGenerateSuffixesMemberNames(t *testing.T) {
	models := "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n" +
		"\tOrder int64 ~keel:\"ord\"~\n\tCopy string ~keel:\"copy\"~\n\tGet string ~keel:\"get\"~\n" +
		"\tBID string ~keel:\"b_id\"~\n\tQuery *B ~keel:\"belongs:b_id\"~\n}\n" +
		"type B struct {\n\tkeel.Table ~keel:\"b\"~\n\tID string ~keel:\"id,pk\"~\n}\n"
	files, err := generate(t, models)
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for _, f := range files {
		out += string(f.Content)
	}
	for _, want := range []string{
		"\tOrderCol keel.Int64Col\n",
		"\tCopyCol  keel.StringCol\n",
		"\tGetCol   keel.StringCol\n",
		"func (t *AsTable) QueryRel() *BsTable {",
		"func (t *AsTable) Order(os ...keel.Order) keel.Query[*models.A] {",
		"func (t *AsTable) Query() keel.Query[*models.A] {",
		"Field: \"Order\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
}

func TestGenerateRefusesMemberNames(t *testing.T) {
	runErrors(t, []errorCase{
		{"suffixed name taken by another column", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n" +
			"\tOrder int64 ~keel:\"ord\"~\n\tOrderCol int64 ~keel:\"ord_col\"~\n}\n",
			field(5) + ": A.OrderCol: the generated AsTable already has a member OrderCol for Order; rename the field"},
	})
}
