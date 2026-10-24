package analytics

import (
	"fmt"
	"strings"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
)

// subject is what a condition compares: an expression, its value type, and the field it reads
// when it is a field (so values bind in the column's own type). grain is set for the bucket
// column of a rollup, a timestamp, to the type of the field it buckets: values written for that
// field are compared with it as the instants they denote.
type subject struct {
	e     keel.DynExpr
	t     formula.Type
	field *Field
	grain formula.Type
}

// condScope says where a filter tree applies.
type condScope uint8

const (
	// scopeWhere filters the source rows.
	scopeWhere condScope = iota
	// scopeHaving filters the grouped rows by output columns.
	scopeHaving
	// scopeMeasure filters the rows a measure aggregates.
	scopeMeasure
)

// condEnv resolves the subjects of a filter tree where it applies.
type condEnv struct {
	scope condScope
	// field returns a field condition's subject and, for a field across a to-many path in the
	// where scope, wraps the comparison in an EXISTS over the path.
	field func(path string) (subject, func(keel.Pred) keel.Pred, error)
	// column returns an output column's value.
	column func(id string) (keel.DynExpr, error)
	// formula renders a checked calc condition.
	formula func(t *formula.Typed) (keel.DynExpr, error)
}

// filterPred compiles a filter tree rooted at pointer at; it returns nil when no condition
// applies (every condition bound to a parameter without a value).
func (c *compiler) filterPred(f *ir.Filter, at string, env condEnv) (keel.Pred, error) {
	if f == nil {
		return nil, nil
	}
	if !f.IsGroup() {
		return c.condition(f, at, env)
	}
	key := "and"
	if f.Or != nil {
		key = "or"
	}
	var parts []keel.Pred
	for i := range f.Children() {
		p, err := c.filterPred(&f.Children()[i], fmt.Sprintf("%s/%s/%d", at, key, i), env)
		if err != nil {
			return nil, err
		}
		if p != nil {
			parts = append(parts, p)
		}
	}
	switch {
	case len(parts) == 0:
		return nil, nil
	case len(parts) == 1:
		return parts[0], nil
	case key == "or":
		return keel.Or(parts...), nil
	}
	return keel.And(parts...), nil
}

// condition compiles one condition.
func (c *compiler) condition(f *ir.Filter, at string, env condEnv) (keel.Pred, error) {
	wrap := func(p keel.Pred) keel.Pred { return p }
	var s subject
	var err error
	switch {
	case f.Field != "":
		s, wrap, err = env.field(f.Field)
	case f.Column != "":
		s, err = c.columnSubject(f.Column, env)
	default:
		s, err = c.calcSubject(at, env)
	}
	if err != nil {
		return nil, err
	}
	if f.Calc != nil && f.Op == "" {
		return wrap(s.e), nil
	}
	if f.Transform != nil {
		s = c.transformed(s, f.Transform)
	}
	v, present := f.Value, true
	if f.Param != "" {
		v, present = c.params[f.Param]
	}
	if !present {
		return nil, nil
	}
	p, err := c.compare(s, f.Op, v, at)
	if err != nil || p == nil {
		return p, err
	}
	return wrap(p), nil
}

// columnSubject is an output column compared in having, or a per-row column in the where scope.
func (c *compiler) columnSubject(id string, env condEnv) (subject, error) {
	e, err := env.column(id)
	if err != nil {
		return subject{}, err
	}
	return subject{e: e, t: c.byID[id].out.Type}, nil
}

// calcSubject is a calc condition's formula, checked by validation under the condition's pointer.
func (c *compiler) calcSubject(at string, env condEnv) (subject, error) {
	typed, ok := c.res.Conditions[at]
	if !ok {
		return subject{}, fail(CodeInvalid, at, "the calc condition was not checked")
	}
	e, err := env.formula(typed)
	if err != nil {
		return subject{}, err
	}
	return subject{e: e, t: typed.Type}, nil
}

// transformed applies a transform to a condition's subject.
func (c *compiler) transformed(s subject, t *ir.Transform) subject {
	return subject{e: transform(s.e, t, s.t), t: t.Op.ResultType(s.t)}
}

// compare compiles a comparison of a subject with a value.
func (c *compiler) compare(s subject, op ir.Op, v any, at string) (keel.Pred, error) {
	switch op {
	case ir.OpIsNull:
		return s.e.IsNull(), nil
	case ir.OpIsNotNull:
		return s.e.IsNotNull(), nil
	case ir.OpContains, ir.OpStartsWith, ir.OpEndsWith, ir.OpLike, ir.OpILike:
		return textMatch(s, op, v)
	case ir.OpIn, ir.OpNotIn:
		return c.inList(s, op, v, at)
	case ir.OpBetween:
		return c.between(s, v, at)
	case ir.OpEq, ir.OpNe, ir.OpGt, ir.OpGte, ir.OpLt, ir.OpLte:
		x, err := c.operand(s, v, at)
		if err != nil {
			return nil, err
		}
		return ordering(s.e, op, x), nil
	}
	lo, hi, err := c.relative(op, v)
	if err != nil {
		return nil, err
	}
	a, err := c.operand(s, lo, at)
	if err != nil {
		return nil, err
	}
	b, err := c.operand(s, hi, at)
	if err != nil {
		return nil, err
	}
	return s.e.Gte(a).And(s.e.Lt(b)), nil
}

// ordering compiles eq, ne and the orderings. Not equal includes nulls: a person asking for
// shipments whose status is not Canceled expects the ones without a status too.
func ordering(e keel.DynExpr, op ir.Op, x any) keel.Pred {
	switch op {
	case ir.OpEq:
		return e.Eq(x)
	case ir.OpNe:
		return e.IsNull().Or(e.Ne(x))
	case ir.OpGt:
		return e.Gt(x)
	case ir.OpGte:
		return e.Gte(x)
	case ir.OpLt:
		return e.Lt(x)
	}
	return e.Lte(x)
}

// textMatch compiles contains, starts_with and ends_with (ignoring case, the value matched
// literally) and like and ilike (the value as a pattern), on the subject's text.
func textMatch(s subject, op ir.Op, v any) (keel.Pred, error) {
	text, ok := v.(string)
	if !ok {
		return nil, fail(CodeParameter, "", "%s compares with text", op)
	}
	e := s.e
	if s.t != formula.String {
		e = e.Cast(keel.TypeString)
	}
	switch op {
	case ir.OpContains:
		return e.ILike("%" + escapeLike(text) + "%"), nil
	case ir.OpStartsWith:
		return e.ILike(escapeLike(text) + "%"), nil
	case ir.OpEndsWith:
		return e.ILike("%" + escapeLike(text)), nil
	case ir.OpLike:
		return e.Like(text), nil
	}
	return e.ILike(text), nil
}

// escapeLike escapes LIKE's wildcards and escape character.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// inList compiles in and not_in with the list bound as one array parameter. Not in includes
// nulls, as ne does.
func (c *compiler) inList(s subject, op ir.Op, v any, at string) (keel.Pred, error) {
	list, ok := listOf(v)
	if !ok || len(list) == 0 {
		return nil, fail(CodeParameter, at, "%s needs a non-empty list", op)
	}
	arr, err := c.array(s, list, at)
	if err != nil {
		return nil, err
	}
	if op == ir.OpIn {
		return s.e.In(arr), nil
	}
	return s.e.IsNull().Or(s.e.NotIn(arr)), nil
}

// between compiles between as from (inclusive) to to (exclusive).
func (c *compiler) between(s subject, v any, at string) (keel.Pred, error) {
	list, ok := listOf(v)
	if !ok || len(list) != 2 {
		return nil, fail(CodeParameter, at, "between needs [from, to]")
	}
	lo, err := c.operand(s, list[0], at)
	if err != nil {
		return nil, err
	}
	hi, err := c.operand(s, list[1], at)
	if err != nil {
		return nil, err
	}
	return s.e.Gte(lo).And(s.e.Lt(hi)), nil
}

// operand converts a value for comparison with a subject: in the field's own Go type for a field,
// so Postgres infers the parameter from the column, and as a typed literal otherwise. Decimals
// are always bound as exact text and cast.
func (c *compiler) operand(s subject, v any, at string) (any, error) {
	if s.grain != formula.Unknown {
		x, err := Instant(v, s.grain, c.loc)
		if err != nil {
			return nil, &Error{Code: CodeParameter, Message: err.Error(), Pointer: at}
		}
		return lit(x, formula.Timestamp), nil
	}
	if s.field != nil && s.field.numericEnum() {
		n, err := toInt(v)
		if err != nil {
			return nil, fail(CodeParameter, at, "%v is not a value of %s", v, s.field.Key)
		}
		return n, nil
	}
	x, err := convert(s.t, v, c.loc)
	if err != nil {
		return nil, &Error{Code: CodeParameter, Message: err.Error(), Pointer: at}
	}
	if s.field == nil || s.t == formula.Decimal || s.t == formula.Date {
		return lit(x, s.t), nil
	}
	return x, nil
}

// array converts a list for in and not_in: a typed slice, or a decimal array bound as text and
// cast.
func (c *compiler) array(s subject, list []any, at string) (any, error) {
	if s.field != nil && s.field.numericEnum() {
		out := make([]int64, len(list))
		for i, x := range list {
			n, err := toInt(x)
			if err != nil {
				return nil, fail(CodeParameter, at, "%v is not a value of %s", x, s.field.Key)
			}
			out[i] = n
		}
		return out, nil
	}
	values := make([]any, len(list))
	for i, x := range list {
		var v any
		var err error
		if s.grain != formula.Unknown {
			v, err = Instant(x, s.grain, c.loc)
		} else {
			v, err = convert(s.t, x, c.loc)
		}
		if err != nil {
			return nil, &Error{Code: CodeParameter, Message: err.Error(), Pointer: at}
		}
		values[i] = v
	}
	return typedSlice(s.t, values), nil
}

// typedSlice returns converted values as the slice type pgx binds for t.
func typedSlice(t formula.Type, values []any) any {
	switch t {
	case formula.Int, formula.Epoch:
		out := make([]int64, len(values))
		for i, v := range values {
			out[i] = v.(int64)
		}
		return out
	case formula.Float:
		out := make([]float64, len(values))
		for i, v := range values {
			out[i] = v.(float64)
		}
		return out
	case formula.Bool:
		out := make([]bool, len(values))
		for i, v := range values {
			out[i] = v.(bool)
		}
		return out
	case formula.Timestamp:
		out := make([]time.Time, len(values))
		for i, v := range values {
			out[i] = v.(time.Time)
		}
		return out
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = textOf(v)
	}
	switch t {
	case formula.Decimal:
		return keel.DynOf(keel.Raw("CAST(? AS numeric[])", keel.DynOf(keel.Raw("?::text[]", out))))
	case formula.Date:
		return keel.DynOf(keel.Raw("CAST(? AS date[])", keel.DynOf(keel.Raw("?::text[]", out))))
	}
	return out
}

// relative returns the half-open range of a relative date operator in the report's time zone.
func (c *compiler) relative(op ir.Op, v any) (time.Time, time.Time, error) {
	return RelativeRange(op, v, c.now, c.loc)
}

// RelativeRange returns the half-open range [from, to) a relative date operator (today,
// last_week, last_n_days, …) selects at now in loc, with v the day count of last_n_days and
// next_n_days: days start at local midnight and weeks on Monday; last_n_days N is the N days
// before today and today, next_n_days N today and the N days after it. The compiler compares
// fields with these bounds; a rollup router checks them against its buckets.
func RelativeRange(op ir.Op, v any, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
	now = now.In(loc)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	week := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	quarter := time.Date(now.Year(), now.Month()-(now.Month()-1)%3, 1, 0, 0, 0, 0, loc)
	year := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)
	switch op {
	case ir.OpLastNDays, ir.OpNextNDays:
		n, err := toInt(v)
		if err != nil || n < 1 || n > ir.MaxDays {
			return time.Time{}, time.Time{}, fail(CodeParameter, "", "%s needs a day count from 1 to %d", op, ir.MaxDays)
		}
		if op == ir.OpLastNDays {
			return day.AddDate(0, 0, -int(n)), day.AddDate(0, 0, 1), nil
		}
		return day, day.AddDate(0, 0, int(n)+1), nil
	case ir.OpToday:
		return day, day.AddDate(0, 0, 1), nil
	case ir.OpYesterday:
		return day.AddDate(0, 0, -1), day, nil
	case ir.OpTomorrow:
		return day.AddDate(0, 0, 1), day.AddDate(0, 0, 2), nil
	case ir.OpThisWeek:
		return week, week.AddDate(0, 0, 7), nil
	case ir.OpLastWeek:
		return week.AddDate(0, 0, -7), week, nil
	case ir.OpThisMonth:
		return month, month.AddDate(0, 1, 0), nil
	case ir.OpLastMonth:
		return month.AddDate(0, -1, 0), month, nil
	case ir.OpThisQuarter:
		return quarter, quarter.AddDate(0, 3, 0), nil
	case ir.OpLastQuarter:
		return quarter.AddDate(0, -3, 0), quarter, nil
	case ir.OpThisYear:
		return year, year.AddDate(1, 0, 0), nil
	case ir.OpLastYear:
		return year.AddDate(-1, 0, 0), year, nil
	}
	return time.Time{}, time.Time{}, fail(CodeInvalid, "", "unknown operator %s", op)
}

// transform applies a column or condition transform to a value of type t.
func transform(e keel.DynExpr, t *ir.Transform, in formula.Type) keel.DynExpr {
	if t == nil {
		return e
	}
	places := int64(0)
	if t.Precision != nil {
		places = int64(*t.Precision)
	}
	num := e
	if in != formula.Decimal {
		num = e.Cast(keel.TypeNumeric)
	}
	switch t.Op {
	case ir.TransformRound:
		return keel.DynOf(keel.Raw("round(?, ?::integer)", num, places))
	case ir.TransformTruncate:
		return keel.DynOf(keel.Raw("trunc(?, ?::integer)", num, places))
	case ir.TransformCeil:
		return keel.DynOf(keel.Raw("(ceil(? * ?) / ?)", num, pow10Lit(places), pow10Lit(places)))
	case ir.TransformFloor:
		return keel.DynOf(keel.Raw("(floor(? * ?) / ?)", num, pow10Lit(places), pow10Lit(places)))
	case ir.TransformAbs:
		return keel.DynFunc("abs", e)
	case ir.TransformScale:
		return num.Mul(lit(keel.Decimal(fmt.Sprint(*t.Factor)), formula.Decimal))
	case ir.TransformUpper:
		return keel.DynFunc("upper", textOfExpr(e, in))
	case ir.TransformLower:
		return keel.DynFunc("lower", textOfExpr(e, in))
	case ir.TransformTitle:
		return keel.DynFunc("initcap", textOfExpr(e, in))
	}
	return keel.DynFunc("btrim", textOfExpr(e, in))
}

// pow10Lit is 10^n as an exact decimal literal.
func pow10Lit(n int64) keel.DynExpr {
	return lit(keel.Decimal("1"+strings.Repeat("0", int(n))), formula.Decimal)
}

// textOfExpr casts a non-text value to text.
func textOfExpr(e keel.DynExpr, t formula.Type) keel.DynExpr {
	if t == formula.String {
		return e
	}
	return e.Cast(keel.TypeString)
}
