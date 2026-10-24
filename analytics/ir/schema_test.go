package ir_test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics/ir"
)

type schemaChecker struct {
	defs map[string]any
}

func newChecker(t *testing.T) *schemaChecker {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(ir.Schema(), &root); err != nil {
		t.Fatal(err)
	}
	return &schemaChecker{defs: root["$defs"].(map[string]any)}
}

func (c *schemaChecker) check(s map[string]any, v any, path string) error {
	if ref, ok := s["$ref"].(string); ok {
		return c.check(c.defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any), v, path)
	}
	checks := []func(map[string]any, any, string) error{c.typ, c.enum, c.object, c.array, c.combinators, c.str}
	for _, fn := range checks {
		if err := fn(s, v, path); err != nil {
			return err
		}
	}
	return nil
}

func (c *schemaChecker) typ(s map[string]any, v any, path string) error {
	if want, ok := s["const"]; ok && !reflect.DeepEqual(want, v) {
		return fmt.Errorf("%s: want %v", path, want)
	}
	t, ok := s["type"].(string)
	if !ok {
		return nil
	}
	var good bool
	switch t {
	case "object":
		_, good = v.(map[string]any)
	case "array":
		_, good = v.([]any)
	case "string":
		_, good = v.(string)
	case "boolean":
		_, good = v.(bool)
	case "number":
		_, good = v.(float64)
	case "integer":
		f, isNum := v.(float64)
		good = isNum && f == float64(int64(f))
		if min, ok := s["minimum"].(float64); good && ok && f < min {
			good = false
		}
	}
	if !good {
		return fmt.Errorf("%s: want %s, got %T", path, t, v)
	}
	return nil
}

func (c *schemaChecker) enum(s map[string]any, v any, path string) error {
	values, ok := s["enum"].([]any)
	if !ok {
		return nil
	}
	for _, e := range values {
		if e == v {
			return nil
		}
	}
	return fmt.Errorf("%s: %v is not in the enum", path, v)
}

func (c *schemaChecker) str(s map[string]any, v any, path string) error {
	p, ok := s["pattern"].(string)
	if str, isStr := v.(string); ok && isStr && !regexp.MustCompile(p).MatchString(str) {
		return fmt.Errorf("%s: %q does not match %s", path, str, p)
	}
	return nil
}

func (c *schemaChecker) object(s map[string]any, v any, path string) error {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if max, ok := s["maxProperties"].(float64); ok && float64(len(obj)) > max {
		return fmt.Errorf("%s: too many properties", path)
	}
	for _, r := range asSlice(s["required"]) {
		if _, ok := obj[r.(string)]; !ok {
			return fmt.Errorf("%s: missing %s", path, r)
		}
	}
	props, _ := s["properties"].(map[string]any)
	for k, child := range obj {
		ps, ok := props[k].(map[string]any)
		if !ok {
			if s["additionalProperties"] == false {
				return fmt.Errorf("%s: unexpected property %s", path, k)
			}
			continue
		}
		if err := c.check(ps, child, path+"/"+k); err != nil {
			return err
		}
	}
	return nil
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func (c *schemaChecker) array(s map[string]any, v any, path string) error {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	if min, ok := s["minItems"].(float64); ok && float64(len(arr)) < min {
		return fmt.Errorf("%s: too few items", path)
	}
	if max, ok := s["maxItems"].(float64); ok && float64(len(arr)) > max {
		return fmt.Errorf("%s: too many items", path)
	}
	items, ok := s["items"].(map[string]any)
	if !ok {
		return nil
	}
	for i, x := range arr {
		if err := c.check(items, x, fmt.Sprintf("%s/%d", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func (c *schemaChecker) combinators(s map[string]any, v any, path string) error {
	if one := asSlice(s["oneOf"]); one != nil {
		matched := 0
		for _, alt := range one {
			if c.check(alt.(map[string]any), v, path) == nil {
				matched++
			}
		}
		if matched != 1 {
			return fmt.Errorf("%s: matches %d of oneOf", path, matched)
		}
	}
	if anyOf := asSlice(s["anyOf"]); anyOf != nil {
		ok := false
		for _, alt := range anyOf {
			ok = ok || c.check(alt.(map[string]any), v, path) == nil
		}
		if !ok {
			return fmt.Errorf("%s: matches none of anyOf", path)
		}
	}
	if not, ok := s["not"].(map[string]any); ok && c.check(not, v, path) == nil {
		return fmt.Errorf("%s: matches not", path)
	}
	return nil
}

func (c *schemaChecker) definition(src []byte) error {
	var v any
	if err := json.Unmarshal(src, &v); err != nil {
		return err
	}
	return c.check(c.defs["Definition"].(map[string]any), v, "")
}

const example = `{
  "version": 2, "entity": "shipment",
  "columns": [
    {"id": "customer", "label": "Customer", "dimension": {"field": "customer.name"}},
    {"id": "month", "dimension": {"field": "shippedAt", "bucket": {"param": "grain"}}},
    {"id": "band", "dimension": {"field": "totalCharge", "band": {"edges": [0, 1000]}}},
    {"id": "revenue", "measure": {"field": "totalCharge", "agg": "sum", "filter": {"or": [{"field": "hazmat", "op": "eq", "value": true}]}}},
    {"id": "rpm", "calc": "round([revenue] / [miles], 2)", "format": "money", "display": {"style": "currency"}},
    {"id": "ast", "calc": {"case": {"when": [{"if": {"op": ">", "args": [{"column": "revenue"}, {"number": "0"}]}, "then": {"string": "yes"}}], "else": {"null": true}}}},
    {"id": "share", "window": {"of": "revenue", "fn": "percent_of_total", "partition": ["month"]}, "transform": {"op": "round", "precision": 3}}
  ],
  "filters": {"and": [{"field": "shippedAt", "op": "last_n_days", "param": "window"}, {"calc": "{hazmat}"}, {"or": []}]},
  "having": {"and": [{"column": "revenue", "op": "gt", "value": 1000}]},
  "groupings": {"subtotals": ["customer"], "total": true},
  "top": {"n": {"param": "n"}, "by": "revenue", "per": ["month"]},
  "pivot": {"field": "status", "top": {"n": 5, "by": "revenue"}, "columns": ["revenue"], "other": true},
  "sort": [{"column": "revenue:__other__", "desc": true}],
  "limit": 500, "page": {"size": 50, "after": "abc"},
  "parameters": [{"name": "window", "type": "int", "default": 30}, {"name": "statuses", "type": "enum", "multi": true, "allowed": ["A"]}],
  "charts": [{"id": "c", "type": "bar", "x": "customer", "series": ["revenue"], "goal": {"value": 5}}]
}`

func TestSchemaAcceptsValidDefinitions(t *testing.T) {
	c := newChecker(t)
	if err := c.definition([]byte(example)); err != nil {
		t.Fatalf("example: %v", err)
	}
	raw, err := os.ReadFile("testdata/trenova/canned.json")
	if err != nil {
		t.Fatal(err)
	}
	var reports map[string]json.RawMessage
	_ = json.Unmarshal(raw, &reports)
	for key, r := range reports {
		def, err := ir.Decode(r)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := ir.Encode(def)
		if err := c.definition(out); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
}

func TestSchemaRejectsInvalidShapes(t *testing.T) {
	c := newChecker(t)
	cases := map[string]string{
		`{"version": 2, "columns": []}`:                                                                    "missing entity",
		`{"version": 3, "entity": "x", "columns": []}`:                                                     "want 2",
		`{"version": 2, "entity": "x", "columns": [], "colums": []}`:                                       "unexpected property colums",
		`{"version": 2, "entity": "x", "columns": [{"id": "a", "measure": {"agg": "median"}}]}`:            "not in the enum",
		`{"version": 2, "entity": "x", "columns": [], "filters": {"and": [], "field": "f"}}`:               "oneOf",
		`{"version": 2, "entity": "x", "columns": [], "filters": {"field": "f", "calc": "x", "op": "eq"}}`: "oneOf",
		`{"version": 2, "entity": "x", "columns": [{"id": "a", "calc": {"number": "-1"}}]}`:                "oneOf",
		`{"version": 2, "entity": "x", "columns": [{"id": "a", "calc": {"call": "pow", "args": []}}]}`:     "oneOf",
		`{"version": 2, "entity": "x", "columns": [], "limit": -1}`:                                        "oneOf",
		`{"version": 2, "entity": "x", "columns": [], "limit": {"param": 1}}`:                              "oneOf",
	}
	for src, want := range cases {
		err := c.definition([]byte(src))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", src, err, want)
		}
	}
}
