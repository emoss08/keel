package keel

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestUnmarshalNumberKeepsTheLiteral(t *testing.T) {
	var got map[string]any
	if err := unmarshalNumber([]byte(`{"id": 9007199254740993, "ok": true} `), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != json.Number("9007199254740993") || got["ok"] != true {
		t.Fatalf("decoded %#v", got)
	}
}

func TestUnmarshalNumberRejectsWhatUnmarshalRejects(t *testing.T) {
	var got map[string]any
	if err := unmarshalNumber([]byte(`{"a": 1} {"b": 2}`), &got); !errors.Is(err, errTrailingJSON) {
		t.Fatalf("trailing value: %v", err)
	}
	if err := unmarshalNumber([]byte(`{"a": 1} x`), &got); !errors.Is(err, errTrailingJSON) {
		t.Fatalf("trailing text: %v", err)
	}
	if err := unmarshalNumber([]byte(`{"a":`), &got); err == nil {
		t.Fatal("a truncated value decoded")
	}
	if err := json.Unmarshal([]byte(`{"a": 1} x`), &got); err == nil {
		t.Fatal("json.Unmarshal accepted trailing text")
	}
}
