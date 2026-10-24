package main

import (
	"bytes"
	"os"
	"testing"
)

func TestGeneratedFilesAreCurrent(t *testing.T) {
	files, err := outputs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is out of date; run go generate ./analytics/ir", path)
		}
	}
}

func TestReplaceBlock(t *testing.T) {
	out, err := replaceBlock([]byte("a\n"+guideStart+"\nold\n"+guideEnd+"\nb\n"), "new")
	if err != nil || string(out) != "a\n"+guideStart+"\n\nnew\n"+guideEnd+"\nb\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := replaceBlock([]byte("no markers"), "x"); err == nil {
		t.Fatal("missing markers")
	}
}
