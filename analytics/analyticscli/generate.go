package analyticscli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir/tsgen"
	"github.com/emoss08/keel/keelcli"
)

// ts writes or checks the TypeScript module.
func (e *env) ts(args []string) error {
	path, check, err := e.outputFlags("ts", args, true)
	if err != nil {
		return err
	}
	if path == "" {
		return keelcli.Usagef("analyticscli: ts: -out is required")
	}
	if check {
		if err := tsgen.Check(path); err != nil {
			return problems(fmt.Errorf("analyticscli: ts: %w", err))
		}
		return e.wrote(path, true)
	}
	if err := tsgen.Write(path); err != nil {
		return fmt.Errorf("analyticscli: ts: %w", err)
	}
	return e.wrote(path, false)
}

// i18n writes or checks the message catalog.
func (e *env) i18n(args []string) error {
	path, check, err := e.outputFlags("i18n", args, true)
	if err != nil {
		return err
	}
	if path == "" {
		return keelcli.Usagef("analyticscli: i18n: -out is required")
	}
	cat, err := e.catalog()
	if err != nil {
		return err
	}
	if check {
		if err := CheckMessages(path, cat); err != nil {
			return problems(err)
		}
		return e.wrote(path, true)
	}
	if err := WriteMessages(path, cat); err != nil {
		return err
	}
	return e.wrote(path, false)
}

// describe writes the description to stdout or a file.
func (e *env) describe(args []string) error {
	path, _, err := e.outputFlags("describe", args, false)
	if err != nil {
		return err
	}
	cat, err := e.catalog()
	if err != nil {
		return err
	}
	text := Describe(cat, e.cfg.Rollups...)
	if path == "" {
		_, err := e.out.Write([]byte(text))
		return err
	}
	if err := writeFile(path, []byte(text)); err != nil {
		return fmt.Errorf("analyticscli: describe: %w", err)
	}
	return e.wrote(path, false)
}

// encodeMessages returns the catalog as indented JSON with sorted keys and unescaped HTML
// characters, ending in a newline.
func encodeMessages(m map[string]string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// catalog builds the catalog of the configured tables.
func (e *env) catalog() (*analytics.Catalog, error) {
	if len(e.cfg.Tables) == 0 {
		return nil, keelcli.Usagef("analyticscli: Config.Tables is empty; pass keeltables.All()")
	}
	cat, err := analytics.Build(e.cfg.Tables, analytics.Options{})
	if err != nil {
		return nil, fmt.Errorf("analyticscli: %w", err)
	}
	return cat, nil
}

// WriteMessages writes the catalog's messages (Catalog.Messages: every translation key the engine
// asks Options.Translate for, with its fallback text) to path as a JSON object sorted by key,
// creating the file's directory.
func WriteMessages(path string, cat *analytics.Catalog) error {
	data, err := encodeMessages(cat.Messages())
	if err != nil {
		return fmt.Errorf("analyticscli: i18n: %w", err)
	}
	if err := writeFile(path, data); err != nil {
		return fmt.Errorf("analyticscli: i18n: %w", err)
	}
	return nil
}

// CheckMessages returns an error naming the keys that differ when the file at path is not what
// WriteMessages writes.
func CheckMessages(path string, cat *analytics.Catalog) error {
	want := cat.Messages()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("analyticscli: i18n: %w", err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		return fmt.Errorf("analyticscli: i18n: %s: %w", path, err)
	}
	var diff []string
	for k, v := range want {
		if got[k] != v {
			diff = append(diff, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			diff = append(diff, k)
		}
	}
	if len(diff) > 0 {
		slices.Sort(diff)
		return fmt.Errorf("analyticscli: i18n: %s is out of date at %v; regenerate it", path, diff)
	}
	if canonical, err := encodeMessages(want); err == nil && !bytes.Equal(canonical, data) {
		return fmt.Errorf("analyticscli: i18n: %s is not formatted as generated; regenerate it", path)
	}
	return nil
}

// writeFile writes data to path, creating its directory.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
