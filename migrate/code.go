package migrate

import (
	"fmt"
	"strings"
)

// maxDiffLines bounds the lines of each side a diff shows, so a rewritten file stays readable in
// an error message.
const maxDiffLines = 40

// codeOf returns what sql runs: its --keel: directives and its text without comments, with each
// run of whitespace outside a literal reduced to one space. Two files with the same code differ
// only in ways the server cannot see, so its checksum tells a comment or formatting edit from one
// that changes the migration. String, dollar-quoted and quoted-identifier text is kept as written,
// comments inside a function body included, since that text reaches the server.
func codeOf(sql string) string {
	var b strings.Builder
	for _, ln := range lex(sql).lines {
		if text := strings.TrimSpace(ln.text); ln.code && strings.HasPrefix(text, "--keel:") {
			b.WriteString(strings.Join(strings.Fields(text), " "))
			b.WriteByte('\n')
		}
	}
	s := &scanner{src: sql, out: []byte(sql), code: true}
	gap := false
	for s.i < len(sql) {
		from, state := s.i, s.state
		s.step()
		switch {
		case isComment(state) || isComment(s.state):
			gap = true
		case state == lexCode && s.state == lexCode && strings.TrimSpace(sql[from:s.i]) == "":
			gap = true
		default:
			if gap && b.Len() > 0 {
				b.WriteByte(' ')
			}
			gap = false
			b.WriteString(sql[from:s.i])
		}
	}
	return b.String()
}

// isComment reports whether state is inside a comment.
func isComment(state lexState) bool {
	return state == lexLineComment || state == lexBlockComment
}

// lineDiff shows the lines that differ between two texts, without the lines they share at the
// start and end: "- " for a line of before, "+ " for a line of after, each side capped.
func lineDiff(before, after string) string {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	end := 0
	for end < len(a)-start && end < len(b)-start && a[len(a)-1-end] == b[len(b)-1-end] {
		end++
	}
	var out strings.Builder
	side := func(sign string, lines []string) {
		for i, line := range lines {
			if i == maxDiffLines {
				fmt.Fprintf(&out, "  %s … %d more lines\n", sign, len(lines)-i)
				return
			}
			fmt.Fprintf(&out, "  %s %s\n", sign, line)
		}
	}
	side("-", a[start:len(a)-end])
	side("+", b[start:len(b)-end])
	return strings.TrimRight(out.String(), "\n")
}
