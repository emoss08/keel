package gen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// modelsHeader starts every generated models.go; models begin on line headerLines+1.
const modelsHeader = "package models\n\nimport (\n\t\"context\"\n\t\"time\"\n\n\t\"github.com/emoss08/keel\"\n)\n\n" +
	"var _ = context.Background\nvar _ = time.Now\nvar _ keel.Table\n\n"

// headerLines is the number of lines modelsHeader takes.
const headerLines = 13

// template holds the go.mod and go.sum of a tidied module that uses this repository's keel, so
// each test module is a copy instead of a go mod tidy.
var template struct {
	once     sync.Once
	mod, sum []byte
	err      error
}

// tidyTemplate tidies a module that imports keel once per test binary.
func tidyTemplate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	template.once.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		repo := filepath.Dir(filepath.Dir(file))
		dir, err := os.MkdirTemp("", "keelgen")
		if err != nil {
			template.err = err
			return
		}
		defer os.RemoveAll(dir)
		sum, err := os.ReadFile(filepath.Join(repo, "go.sum"))
		if err != nil {
			template.err = err
			return
		}
		files := map[string]string{
			"go.mod": "module example.com/app\n\ngo 1.26.0\n\nrequire github.com/emoss08/keel v0.0.0\n\n" +
				"replace github.com/emoss08/keel => " + repo + "\n",
			"go.sum":           string(sum),
			"models/models.go": modelsHeader,
		}
		if template.err = writeFiles(dir, files); template.err != nil {
			return
		}
		tidy := exec.Command("go", "mod", "tidy")
		tidy.Dir = dir
		tidy.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
		if out, err := tidy.CombinedOutput(); err != nil {
			template.err = fmt.Errorf("go mod tidy: %v\n%s", err, out)
			return
		}
		template.mod, _ = os.ReadFile(filepath.Join(dir, "go.mod"))
		template.sum, template.err = os.ReadFile(filepath.Join(dir, "go.sum"))
	})
	if template.err != nil {
		t.Fatal(template.err)
	}
	return template.mod, template.sum
}

// writeFiles writes files under dir, creating directories as needed.
func writeFiles(dir string, files map[string]string) error {
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// tick turns ~ into backquotes, so models with struct tags read as Go in raw strings.
func tick(s string) string { return strings.ReplaceAll(s, "~", "`") }

// moduleFiles writes a module that uses the keel in this repository: models/models.go holds
// models after the standard header, and extra holds further files by path.
func moduleFiles(t *testing.T, models string, extra map[string]string) string {
	t.Helper()
	mod, sum := tidyTemplate(t)
	dir := t.TempDir()
	files := map[string]string{"go.mod": string(mod), "go.sum": string(sum), "models/models.go": modelsHeader + tick(models)}
	for k, v := range extra {
		files[k] = tick(v)
	}
	if err := writeFiles(dir, files); err != nil {
		t.Fatal(err)
	}
	return dir
}

// generate generates the models of a fresh module with default options.
func generate(t *testing.T, models string) ([]File, error) {
	t.Helper()
	dir := moduleFiles(t, models, nil)
	return Generate(Options{Dir: dir, Patterns: []string{"./..."}})
}

// at is the position of line l, column c of the models, as errors print it.
func at(l, c int) string { return fmt.Sprintf("models/models.go:%d:%d", headerLines+l, c) }

// field is the position of a field on line l of the models.
func field(l int) string { return at(l, 2) }

// errorCase is a model source keel gen must refuse with an error containing every …-separated
// part of want.
type errorCase struct {
	name   string
	models string
	want   string
}

// runErrors checks that every case fails with its error.
func runErrors(t *testing.T, cases []errorCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := generate(t, c.models)
			if err == nil {
				t.Fatalf("no error, want %q", c.want)
			}
			for _, part := range strings.Split(c.want, "…") {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("got %v, want an error containing %q", err, part)
				}
			}
		})
	}
}

// oneA is a model A with one field on line 3 after its table tag.
func oneA(fieldSrc string) string {
	return "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\t" + fieldSrc + "\n}\n"
}

func TestGenerateErrors(t *testing.T) {
	runErrors(t, []errorCase{
		{"int field", oneA(`N int ~keel:"n"~`), field(3) + ": A.N: use int64"},
		{"struct needs jsonb", oneA(`M map[string]any ~keel:"m"~`), field(3) + ": A.M: map[string]any needs the jsonb option"},
		{"version type", oneA(`V string ~keel:"v,version"~`), field(3) + ": A.V: version columns must be int64"},
		{"autoid type", oneA(`V int64 ~keel:"v,autoid:a_"~`), field(3) + ": A.V: autoid columns must be strings"},
		{"timestamp type", oneA(`V string ~keel:"v,autocreate"~`), field(3) + ": A.V: timestamp columns"},
		{"duplicate column", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tX string ~keel:\"x\"~\n\tY string ~keel:\"x\"~\n}\n",
			field(4) + ": A.Y: column x is declared twice"},
		{"two versions", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tX int64 ~keel:\"x,version\"~\n\tY int64 ~keel:\"y,version\"~\n}\n",
			field(4) + ": A: X and Y both carry the same option"},
		{"missing fk", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n\tB *B ~keel:\"belongs:b_id\"~\n}\n" +
			"type B struct {\n\tkeel.Table ~keel:\"b\"~\n\tID string ~keel:\"id,pk\"~\n}\n", field(4) + ": A.B: A has no column b_id"},
		{"relation to non-model", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n\tBID string ~keel:\"b_id\"~\n\tB *C ~keel:\"belongs:b_id\"~\n}\n" +
			"type C struct{ X int }\n", field(5) + ": A.B: example.com/app/models.C is not a model"},
		{"to-many needs slice", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n\tB *B ~keel:\"many:a_id\"~\n}\n" +
			"type B struct {\n\tkeel.Table ~keel:\"b\"~\n\tID string ~keel:\"id,pk\"~\n\tAID string ~keel:\"a_id\"~\n}\n", field(4) + ": A.B: many relations need a slice field"},
		{"bad hook", oneA(`ID string ~keel:"id,pk"~`) + "func (a *A) BeforeSave() error { return nil }\n", at(5, 13) + ": A.BeforeSave must be func(context.Context) error"},
		{"unexported", oneA(`id string ~keel:"id,pk"~`), field(3) + ": A.id: stored fields must be exported"},
		{"no models", "type A struct{ X int }\n", "no models"},
		{"nullzero pointer", oneA(`P *string ~keel:"p,nullzero"~`), field(3) + ": A.P: nullzero needs a non-pointer field"},
		{"nullzero pk", oneA(`ID string ~keel:"id,pk,nullzero"~`), field(3) + ": A.…nullzero excludes pk"},
		{"nullzero tenant", oneA(`T string ~keel:"t,tenant,nullzero"~`), field(3) + ": A.…nullzero excludes pk, tenant"},
		{"nullzero not comparable", "type S struct{ X []int }\n\n" + oneA(`S S ~keel:"s,jsonb,nullzero"~`),
			field(5) + ": A.S: nullzero needs a comparable type or an IsZero() bool method"},
		{"nullzero nullable scanner", "type NullX struct{ V string }\n\nfunc (n *NullX) Scan(any) error { return nil }\n\n" +
			oneA(`X NullX ~keel:"x,nullzero"~`), field(7) + ": A.X: nullzero needs a non-nullable type; NullX already stores NULL"},
	})
}

func TestGenerateNullZero(t *testing.T) {
	files, err := generate(t, `type Code string

type Decimal struct{ v *int }

func (d Decimal) IsZero() bool { return d.v == nil }

func (d *Decimal) Scan(any) error { return nil }

type Point struct{ X, Y float64 }

func (p *Point) Scan(any) error { return nil }

type A struct {
	keel.Table ~keel:"a"~

	ID    string         ~keel:"id,pk"~
	Code  Code           ~keel:"code,nullzero"~
	N     int32          ~keel:"n,nullzero"~
	B     bool           ~keel:"b,nullzero"~
	At    time.Time      ~keel:"at,nullzero"~
	Dec   Decimal        ~keel:"dec,nullzero"~
	Pt    Point          ~keel:"pt,nullzero"~
	Tags  []Code         ~keel:"tags,nullzero"~
	Attrs map[string]any ~keel:"attrs,jsonb,nullzero"~
}
`)
	if err != nil {
		t.Fatal(err)
	}
	src := string(files[0].Content)
	for _, want := range []string{
		"Flags: schema.FlagNullZero",
		"if r.Code == \"\" {\n\t\t\treturn nil\n\t\t}\n\t\treturn string(r.Code)",
		"if r.N == 0 {",
		"if !r.B {",
		"if r.At.IsZero() {",
		"if r.Dec.IsZero() {",
		"if r.Pt == (models.Point{}) {",
		"if r.Tags == nil {\n\t\t\treturn nil\n\t\t}\n\t\treturn keel.Strings(r.Tags)",
		"if r.Attrs == nil {\n\t\t\treturn nil\n\t\t}\n\t\treturn r.Attrs",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(src)
	}
}

func TestGenerateRefusesBadAcronyms(t *testing.T) {
	dir := moduleFiles(t, oneA(`ID string ~keel:"id,pk"~`), nil)
	_, err := Generate(Options{Dir: dir, Patterns: []string{"./..."}, Acronyms: []string{"PRO", "B-L"}})
	if err == nil || !strings.Contains(err.Error(), `acronym "B-L" must be letters and digits`) {
		t.Fatalf("got %v", err)
	}
}

func TestGenerateTagErrors(t *testing.T) {
	runErrors(t, []errorCase{
		{"unknown option", oneA(`ID string ~keel:"id,pk,nope"~`), field(3) + `: A.…A.ID: keel tag "id,pk,nope": unknown option "nope"`},
		{"repeated option", oneA(`ID string ~keel:"id,pk,pk"~`), field(3) + `: A.…A.ID: keel tag "id,pk,pk": option pk is repeated`},
		{"flag with value", oneA(`ID string ~keel:"id,pk=1"~`), field(3) + `: A.…option pk takes no value`},
		{"empty option", oneA(`ID string ~keel:"id,,pk"~`), field(3) + `: A.…A.ID: keel tag "id,,pk": empty option`},
		{"bad column name", oneA(`ID string ~keel:"Id"~`), field(3) + `: A.…A.ID: keel tag "Id": identifier "Id" must match [a-z0-9_]+`},
		{"empty label", oneA(`ID string ~keel:"id,label="~`), field(3) + `: A.…option label needs a value (label=…)`},
		{"empty autoid", oneA(`ID string ~keel:"id,autoid:"~`), field(3) + `: A.…option autoid needs a value (autoid:…)`},
		{"autoid with =", oneA(`ID string ~keel:"id,autoid=a_"~`), field(3) + `: A.…write autoid:<prefix>`},
		{"bad autoid", oneA(`ID string ~keel:"id,autoid:a b"~`), field(3) + `: A.…autoid prefix "a b" must match`},
		{"readonly pk", oneA(`ID string ~keel:"id,pk,readonly"~`), field(3) + `: A.…readonly excludes write options`},
		{"jsonb tsvector", oneA(`ID string ~keel:"id,jsonb,tsvector"~`), field(3) + `: A.…jsonb and tsvector exclude each other`},
		{"bad format", oneA(`ID string ~keel:"id,format=a b"~`), field(3) + `: A.…format "a b" must match`},
		{"bad type", oneA(`ID string ~keel:"id,type=money"~`), field(3) + `: A.…type=money: want epoch, date or decimal`},
		{"bad agg", oneA(`ID string ~keel:"id,agg=sum|median"~`), field(3) + `: A.…agg=sum|median: unknown aggregation "median"`},
		{"repeated agg", oneA(`ID string ~keel:"id,agg=min|min"~`), field(3) + `: A.…agg=min|min: min is repeated`},
		{"report noreport", oneA(`ID string ~keel:"id,report,noreport"~`), field(3) + `: A.…report and noreport exclude each other`},
		{"dimension measure", oneA(`ID string ~keel:"id,dimension,measure"~`), field(3) + `: A.…dimension and measure exclude each other`},
		{"report value on column", oneA(`ID string ~keel:"id,report=x"~`), field(3) + `: A.…option report takes no value`},
		{"column option on relation", oneA(`B *A ~keel:"belongs:b_id,pk"~`), field(3) + `: A.…relation option "pk"`},
		{"order on belongs", oneA(`B *A ~keel:"belongs:b_id,first=b.id"~`), field(3) + `: A.…option first is only for pick relations`},
		{"bad m2m", oneA(`B []*A ~keel:"m2m:a_b.a_id"~`), field(3) + `: A.…m2m needs m2m:<join_table>.<local_fk>.<remote_fk>`},
		{"bad relation key", oneA(`B *A ~keel:"belongs:B"~`), field(3) + `: A.…identifier "B" must match`},
		{"pick without order", oneA(`B *A ~keel:"pick:bs"~`), field(3) + `: A.…pick relations need first= or last=`},
		{"pick first and last", oneA(`B *A ~keel:"pick:bs,first=bs.id,last=bs.id"~`), field(3) + `: A.…first= and last= exclude each other`},
		{"pick bad path", oneA(`B *A ~keel:"pick:bs..cs,first=bs.id"~`), field(3) + `: A.…pick path: empty identifier`},
		{"pick order not in path", oneA(`B *A ~keel:"pick:bs,first=cs.id"~`), field(3) + `: A.…first=cs.id: cs is not a segment of the path bs`},
		{"pick malformed order", oneA(`B *A ~keel:"pick:bs,last=id"~`), field(3) + `: A.…last=id: "id" must be <segment>.<column>`},
		{"empty table tag", "type A struct {\n\tkeel.Table\n\tID string ~keel:\"id,pk\"~\n}\n", at(2, 7) + `: A: …A: keel tag "": table name: empty identifier`},
		{"unknown table option", "type A struct {\n\tkeel.Table ~keel:\"a,alias=x\"~\n}\n", at(2, 7) + `: A: …A: keel tag "a,alias=x": unknown table option "alias"`},
		{"empty category", "type A struct {\n\tkeel.Table ~keel:\"a,report=\"~\n}\n", at(2, 7) + `: A: …option report needs a value`},
		{"empty plural", "type A struct {\n\tkeel.Table ~keel:\"a,plural=\"~\n}\n", at(2, 7) + `: A: …option plural needs a value`},
	})
}

func TestGenerateOutput(t *testing.T) {
	files, err := generate(t, `type Status string

type Account struct {
	keel.Table ~keel:"accounts"~

	ID        string    ~keel:"id,pk,autoid:acc_"~
	Status    Status    ~keel:"status"~
	Tags      []Status  ~keel:"tags"~
	Meta      []string  ~keel:"meta,jsonb"~
	Seen      *time.Time ~keel:"seen"~
	Version   int64     ~keel:"version,version"~
	CreatedAt time.Time ~keel:"created_at,autocreate"~
	Notes     []*Note   ~keel:"many:account_id"~
}

type Note struct {
	keel.Table ~keel:"notes"~

	ID        string   ~keel:"id,pk"~
	AccountID string   ~keel:"account_id"~
	Account   *Account ~keel:"belongs:account_id"~
}

func (n *Note) AfterLoad(ctx context.Context) error { return nil }
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("%d files", len(files))
	}
	src := string(files[0].Content)
	if !strings.HasSuffix(files[0].Path, filepath.Join("models", "modelsdb", "keel_gen.go")) {
		t.Fatalf("path %s", files[0].Path)
	}
	tables := string(files[1].Content)
	if !strings.HasSuffix(files[1].Path, filepath.Join("keeltables", "keel_gen.go")) ||
		!strings.Contains(tables, "modelsdb.AccountsSchema(),\n\t\tmodelsdb.NotesSchema(),") {
		t.Errorf("table list %s:\n%s", files[1].Path, tables)
	}
	for _, want := range []string{
		"package modelsdb",
		"type AccountsTable struct",
		"Status    StatusCol",
		"type StatusCol struct{ keel.StringCol }",
		"func (c StatusCol) In(vs ...models.Status) keel.Pred",
		"func (t *AccountsTable) Notes() *NotesTable",
		"func (t *NotesTable) Account() *AccountsTable",
		"func (t *AccountsTable) Get(ctx context.Context, db *keel.DB, key string) (*models.Account, error)",
		"return (*[]string)(unsafe.Pointer(&r.Tags))",
		"return keel.Strings(r.Tags)",
		"return keel.EmptyJSONArray",
		"afterLoadNote,",
		"accountsTable.Relations[0].Target = NotesSchema()",
		`IDPrefix:   "acc_"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
	if strings.Contains(src, "Report:") {
		t.Error("a model without report carries report facts")
	}
	if t.Failed() {
		t.Log(src)
	}
}

func TestGenerateIgnoresDBTags(t *testing.T) {
	files, err := generate(t, "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n\tName string ~db:\"name\"~\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if src := string(files[0].Content); strings.Contains(src, `"name"`) {
		t.Fatal("a db tag declared a column")
	}
}

func TestGenerateOverStaleGeneratedCode(t *testing.T) {
	dir := moduleFiles(t, "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n\tName string ~keel:\"name\"~\n}\n", nil)
	o := Options{Dir: dir, Patterns: []string{"./..."}}
	files, err := Generate(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(files); err != nil {
		t.Fatal(err)
	}
	if err := writeFiles(dir, map[string]string{"models/models.go": modelsHeader +
		tick("type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n}\n")}); err != nil {
		t.Fatal(err)
	}
	files, err = Generate(o)
	if err != nil {
		t.Fatalf("a removed field broke regeneration: %v", err)
	}
	if strings.Contains(string(files[0].Content), "r.Name") {
		t.Fatal("the removed field is still generated")
	}
}

func TestPlural(t *testing.T) {
	for in, want := range map[string]string{
		"Shipment": "Shipments", "Address": "Addresses", "Category": "Categories", "Day": "Days",
		"Box": "Boxes", "Match": "Matches", "Status": "Statuses",
	} {
		if got := plural(in); got != want {
			t.Errorf("plural(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestLookupTag(t *testing.T) {
	tag := `json:"id" keel:"id,pk" other:"x"`
	if got := lookupTag(tag, "keel"); got != "id,pk" {
		t.Fatal(got)
	}
	if got := lookupTag(tag, "missing"); got != "" {
		t.Fatal(got)
	}
}
