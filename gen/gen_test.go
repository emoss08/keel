package gen

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// module writes a module that uses the keel in this repository and holds one package of models.
func module(t *testing.T, models string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Dir(filepath.Dir(file))
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := os.ReadFile(filepath.Join(repo, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	write("go.mod", "module example.com/app\n\ngo 1.26.0\n\nrequire github.com/emoss08/keel v0.0.0\n\n"+
		"replace github.com/emoss08/keel => "+repo+"\n")
	write("go.sum", string(sum))
	write("models/models.go", "package models\n\nimport (\n\t\"context\"\n\t\"time\"\n\n\t\"github.com/emoss08/keel\"\n)\n\n"+
		"var _ = context.Background\nvar _ = time.Now\nvar _ keel.Table\n\n"+models)
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	return dir
}

func generate(t *testing.T, models string) ([]File, error) {
	t.Helper()
	dir := module(t, models)
	return Generate(Options{Dir: dir, Patterns: []string{"./..."}})
}

func TestGenerateErrors(t *testing.T) {
	cases := []struct {
		name   string
		models string
		want   string
	}{
		{"bad tag", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tID string `db:\"id,pk,nope\"`\n}\n", "unknown option"},
		{"int field", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tN int `db:\"n\"`\n}\n", "use int64"},
		{"struct needs jsonb", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tM map[string]any `db:\"m\"`\n}\n", "jsonb"},
		{"version type", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tV string `db:\"v,version\"`\n}\n", "version columns must be int64"},
		{"duplicate column", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tX string `db:\"x\"`\n\tY string `db:\"x\"`\n}\n", "declared twice"},
		{"missing fk", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tID string `db:\"id,pk\"`\n\tB *B `db:\"belongs:b_id\"`\n}\n" +
			"type B struct {\n\tkeel.Table `db:\"b\"`\n\tID string `db:\"id,pk\"`\n}\n", "has no column b_id"},
		{"relation to non-model", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tID string `db:\"id,pk\"`\n\tBID string `db:\"b_id\"`\n\tB *C `db:\"belongs:b_id\"`\n}\n" +
			"type C struct{ X int }\n", "is not a model"},
		{"to-many needs slice", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tID string `db:\"id,pk\"`\n\tB *B `db:\"many:a_id\"`\n}\n" +
			"type B struct {\n\tkeel.Table `db:\"b\"`\n\tID string `db:\"id,pk\"`\n\tAID string `db:\"a_id\"`\n}\n", "need a slice"},
		{"bad hook", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tID string `db:\"id,pk\"`\n}\nfunc (a *A) BeforeSave() error { return nil }\n", "must be func(context.Context) error"},
		{"unexported", "type A struct {\n\tkeel.Table `db:\"a\"`\n\tid string `db:\"id,pk\"`\n}\n", "must be exported"},
		{"no models", "type A struct{ X int }\n", "no models"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := generate(t, c.models)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestGenerateOutput(t *testing.T) {
	files, err := generate(t, `type Status string

type Account struct {
	keel.Table `+"`db:\"accounts\"`"+`

	ID        string    `+"`db:\"id,pk,autoid:acc_\"`"+`
	Status    Status    `+"`db:\"status\"`"+`
	Tags      []Status  `+"`db:\"tags\"`"+`
	Meta      []string  `+"`db:\"meta,jsonb\"`"+`
	Seen      *time.Time `+"`db:\"seen\"`"+`
	Version   int64     `+"`db:\"version,version\"`"+`
	CreatedAt time.Time `+"`db:\"created_at,autocreate\"`"+`
	Notes     []*Note   `+"`db:\"many:account_id\"`"+`
}

type Note struct {
	keel.Table `+"`db:\"notes\"`"+`

	ID        string   `+"`db:\"id,pk\"`"+`
	AccountID string   `+"`db:\"account_id\"`"+`
	Account   *Account `+"`db:\"belongs:account_id\"`"+`
}

func (n *Note) AfterLoad(ctx context.Context) error { return nil }
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("%d files", len(files))
	}
	src := string(files[0].Content)
	if !strings.HasSuffix(files[0].Path, filepath.Join("models", "modelsdb", "keel_gen.go")) {
		t.Fatalf("path %s", files[0].Path)
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
	if t.Failed() {
		t.Log(src)
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
	tag := `json:"id" db:"id,pk" other:"x"`
	if got := lookupTag(tag, "db"); got != "id,pk" {
		t.Fatal(got)
	}
	if got := lookupTag(tag, "missing"); got != "" {
		t.Fatal(got)
	}
}
