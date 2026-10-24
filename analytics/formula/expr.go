package formula

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Expr is a formula as it appears in a report definition: text a person typed, or an AST a
// structured editor built. It encodes in the form it was given: a JSON string for text, an
// object for an AST.
type Expr struct {
	// Text is formula text.
	Text string
	// Node is an AST; it wins over Text when both are set.
	Node *Node
}

// Of returns an expression holding formula text.
func Of(text string) *Expr { return &Expr{Text: text} }

// OfNode returns an expression holding an AST.
func OfNode(n *Node) *Expr { return &Expr{Node: n} }

// Tree returns the expression's AST: the given one after Validate, or the text parsed.
func (e *Expr) Tree(limits Limits) (*Node, error) {
	if e.Node != nil {
		return e.Node, Validate(e.Node, limits)
	}
	return ParseWith(e.Text, limits)
}

// String returns the canonical text of the expression, or its raw text when it does not parse.
func (e *Expr) String() string {
	if e.Node != nil {
		return Format(e.Node)
	}
	if n, err := Parse(e.Text); err == nil {
		return Format(n)
	}
	return e.Text
}

// IsZero reports whether the expression holds nothing.
func (e Expr) IsZero() bool { return e.Text == "" && e.Node == nil }

// MarshalJSON encodes the text as a string or the AST as an object.
func (e Expr) MarshalJSON() ([]byte, error) {
	if e.Node != nil {
		return json.Marshal(e.Node)
	}
	return json.Marshal(e.Text)
}

// UnmarshalJSON accepts a string of formula text or an AST object.
func (e *Expr) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '"':
		*e = Expr{}
		return json.Unmarshal(trimmed, &e.Text)
	case len(trimmed) > 0 && trimmed[0] == '{':
		n := new(Node)
		if err := json.Unmarshal(trimmed, n); err != nil {
			return err
		}
		*e = Expr{Node: n}
		return nil
	default:
		return errors.New("a formula is text or an AST object")
	}
}
