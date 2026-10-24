package analytics

import (
	"errors"
	"fmt"

	"github.com/emoss08/keel/analytics/ir"
)

// Code classifies an analytics error so an application can translate it and decide what to tell
// the person who ran the report.
type Code string

// The error codes.
const (
	// CodeCatalog is a schema the catalog cannot be built from.
	CodeCatalog Code = "catalog"
	// CodeInvalid is a definition that does not validate against the catalog; Details holds the
	// definition errors.
	CodeInvalid Code = "invalid_definition"
	// CodeParameter is a parameter value that is missing, of the wrong type or not allowed.
	CodeParameter Code = "parameter"
	// CodeDenied is a definition the policy does not allow; Reason holds the policy's reason.
	CodeDenied Code = "denied"
	// CodeNotAllowed is a definition the compiler cannot answer as asked, such as a per-row
	// calculation over a to-many relation.
	CodeNotAllowed Code = "not_allowed"
	// CodeTooComplex is a definition past the runner's complexity limits.
	CodeTooComplex Code = "too_complex"
	// CodeTooCostly is a statement whose planned cost or rows exceed the runner's limits.
	CodeTooCostly Code = "too_costly"
	// CodeBusy is a tenant already running as many reports as it may.
	CodeBusy Code = "busy"
	// CodeSuperseded is a run cancelled because a later run with the same key started.
	CodeSuperseded Code = "superseded"
	// CodeTimeout is a run that exceeded its entry point's timeout.
	CodeTimeout Code = "timeout"
	// CodeCursor is a page cursor this report did not produce.
	CodeCursor Code = "cursor"
)

// Error is an analytics error: a code, a message for a person, and where it applies.
type Error struct {
	// Code classifies the error.
	Code Code `json:"code"`
	// Message explains the error.
	Message string `json:"message"`
	// Pointer locates the offending part of the definition (RFC 6901), when there is one.
	Pointer string `json:"pointer,omitempty"`
	// Reason is the policy's reason code for a denial.
	Reason string `json:"reason,omitempty"`
	// Details are the definition errors of CodeInvalid.
	Details ir.Errors `json:"details,omitempty"`
	// Err is the underlying error, if any.
	Err error `json:"-"`
}

// Error formats the error with the package prefix.
func (e *Error) Error() string {
	msg := "analytics: " + e.Message
	if e.Pointer != "" {
		msg += " (" + e.Pointer + ")"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Is matches another *Error with the same code, so errors.Is(err, &analytics.Error{Code:
// analytics.CodeBusy}) tests the code.
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t.Code == e.Code && t.Message == ""
}

// ErrorCode returns the code of an analytics error in err's chain, or "".
func ErrorCode(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// fail returns an *Error with a formatted message.
func fail(code Code, pointer, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Pointer: pointer}
}

// invalid wraps definition errors.
func invalid(err error) error {
	var es ir.Errors
	if errors.As(err, &es) {
		msg := "the definition is not valid"
		if len(es) > 0 {
			msg = es[0].Error()
			if len(es) > 1 {
				msg += fmt.Sprintf(" (and %d more)", len(es)-1)
			}
		}
		return &Error{Code: CodeInvalid, Message: msg, Details: es}
	}
	return &Error{Code: CodeInvalid, Message: "the definition is not valid", Err: err}
}
