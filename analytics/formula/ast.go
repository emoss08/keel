package formula

import "strings"

// Kind is the kind of an AST node.
type Kind string

const (
	// KindNumber is an exact decimal literal; Value holds its canonical digits, never a sign.
	KindNumber Kind = "number"
	// KindString is a text literal; Value holds the text.
	KindString Kind = "string"
	// KindBool is true or false; Value is "true" or "false".
	KindBool Kind = "bool"
	// KindNull is the literal null.
	KindNull Kind = "null"
	// KindDate is a date literal; Value is YYYY-MM-DD.
	KindDate Kind = "date"
	// KindTimestamp is an instant literal; Value is RFC 3339.
	KindTimestamp Kind = "timestamp"
	// KindColumn references an output column of the report by id: [revenue].
	KindColumn Kind = "column"
	// KindField references a catalog field by dotted path: {customer.name}.
	KindField Kind = "field"
	// KindParam references a report parameter: @window.
	KindParam Kind = "param"
	// KindUnary applies Value ("-", "not", "is null", "is not null") to Args[0].
	KindUnary Kind = "unary"
	// KindBinary applies the operator in Value to Args[0] and Args[1].
	KindBinary Kind = "binary"
	// KindCall calls the registry function named by Value with Args.
	KindCall Kind = "call"
	// KindCase is case [Subject] when Args[0] then Args[1] … [else Else] end.
	KindCase Kind = "case"
)

// Node is one node of a formula AST. Kind decides which other fields are set; in JSON a node is an
// object keyed by its kind, such as {"column": "revenue"} or {"call": "round", "args": […]}.
type Node struct {
	// Kind is the node kind.
	Kind Kind
	// Value is the literal, name or operator.
	Value string
	// Args are operands, call arguments or case branches.
	Args []*Node
	// Subject is a case's subject.
	Subject *Node
	// Else is a case's else branch.
	Else *Node
	// Pos is where the node starts in the text.
	Pos Pos
}

// Number returns a number literal node.
func Number(digits string) *Node { return &Node{Kind: KindNumber, Value: digits} }

// Text returns a string literal node.
func Text(s string) *Node { return &Node{Kind: KindString, Value: s} }

// ColumnRef returns a column reference node.
func ColumnRef(id string) *Node { return &Node{Kind: KindColumn, Value: id} }

// FieldRef returns a field reference node for a dotted path.
func FieldRef(path string) *Node { return &Node{Kind: KindField, Value: path} }

// ParamRef returns a parameter reference node.
func ParamRef(name string) *Node { return &Node{Kind: KindParam, Value: name} }

// Binary returns a binary operator node.
func Binary(op string, left, right *Node) *Node {
	return &Node{Kind: KindBinary, Value: op, Args: []*Node{left, right}}
}

// Unary returns a unary operator node.
func Unary(op string, operand *Node) *Node {
	return &Node{Kind: KindUnary, Value: op, Args: []*Node{operand}}
}

// Call returns a function call node.
func Call(name string, args ...*Node) *Node { return &Node{Kind: KindCall, Value: name, Args: args} }

// Path splits a field reference into its edges and the field.
func (n *Node) Path() []string { return strings.Split(n.Value, ".") }

// Walk calls fn for n and every node below it, parents first, until fn returns false.
func (n *Node) Walk(fn func(*Node) bool) bool {
	if n == nil {
		return true
	}
	if !fn(n) {
		return false
	}
	if !n.Subject.Walk(fn) {
		return false
	}
	for _, a := range n.Args {
		if !a.Walk(fn) {
			return false
		}
	}
	return n.Else.Walk(fn)
}

// Equal reports whether two trees are the same formula, ignoring positions.
func Equal(a, b *Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind || a.Value != b.Value || len(a.Args) != len(b.Args) {
		return false
	}
	for i := range a.Args {
		if !Equal(a.Args[i], b.Args[i]) {
			return false
		}
	}
	return Equal(a.Subject, b.Subject) && Equal(a.Else, b.Else)
}

// References returns the distinct names referenced with the given kind (column, field or
// param), in first-use order.
func (n *Node) References(kind Kind) []string {
	var out []string
	seen := map[string]bool{}
	n.Walk(func(m *Node) bool {
		if m.Kind == kind && !seen[m.Value] {
			seen[m.Value] = true
			out = append(out, m.Value)
		}
		return true
	})
	return out
}

func (n *Node) size() int {
	count := 0
	n.Walk(func(*Node) bool {
		count++
		return true
	})
	return count
}

func (n *Node) depth() int {
	if n == nil {
		return 0
	}
	d := max(n.Subject.depth(), n.Else.depth())
	for _, a := range n.Args {
		d = max(d, a.depth())
	}
	return d + 1
}
