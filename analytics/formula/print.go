package formula

import "strings"

const precLeaf = 100

// Format prints an AST as canonical formula text: lower-case keywords, single spaces around
// operators and the fewest parentheses that keep the tree's shape. Parsing the result gives an
// equal tree.
func Format(n *Node) string {
	var b strings.Builder
	write(&b, n)
	return b.String()
}

func precOf(n *Node) int {
	switch n.Kind {
	case KindBinary:
		return binaryPrec(n.Value)
	case KindUnary:
		switch n.Value {
		case "-":
			return precNeg
		case "not":
			return precNot
		default:
			return precCompare
		}
	default:
		return precLeaf
	}
}

func write(b *strings.Builder, n *Node) {
	switch n.Kind {
	case KindNumber, KindBool:
		b.WriteString(n.Value)
	case KindNull:
		b.WriteString("null")
	case KindString:
		b.WriteString(quote(n.Value))
	case KindDate, KindTimestamp:
		b.WriteString(string(n.Kind) + " " + quote(n.Value))
	case KindColumn:
		b.WriteString("[" + n.Value + "]")
	case KindField:
		b.WriteString("{" + n.Value + "}")
	case KindParam:
		b.WriteString("@" + n.Value)
	case KindUnary:
		writeUnary(b, n)
	case KindBinary:
		writeBinary(b, n)
	case KindCall:
		writeCall(b, n)
	case KindCase:
		writeCase(b, n)
	}
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func writeWrapped(b *strings.Builder, n *Node, wrap bool) {
	if wrap {
		b.WriteString("(")
	}
	write(b, n)
	if wrap {
		b.WriteString(")")
	}
}

func writeUnary(b *strings.Builder, n *Node) {
	operand := n.Args[0]
	p := precOf(operand)
	switch n.Value {
	case "-":
		b.WriteString("-")
		writeWrapped(b, operand, p <= precNeg)
	case "not":
		b.WriteString("not ")
		writeWrapped(b, operand, p < precNot)
	default:
		writeWrapped(b, operand, p <= precCompare)
		b.WriteString(" " + n.Value)
	}
}

func writeBinary(b *strings.Builder, n *Node) {
	p := binaryPrec(n.Value)
	left, right := n.Args[0], n.Args[1]
	lp := precOf(left)
	writeWrapped(b, left, lp < p || (p == precCompare && lp == precCompare))
	b.WriteString(" " + n.Value + " ")
	writeWrapped(b, right, precOf(right) <= p)
}

func writeCall(b *strings.Builder, n *Node) {
	b.WriteString(n.Value + "(")
	for i, a := range n.Args {
		if i > 0 {
			b.WriteString(", ")
		}
		write(b, a)
	}
	b.WriteString(")")
}

func writeCase(b *strings.Builder, n *Node) {
	b.WriteString("case ")
	if n.Subject != nil {
		write(b, n.Subject)
		b.WriteString(" ")
	}
	for i := 0; i+1 < len(n.Args); i += 2 {
		b.WriteString("when ")
		write(b, n.Args[i])
		b.WriteString(" then ")
		write(b, n.Args[i+1])
		b.WriteString(" ")
	}
	if n.Else != nil {
		b.WriteString("else ")
		write(b, n.Else)
		b.WriteString(" ")
	}
	b.WriteString("end")
}
