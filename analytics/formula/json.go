package formula

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

type nodeJSON struct {
	Number    *string   `json:"number,omitempty"`
	String    *string   `json:"string,omitempty"`
	Bool      *bool     `json:"bool,omitempty"`
	Null      *bool     `json:"null,omitempty"`
	Date      *string   `json:"date,omitempty"`
	Timestamp *string   `json:"timestamp,omitempty"`
	Column    *string   `json:"column,omitempty"`
	Field     *string   `json:"field,omitempty"`
	Param     *string   `json:"param,omitempty"`
	Op        *string   `json:"op,omitempty"`
	Call      *string   `json:"call,omitempty"`
	Args      []*Node   `json:"args,omitempty"`
	Case      *caseJSON `json:"case,omitempty"`
}

type caseJSON struct {
	Subject *Node      `json:"subject,omitempty"`
	When    []whenJSON `json:"when"`
	Else    *Node      `json:"else,omitempty"`
}

type whenJSON struct {
	If   *Node `json:"if"`
	Then *Node `json:"then"`
}

// MarshalJSON encodes the node in the AST form a structured editor produces, such as
// {"op": "/", "args": [{"column": "revenue"}, {"column": "miles"}]}.
func (n Node) MarshalJSON() ([]byte, error) {
	v := n.Value
	var j nodeJSON
	switch n.Kind {
	case KindNumber:
		j.Number = &v
	case KindString:
		j.String = &v
	case KindBool:
		b := v == "true"
		j.Bool = &b
	case KindNull:
		t := true
		j.Null = &t
	case KindDate:
		j.Date = &v
	case KindTimestamp:
		j.Timestamp = &v
	case KindColumn:
		j.Column = &v
	case KindField:
		j.Field = &v
	case KindParam:
		j.Param = &v
	case KindUnary, KindBinary:
		j.Op, j.Args = &v, n.Args
	case KindCall:
		j.Call, j.Args = &v, n.Args
		if j.Args == nil {
			j.Args = []*Node{}
		}
	case KindCase:
		j.Case = encodeCase(&n)
	default:
		return nil, fmt.Errorf("formula: cannot encode a node of kind %q", n.Kind)
	}
	return json.Marshal(j)
}

func encodeCase(n *Node) *caseJSON {
	c := &caseJSON{Subject: n.Subject, Else: n.Else, When: []whenJSON{}}
	for i := 0; i+1 < len(n.Args); i += 2 {
		c.When = append(c.When, whenJSON{If: n.Args[i], Then: n.Args[i+1]})
	}
	return c
}

// UnmarshalJSON decodes the AST form. It checks the node's own shape; Validate checks the tree.
func (n *Node) UnmarshalJSON(data []byte) error {
	var j nodeJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return fmt.Errorf("formula node: %w", err)
	}
	decoded, err := j.node()
	if err != nil {
		return err
	}
	*n = *decoded
	return nil
}

func (j *nodeJSON) node() (*Node, error) {
	var found []func() *Node
	add := func(set bool, n func() *Node) {
		if set {
			found = append(found, n)
		}
	}
	leaf := func(kind Kind, v *string) func() *Node {
		return func() *Node { return &Node{Kind: kind, Value: deref(v)} }
	}
	add(j.Number != nil, leaf(KindNumber, j.Number))
	add(j.String != nil, leaf(KindString, j.String))
	add(j.Bool != nil, func() *Node { return &Node{Kind: KindBool, Value: strconv.FormatBool(*j.Bool)} })
	add(j.Null != nil, func() *Node { return &Node{Kind: KindNull} })
	add(j.Date != nil, leaf(KindDate, j.Date))
	add(j.Timestamp != nil, leaf(KindTimestamp, j.Timestamp))
	add(j.Column != nil, leaf(KindColumn, j.Column))
	add(j.Field != nil, leaf(KindField, j.Field))
	add(j.Param != nil, leaf(KindParam, j.Param))
	add(j.Op != nil, func() *Node { return opNode(*j.Op, j.Args) })
	add(j.Call != nil, func() *Node { return &Node{Kind: KindCall, Value: *j.Call, Args: j.Args} })
	add(j.Case != nil, func() *Node { return caseNode(j.Case) })
	if len(found) != 1 {
		return nil, errors.New("formula node: give exactly one of number, string, bool, null, date, timestamp, column, field, param, op, call or case")
	}
	if j.Args != nil && j.Op == nil && j.Call == nil {
		return nil, errors.New("formula node: only op and call nodes take args")
	}
	return found[0](), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func opNode(op string, args []*Node) *Node {
	if len(args) == 1 {
		return &Node{Kind: KindUnary, Value: op, Args: args}
	}
	return &Node{Kind: KindBinary, Value: op, Args: args}
}

func caseNode(c *caseJSON) *Node {
	n := &Node{Kind: KindCase, Subject: c.Subject, Else: c.Else}
	for _, w := range c.When {
		n.Args = append(n.Args, w.If, w.Then)
	}
	return n
}

// Validate checks a tree built in code or decoded from JSON for everything Parse guarantees:
// known kinds and operators, canonical literals, valid names, known functions with the right
// number of arguments, and the depth and size limits. Errors carry a JSON pointer into the AST.
func Validate(n *Node, limits Limits) error {
	if err := checkShape(n, limits.withDefaults()); err != nil {
		return Errors{err}
	}
	return nil
}

func checkShape(n *Node, limits Limits) *Error {
	if n == nil {
		return &Error{Code: CodeInvalid, Message: "the formula is empty"}
	}
	if d := n.depth(); d > limits.MaxDepth {
		return &Error{Code: CodeTooComplex, Message: fmt.Sprintf("a formula can nest at most %d levels deep; split it into several calculated columns", limits.MaxDepth)}
	}
	if s := n.size(); s > limits.MaxNodes {
		return &Error{Code: CodeTooComplex, Message: fmt.Sprintf("a formula can have at most %d terms; split it into several calculated columns", limits.MaxNodes)}
	}
	return shapeAt(n, "")
}

func shapeAt(n *Node, ptr string) *Error {
	if n == nil {
		return &Error{Code: CodeInvalid, Message: "a formula node is missing", Pointer: ptr}
	}
	if msg := leafProblem(n); msg != "" {
		return &Error{Code: CodeInvalid, Message: msg, Pos: n.Pos, Pointer: ptr}
	}
	if err := shapeArgs(n, ptr); err != nil {
		return err
	}
	for i, a := range n.Args {
		if err := shapeAt(a, ptr+"/args/"+strconv.Itoa(i)); err != nil {
			return err
		}
	}
	if n.Subject != nil {
		if err := shapeAt(n.Subject, ptr+"/case/subject"); err != nil {
			return err
		}
	}
	if n.Else != nil {
		return shapeAt(n.Else, ptr+"/case/else")
	}
	return nil
}

func leafProblem(n *Node) string {
	switch n.Kind {
	case KindNumber:
		if !canonicalDigits(n.Value) {
			return fmt.Sprintf("%q is not a number in canonical form (digits, an optional fraction, no sign or leading zeros)", n.Value)
		}
	case KindBool:
		if n.Value != "true" && n.Value != "false" {
			return "a bool node holds true or false"
		}
	case KindDate, KindTimestamp:
		if c, err := canonicalTemporal(n.Kind, n.Value); err != nil || c != n.Value {
			return fmt.Sprintf("%q is not a canonical %s", n.Value, n.Kind)
		}
	case KindColumn:
		if !ValidColumnID(n.Value) {
			return fmt.Sprintf("%q is not a column id", n.Value)
		}
	case KindField:
		if !ValidPath(n.Value) {
			return fmt.Sprintf("%q is not a field path", n.Value)
		}
	case KindParam:
		if !ValidName(n.Value) {
			return fmt.Sprintf("%q is not a parameter name", n.Value)
		}
	case KindString, KindNull, KindUnary, KindBinary, KindCall, KindCase:
	default:
		return fmt.Sprintf("unknown node kind %q", n.Kind)
	}
	return ""
}

func canonicalDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isDigit(r) && r != '.' {
			return false
		}
	}
	whole, frac, hasFrac := 0, 0, false
	for _, r := range s {
		switch {
		case r == '.' && hasFrac:
			return false
		case r == '.':
			hasFrac = true
		case hasFrac:
			frac++
		default:
			whole++
		}
	}
	return whole > 0 && (!hasFrac || frac > 0) && canonicalNumber(s) == s
}

func shapeArgs(n *Node, ptr string) *Error {
	bad := func(format string, args ...any) *Error {
		return &Error{Code: CodeInvalid, Message: fmt.Sprintf(format, args...), Pos: n.Pos, Pointer: ptr}
	}
	switch n.Kind {
	case KindUnary:
		if (n.Value != "-" && n.Value != "not" && n.Value != "is null" && n.Value != "is not null") || len(n.Args) != 1 {
			return bad("%q with %d operands is not an operator", n.Value, len(n.Args))
		}
	case KindBinary:
		if binaryPrec(n.Value) == 0 || len(n.Args) != 2 {
			return bad("%q with %d operands is not an operator", n.Value, len(n.Args))
		}
	case KindCall:
		fn, ok := Lookup(n.Value)
		if !ok || fn.Name != n.Value {
			e := bad("unknown function %s", n.Value)
			didYouMean(e, n.Value, functionNames(), func(s string) string { return s })
			return e
		}
		if err := checkArity(fn, len(n.Args), n.Pos, n.Pos); err != nil {
			err.Pointer = ptr
			return err
		}
	case KindCase:
		if len(n.Args) < 2 || len(n.Args)%2 != 0 {
			return bad("a case needs at least one when … then branch")
		}
	default:
		if len(n.Args) > 0 {
			return bad("a %s node takes no arguments", n.Kind)
		}
	}
	return nil
}
