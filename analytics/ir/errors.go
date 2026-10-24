package ir

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/emoss08/keel/analytics/formula"
)

// Code classifies a definition error so an application can translate it. Formula errors carry
// the formula package's codes.
type Code string

const (
	// CodeJSON is input that does not decode into a definition.
	CodeJSON Code = "json"
	// CodeUnsupportedVersion is a version this package cannot read.
	CodeUnsupportedVersion Code = "unsupported_version"
	// CodeRequired is a missing value.
	CodeRequired Code = "required"
	// CodeInvalid is a malformed or out-of-range value.
	CodeInvalid Code = "invalid"
	// CodeDuplicate repeats an id or name that must be unique.
	CodeDuplicate Code = "duplicate"
	// CodeConflict sets options that exclude each other.
	CodeConflict Code = "conflict"
	// CodeLimit exceeds a configured limit.
	CodeLimit Code = "limit"
	// CodeUnknownEntity names an entity the catalog does not have.
	CodeUnknownEntity Code = "unknown_entity"
	// CodeUnknownField names a field or relation the catalog does not have.
	CodeUnknownField Code = "unknown_field"
	// CodeUnknownColumn names a column the report does not have.
	CodeUnknownColumn Code = "unknown_column"
	// CodeUnknownParameter names a parameter the report does not declare.
	CodeUnknownParameter Code = "unknown_parameter"
	// CodeType uses a value of the wrong type, or an operation a type does not support.
	CodeType Code = "type"
	// CodeLevel uses a per-row value where a per-group one is needed, or the reverse.
	CodeLevel Code = "level"
	// CodeNotAllowed uses a field in a way the catalog does not allow.
	CodeNotAllowed Code = "not_allowed"
)

// Error is one problem in a definition: a code, a message for a person, a JSON pointer (RFC 6901)
// to the offending part, suggestions for unknown names, and for a formula the position in its text.
type Error struct {
	// Code classifies the error.
	Code Code `json:"code"`
	// Message explains the error to a person.
	Message string `json:"message"`
	// Pointer locates the offending part of the definition.
	Pointer string `json:"pointer"`
	// Suggestions are names close to an unknown one.
	Suggestions []string `json:"suggestions,omitempty"`
	// Formula is the formula error with its position, for errors inside a formula.
	Formula *formula.Error `json:"formula,omitempty"`
}

// Error formats the error as "pointer: message".
func (e *Error) Error() string {
	if e.Pointer == "" {
		return e.Message
	}
	return e.Pointer + ": " + e.Message
}

// Errors is every problem found in a definition.
type Errors []*Error

// Error joins the messages, one per line.
func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

type collector struct {
	errs Errors
}

func (c *collector) add(code Code, ptr, format string, args ...any) *Error {
	e := &Error{Code: code, Message: fmt.Sprintf(format, args...), Pointer: ptr}
	c.errs = append(c.errs, e)
	return e
}

func (c *collector) unknown(code Code, ptr, what, name string, candidates []string) {
	e := c.add(code, ptr, "unknown %s %q", what, name)
	suggest(e, name, candidates)
}

func suggest(e *Error, name string, candidates []string) {
	e.Suggestions = formula.Suggest(name, candidates)
	if len(e.Suggestions) > 0 {
		e.Message += "; did you mean " + quoteAll(e.Suggestions) + "?"
	}
}

func quoteAll(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	return strings.Join(q, " or ")
}

func (c *collector) formula(ptr string, err error) {
	es, ok := err.(formula.Errors)
	if !ok {
		c.add(CodeInvalid, ptr, "%s", err.Error())
		return
	}
	for _, fe := range es {
		p := ptr
		if fe.Pointer != "" {
			p += fe.Pointer
		}
		c.errs = append(c.errs, &Error{Code: Code(fe.Code), Message: fe.Error(), Pointer: p, Suggestions: fe.Suggestions, Formula: fe})
	}
}

func ptr(base string, parts ...any) string {
	var b strings.Builder
	b.WriteString(base)
	for _, p := range parts {
		b.WriteString("/")
		switch v := p.(type) {
		case int:
			b.WriteString(strconv.Itoa(v))
		case string:
			b.WriteString(strings.ReplaceAll(strings.ReplaceAll(v, "~", "~0"), "/", "~1"))
		}
	}
	return b.String()
}
