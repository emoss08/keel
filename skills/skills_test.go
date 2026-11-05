package skills_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/emoss08/keel/skills"
)

func TestFrontMatter(t *testing.T) {
	b, err := skills.Read("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	fm, ok := skills.FrontMatter(b)
	if !ok {
		t.Fatal("SKILL.md has no front matter")
	}
	if fm["name"] != skills.Name {
		t.Fatalf("name %q, want %q", fm["name"], skills.Name)
	}
	// Claude Code truncates description and when_to_use together at 1,536 characters.
	if d := fm["description"]; d == "" || len(d) > 1536 {
		t.Fatalf("description is %d characters, want 1 to 1536", len(d))
	}
	if n := strings.Count(string(b), "\n"); n > 500 {
		t.Errorf("SKILL.md has %d lines; keep it under 500 and move detail into references", n)
	}
}

// TestLinks checks that every relative link in the skill names a file the skill holds, and that
// every reference file is linked from SKILL.md.
func TestLinks(t *testing.T) {
	link := regexp.MustCompile(`\]\(([^)#\s]+)\)`)
	code := regexp.MustCompile("(?s)```.*?```|`[^`\n]*`") // links inside code are code
	linked := map[string]bool{}
	err := fs.WalkDir(skills.Keel(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := fs.ReadFile(skills.Keel(), p)
		if err != nil {
			return err
		}
		for _, m := range link.FindAllStringSubmatch(code.ReplaceAllString(string(b), ""), -1) {
			target := m[1]
			if strings.Contains(target, "://") {
				continue
			}
			full := filepath.ToSlash(filepath.Join(filepath.Dir(p), target))
			if _, err := fs.Stat(skills.Keel(), full); err != nil {
				t.Errorf("%s links to %s, which the skill does not hold", p, target)
			}
			if p == "SKILL.md" {
				linked[full] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := fs.Glob(skills.Keel(), "references/*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Fatal("no reference files")
	}
	for _, r := range refs {
		if !linked[r] {
			t.Errorf("SKILL.md does not link %s", r)
		}
	}
}

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	written, err := skills.Install(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(written, "SKILL.md") {
		t.Fatalf("written %v lacks SKILL.md", written)
	}
	for _, p := range written {
		want, err := skills.Read(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, skills.Name, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from the embedded file", p)
		}
	}

	// A reinstall replaces the skill whole: a file an older version had is removed.
	stale := filepath.Join(dir, skills.Name, "references", "removed.md")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := skills.Install(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file survived a reinstall: %v", err)
	}
}

func TestInstallRefusesForeignDirectory(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, skills.Name)
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(other, "notes.md")
	if err := os.WriteFile(mine, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := skills.Install(dir, false); err == nil {
		t.Fatal("Install wrote over a directory that is not a keel skill")
	}
	if _, err := skills.Install(dir, true); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(mine); err != nil || string(b) != "mine" {
		t.Fatalf("-force removed a file the skill does not own: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(other, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}
