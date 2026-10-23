package migrate

import (
	"sort"
	"strings"
)

// lexState is where the scanner is in the SQL text.
type lexState uint8

const (
	lexCode lexState = iota
	lexLineComment
	lexBlockComment
	lexString
	lexIdent
	lexDollar
)

// srcLine is one line of a SQL text.
type srcLine struct {
	text  string
	start int
	code  bool
}

// lexed is a SQL text with its comments and literals located, without parsing it.
type lexed struct {
	src    string
	masked string
	lines  []srcLine
}

// lex scans sql. masked holds sql with every comment and the contents of every string and
// dollar-quoted literal replaced by spaces, so keywords can be matched without false positives;
// byte offsets and newlines are kept. A line's code flag is true when it begins outside any
// literal or block comment.
func lex(sql string) lexed {
	s := &scanner{src: sql, out: []byte(sql), code: true}
	for s.i < len(sql) {
		s.step()
	}
	s.lines = append(s.lines, srcLine{text: sql[s.lineStart:], start: s.lineStart, code: s.code})
	return lexed{src: sql, masked: string(s.out), lines: s.lines}
}

// scanner holds the state of one lex call.
type scanner struct {
	src       string
	out       []byte
	i         int
	state     lexState
	depth     int
	tag       string
	escapes   bool
	lines     []srcLine
	lineStart int
	code      bool
}

// step consumes one or more bytes.
func (s *scanner) step() {
	c := s.src[s.i]
	switch s.state {
	case lexCode:
		if s.open() {
			return
		}
	case lexLineComment:
		if c != '\n' {
			s.blank(s.i)
		} else {
			s.state = lexCode
		}
	case lexBlockComment:
		s.blockComment()
		return
	case lexString:
		if s.stringByte() {
			return
		}
	case lexIdent:
		if c == '"' {
			if s.peek(1) == '"' {
				s.i += 2
				return
			}
			s.state = lexCode
		}
	case lexDollar:
		if strings.HasPrefix(s.src[s.i:], s.tag) {
			s.i += len(s.tag)
			s.state = lexCode
			return
		}
		s.blank(s.i)
	}
	s.advance()
}

// open starts a comment or literal at the current byte and reports whether it consumed it.
func (s *scanner) open() bool {
	c, next := s.src[s.i], s.peek(1)
	switch {
	case c == '-' && next == '-':
		s.state = lexLineComment
		s.blank(s.i)
		s.blank(s.i + 1)
		s.i += 2
		return true
	case c == '/' && next == '*':
		s.state, s.depth = lexBlockComment, 1
		s.blank(s.i)
		s.blank(s.i + 1)
		s.i += 2
		return true
	case c == '\'':
		s.state = lexString
		s.escapes = s.i > 0 && (s.src[s.i-1] == 'E' || s.src[s.i-1] == 'e') && (s.i < 2 || !identByte(s.src[s.i-2]))
		s.i++
		return true
	case c == '"':
		s.state = lexIdent
		s.i++
		return true
	case c == '$' && (s.i == 0 || !identByte(s.src[s.i-1])):
		if tag, ok := dollarTag(s.src[s.i:]); ok {
			s.state, s.tag = lexDollar, tag
			s.i += len(tag)
			return true
		}
	}
	return false
}

// blockComment consumes one byte or delimiter of a possibly nested block comment.
func (s *scanner) blockComment() {
	c, next := s.src[s.i], s.peek(1)
	switch {
	case c == '/' && next == '*':
		s.depth++
	case c == '*' && next == '/':
		s.depth--
	default:
		s.blank(s.i)
		s.advance()
		return
	}
	s.blank(s.i)
	s.blank(s.i + 1)
	s.i += 2
	if s.depth == 0 {
		s.state = lexCode
	}
}

// stringByte consumes a byte of a string literal and reports whether it advanced past it.
func (s *scanner) stringByte() bool {
	c := s.src[s.i]
	switch {
	case c == '\\' && s.escapes && s.peek(1) != '\n' && s.peek(1) != 0:
		s.blank(s.i)
		s.blank(s.i + 1)
		s.i += 2
		return true
	case c == '\'' && s.peek(1) == '\'':
		s.blank(s.i)
		s.blank(s.i + 1)
		s.i += 2
		return true
	case c == '\'':
		s.state = lexCode
	default:
		s.blank(s.i)
	}
	return false
}

// advance moves past the current byte, closing the line at a newline.
func (s *scanner) advance() {
	if s.src[s.i] == '\n' {
		s.lines = append(s.lines, srcLine{text: s.src[s.lineStart:s.i], start: s.lineStart, code: s.code})
		s.lineStart = s.i + 1
		s.code = s.state == lexCode
	}
	s.i++
}

// peek returns the byte n positions ahead, or 0 past the end.
func (s *scanner) peek(n int) byte {
	if s.i+n < len(s.src) {
		return s.src[s.i+n]
	}
	return 0
}

// blank replaces the byte at i with a space unless it is a newline.
func (s *scanner) blank(i int) {
	if i < len(s.out) && s.out[i] != '\n' {
		s.out[i] = ' '
	}
}

// identByte reports whether c can be part of an unquoted identifier.
func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') ||
		('0' <= c && c <= '9')
}

// dollarTag returns the dollar-quote delimiter ($$ or $tag$) at the start of s.
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if !identByte(c) || (j == 1 && '0' <= c && c <= '9') {
			return "", false
		}
	}
	return "", false
}

// split splits sql at lines holding only "--keel:split" outside any literal, leaving out the other
// directive lines and empty parts.
func split(sql string) []string {
	var out []string
	l := lex(sql)
	var cur strings.Builder
	flush := func() {
		if part := strings.TrimSpace(cur.String()); part != "" {
			out = append(out, part)
		}
		cur.Reset()
	}
	for _, ln := range l.lines {
		text := strings.TrimSpace(ln.text)
		switch {
		case ln.code && text == "--keel:split":
			flush()
		case ln.code && strings.HasPrefix(text, "--keel:"):
		default:
			cur.WriteString(ln.text)
			cur.WriteByte('\n')
		}
	}
	flush()
	return out
}

// statement is one top-level statement of a SQL text, located by its masked text.
type statement struct {
	text  string
	start int
}

// statements splits the masked text of l at top-level semicolons. Quoted identifiers are the
// only quotes left in the masked text, so a semicolon between double quotes is part of a name.
func (l lexed) statements() []statement {
	var out []statement
	from, quoted := 0, false
	for i := 0; i <= len(l.masked); i++ {
		if i < len(l.masked) && l.masked[i] == '"' {
			quoted = !quoted
		}
		if i < len(l.masked) && (quoted || l.masked[i] != ';') {
			continue
		}
		part := l.masked[from:i]
		if trimmed := strings.TrimLeft(part, " \t\r\n"); strings.TrimSpace(trimmed) != "" {
			out = append(out, statement{text: strings.TrimSpace(trimmed), start: from + len(part) - len(trimmed)})
		}
		from = i + 1
	}
	return out
}

// line returns the 1-based line number of byte offset off.
func (l lexed) line(off int) int {
	return sort.Search(len(l.lines), func(i int) bool { return l.lines[i].start > off })
}
