// Package sqlview highlights migration SQL for the terminal: keywords, strings, numbers, comments
// and keel directives, with line numbers and a marked line for lint findings.
//
// It is a small lexer rather than a general highlighter because migration SQL needs only those
// classes, the colours must come from keelcli's theme, and --keel: directives deserve their own
// colour, which a general SQL lexer does not know.
package sqlview

import (
	"fmt"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/internal/ui/theme"
)

type class uint8

const (
	plain class = iota
	keyword
	str
	number
	comment
	directive
)

type token struct {
	class class
	text  string
}

var keywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`ADD ALL ALTER ALWAYS AND ANY AS ASC BEGIN BETWEEN BIGINT BIGSERIAL BOOLEAN BY
		CASCADE CASE CHECK COLUMN COMMENT COMMIT CONCURRENTLY CONSTRAINT COPY CREATE CROSS CURRENT_TIMESTAMP
		DATE DECLARE DEFAULT DEFERRABLE DELETE DESC DISTINCT DO DROP EACH ELSE ENABLE END ENUM EXCEPTION
		EXECUTE EXISTS EXTENSION FALSE FOR FOREIGN FROM FULL FUNCTION GENERATED GRANT GROUP HAVING IDENTITY
		IF IMMUTABLE IN INDEX INNER INSERT INTEGER INTO IS JOIN JSONB KEY LANGUAGE LEFT LEVEL LIKE LIMIT LOCK
		LOOP MODE NOT NOTHING NULL NUMERIC OF ON OR ORDER OUTER OWNER PARTITION PERFORM POLICY PRIMARY
		PROCEDURE RAISE RECORD REFERENCES RENAME REPLACE RESTRICT RETURN RETURNING RETURNS REVOKE RIGHT ROLE ROW
		SCHEMA SECURITY SELECT SEQUENCE SERIAL SET SMALLINT STABLE TABLE TEXT THEN TIMESTAMPTZ TO TRIGGER TRUE
		TYPE UNION UNIQUE UPDATE USING UUID VALIDATE VALID VALUE VALUES VARCHAR VIEW VOID VOLATILE WHEN WHERE
		WITH WITHOUT DATERANGE TIMESTAMP INTERVAL PLPGSQL SQL`) {
		keywords[k] = true
	}
}

func lex(src string) []token {
	var out []token
	emit := func(c class, s string) {
		if s == "" {
			return
		}
		if n := len(out); n > 0 && out[n-1].class == c {
			out[n-1].text += s
			return
		}
		out = append(out, token{c, s})
	}
	i := 0
	for i < len(src) {
		rest := src[i:]
		switch {
		case strings.HasPrefix(rest, "--"):
			end := strings.IndexByte(rest, '\n')
			if end < 0 {
				end = len(rest)
			}
			c := comment
			if strings.HasPrefix(rest, "--keel:") {
				c = directive
			}
			emit(c, rest[:end])
			i += end
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			n := len(rest)
			if end >= 0 {
				n = end + 4
			}
			emit(comment, rest[:n])
			i += n
		case rest[0] == '\'':
			n := quoted(rest)
			emit(str, rest[:n])
			i += n
		case rest[0] == '$':
			if tag := dollarTag(rest); tag != "" {
				emit(str, tag)
				i += len(tag)
				continue
			}
			emit(plain, "$")
			i++
		case isDigit(rest[0]):
			n := 0
			for n < len(rest) && (isDigit(rest[n]) || rest[n] == '.') {
				n++
			}
			emit(number, rest[:n])
			i += n
		case isWord(rune(rest[0])):
			n := 0
			for n < len(rest) && (isWord(rune(rest[n])) || isDigit(rest[n])) {
				n++
			}
			w := rest[:n]
			if keywords[strings.ToUpper(w)] {
				emit(keyword, w)
			} else {
				emit(plain, w)
			}
			i += n
		default:
			emit(plain, rest[:1])
			i++
		}
	}
	return out
}

func quoted(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			i++
			continue
		}
		return i + 1
	}
	return len(s)
}

func dollarTag(s string) string {
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '$':
			return s[:i+1]
		case !isWord(rune(s[i])) && !isDigit(s[i]):
			return ""
		}
	}
	return ""
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) }

// Highlight styles src line by line.
func Highlight(src string, t theme.Theme) []string {
	styles := map[class]lipgloss.Style{
		plain: t.Text, keyword: t.SQLKeyword, str: t.SQLString, number: t.SQLNumber,
		comment: t.SQLComment, directive: t.SQLDirective,
	}
	lines := []string{""}
	for _, tok := range lex(strings.TrimRight(src, "\n")) {
		parts := strings.Split(tok.text, "\n")
		for i, part := range parts {
			if i > 0 {
				lines = append(lines, "")
			}
			if part != "" {
				lines[len(lines)-1] += styles[tok.class].Render(part)
			}
		}
	}
	return lines
}

// Render highlights src with line numbers, truncating lines to width and marking line mark (1-based;
// 0 marks none).
func Render(src string, t theme.Theme, width, mark int) []string {
	lines := Highlight(src, t)
	gutter := len(fmt.Sprint(len(lines)))
	out := make([]string, len(lines))
	for i, l := range lines {
		num := t.LineNumber.Render(fmt.Sprintf("%*d ", gutter, i+1))
		body := ansi.Truncate(l, max(width-gutter-2, 1), "…")
		row := num + "│" + body
		if i+1 == mark {
			pad := max(width-ansi.StringWidth(row), 0)
			row = t.LineMark.Render(ansi.Strip(fmt.Sprintf("%*d ", gutter, i+1)) + "▶" + ansi.Strip(body) + strings.Repeat(" ", pad))
		}
		out[i] = row
	}
	return out
}
