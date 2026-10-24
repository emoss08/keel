// Command gen writes the generated files of analytics/ir: the JSON Schema, the TypeScript golden
// module and the formula reference in the guide. go generate runs it from analytics/ir.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir/internal/schemagen"
	"github.com/emoss08/keel/analytics/ir/tsgen"
)

const (
	guideStart = "<!-- keel:formula-reference:start -->"
	guideEnd   = "<!-- keel:formula-reference:end -->"
)

func main() {
	files, err := outputs(".")
	if err == nil {
		err = write(files)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func write(files map[string][]byte) error {
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func outputs(root string) (map[string][]byte, error) {
	schema, err := schemagen.Build(root, filepath.Join(root, "..", "formula"))
	if err != nil {
		return nil, err
	}
	guidePath := filepath.Join(root, "..", "..", "docs", "guide.md")
	guide, err := os.ReadFile(guidePath)
	if err != nil {
		return nil, err
	}
	updated, err := replaceBlock(guide, formula.Reference())
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{filepath.Join(root, "schema.json"): schema, guidePath: updated}
	ts, err := tsgen.Generate(tsgen.Options{Schema: schema})
	if err != nil {
		return nil, err
	}
	files[filepath.Join(root, "tsgen", "testdata", "analytics.ts")] = ts
	return files, nil
}

func replaceBlock(doc []byte, body string) ([]byte, error) {
	start := bytes.Index(doc, []byte(guideStart))
	end := bytes.Index(doc, []byte(guideEnd))
	if start < 0 || end < start {
		return nil, errors.New("docs/guide.md has no formula reference markers")
	}
	var out bytes.Buffer
	out.Write(doc[:start+len(guideStart)])
	out.WriteString("\n\n" + body + "\n")
	out.Write(doc[end:])
	return out.Bytes(), nil
}
