package formula

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Pos is a position in formula text: a byte offset, and a 1-based line and column counted in
// characters. A node decoded from JSON has the zero Pos.
type Pos struct {
	// Offset is the byte offset from the start of the text.
	Offset int `json:"offset"`
	// Line is the 1-based line.
	Line int `json:"line"`
	// Column is the 1-based column, in characters.
	Column int `json:"column"`
}

// Known reports whether the position came from formula text.
func (p Pos) Known() bool { return p.Line > 0 }

// Code classifies a formula error so an application can translate it.
type Code string

const (
	// CodeSyntax is text that does not follow the grammar.
	CodeSyntax Code = "syntax"
	// CodeTooComplex is a formula past the depth, size or length limits.
	CodeTooComplex Code = "too_complex"
	// CodeUnknownFunction names a function the registry does not have.
	CodeUnknownFunction Code = "unknown_function"
	// CodeArity is a call with the wrong number of arguments.
	CodeArity Code = "arity"
	// CodeUnknownColumn references a column the report does not have.
	CodeUnknownColumn Code = "unknown_column"
	// CodeUnknownField references a field the catalog does not have.
	CodeUnknownField Code = "unknown_field"
	// CodeUnknownParameter references a parameter the report does not declare.
	CodeUnknownParameter Code = "unknown_parameter"
	// CodeType is a value of the wrong type.
	CodeType Code = "type"
	// CodeLevel mixes per-row and per-group values in a way SQL cannot evaluate.
	CodeLevel Code = "level"
	// CodeInvalid is a malformed node, such as a bad literal in a JSON AST.
	CodeInvalid Code = "invalid"
)

// Error is one problem in a formula: the span of text it covers, or for a tree decoded from JSON
// a JSON pointer into the AST, and suggestions for unknown names.
type Error struct {
	// Code classifies the error.
	Code Code `json:"code"`
	// Message explains the error to a person.
	Message string `json:"message"`
	// Pos is where the offending text starts.
	Pos Pos `json:"pos"`
	// End is where it ends.
	End Pos `json:"end"`
	// Suggestions are names close to an unknown one.
	Suggestions []string `json:"suggestions,omitempty"`
	// Pointer locates the node in a JSON AST.
	Pointer string `json:"pointer,omitempty"`
}

// Error formats the error as "line:column: message".
func (e *Error) Error() string {
	if !e.Pos.Known() {
		return e.Message
	}
	return fmt.Sprintf("%d:%d: %s", e.Pos.Line, e.Pos.Column, e.Message)
}

// Excerpt returns the line of src the error is on with a caret line under the span it covers.
func (e *Error) Excerpt(src string) string {
	if !e.Pos.Known() {
		return ""
	}
	lines := strings.Split(src, "\n")
	if e.Pos.Line > len(lines) {
		return ""
	}
	line := lines[e.Pos.Line-1]
	width := 1
	if e.End.Line == e.Pos.Line && e.End.Column > e.Pos.Column {
		width = e.End.Column - e.Pos.Column
	}
	if limit := utf8.RuneCountInString(line) - e.Pos.Column + 1; width > limit && limit > 0 {
		width = limit
	}
	return line + "\n" + strings.Repeat(" ", e.Pos.Column-1) + strings.Repeat("^", width)
}

// Errors is every problem found in one formula, in source order.
type Errors []*Error

// Error joins the messages.
func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Describe returns every error with its excerpt from src, for a terminal or a log.
func (es Errors) Describe(src string) string {
	var b strings.Builder
	for i, e := range es {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(e.Error())
		if ex := e.Excerpt(src); ex != "" {
			b.WriteString("\n" + ex)
		}
	}
	return b.String()
}

func errorAt(code Code, pos, end Pos, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Pos: pos, End: end}
}

func didYouMean(e *Error, name string, candidates []string, quote func(string) string) *Error {
	e.Suggestions = Suggest(name, candidates)
	if len(e.Suggestions) > 0 {
		quoted := make([]string, len(e.Suggestions))
		for i, s := range e.Suggestions {
			quoted[i] = quote(s)
		}
		e.Message += "; did you mean " + strings.Join(quoted, " or ") + "?"
	}
	return e
}
