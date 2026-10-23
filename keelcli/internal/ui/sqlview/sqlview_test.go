package sqlview

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/internal/ui/theme"
)

const src = `--keel:notx
-- a comment
CREATE INDEX CONCURRENTLY idx ON t (a);
SELECT 'it''s', 42 /* block
comment */ FROM t;
CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $body$
BEGIN END;
$body$;`

func TestLex(t *testing.T) {
	var got []string
	for _, tok := range lex(src) {
		if tok.class != plain {
			got = append(got, tok.text)
		}
	}
	joined := strings.Join(got, "|")
	for _, want := range []string{"--keel:notx", "-- a comment", "CREATE", "CONCURRENTLY", "'it''s'", "42", "/* block\ncomment */", "$body$"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tokens lack %q: %s", want, joined)
		}
	}
	for _, tok := range lex(src) {
		if tok.text == "--keel:notx" && tok.class != directive {
			t.Error("directive not classified")
		}
		if tok.text == "idx" && tok.class != plain {
			t.Error("identifier classified")
		}
	}
}

func TestHighlightKeepsText(t *testing.T) {
	lines := Highlight(src, theme.New(true))
	want := strings.Split(src, "\n")
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d", len(lines), len(want))
	}
	for i := range want {
		if got := ansi.Strip(lines[i]); got != want[i] {
			t.Fatalf("line %d: %q, want %q", i+1, got, want[i])
		}
	}
}

func TestRenderNumbersMarksAndTruncates(t *testing.T) {
	lines := Render(src, theme.New(false), 30, 3)
	if len(lines) != 8 {
		t.Fatalf("%d lines", len(lines))
	}
	if got := ansi.Strip(lines[0]); got != "1 │--keel:notx" {
		t.Fatalf("line 1: %q", got)
	}
	if got := ansi.Strip(lines[2]); !strings.HasPrefix(got, "3 ▶CREATE INDEX") {
		t.Fatalf("marked line: %q", got)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > 30 {
			t.Fatalf("line %d is %d cells", i+1, w)
		}
	}
}
