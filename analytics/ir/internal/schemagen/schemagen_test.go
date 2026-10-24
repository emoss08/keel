package schemagen

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics/ir"
)

func TestBuildMatchesTheEmbeddedSchema(t *testing.T) {
	out, err := Build("../..", "../../../formula")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, ir.Schema()) {
		t.Fatal("ir/schema.json is out of date; run go generate ./analytics/ir")
	}
	for _, want := range []string{`"description": "Definition is a report`, `"const": 2`, `"$ref": "#/$defs/FormulaNode"`, `"<"`} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("schema lacks %s", want)
		}
	}
}

type unregistered string

type unsupported struct {
	Ch chan int `json:"ch"`
}

func TestBuildRefusesWhatItCannotDescribe(t *testing.T) {
	g := &generator{docs: map[string]string{}, defs: object{}, enums: map[reflect.Type][]string{}}
	if _, err := g.schema(reflect.TypeFor[unregistered]()); err == nil || !strings.Contains(err.Error(), "no registered values") {
		t.Errorf("unregistered enum: %v", err)
	}
	if _, err := g.schema(reflect.TypeFor[unsupported]()); err == nil || !strings.Contains(err.Error(), "no schema for") {
		t.Errorf("unsupported kind: %v", err)
	}
	if _, err := Build("missing", "missing"); err == nil {
		t.Error("missing source directories are reported")
	}
}
