package keel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// big is an integer past 2^53, which a float64 cannot hold exactly.
const big = "9007199254740993"

// TestJSONNumberKeepsLargeIntegers reads jsonnumber columns back with every digit: a jsonb map, a
// nullzero slice, a pointer over a json column, nested values, NULL into the nullzero and pointer
// fields, and rows a bulk update left. A jsonb column without the option still decodes numbers as
// float64.
func TestJSONNumberKeepsLargeIntegers(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	var raw any = map[string]any{"n": json.Number(big)}
	e := &fleet.AuditEntry{Tool: "dispatch", Args: map[string]any{"load": json.Number(big), "nested": []any{json.Number("1.5")}},
		Result: []any{json.Number(big)}, Raw: &raw, Meta: map[string]any{"n": json.Number(big)}}
	ok(t, fleetdb.AuditEntries.Create(ctx, d, e))
	empty := &fleet.AuditEntry{Tool: "noop"}
	ok(t, fleetdb.AuditEntries.Create(ctx, d, empty))
	a := fleetdb.AuditEntries
	got, err := a.Where(a.ID.Eq(e.ID)).One(ctx, d)
	ok(t, err)
	if got.Args["load"] != json.Number(big) || !reflect.DeepEqual(got.Args["nested"], []any{json.Number("1.5")}) {
		t.Fatalf("args %#v", got.Args)
	}
	if !reflect.DeepEqual(got.Result, []any{json.Number(big)}) {
		t.Fatalf("result %#v", got.Result)
	}
	if got.Raw == nil || !reflect.DeepEqual(*got.Raw, map[string]any{"n": json.Number(big)}) {
		t.Fatalf("raw (a json column) %#v", got.Raw)
	}
	if f, isFloat := got.Meta["n"].(float64); !isFloat || f != 9007199254740992 {
		t.Fatalf("a jsonb column without jsonnumber decoded %#v", got.Meta["n"])
	}
	none, err := a.Where(a.ID.Eq(empty.ID)).One(ctx, d)
	ok(t, err)
	if none.Result != nil || none.Raw != nil || none.Args == nil || len(none.Args) != 0 {
		t.Fatalf("NULL and empty values: %#v", none)
	}
	_, err = a.Where(a.ID.Eq(e.ID)).Update(ctx, d, a.Tool.To("redispatch"))
	ok(t, err)
	rows, err := a.Where(a.Tool.Eq("redispatch")).Limit(10).List(ctx, d)
	ok(t, err)
	if len(rows) != 1 || rows[0].Args["load"] != json.Number(big) {
		t.Fatalf("after update %#v", rows)
	}
}

// TestJSONNumberUsesTheConfiguredDecoder decodes jsonnumber columns with
// Config.JSON.UnmarshalNumber and every other jsonb column with Config.JSON.Unmarshal.
func TestJSONNumberUsesTheConfiguredDecoder(t *testing.T) {
	db(t)
	var numbers, plain atomic.Int64
	cfg := testConfig(testDSN)
	cfg.JSON = &keel.JSONCodec{
		Marshal: json.Marshal,
		Unmarshal: func(data []byte, v any) error {
			plain.Add(1)
			return json.Unmarshal(data, v)
		},
		UnmarshalNumber: func(data []byte, v any) error {
			numbers.Add(1)
			d := json.NewDecoder(bytes.NewReader(data))
			d.UseNumber()
			return d.Decode(v)
		},
	}
	d, err := keel.Open(context.Background(), cfg)
	ok(t, err)
	t.Cleanup(d.Close)
	ctx := newTenant()
	e := &fleet.AuditEntry{Tool: "t", Args: map[string]any{"n": json.Number(big)}, Meta: map[string]any{}}
	ok(t, fleetdb.AuditEntries.Create(ctx, d, e))
	got, err := fleetdb.AuditEntries.Where(fleetdb.AuditEntries.ID.Eq(e.ID)).One(ctx, d)
	ok(t, err)
	if got.Args["n"] != json.Number(big) || numbers.Load() != 1 || plain.Load() != 1 {
		t.Fatalf("args %#v, UnmarshalNumber called %d times, Unmarshal %d", got.Args, numbers.Load(), plain.Load())
	}
	if d.JSON().UnmarshalNumber == nil || db(t).JSON().UnmarshalNumber == nil {
		t.Fatal("DB.JSON has no UnmarshalNumber")
	}
}

// TestDefaultJSONMatchesEncodingJSON checks that the default codec, used when Config.JSON is
// unset, encodes to the bytes encoding/json gives and decodes jsonnumber values to json.Number,
// rejecting what encoding/json rejects.
func TestDefaultJSONMatchesEncodingJSON(t *testing.T) {
	c := db(t).JSON()
	in := map[string]any{"z": 1, "a": "<b>&", "m": []any{1.5, nil, true}, "n": map[string]any{"y": "é", "b": 2}}
	got, err := c.Marshal(in)
	ok(t, err)
	want, err := json.Marshal(in)
	ok(t, err)
	if string(got) != string(want) {
		t.Fatalf("marshaled %s, encoding/json gives %s", got, want)
	}
	var v any
	ok(t, c.UnmarshalNumber([]byte(` {"a": [`+big+`, 2.50]} `), &v))
	if !reflect.DeepEqual(v, map[string]any{"a": []any{json.Number(big), json.Number("2.50")}}) {
		t.Fatalf("decoded %#v", v)
	}
	for _, bad := range []string{`{} {}`, `{`, `{"a":1,}`} {
		if c.Unmarshal([]byte(bad), &v) == nil || c.UnmarshalNumber([]byte(bad), &v) == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
}

// TestJSONStringValuesBindAsJSONStrings writes a Go string held in an interface to a json column
// through every write path: it is a JSON string value, not JSON text the caller pre-encoded.
func TestJSONStringValuesBindAsJSONStrings(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	a := fleetdb.AuditEntries
	var raw any = "Active"
	e := &fleet.AuditEntry{Tool: "string", Args: map[string]any{}, Raw: &raw, Meta: map[string]any{}}
	ok(t, a.Create(ctx, d, e))
	got := must(a.Where(a.ID.Eq(e.ID)).One(ctx, d))(t)
	if got.Raw == nil || *got.Raw != "Active" {
		t.Fatalf("created raw %#v", got.Raw)
	}

	raw = "Lapsed"
	ok(t, a.Save(ctx, d, e, a.Raw))
	got = must(a.Where(a.ID.Eq(e.ID)).One(ctx, d))(t)
	if *got.Raw != "Lapsed" {
		t.Fatalf("saved raw %#v", *got.Raw)
	}

	n := must(a.Where(a.ID.Eq(e.ID)).Update(ctx, d, a.Raw.To("Cleared")))(t)
	eq(t, n, int64(1))
	got = must(a.Where(a.ID.Eq(e.ID)).One(ctx, d))(t)
	if *got.Raw != "Cleared" {
		t.Fatalf("updated raw %#v", *got.Raw)
	}

	pre := &fleet.AuditEntry{Tool: "raw", Args: map[string]any{}, Meta: map[string]any{}}
	var encoded any = json.RawMessage(`{"pre":"encoded"}`)
	pre.Raw = &encoded
	ok(t, a.Create(ctx, d, pre))
	got = must(a.Where(a.ID.Eq(pre.ID)).One(ctx, d))(t)
	if !reflect.DeepEqual(*got.Raw, map[string]any{"pre": "encoded"}) {
		t.Fatalf("a json.RawMessage is written as the JSON it holds: %#v", *got.Raw)
	}
}
