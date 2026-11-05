// Package skills holds keel's agent skill: instructions and references that teach coding agents
// such as Claude Code to write keel code well. The same files are a Claude Code plugin (see
// .claude-plugin at the repository root), and keel skill install writes them into a project or a
// user's skills directory, so the skill an agent reads matches the keel version the project uses.
package skills

import (
	"bufio"
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Name is the keel skill's name, its directory name and the name in its SKILL.md front matter.
const Name = "keel"

//go:embed keel
var files embed.FS

// Keel returns the keel skill's files, with SKILL.md at the root.
func Keel() fs.FS {
	sub, err := fs.Sub(files, Name)
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}

// Install writes the keel skill into dir/keel, where dir is a skills directory such as
// .claude/skills or ~/.claude/skills, and returns the files it wrote, relative to dir/keel.
//
// A keel skill already there (a SKILL.md whose front matter names keel) is replaced whole, so
// files a newer or older version no longer has do not linger. Any other non-empty directory at
// dir/keel is refused unless force is set, and then only the skill's own files are written over.
func Install(dir string, force bool) ([]string, error) {
	target := filepath.Join(dir, Name)
	switch owned, err := isSkill(target); {
	case err != nil:
		return nil, err
	case owned:
		if err := os.RemoveAll(target); err != nil {
			return nil, err
		}
	case !force:
		if empty, err := emptyOrMissing(target); err != nil {
			return nil, err
		} else if !empty {
			return nil, fmt.Errorf("skills: %s exists and is not a keel skill; pass -force to write over it", target)
		}
	}
	var written []string
	err := fs.WalkDir(Keel(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out := filepath.Join(target, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := fs.ReadFile(Keel(), p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return err
		}
		written = append(written, p)
		return nil
	})
	return written, err
}

// isSkill reports whether dir holds a SKILL.md whose front matter names the keel skill.
func isSkill(dir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fm, ok := FrontMatter(b)
	return ok && fm["name"] == Name, nil
}

// emptyOrMissing reports whether dir does not exist or holds nothing.
func emptyOrMissing(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// FrontMatter returns the top-level "key: value" pairs of a SKILL.md's YAML front matter, the
// block between the leading "---" lines, and whether the file has one. Values are returned as
// written, without unquoting; nested and multi-line values are not parsed.
func FrontMatter(b []byte) (map[string]string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return nil, false
	}
	out := map[string]string{}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			return out, true
		}
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return nil, false
}

// Read returns one file of the skill by its slash-separated path ("SKILL.md",
// "references/writing.md").
func Read(name string) ([]byte, error) {
	return fs.ReadFile(Keel(), path.Clean(name))
}
