package formula

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ColumnInfo is what the environment knows about an output column a formula references.
type ColumnInfo struct {
	// Type is the column's output type.
	Type Type
	// Level is the column's level.
	Level Level
}

// ParamInfo is what the environment knows about a report parameter.
type ParamInfo struct {
	// Type is the parameter's type.
	Type Type
	// Multi parameters take lists and cannot appear in formulas.
	Multi bool
}

// Env resolves the references in a formula. Column resolves [id] to an output column of the
// report; Field resolves {path.field} through the catalog and, when the path does not resolve,
// returns the full paths worth suggesting; Param resolves @name. ColumnIDs and ParamNames list
// what exists, for suggestions.
type Env interface {
	Column(id string) (ColumnInfo, bool)
	ColumnIDs() []string
	Field(path string) (Type, []string, bool)
	Param(name string) (ParamInfo, bool)
	ParamNames() []string
}

// Typed is a checked node: its type, its level, the type the compiler converts it to before use
// (see Coercion), and for calls the registry entry that renders it.
type Typed struct {
	// Node is the checked AST node.
	Node *Node
	// Type is the node's value type.
	Type Type
	// Level is the node's level.
	Level Level
	// Coerce is the type to convert the value to before use, or Unknown.
	Coerce Type
	// Func is the registry entry of a call.
	Func *Function
	// Args are the checked operands, arguments or case branches.
	Args []*Typed
	// Subject is a case's checked subject.
	Subject *Typed
	// Else is a case's checked else branch.
	Else *Typed

	loose *Node
}

// RowLevel reports whether the formula is a value per source row (or a constant), usable as a
// dimension and in row filters.
func (t *Typed) RowLevel() bool { return t.Level <= Row }

type checker struct {
	env      Env
	errs     Errors
	levelErr bool
}

// Check type-checks an AST against an environment. It returns every error it finds, so one pass
// reports all the problems in a formula.
func Check(n *Node, env Env) (*Typed, error) {
	c := &checker{env: env}
	t := c.check(n)
	if len(c.errs) > 0 {
		return t, c.errs
	}
	return t, nil
}

// CallType returns the result type of calling a registry function with arguments of the given
// types, as the checker computes it.
func CallType(name string, args ...Type) (Type, error) {
	fn, ok := Lookup(name)
	if !ok {
		return Unknown, fmt.Errorf("unknown function %s", name)
	}
	c := &checker{}
	typed := make([]*Typed, len(args))
	for i, a := range args {
		typed[i] = &Typed{Node: &Node{Kind: KindField}, Type: a, Level: Row}
	}
	n := &Node{Kind: KindCall, Value: fn.Name}
	t := c.typeCall(n, fn, typed)
	if len(c.errs) > 0 {
		return Unknown, c.errs
	}
	return t.Type, nil
}

func (c *checker) fail(n *Node, code Code, format string, args ...any) *Error {
	e := errorAt(code, n.Pos, endOf(n), format, args...)
	c.errs = append(c.errs, e)
	return e
}

func endOf(n *Node) Pos {
	if !n.Pos.Known() {
		return n.Pos
	}
	var width int
	switch n.Kind {
	case KindUnary, KindBinary, KindCall:
		width = len([]rune(n.Value))
		if strings.HasPrefix(n.Value, "is ") {
			width = 2
		}
	case KindCase:
		width = len("case")
	default:
		width = len([]rune(Format(n)))
	}
	return Pos{Offset: n.Pos.Offset + width, Line: n.Pos.Line, Column: n.Pos.Column + width}
}

func (c *checker) check(n *Node) *Typed {
	switch n.Kind {
	case KindNumber:
		return &Typed{Node: n, Type: numberType(n.Value)}
	case KindString:
		return &Typed{Node: n, Type: String}
	case KindBool:
		return &Typed{Node: n, Type: Bool}
	case KindNull:
		return &Typed{Node: n, Type: Null}
	case KindDate:
		return &Typed{Node: n, Type: Date}
	case KindTimestamp:
		return &Typed{Node: n, Type: Timestamp}
	case KindColumn:
		return c.column(n)
	case KindField:
		return c.field(n)
	case KindParam:
		return c.param(n)
	case KindUnary:
		return c.unary(n)
	case KindBinary:
		return c.binary(n)
	case KindCall:
		return c.call(n)
	default:
		return c.caseExpr(n)
	}
}

func numberType(digits string) Type {
	if strings.Contains(digits, ".") {
		return Decimal
	}
	if _, err := strconv.ParseInt(digits, 10, 64); err != nil {
		return Decimal
	}
	return Int
}

func (c *checker) column(n *Node) *Typed {
	info, ok := c.env.Column(n.Value)
	if !ok {
		e := c.fail(n, CodeUnknownColumn, "unknown column [%s]", n.Value)
		didYouMean(e, n.Value, c.env.ColumnIDs(), func(s string) string { return "[" + s + "]" })
		return &Typed{Node: n}
	}
	return &Typed{Node: n, Type: info.Type, Level: info.Level}
}

func (c *checker) field(n *Node) *Typed {
	t, candidates, ok := c.env.Field(n.Value)
	if !ok {
		e := c.fail(n, CodeUnknownField, "unknown field {%s}", n.Value)
		didYouMean(e, n.Value, candidates, func(s string) string { return "{" + s + "}" })
		return &Typed{Node: n, Level: Row, loose: n}
	}
	return &Typed{Node: n, Type: t, Level: Row, loose: n}
}

func (c *checker) param(n *Node) *Typed {
	info, ok := c.env.Param(n.Value)
	if !ok {
		e := c.fail(n, CodeUnknownParameter, "unknown parameter @%s", n.Value)
		didYouMean(e, n.Value, c.env.ParamNames(), func(s string) string { return "@" + s })
		return &Typed{Node: n}
	}
	if info.Multi {
		c.fail(n, CodeType, "@%s takes several values, so it can only be used in an in or not in filter", n.Value)
	}
	return &Typed{Node: n, Type: info.Type}
}

func (c *checker) combine(n *Node, t *Typed, parts ...*Typed) *Typed {
	for _, p := range parts {
		if p == nil {
			continue
		}
		t.Level = max(t.Level, p.Level)
		if t.loose == nil {
			t.loose = p.loose
		}
	}
	if t.Level >= Aggregate && t.loose != nil && !c.levelErr {
		c.levelErr = true
		c.fail(t.loose, CodeLevel, "{%s} is a value per row, but %s combines it with values per group; aggregate it, as in sum({%s}), or reference a dimension column instead", t.loose.Value, describe(n), t.loose.Value)
	}
	return t
}

func describe(n *Node) string {
	switch n.Kind {
	case KindCall:
		return n.Value + "()"
	case KindCase:
		return "this case"
	default:
		return "this calculation"
	}
}

func (c *checker) unary(n *Node) *Typed {
	x := c.check(n.Args[0])
	t := &Typed{Node: n, Args: []*Typed{x}}
	switch n.Value {
	case "-":
		t.Type = x.Type
		if !numericOrOpen(x.Type) {
			c.fail(n, CodeType, "- negates numbers, not %s", typeName(x.Type))
		}
	case "not":
		t.Type = Bool
		c.want(x, Bool, "not")
	default:
		t.Type = Bool
	}
	return c.combine(n, t, x)
}

func numericOrOpen(t Type) bool { return t.Numeric() || t == Unknown || t == Null }

func typeName(t Type) string {
	if t == Unknown {
		return "an unknown type"
	}
	return string(t)
}

func (c *checker) want(x *Typed, t Type, what string) {
	if x.Type != t && x.Type != Unknown && x.Type != Null {
		c.fail(x.Node, CodeType, "%s needs %s, but this is %s", what, t, typeName(x.Type))
	}
}

func (c *checker) binary(n *Node) *Typed {
	l, r := c.check(n.Args[0]), c.check(n.Args[1])
	t := &Typed{Node: n, Args: []*Typed{l, r}}
	switch n.Value {
	case "and", "or":
		t.Type = Bool
		c.want(l, Bool, n.Value)
		c.want(r, Bool, n.Value)
	case "=", "!=", "<", "<=", ">", ">=":
		t.Type = Bool
		c.compare(n, l, r)
	case "||":
		t.Type = String
		toText(l)
		toText(r)
	default:
		t.Type = c.arithmetic(n, l, r)
	}
	return c.combine(n, t, l, r)
}

func toText(x *Typed) {
	if x.Type != String && x.Type != Null && x.Type != Unknown {
		x.Coerce = String
	}
}

func (c *checker) arithmetic(n *Node, l, r *Typed) Type {
	for _, x := range []*Typed{l, r} {
		if !numericOrOpen(x.Type) {
			hint := ""
			if x.Type.Temporal() {
				hint = "; use date_diff to measure time between dates"
			}
			c.fail(x.Node, CodeType, "%s needs numbers, but this is %s%s", n.Value, typeName(x.Type), hint)
			return Unknown
		}
	}
	if l.Type == Unknown || r.Type == Unknown {
		return Unknown
	}
	result := Wider(numberOf(l.Type), numberOf(r.Type))
	if n.Value == "/" && result == Int {
		result = Decimal
	}
	if n.Value == "%" && result == Float {
		c.fail(n, CodeType, "%% needs int or decimal operands, not float")
	}
	coerceTo(l, result)
	coerceTo(r, result)
	return result
}

func numberOf(t Type) Type {
	if t == Null {
		return Int
	}
	return t
}

func coerceTo(x *Typed, t Type) {
	if _, ok := Coercion(x.Type, t); ok {
		x.Coerce = t
	}
}

func (c *checker) compare(n *Node, l, r *Typed) {
	if l.Type == Unknown || r.Type == Unknown {
		return
	}
	if l.Type == Null || r.Type == Null {
		c.fail(n, CodeType, "comparing with null is never true; use is null or is not null")
		return
	}
	ordering := n.Value != "=" && n.Value != "!="
	if ordering && (!l.Type.Ordered() || !r.Type.Ordered()) {
		c.fail(n, CodeType, "%s compares numbers, text and dates, not %s and %s", n.Value, l.Type, r.Type)
		return
	}
	if !unify(l, r) {
		c.fail(n, CodeType, "%s cannot compare %s with %s", n.Value, l.Type, r.Type)
	}
}

func unify(l, r *Typed) bool {
	switch {
	case l.Type == r.Type:
		return l.Type != JSON
	case l.Type.Numeric() && r.Type.Numeric():
		w := Wider(l.Type, r.Type)
		coerceTo(l, w)
		coerceTo(r, w)
	case l.Type.Textual() && r.Type.Textual(), l.Type == Ref && r.Type.Textual(), r.Type == Ref && l.Type.Textual():
		toText(l)
		toText(r)
	case l.Type.Temporal() && r.Type.Temporal():
		coerceTo(l, Timestamp)
		coerceTo(r, Timestamp)
	default:
		return false
	}
	return true
}

func (c *checker) call(n *Node) *Typed {
	fn, ok := Lookup(n.Value)
	if !ok {
		c.fail(n, CodeUnknownFunction, "unknown function %s", n.Value)
		return &Typed{Node: n}
	}
	args := make([]*Typed, len(n.Args))
	for i, a := range n.Args {
		args[i] = c.check(a)
	}
	return c.typeCall(n, fn, args)
}

func (c *checker) typeCall(n *Node, fn *Function, args []*Typed) *Typed {
	t := &Typed{Node: n, Func: fn, Args: args}
	for i, a := range args {
		c.argument(fn, fn.param(i), a)
	}
	t.Type = c.result(n, fn, args)
	if !fn.Aggregate {
		return c.combine(n, t, args...)
	}
	for _, a := range args {
		if a.Level >= Aggregate {
			c.fail(a.Node, CodeLevel, "%s already works on groups, so its argument must be a value per row, not another aggregate", fn.Name)
		}
	}
	t.Level = Aggregate
	return t
}

func (c *checker) argument(fn *Function, p Param, a *Typed) {
	open := a.Type == Unknown || a.Type == Null
	bad := func(want string) {
		c.fail(a.Node, CodeType, "%s of %s must be %s, but this is %s", p.Name, fn.Name, want, typeName(a.Type))
	}
	switch p.Class {
	case ClassNumber:
		if !open && !a.Type.Numeric() {
			bad("a number")
		}
	case ClassText:
		if !open && !a.Type.Textual() {
			bad("text")
		}
		coerceTo(a, String)
	case ClassTemporal:
		if !open && !a.Type.Temporal() {
			bad("a date or time")
		}
		coerceTo(a, Timestamp)
	case ClassBool:
		c.want(a, Bool, p.Name+" of "+fn.Name)
	case ClassOrdered:
		if !open && !a.Type.Ordered() {
			bad("a number, text or date")
		}
	case ClassDigits:
		c.digits(fn, p, a)
	case ClassUnit:
		c.unit(fn, p, a)
	}
}

func (c *checker) digits(fn *Function, p Param, a *Typed) {
	v, err := strconv.Atoi(a.Node.Value)
	if a.Node.Kind != KindNumber || err != nil || v > MaxDigits {
		c.fail(a.Node, CodeType, "%s of %s must be a whole number from 0 to %d written in the formula", p.Name, fn.Name, MaxDigits)
	}
}

func (c *checker) unit(fn *Function, p Param, a *Typed) {
	if a.Node.Kind == KindString && slices.Contains(p.Units, a.Node.Value) {
		return
	}
	e := c.fail(a.Node, CodeType, "%s of %s must be one of '%s' written in the formula", p.Name, fn.Name, strings.Join(p.Units, "', '"))
	if a.Node.Kind == KindString {
		didYouMean(e, a.Node.Value, p.Units, quote)
	}
}

func (c *checker) result(n *Node, fn *Function, args []*Typed) Type {
	switch fn.Result {
	case ResultFixed:
		if fn.Type == Decimal {
			for i, a := range args {
				if fn.param(i).Class == ClassNumber {
					coerceTo(a, Decimal)
				}
			}
		}
		return fn.Type
	case ResultSame:
		return args[0].Type
	case ResultFractional:
		if args[0].Type == Float || args[0].Type == Unknown {
			return args[0].Type
		}
		coerceTo(args[0], Decimal)
		return Decimal
	default:
		var values []*Typed
		for i, a := range args {
			if fn.param(i).Value {
				values = append(values, a)
			}
		}
		return c.common(n, values)
	}
}

func (c *checker) common(n *Node, values []*Typed) Type {
	t := Null
	for _, v := range values {
		next, ok := Common(t, v.Type)
		if !ok {
			c.fail(v.Node, CodeType, "%s mixes %s with %s; every branch must have a compatible type", describe(n), t, v.Type)
			return Unknown
		}
		t = next
	}
	for _, v := range values {
		coerceTo(v, t)
	}
	return t
}

func (c *checker) caseExpr(n *Node) *Typed {
	t := &Typed{Node: n}
	parts := []*Typed{}
	if n.Subject != nil {
		t.Subject = c.check(n.Subject)
		parts = append(parts, t.Subject)
	}
	var results []*Typed
	for i := 0; i+1 < len(n.Args); i += 2 {
		cond, res := c.check(n.Args[i]), c.check(n.Args[i+1])
		if t.Subject != nil {
			c.compare(&Node{Kind: KindBinary, Value: "=", Pos: cond.Node.Pos}, t.Subject, cond)
		} else {
			c.want(cond, Bool, "a when condition")
		}
		t.Args = append(t.Args, cond, res)
		results = append(results, res)
	}
	if n.Else != nil {
		t.Else = c.check(n.Else)
		results = append(results, t.Else)
	}
	t.Type = c.common(n, results)
	return c.combine(n, t, append(append(parts, t.Args...), t.Else)...)
}
