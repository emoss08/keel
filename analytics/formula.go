package analytics

import (
	"strconv"
	"strings"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
)

// formulaEnv resolves the references of a formula where it renders: output columns, fields and
// aggregate calls over fields.
type formulaEnv interface {
	column(id string) (keel.DynExpr, error)
	field(path string) (keel.DynExpr, error)
	aggregate(t *formula.Typed) (keel.DynExpr, error)
}

// render renders a checked formula through the function registry's SQL templates. Literals and
// parameters are bound values; identifiers come only from the catalog through env.
func (c *compiler) render(t *formula.Typed, env formulaEnv) (keel.DynExpr, error) {
	e, err := c.renderNode(t, env)
	if err != nil {
		return keel.DynExpr{}, err
	}
	if t.Coerce != formula.Unknown {
		if tmpl, ok := formula.Coercion(t.Type, t.Coerce); ok {
			return c.template(tmpl, []keel.DynExpr{e})
		}
	}
	return e, nil
}

// renderNode renders a node without its coercion.
func (c *compiler) renderNode(t *formula.Typed, env formulaEnv) (keel.DynExpr, error) {
	n := t.Node
	switch n.Kind {
	case formula.KindNumber:
		return numberLit(n.Value, t.Type), nil
	case formula.KindString:
		return lit(n.Value, formula.String), nil
	case formula.KindBool:
		return lit(n.Value == "true", formula.Bool), nil
	case formula.KindNull:
		return nullLit(), nil
	case formula.KindDate:
		return lit(n.Value, formula.Date), nil
	case formula.KindTimestamp:
		at, err := time.Parse(time.RFC3339Nano, n.Value)
		if err != nil {
			return keel.DynExpr{}, fail(CodeInvalid, "", "invalid timestamp %q", n.Value)
		}
		return lit(at, formula.Timestamp), nil
	case formula.KindColumn:
		return env.column(n.Value)
	case formula.KindField:
		return env.field(n.Value)
	case formula.KindParam:
		return c.paramLit(n.Value), nil
	case formula.KindUnary, formula.KindBinary:
		return c.operator(t, env)
	case formula.KindCall:
		return c.call(t, env)
	}
	return c.caseExpr(t, env)
}

// operator renders a unary or binary operator through its template.
func (c *compiler) operator(t *formula.Typed, env formulaEnv) (keel.DynExpr, error) {
	args, err := c.renderAll(t.Args, env)
	if err != nil {
		return keel.DynExpr{}, err
	}
	return c.template(formula.OperatorSQL(t.Node), args)
}

// call renders a function call: an aggregate through env, anything else through the registry's
// template for its arguments, with the cast its result type asks for.
func (c *compiler) call(t *formula.Typed, env formulaEnv) (keel.DynExpr, error) {
	if t.Func.Aggregate {
		return env.aggregate(t)
	}
	args := make([]keel.DynExpr, len(t.Args))
	for i, a := range t.Args {
		var err error
		switch p := t.Func.Params[min(i, len(t.Func.Params)-1)]; p.Class {
		case formula.ClassUnit:
			args[i] = lit(a.Node.Value, formula.String)
		case formula.ClassDigits:
			d, _ := strconv.Atoi(a.Node.Value)
			args[i] = keel.DynOf(keel.Raw("?::integer", int64(d)))
		default:
			args[i], err = c.render(a, env)
		}
		if err != nil {
			return keel.DynExpr{}, err
		}
	}
	nodes := make([]*formula.Node, len(t.Args))
	for i, a := range t.Args {
		nodes[i] = a.Node
	}
	e, err := c.template(t.Func.Template(nodes), args)
	if err != nil {
		return keel.DynExpr{}, err
	}
	return c.castResult(t, e)
}

// castResult wraps a call by its function's cast for the result type.
func (c *compiler) castResult(t *formula.Typed, e keel.DynExpr) (keel.DynExpr, error) {
	if tmpl, ok := t.Func.SQL.Cast[t.Type]; ok {
		return c.template(tmpl, []keel.DynExpr{e})
	}
	return e, nil
}

// caseExpr renders case [subject] when … then … [else …] end.
func (c *compiler) caseExpr(t *formula.Typed, env formulaEnv) (keel.DynExpr, error) {
	var b strings.Builder
	var args []any
	b.WriteString("CASE")
	if t.Subject != nil {
		s, err := c.render(t.Subject, env)
		if err != nil {
			return keel.DynExpr{}, err
		}
		b.WriteString(" ?")
		args = append(args, s)
	}
	for i := 0; i+1 < len(t.Args); i += 2 {
		when, err := c.render(t.Args[i], env)
		if err != nil {
			return keel.DynExpr{}, err
		}
		then, err := c.render(t.Args[i+1], env)
		if err != nil {
			return keel.DynExpr{}, err
		}
		b.WriteString(" WHEN ? THEN ?")
		args = append(args, when, then)
	}
	if t.Else != nil {
		e, err := c.render(t.Else, env)
		if err != nil {
			return keel.DynExpr{}, err
		}
		b.WriteString(" ELSE ?")
		args = append(args, e)
	}
	b.WriteString(" END")
	return keel.DynOf(keel.Raw(b.String(), args...)), nil
}

// renderAll renders operands.
func (c *compiler) renderAll(ts []*formula.Typed, env formulaEnv) ([]keel.DynExpr, error) {
	out := make([]keel.DynExpr, len(ts))
	for i, a := range ts {
		e, err := c.render(a, env)
		if err != nil {
			return nil, err
		}
		out[i] = e
	}
	return out, nil
}

// template renders a registry template: {0}, {1}, … are the arguments, {args} all of them joined
// with ", ", {tz} the report's time zone as a bound parameter. Templates are keel's own; nothing a
// person typed reaches them.
func (c *compiler) template(tmpl string, args []keel.DynExpr) (keel.DynExpr, error) {
	var b strings.Builder
	var bound []any
	for i := 0; i < len(tmpl); i++ {
		ch := tmpl[i]
		switch {
		case ch == '?':
			b.WriteString("??")
		case ch == '{':
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 {
				return keel.DynExpr{}, fail(CodeNotAllowed, "", "malformed template %q", tmpl)
			}
			token := tmpl[i+1 : i+end]
			i += end
			var err error
			bound, err = c.placeholder(&b, token, args, bound)
			if err != nil {
				return keel.DynExpr{}, err
			}
		default:
			b.WriteByte(ch)
		}
	}
	return keel.DynOf(keel.Raw(b.String(), bound...)), nil
}

// placeholder writes one template token's placeholders and appends their arguments.
func (c *compiler) placeholder(b *strings.Builder, token string, args []keel.DynExpr, bound []any) ([]any, error) {
	switch token {
	case "tz":
		b.WriteByte('?')
		return append(bound, c.tz), nil
	case "args":
		for j, a := range args {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('?')
			bound = append(bound, a)
		}
		return bound, nil
	}
	k, err := strconv.Atoi(token)
	if err != nil || k < 0 || k >= len(args) {
		return nil, fail(CodeNotAllowed, "", "template token {%s} has no argument", token)
	}
	b.WriteByte('?')
	return append(bound, args[k]), nil
}

// paramLit is a formula's reference to a parameter: its value bound with its type's cast, or NULL
// when it has none.
func (c *compiler) paramLit(name string) keel.DynExpr {
	v, ok := c.params[name]
	p, _ := c.def.Parameter(name)
	if !ok || p == nil {
		return nullLit()
	}
	return lit(v, p.Type)
}

// lit binds v with the cast of type t, so Postgres knows the parameter's type wherever it is
// used. Decimals and dates are bound as text and cast, keeping every digit.
func lit(v any, t formula.Type) keel.DynExpr {
	switch t {
	case formula.Int, formula.Epoch:
		return keel.DynLit(v).Cast(keel.TypeInt64)
	case formula.Float:
		return keel.DynLit(v).Cast(keel.TypeFloat64)
	case formula.Decimal:
		return keel.DynOf(keel.Raw("CAST(? AS numeric)", keel.DynLit(textOf(v)).Cast(keel.TypeString)))
	case formula.Bool:
		return keel.DynLit(v).Cast(keel.TypeBool)
	case formula.Timestamp:
		return keel.DynLit(v).Cast(keel.TypeTime)
	case formula.Date:
		return keel.DynOf(keel.Raw("CAST(? AS date)", keel.DynLit(textOf(v)).Cast(keel.TypeString)))
	}
	return keel.DynLit(textOf(v)).Cast(keel.TypeString)
}

// numberLit binds a formula's number literal: an integer or an exact decimal.
func numberLit(digits string, t formula.Type) keel.DynExpr {
	if t == formula.Int {
		if n, err := strconv.ParseInt(digits, 10, 64); err == nil {
			return lit(n, formula.Int)
		}
	}
	return lit(keel.Decimal(digits), formula.Decimal)
}

// nullLit is NULL.
func nullLit() keel.DynExpr { return keel.DynLit(nil) }
