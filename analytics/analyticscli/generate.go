package analyticscli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/emoss08/keel/analytics/ir/tsgen"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/keelcli"
	"github.com/emoss08/keel/schema"
)

// ts writes or checks the TypeScript module.
func (e *env) ts(args []string) error {
	path, check, err := e.outputFlags("ts", args, true)
	if err != nil {
		return err
	}
	if path == "" {
		return keelcli.Usagef("analytics ts: -out is required")
	}
	if check {
		if err := tsgen.Check(path); err != nil {
			return problems(fmt.Errorf("analytics ts: %w", err))
		}
		return e.wrote(path, true)
	}
	if err := tsgen.Write(path); err != nil {
		return fmt.Errorf("analytics ts: %w", err)
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
		return keelcli.Usagef("analytics i18n: -out is required")
	}
	if check {
		if err := CheckMessages(path, e.cfg.Tables, e.cfg.Keys); err != nil {
			return problems(err)
		}
		return e.wrote(path, true)
	}
	if err := WriteMessages(path, e.cfg.Tables, e.cfg.Keys); err != nil {
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
	text := Describe(e.cfg.Tables, e.cfg.Keys, e.cfg.Rollups...)
	if path == "" {
		_, err := e.out.Write([]byte(text))
		return err
	}
	if err := writeFile(path, []byte(text)); err != nil {
		return fmt.Errorf("analytics describe: %w", err)
	}
	return e.wrote(path, false)
}

// Messages returns the translation keys of the reportable tables' labels with the derived text as
// each key's fallback:
//
//	shipment                         the entity's label           Shipment
//	shipment@plural                  its plural                   Shipments
//	shipment@description             its doc comment
//	shipment.proNumber               a field's label              PRO Number
//	shipment.proNumber@description   the field's doc comment
//	shipment.status.InTransit        an enum value's label        In Transit
//	shipment.customer                a relation's label           Customer
//	@category.Operations             a report category            Operations
//
// Keys are the catalog's entity, field and edge keys; a field, relation or value with nothing to
// say is left out. Hidden fields, relations marked noreport and relations to tables that are not
// reportable are left out too.
func Messages(tables []*schema.Table, keys rollup.Keys) map[string]string {
	keys = keys.WithDefaults()
	reportable := map[*schema.Table]bool{}
	for _, t := range tables {
		reportable[t] = t.Report != nil
	}
	out := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	for _, t := range tables {
		if t.Report == nil {
			continue
		}
		ent := keys.Entity(t)
		put(ent, t.Label)
		put(ent+"@plural", t.Plural)
		put(ent+"@description", t.Doc)
		if t.Report.Category != "" {
			put("@category."+t.Report.Category, t.Report.Category)
		}
		for i := range t.Columns {
			c := &t.Columns[i]
			if c.Report == nil || !c.Report.Included {
				continue
			}
			f := ent + "." + keys.Field(t, int32(i))
			put(f, c.Label)
			put(f+"@description", c.Doc)
			for _, v := range c.Enum {
				put(f+"."+v.Value, v.Label)
			}
		}
		for i := range t.Relations {
			r := &t.Relations[i]
			if r.NoReport || !reportable[r.Target] {
				continue
			}
			e := ent + "." + keys.Edge(t, int32(i))
			put(e, r.Label)
			put(e+"@description", r.Doc)
		}
	}
	return out
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

// WriteMessages writes Messages to path as a JSON object of key to fallback text, sorted by key,
// creating the file's directory.
func WriteMessages(path string, tables []*schema.Table, keys rollup.Keys) error {
	data, err := encodeMessages(Messages(tables, keys))
	if err != nil {
		return fmt.Errorf("analytics i18n: %w", err)
	}
	if err := writeFile(path, data); err != nil {
		return fmt.Errorf("analytics i18n: %w", err)
	}
	return nil
}

// CheckMessages returns an error naming the keys that differ when the file at path is not what
// WriteMessages writes.
func CheckMessages(path string, tables []*schema.Table, keys rollup.Keys) error {
	want := Messages(tables, keys)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("analytics i18n: %w", err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		return fmt.Errorf("analytics i18n: %s: %w", path, err)
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
		return fmt.Errorf("analytics i18n: %s is out of date at %v; regenerate it", path, diff)
	}
	if canonical, err := encodeMessages(want); err == nil && !bytes.Equal(canonical, data) {
		return fmt.Errorf("analytics i18n: %s is not formatted as generated; regenerate it", path)
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
