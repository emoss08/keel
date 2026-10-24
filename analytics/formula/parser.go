package formula

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits bound the size of a formula. A zero field takes its default.
type Limits struct {
	// MaxLength caps the text in bytes (default 4000).
	MaxLength int
	// MaxDepth caps the nesting of the tree (default 64).
	MaxDepth int
	// MaxNodes caps the number of nodes (default 256).
	MaxNodes int
}

const (
	defaultMaxLength = 4000
	defaultMaxDepth  = 64
	defaultMaxNodes  = 256
)

func (l Limits) withDefaults() Limits {
	if l.MaxLength <= 0 {
		l.MaxLength = defaultMaxLength
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = defaultMaxDepth
	}
	if l.MaxNodes <= 0 {
		l.MaxNodes = defaultMaxNodes
	}
	return l
}

type parser struct {
	lex    *lexer
	tok    token
	limits Limits
	depth  int
	nodes  int
}

// Parse parses formula text into an AST with the default limits.
func Parse(src string) (*Node, error) { return ParseWith(src, Limits{}) }

// ParseWith parses formula text into an AST. Errors are Errors carrying positions and, for
// unknown function names, suggestions.
func ParseWith(src string, limits Limits) (*Node, error) {
	limits = limits.withDefaults()
	if len(src) > limits.MaxLength {
		end := newLexer(src)
		return nil, Errors{errorAt(CodeTooComplex, end.pos(), end.pos(), "a formula can be at most %d characters long", limits.MaxLength)}
	}
	if !utf8.ValidString(src) {
		start := newLexer(src).pos()
		return nil, Errors{errorAt(CodeSyntax, start, start, "the formula is not valid UTF-8 text")}
	}
	p := &parser{lex: newLexer(src), limits: limits}
	n, err := p.parse()
	if err != nil {
		return nil, Errors{err}
	}
	if err := checkShape(n, limits); err != nil {
		return nil, Errors{err}
	}
	return n, nil
}

func (p *parser) parse() (*Node, *Error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind == tokEOF {
		return nil, errorAt(CodeSyntax, p.tok.pos, p.tok.end, "the formula is empty")
	}
	n, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokEOF {
		return nil, p.unexpected("an operator or the end of the formula")
	}
	return n, nil
}

func (p *parser) advance() *Error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *parser) keyword(word string) bool {
	return p.tok.kind == tokIdent && p.tok.upper == strings.ToUpper(word)
}

func (p *parser) unexpected(want string) *Error {
	got := "the end of the formula"
	if p.tok.kind != tokEOF {
		got = describeToken(p.tok)
	}
	return errorAt(CodeSyntax, p.tok.pos, p.tok.end, "expected %s, found %s", want, got)
}

func describeToken(t token) string {
	switch t.kind {
	case tokString:
		return "'" + t.text + "'"
	case tokColumn:
		return "[" + t.text + "]"
	case tokField:
		return "{" + t.text + "}"
	case tokParam:
		return "@" + t.text
	default:
		return "\"" + t.text + "\""
	}
}

func (p *parser) node(n *Node, pos Pos) (*Node, *Error) {
	p.nodes++
	if p.nodes > p.limits.MaxNodes {
		return nil, errorAt(CodeTooComplex, pos, p.tok.pos, "a formula can have at most %d terms; split it into several calculated columns", p.limits.MaxNodes)
	}
	n.Pos = pos
	return n, nil
}

func (p *parser) enter(pos Pos) *Error {
	p.depth++
	if p.depth > p.limits.MaxDepth {
		return errorAt(CodeTooComplex, pos, p.tok.end, "a formula can nest at most %d levels deep; split it into several calculated columns", p.limits.MaxDepth)
	}
	return nil
}

func (p *parser) expr(minPrec int) (*Node, *Error) {
	if err := p.enter(p.tok.pos); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	left, err := p.prefix()
	if err != nil {
		return nil, err
	}
	for {
		op, prec := p.infix()
		if prec == 0 || prec <= minPrec {
			return left, nil
		}
		if left, err = p.led(left, op, prec); err != nil {
			return nil, err
		}
		if prec == precCompare {
			if _, next := p.infix(); next == precCompare {
				return nil, errorAt(CodeSyntax, p.tok.pos, p.tok.end, "comparisons cannot be chained; join them with and")
			}
		}
	}
}

func (p *parser) infix() (string, int) {
	switch {
	case p.tok.kind == tokOp:
		return p.tok.text, binaryPrec(p.tok.text)
	case p.keyword("or"):
		return "or", precOr
	case p.keyword("and"):
		return "and", precAnd
	case p.keyword("is"):
		return "is", precCompare
	default:
		return "", 0
	}
}

func (p *parser) led(left *Node, op string, prec int) (*Node, *Error) {
	pos := p.tok.pos
	if err := p.advance(); err != nil {
		return nil, err
	}
	if op == "is" {
		return p.isNull(left, pos)
	}
	right, err := p.expr(prec)
	if err != nil {
		return nil, err
	}
	return p.node(Binary(op, left, right), pos)
}

func (p *parser) isNull(left *Node, pos Pos) (*Node, *Error) {
	op := "is null"
	if p.keyword("not") {
		op = "is not null"
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if !p.keyword("null") {
		return nil, p.unexpected("null after is")
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	return p.node(Unary(op, left), pos)
}

func (p *parser) prefix() (*Node, *Error) {
	t := p.tok
	switch t.kind {
	case tokNumber:
		return p.leaf(&Node{Kind: KindNumber, Value: t.text})
	case tokString:
		return p.leaf(&Node{Kind: KindString, Value: t.text})
	case tokColumn:
		return p.leaf(&Node{Kind: KindColumn, Value: t.text})
	case tokField:
		return p.leaf(&Node{Kind: KindField, Value: t.text})
	case tokParam:
		return p.leaf(&Node{Kind: KindParam, Value: t.text})
	case tokLParen:
		return p.group()
	case tokOp:
		if t.text == "-" || t.text == "+" {
			return p.negate()
		}
	case tokIdent:
		return p.word()
	}
	return nil, p.unexpected("a value")
}

func (p *parser) leaf(n *Node) (*Node, *Error) {
	pos := p.tok.pos
	if err := p.advance(); err != nil {
		return nil, err
	}
	return p.node(n, pos)
}

func (p *parser) group() (*Node, *Error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	n, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokRParen {
		return nil, p.unexpected("a closing )")
	}
	return n, p.advance()
}

func (p *parser) negate() (*Node, *Error) {
	t := p.tok
	if err := p.advance(); err != nil {
		return nil, err
	}
	operand, err := p.expr(precNeg)
	if err != nil {
		return nil, err
	}
	if t.text == "+" {
		return operand, nil
	}
	return p.node(Unary("-", operand), t.pos)
}

func (p *parser) word() (*Node, *Error) {
	t := p.tok
	switch strings.ToLower(t.text) {
	case "true", "false":
		return p.leaf(&Node{Kind: KindBool, Value: strings.ToLower(t.text)})
	case "null":
		return p.leaf(&Node{Kind: KindNull})
	case "not":
		if err := p.advance(); err != nil {
			return nil, err
		}
		operand, err := p.expr(precNot)
		if err != nil {
			return nil, err
		}
		return p.node(Unary("not", operand), t.pos)
	case "date", "timestamp":
		return p.temporal(t)
	case "case":
		return p.caseExpr(t)
	case "and", "or", "is", "when", "then", "else", "end":
		return nil, p.unexpected("a value")
	}
	return p.call(t)
}

func (p *parser) temporal(t token) (*Node, *Error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind != tokString {
		return nil, p.unexpected("a quoted " + strings.ToLower(t.text) + " such as '2026-01-31'")
	}
	kind := KindDate
	if strings.EqualFold(t.text, "timestamp") {
		kind = KindTimestamp
	}
	value, err := canonicalTemporal(kind, p.tok.text)
	if err != nil {
		return nil, errorAt(CodeSyntax, p.tok.pos, p.tok.end, "%s", err.Message)
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	return p.node(&Node{Kind: kind, Value: value}, t.pos)
}

func canonicalTemporal(kind Kind, s string) (string, *Error) {
	if kind == KindDate {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			return "", &Error{Code: CodeInvalid, Message: "a date is written as YYYY-MM-DD, such as '2026-01-31'"}
		}
		return d.Format(time.DateOnly), nil
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", &Error{Code: CodeInvalid, Message: "a timestamp is written in RFC 3339 with a zone, such as '2026-01-31T08:00:00Z'"}
	}
	return ts.Format(time.RFC3339Nano), nil
}

func (p *parser) caseExpr(t token) (*Node, *Error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	n := &Node{Kind: KindCase}
	var err *Error
	if p.keyword("end") || p.keyword("else") {
		return nil, p.unexpected("when")
	}
	if !p.keyword("when") {
		if n.Subject, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	for p.keyword("when") {
		if err = p.branch(n); err != nil {
			return nil, err
		}
	}
	if len(n.Args) == 0 {
		return nil, p.unexpected("when")
	}
	if p.keyword("else") {
		if err = p.advance(); err != nil {
			return nil, err
		}
		if n.Else, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	if !p.keyword("end") {
		return nil, p.unexpected("when, else or end")
	}
	if err = p.advance(); err != nil {
		return nil, err
	}
	return p.node(n, t.pos)
}

func (p *parser) branch(n *Node) *Error {
	if err := p.advance(); err != nil {
		return err
	}
	cond, err := p.expr(0)
	if err != nil {
		return err
	}
	if !p.keyword("then") {
		return p.unexpected("then")
	}
	if err = p.advance(); err != nil {
		return err
	}
	result, err := p.expr(0)
	if err != nil {
		return err
	}
	n.Args = append(n.Args, cond, result)
	return nil
}

func (p *parser) call(t token) (*Node, *Error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind != tokLParen {
		e := errorAt(CodeSyntax, t.pos, t.end, "unknown name %s; columns are written [id], fields {path.field} and parameters @name", t.text)
		return nil, e
	}
	fn, ok := Lookup(t.text)
	if !ok {
		e := errorAt(CodeUnknownFunction, t.pos, t.end, "unknown function %s", t.text)
		return nil, didYouMean(e, strings.ToLower(t.text), functionNames(), func(s string) string { return s })
	}
	n := &Node{Kind: KindCall, Value: fn.Name}
	if err := p.arguments(n); err != nil {
		return nil, err
	}
	if err := checkArity(fn, len(n.Args), t.pos, p.tok.pos); err != nil {
		return nil, err
	}
	return p.node(n, t.pos)
}

func (p *parser) arguments(n *Node) *Error {
	if err := p.advance(); err != nil {
		return err
	}
	for p.tok.kind != tokRParen {
		if len(n.Args) > 0 {
			if p.tok.kind != tokComma {
				return p.unexpected(", or )")
			}
			if err := p.advance(); err != nil {
				return err
			}
		}
		arg, err := p.expr(0)
		if err != nil {
			return err
		}
		n.Args = append(n.Args, arg)
	}
	return p.advance()
}

func checkArity(fn *Function, got int, pos, end Pos) *Error {
	lo, hi := fn.arity()
	if got >= lo && (hi < 0 || got <= hi) {
		return nil
	}
	return errorAt(CodeArity, pos, end, "%s takes %s, not %d; it is %s", fn.Name, arityText(lo, hi), got, fn.Signature())
}

func arityText(lo, hi int) string {
	switch {
	case hi < 0:
		return plural(lo, "argument") + " or more"
	case lo == hi:
		return plural(lo, "argument")
	default:
		return strconv.Itoa(lo) + " to " + plural(hi, "argument")
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
