package formula

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokNumber
	tokString
	tokIdent
	tokColumn
	tokField
	tokParam
	tokLParen
	tokRParen
	tokComma
	tokOp
)

type token struct {
	kind  tokenKind
	text  string
	pos   Pos
	end   Pos
	upper string
}

type lexer struct {
	src  string
	off  int
	line int
	col  int
}

func newLexer(src string) *lexer { return &lexer{src: src, line: 1, col: 1} }

func (l *lexer) pos() Pos { return Pos{Offset: l.off, Line: l.line, Column: l.col} }

func (l *lexer) peekRune() rune {
	if l.off >= len(l.src) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.off:])
	return r
}

func (l *lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(l.src[l.off:])
	l.off += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *lexer) skipSpace() {
	for l.off < len(l.src) && unicode.IsSpace(l.peekRune()) {
		l.advance()
	}
}

func (l *lexer) next() (token, *Error) {
	l.skipSpace()
	start := l.pos()
	if l.off >= len(l.src) {
		return token{kind: tokEOF, pos: start, end: start}, nil
	}
	r := l.peekRune()
	switch {
	case isDigit(r) || (r == '.' && l.off+1 < len(l.src) && isDigit(rune(l.src[l.off+1]))):
		return l.number(start)
	case isIdentStart(r):
		return l.ident(start), nil
	case r == '\'':
		return l.str(start)
	case r == '[':
		return l.delimited(start, tokColumn, ']', "column reference", isIdentPart)
	case r == '{':
		return l.delimited(start, tokField, '}', "field reference", isPathPart)
	case r == '@':
		return l.param(start)
	}
	return l.punct(start)
}

func (l *lexer) tok(kind tokenKind, start Pos, text string) token {
	return token{kind: kind, text: text, pos: start, end: l.pos(), upper: strings.ToUpper(text)}
}

func (l *lexer) number(start Pos) (token, *Error) {
	begin := l.off
	for isDigit(l.peekRune()) {
		l.advance()
	}
	if l.peekRune() == '.' {
		l.advance()
		if !isDigit(l.peekRune()) {
			return token{}, errorAt(CodeSyntax, start, l.pos(), "a number needs digits after the decimal point")
		}
		for isDigit(l.peekRune()) {
			l.advance()
		}
	}
	if r := l.peekRune(); r == 'e' || r == 'E' {
		return token{}, errorAt(CodeSyntax, start, l.pos(), "write numbers in full; scientific notation is not supported")
	}
	if isIdentStart(l.peekRune()) {
		return token{}, errorAt(CodeSyntax, start, l.pos(), "a number cannot run into a name; add a space or an operator")
	}
	return l.tok(tokNumber, start, canonicalNumber(l.src[begin:l.off])), nil
}

func canonicalNumber(s string) string {
	whole, frac, hasFrac := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	if hasFrac {
		return whole + "." + frac
	}
	return whole
}

func (l *lexer) ident(start Pos) token {
	begin := l.off
	for isIdentPart(l.peekRune()) {
		l.advance()
	}
	return l.tok(tokIdent, start, l.src[begin:l.off])
}

func (l *lexer) str(start Pos) (token, *Error) {
	l.advance()
	var b strings.Builder
	for {
		if l.off >= len(l.src) {
			return token{}, errorAt(CodeSyntax, start, l.pos(), "this string is never closed; end it with '")
		}
		r := l.advance()
		if r != '\'' {
			b.WriteRune(r)
			continue
		}
		if l.peekRune() != '\'' {
			break
		}
		l.advance()
		b.WriteRune('\'')
	}
	t := l.tok(tokString, start, "")
	t.text = b.String()
	return t, nil
}

func (l *lexer) delimited(start Pos, kind tokenKind, closer rune, what string, ok func(rune) bool) (token, *Error) {
	l.advance()
	begin := l.off
	for l.off < len(l.src) && l.peekRune() != closer {
		if !ok(l.peekRune()) {
			p := l.pos()
			l.advance()
			return token{}, errorAt(CodeSyntax, p, l.pos(), "%q cannot appear in a %s", string(l.src[p.Offset:l.off]), what)
		}
		l.advance()
	}
	if l.off >= len(l.src) {
		return token{}, errorAt(CodeSyntax, start, l.pos(), "this %s is never closed; end it with %c", what, closer)
	}
	name := l.src[begin:l.off]
	l.advance()
	if name == "" || (kind == tokField && !ValidPath(name)) {
		return token{}, errorAt(CodeSyntax, start, l.pos(), "this %s is empty or malformed", what)
	}
	t := l.tok(kind, start, "")
	t.text = name
	return t, nil
}

func (l *lexer) param(start Pos) (token, *Error) {
	l.advance()
	if !isIdentStart(l.peekRune()) {
		return token{}, errorAt(CodeSyntax, start, l.pos(), "@ must be followed by a parameter name")
	}
	t := l.ident(start)
	t.kind = tokParam
	return t, nil
}

var punctuation = []string{"||", "<=", ">=", "<>", "!=", "==", "&&", "(", ")", ",", "+", "-", "*", "/", "%", "=", "<", ">"}

func (l *lexer) punct(start Pos) (token, *Error) {
	for _, p := range punctuation {
		if !strings.HasPrefix(l.src[l.off:], p) {
			continue
		}
		for range p {
			l.advance()
		}
		switch p {
		case "==":
			return token{}, errorAt(CodeSyntax, start, l.pos(), "use = to compare, not ==")
		case "&&":
			return token{}, errorAt(CodeSyntax, start, l.pos(), "use and, not &&")
		case "(":
			return l.tok(tokLParen, start, p), nil
		case ")":
			return l.tok(tokRParen, start, p), nil
		case ",":
			return l.tok(tokComma, start, p), nil
		case "<>":
			return l.tok(tokOp, start, "!="), nil
		}
		return l.tok(tokOp, start, p), nil
	}
	r := l.advance()
	if r == '"' {
		return token{}, errorAt(CodeSyntax, start, l.pos(), "strings use single quotes: 'text'")
	}
	return token{}, errorAt(CodeSyntax, start, l.pos(), "unexpected character %q", string(r))
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isIdentStart(r rune) bool { return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

func isIdentPart(r rune) bool { return isIdentStart(r) || isDigit(r) }

func isPathPart(r rune) bool { return isIdentPart(r) || r == '.' }

// ValidName reports whether s is a valid parameter, function or path segment name: a letter or
// underscore followed by letters, digits and underscores.
func ValidName(s string) bool {
	if s == "" || !isIdentStart(rune(s[0])) {
		return false
	}
	for _, r := range s {
		if !isIdentPart(r) {
			return false
		}
	}
	return true
}

// ValidColumnID reports whether s can name a report column: letters, digits and underscores.
func ValidColumnID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isIdentPart(r) {
			return false
		}
	}
	return true
}

// ValidPath reports whether s is a dotted field path of valid names.
func ValidPath(s string) bool {
	for _, seg := range strings.Split(s, ".") {
		if !ValidName(seg) {
			return false
		}
	}
	return true
}
