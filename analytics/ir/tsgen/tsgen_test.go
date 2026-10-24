package tsgen

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics/ir"
)

const golden = "testdata/analytics.ts"

func TestGoldenIsCurrent(t *testing.T) {
	if err := Check(golden); err != nil {
		t.Fatal(err)
	}
	if err := Check("testdata/missing.ts"); err == nil {
		t.Fatal("a missing file is not current")
	}
}

func TestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pkg", "analytics.ts")
	if err := Write(path); err != nil {
		t.Fatal(err)
	}
	if err := Check(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(path); err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("stale file: %v", err)
	}
}

func TestModuleCarriesTheRules(t *testing.T) {
	out, err := Generate(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export const DEFINITION_VERSION = 2;",
		`export type Op = "eq" | "ne"`,
		"export type Definition = {",
		"  columns: Column[];",
		"export type Formula = string | FormulaNode;",
		`"enum": [`,
		`"days"`,
		`"name": "date_diff"`,
		`"sql": "({0} / nullif({1}, 0))"`,
		"export function applyFilters(",
		`  "bool": [`,
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("module lacks %q", want)
		}
	}
	if _, err := Generate(Options{Schema: []byte("{")}); err == nil {
		t.Error("a broken schema is reported")
	}
}

func TestModuleTypeChecks(t *testing.T) {
	tsc, err := exec.LookPath("tsc")
	if err != nil {
		t.Skip("tsc is not installed")
	}
	dir := t.TempDir()
	src, _ := os.ReadFile(golden)
	path := filepath.Join(dir, "analytics.ts")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(tsc, "--noEmit", "--strict", "--target", "es2022", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tsc: %v\n%s", err, out)
	}
}

type parityCase struct {
	Definition *ir.Definition `json:"definition"`
	Filters    []struct {
		Filter ir.DashboardFilter `json:"filter"`
		Value  any                `json:"value"`
	} `json:"filters"`
	Values   map[string]any    `json:"values"`
	Bindings map[string]string `json:"bindings"`
}

const parityCases = `[
  {"definition": {"version": 2, "entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}],
    "filters": {"and": [{"field": "status", "op": "eq", "value": "Completed"}, {"or": [{"field": "status", "op": "eq", "param": "s"}, {"field": "bol", "op": "eq", "value": "B"}]}]},
    "parameters": [{"name": "windowDays", "type": "int"}, {"name": "region", "type": "string"}, {"name": "unset", "type": "string"}]},
   "filters": [
     {"filter": {"id": "f1", "entity": "shipment", "field": "status", "op": "ne"}, "value": "New"},
     {"filter": {"id": "f2", "entity": "shipment", "field": "customer.name", "op": "in"}, "value": ["Acme"]},
     {"filter": {"id": "f3", "entity": "invoice", "field": "status", "op": "eq"}, "value": "Posted"},
     {"filter": {"id": "f4", "entity": "shipment", "field": "proNumber", "op": "eq"}, "value": ""},
     {"filter": {"id": "f5", "entity": "shipment", "field": "weight", "op": "gt"}, "value": 0}
   ],
   "values": {"lookback": 90, "windowDays": 30, "region": "West", "unset": null},
   "bindings": {"windowDays": "lookback"}},
  {"definition": {"version": 2, "entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}]},
   "filters": [{"filter": {"id": "f1", "entity": "shipment", "field": "status", "op": "eq"}, "value": "New"}]},
  {"definition": {"version": 2, "entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}],
    "filters": {"and": [{"field": "status", "op": "eq", "value": "Completed"}]}},
   "filters": [{"filter": {"id": "f1", "entity": "shipment", "field": "status", "op": "eq"}, "value": "New"}]},
  {"definition": {"version": 2, "entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}]},
   "filters": [{"filter": {"id": "f1", "entity": "shipment", "field": "status", "op": "eq"}, "value": []}]}
]`

const parityRunner = `import { applyFilters } from "./analytics.ts";
import { readFileSync } from "node:fs";
const cases = JSON.parse(readFileSync(process.argv[2], "utf8"));
const results = cases.map((c) => {
  const r = applyFilters(c.definition, c.filters, { values: c.values, bindings: c.bindings });
  return { definition: r.definition, params: r.params ?? null };
});
console.log(JSON.stringify(results));
`

func TestApplyFiltersMatchesTheServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	src, _ := os.ReadFile(golden)
	files := map[string]string{"analytics.ts": string(src), "run.ts": parityRunner, "cases.json": parityCases, "package.json": `{"type": "module"}`}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", filepath.Join(dir, "run.ts"), filepath.Join(dir, "cases.json"))
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("node cannot run TypeScript here: %v", err)
	}
	var fromTS []map[string]any
	if err := json.Unmarshal(out, &fromTS); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var cases []parityCase
	if err := json.Unmarshal([]byte(parityCases), &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		var filters []ir.FilterValue
		for _, f := range c.Filters {
			filters = append(filters, ir.FilterValue{Filter: f.Filter, Value: f.Value})
		}
		def, params := ir.ApplyFilters(c.Definition, filters, ir.Params{Values: c.Values, Bindings: c.Bindings})
		goJSON, _ := json.Marshal(map[string]any{"definition": def, "params": params})
		var fromGo map[string]any
		_ = json.Unmarshal(goJSON, &fromGo)
		if !reflect.DeepEqual(fromGo, fromTS[i]) {
			t.Errorf("case %d differs\nGo: %v\nTS: %v", i, fromGo, fromTS[i])
		}
	}
}
