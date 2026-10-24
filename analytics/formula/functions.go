package formula

import (
	"slices"
	"strings"
)

// Class is the kind of argument a function parameter accepts.
type Class string

const (
	// ClassAny accepts a value of any type.
	ClassAny Class = "any"
	// ClassNumber accepts int, decimal and float.
	ClassNumber Class = "number"
	// ClassText accepts string and enum; an enum is converted to text.
	ClassText Class = "text"
	// ClassTemporal accepts date, timestamp and epoch, converted to a timestamp.
	ClassTemporal Class = "temporal"
	// ClassBool accepts bool.
	ClassBool Class = "bool"
	// ClassOrdered accepts any type with an order: numbers, text and instants.
	ClassOrdered Class = "ordered"
	// ClassDigits accepts an integer literal from 0 to MaxDigits.
	ClassDigits Class = "digits"
	// ClassUnit accepts a text literal from the parameter's Units.
	ClassUnit Class = "unit"
)

// Classes returns every parameter class.
func Classes() []Class {
	return []Class{ClassAny, ClassNumber, ClassText, ClassTemporal, ClassBool, ClassOrdered, ClassDigits, ClassUnit}
}

// MaxDigits is the largest precision round and trunc accept.
const MaxDigits = 10

// Param is one parameter of a function signature.
type Param struct {
	// Name is the parameter name shown in signatures.
	Name string `json:"name"`
	// Class is the kind of argument accepted.
	Class Class `json:"class"`
	// Optional parameters may be left out.
	Optional bool `json:"optional,omitempty"`
	// Variadic accepts any number of further arguments.
	Variadic bool `json:"variadic,omitempty"`
	// Value arguments take part in the common result type and are converted to it.
	Value bool `json:"value,omitempty"`
	// Units are the literals a unit parameter accepts.
	Units []string `json:"units,omitempty"`
}

// Result is the rule that gives a call its type from its arguments.
type Result string

const (
	// ResultFixed is always the function's Type.
	ResultFixed Result = "fixed"
	// ResultSame is the type of the first argument.
	ResultSame Result = "same"
	// ResultCommon is the common type of the value arguments, which are converted to it.
	ResultCommon Result = "common"
	// ResultFractional is float for a float argument and decimal otherwise.
	ResultFractional Result = "fractional"
)

// Results returns every result rule.
func Results() []Result { return []Result{ResultFixed, ResultSame, ResultCommon, ResultFractional} }

// SQL is how the compiler renders a call. Templates hold argument positions ({0}, {1}, …), {args}
// for every argument joined with ", ", and {tz} for the report time zone, which the compiler binds
// as a parameter; nothing a user typed is ever spliced into them. Arity overrides the template
// for a number of arguments, Unit for the value of the unit argument, and Cast wraps the rendered
// call ({0}) by its result type.
type SQL struct {
	// Template renders the call.
	Template string `json:"template,omitempty"`
	// Arity overrides the template by argument count.
	Arity map[int]string `json:"arity,omitempty"`
	// Unit overrides the template by the unit argument.
	Unit map[string]string `json:"unit,omitempty"`
	// Cast wraps the rendered call by result type.
	Cast map[Type]string `json:"cast,omitempty"`
}

// Function is one entry of the function registry.
type Function struct {
	// Name is the lower-case function name.
	Name string `json:"name"`
	// Doc says what the function returns.
	Doc string `json:"doc"`
	// Example is a formula using it.
	Example string `json:"example"`
	// Params is the signature.
	Params []Param `json:"params"`
	// Result is the rule giving the result type.
	Result Result `json:"result"`
	// Type is the result type of a fixed result.
	Type Type `json:"type,omitempty"`
	// Aggregate functions work on groups of rows.
	Aggregate bool `json:"aggregate,omitempty"`
	// Syntax describes a special form, such as case, that is not called by name.
	Syntax string `json:"syntax,omitempty"`
	// SQL is how the call renders.
	SQL SQL `json:"sql"`
}

// Signature prints the call shape, such as "round(number: number, digits?: digits)".
func (f *Function) Signature() string {
	if f.Syntax != "" {
		return f.Syntax
	}
	parts := make([]string, len(f.Params))
	for i, p := range f.Params {
		mark := ""
		if p.Optional {
			mark = "?"
		}
		prefix := ""
		if p.Variadic {
			prefix = "..."
		}
		parts[i] = prefix + p.Name + mark + ": " + string(p.Class)
		if p.Class == ClassUnit {
			parts[i] = prefix + p.Name + mark + ": '" + strings.Join(p.Units, "' | '") + "'"
		}
	}
	return f.Name + "(" + strings.Join(parts, ", ") + ")"
}

// Returns describes the result rule in words.
func (f *Function) Returns() string {
	switch f.Result {
	case ResultFixed:
		return string(f.Type)
	case ResultSame:
		return "the type of " + f.Params[0].Name
	case ResultCommon:
		return "the common type of its values"
	default:
		return "decimal (float for a float)"
	}
}

func (f *Function) arity() (lo, hi int) {
	for _, p := range f.Params {
		switch {
		case p.Variadic:
			if !p.Optional {
				lo++
			}
			hi = -1
		case p.Optional:
			if hi >= 0 {
				hi++
			}
		default:
			lo++
			if hi >= 0 {
				hi++
			}
		}
	}
	return lo, hi
}

func (f *Function) param(i int) Param {
	if i < len(f.Params) {
		return f.Params[i]
	}
	return f.Params[len(f.Params)-1]
}

// Template returns the SQL template for a call with these arguments: the arity or unit override
// if one applies, else the default template.
func (f *Function) Template(args []*Node) string {
	if t, ok := f.SQL.Arity[len(args)]; ok {
		return t
	}
	for i, p := range f.Params {
		if p.Class == ClassUnit && i < len(args) {
			if t, ok := f.SQL.Unit[args[i].Value]; ok {
				return t
			}
		}
	}
	return f.SQL.Template
}

var dateUnits = []string{"hour", "day", "week", "month", "quarter", "year"}

var diffUnits = []string{"second", "minute", "hour", "day", "week", "month", "quarter", "year"}

var extractParts = []string{"year", "quarter", "month", "week", "day", "dow", "isodow", "doy", "hour", "minute"}

const (
	localEnd   = "({2} AT TIME ZONE {tz})"
	localStart = "({1} AT TIME ZONE {tz})"
)

var registry = []*Function{
	{Name: "if", Doc: "The second argument when the condition is true, else the third.", Example: "if([miles] > 0, [revenue] / [miles], 0)",
		Params: []Param{{Name: "condition", Class: ClassBool}, {Name: "then", Class: ClassAny, Value: true}, {Name: "else", Class: ClassAny, Value: true}},
		Result: ResultCommon, SQL: SQL{Template: "CASE WHEN {0} THEN {1} ELSE {2} END"}},
	{Name: "case", Doc: "The result of the first branch whose condition holds (or whose value equals the subject), else the else branch or null.",
		Example: "case when [margin] < 0 then 'loss' when [margin] < 0.1 then 'thin' else 'healthy' end", Syntax: "case [subject] when condition then result … [else result] end",
		Params: []Param{{Name: "result", Class: ClassAny, Variadic: true, Value: true}}, Result: ResultCommon,
		SQL: SQL{Template: "CASE [{subject}] WHEN {when} THEN {then} … [ELSE {else}] END"}},
	{Name: "coalesce", Doc: "The first argument that is not null.", Example: "coalesce({customer.name}, 'Unknown')",
		Params: []Param{{Name: "value", Class: ClassAny, Value: true}, {Name: "fallback", Class: ClassAny, Variadic: true, Value: true}},
		Result: ResultCommon, SQL: SQL{Template: "coalesce({args})"}},
	{Name: "nullif", Doc: "Null when both arguments are equal, else the first.", Example: "[revenue] / nullif([miles], 0)",
		Params: []Param{{Name: "value", Class: ClassAny, Value: true}, {Name: "other", Class: ClassAny, Value: true}},
		Result: ResultCommon, SQL: SQL{Template: "nullif({0}, {1})"}},
	{Name: "round", Doc: "The number rounded half away from zero to the given decimal places (0 when omitted).", Example: "round([revenue] / [miles], 2)",
		Params: []Param{{Name: "number", Class: ClassNumber}, {Name: "digits", Class: ClassDigits, Optional: true}},
		Result: ResultFixed, Type: Decimal, SQL: SQL{Arity: map[int]string{1: "round({0})", 2: "round({0}, {1})"}}},
	{Name: "trunc", Doc: "The number truncated toward zero to the given decimal places (0 when omitted).", Example: "trunc({weight} / 1000, 1)",
		Params: []Param{{Name: "number", Class: ClassNumber}, {Name: "digits", Class: ClassDigits, Optional: true}},
		Result: ResultFixed, Type: Decimal, SQL: SQL{Arity: map[int]string{1: "trunc({0})", 2: "trunc({0}, {1})"}}},
	{Name: "floor", Doc: "The largest whole number not greater than the argument.", Example: "floor([miles] / 100)",
		Params: []Param{{Name: "number", Class: ClassNumber}}, Result: ResultFractional, SQL: SQL{Template: "floor({0})"}},
	{Name: "ceil", Doc: "The smallest whole number not less than the argument.", Example: "ceil({weight} / 1000)",
		Params: []Param{{Name: "number", Class: ClassNumber}}, Result: ResultFractional, SQL: SQL{Template: "ceil({0})"}},
	{Name: "abs", Doc: "The absolute value.", Example: "abs([variance])",
		Params: []Param{{Name: "number", Class: ClassNumber}}, Result: ResultSame, SQL: SQL{Template: "abs({0})"}},
	{Name: "greatest", Doc: "The largest of the arguments, ignoring nulls.", Example: "greatest([billed], [quoted])",
		Params: []Param{{Name: "value", Class: ClassOrdered, Value: true}, {Name: "other", Class: ClassOrdered, Variadic: true, Value: true}},
		Result: ResultCommon, SQL: SQL{Template: "greatest({args})"}},
	{Name: "least", Doc: "The smallest of the arguments, ignoring nulls.", Example: "least([billed], [quoted])",
		Params: []Param{{Name: "value", Class: ClassOrdered, Value: true}, {Name: "other", Class: ClassOrdered, Variadic: true, Value: true}},
		Result: ResultCommon, SQL: SQL{Template: "least({args})"}},
	{Name: "concat", Doc: "The arguments as text joined together; nulls are skipped.", Example: "concat({firstName}, ' ', {lastName})",
		Params: []Param{{Name: "value", Class: ClassAny, Variadic: true}}, Result: ResultFixed, Type: String, SQL: SQL{Template: "concat({args})"}},
	{Name: "lower", Doc: "The text in lower case.", Example: "lower({customer.code})",
		Params: []Param{{Name: "text", Class: ClassText}}, Result: ResultFixed, Type: String, SQL: SQL{Template: "lower({0})"}},
	{Name: "upper", Doc: "The text in upper case.", Example: "upper({customer.code})",
		Params: []Param{{Name: "text", Class: ClassText}}, Result: ResultFixed, Type: String, SQL: SQL{Template: "upper({0})"}},
	{Name: "trim", Doc: "The text without leading and trailing spaces.", Example: "trim({reference})",
		Params: []Param{{Name: "text", Class: ClassText}}, Result: ResultFixed, Type: String, SQL: SQL{Template: "btrim({0})"}},
	{Name: "length", Doc: "The number of characters in the text.", Example: "length({proNumber})",
		Params: []Param{{Name: "text", Class: ClassText}}, Result: ResultFixed, Type: Int, SQL: SQL{Template: "char_length({0})"}},
	{Name: "date_trunc", Doc: "The instant the period containing the value starts, in the report time zone.", Example: "date_trunc('month', {shippedAt})",
		Params: []Param{{Name: "unit", Class: ClassUnit, Units: dateUnits}, {Name: "value", Class: ClassTemporal}},
		Result: ResultFixed, Type: Timestamp, SQL: SQL{Template: "date_trunc({0}, {1}, {tz})"}},
	{Name: "date_diff", Doc: "Whole units from start to end: elapsed time for seconds, minutes and hours, calendar boundaries crossed in the report time zone for days and longer.",
		Example: "date_diff('day', {pickedUpAt}, {deliveredAt})",
		Params:  []Param{{Name: "unit", Class: ClassUnit, Units: diffUnits}, {Name: "start", Class: ClassTemporal}, {Name: "end", Class: ClassTemporal}},
		Result:  ResultFixed, Type: Int, SQL: SQL{Unit: map[string]string{
			"second":  "floor(extract(epoch from ({2} - {1})))::bigint",
			"minute":  "floor(extract(epoch from ({2} - {1})) / 60)::bigint",
			"hour":    "floor(extract(epoch from ({2} - {1})) / 3600)::bigint",
			"day":     "(" + localEnd + "::date - " + localStart + "::date)::bigint",
			"week":    "((" + localEnd + "::date - " + localStart + "::date) / 7)::bigint",
			"month":   "((extract(year from " + localEnd + ") - extract(year from " + localStart + ")) * 12 + extract(month from " + localEnd + ") - extract(month from " + localStart + "))::bigint",
			"quarter": "((extract(year from " + localEnd + ") - extract(year from " + localStart + ")) * 4 + extract(quarter from " + localEnd + ") - extract(quarter from " + localStart + "))::bigint",
			"year":    "(extract(year from " + localEnd + ") - extract(year from " + localStart + "))::bigint",
		}}},
	{Name: "extract", Doc: "A part of the value's local date or time in the report time zone; dow counts from 0 on Sunday, isodow from 1 on Monday.", Example: "extract('dow', {shippedAt})",
		Params: []Param{{Name: "part", Class: ClassUnit, Units: extractParts}, {Name: "value", Class: ClassTemporal}},
		Result: ResultFixed, Type: Int, SQL: SQL{Template: "date_part({0}, {1} AT TIME ZONE {tz})::bigint"}},
	{Name: "now", Doc: "The instant the statement started.", Example: "date_diff('day', {dueAt}, now())",
		Params: []Param{}, Result: ResultFixed, Type: Timestamp, SQL: SQL{Template: "now()"}},
	{Name: "today", Doc: "Today's date in the report time zone.", Example: "date_diff('day', {expiresOn}, today())",
		Params: []Param{}, Result: ResultFixed, Type: Date, SQL: SQL{Template: "(now() AT TIME ZONE {tz})::date"}},
	{Name: "count", Doc: "The number of rows in the group, or of rows where the value is not null.", Example: "count({moves.id})", Aggregate: true,
		Params: []Param{{Name: "value", Class: ClassAny, Optional: true}}, Result: ResultFixed, Type: Int,
		SQL: SQL{Arity: map[int]string{0: "count(*)", 1: "count({0})"}}},
	{Name: "count_distinct", Doc: "The number of distinct non-null values in the group.", Example: "count_distinct({customer.id})", Aggregate: true,
		Params: []Param{{Name: "value", Class: ClassAny}}, Result: ResultFixed, Type: Int, SQL: SQL{Template: "count(DISTINCT {0})"}},
	{Name: "sum", Doc: "The total of the values in the group.", Example: "sum({totalCharge}) / sum({moves.distance})", Aggregate: true,
		Params: []Param{{Name: "number", Class: ClassNumber}}, Result: ResultSame, SQL: SQL{Template: "sum({0})", Cast: map[Type]string{Int: "({0})::bigint"}}},
	{Name: "avg", Doc: "The mean of the values in the group.", Example: "avg({weight})", Aggregate: true,
		Params: []Param{{Name: "number", Class: ClassNumber}}, Result: ResultFractional, SQL: SQL{Template: "avg({0})"}},
	{Name: "min", Doc: "The smallest value in the group.", Example: "min({shippedAt})", Aggregate: true,
		Params: []Param{{Name: "value", Class: ClassOrdered}}, Result: ResultSame, SQL: SQL{Template: "min({0})"}},
	{Name: "max", Doc: "The largest value in the group.", Example: "max({deliveredAt})", Aggregate: true,
		Params: []Param{{Name: "value", Class: ClassOrdered}}, Result: ResultSame, SQL: SQL{Template: "max({0})"}},
}

// Functions returns the function registry in a stable order, including the case form. The
// entries are shared and must not be modified.
func Functions() []*Function { return slices.Clone(registry) }

// Lookup returns the callable function with the given name, ignoring case.
func Lookup(name string) (*Function, bool) {
	lower := strings.ToLower(name)
	for _, f := range registry {
		if f.Name == lower && f.Syntax == "" {
			return f, true
		}
	}
	return nil, false
}

func functionNames() []string {
	var names []string
	for _, f := range registry {
		if f.Syntax == "" {
			names = append(names, f.Name)
		}
	}
	return names
}

// Operator is one operator of the language, with its precedence and SQL template.
type Operator struct {
	// Symbol is the operator as written.
	Symbol string `json:"symbol"`
	// Form is infix, prefix or postfix.
	Form string `json:"form"`
	// Precedence orders binding; higher binds tighter.
	Precedence int `json:"precedence"`
	// Doc says what the operator does.
	Doc string `json:"doc"`
	// SQL is the rendering template.
	SQL string `json:"sql"`
}

const (
	precOr      = 10
	precAnd     = 20
	precNot     = 25
	precCompare = 30
	precConcat  = 40
	precAdd     = 50
	precMul     = 60
	precNeg     = 70
)

var operators = []Operator{
	{Symbol: "or", Form: "infix", Precedence: precOr, Doc: "True when either side is true.", SQL: "({0} OR {1})"},
	{Symbol: "and", Form: "infix", Precedence: precAnd, Doc: "True when both sides are true.", SQL: "({0} AND {1})"},
	{Symbol: "not", Form: "prefix", Precedence: precNot, Doc: "True when the operand is false.", SQL: "(NOT {0})"},
	{Symbol: "=", Form: "infix", Precedence: precCompare, Doc: "Equal.", SQL: "({0} = {1})"},
	{Symbol: "!=", Form: "infix", Precedence: precCompare, Doc: "Not equal; <> is accepted too.", SQL: "({0} <> {1})"},
	{Symbol: "<", Form: "infix", Precedence: precCompare, Doc: "Less than.", SQL: "({0} < {1})"},
	{Symbol: "<=", Form: "infix", Precedence: precCompare, Doc: "Less than or equal.", SQL: "({0} <= {1})"},
	{Symbol: ">", Form: "infix", Precedence: precCompare, Doc: "Greater than.", SQL: "({0} > {1})"},
	{Symbol: ">=", Form: "infix", Precedence: precCompare, Doc: "Greater than or equal.", SQL: "({0} >= {1})"},
	{Symbol: "is null", Form: "postfix", Precedence: precCompare, Doc: "True when the operand is null.", SQL: "({0} IS NULL)"},
	{Symbol: "is not null", Form: "postfix", Precedence: precCompare, Doc: "True when the operand is not null.", SQL: "({0} IS NOT NULL)"},
	{Symbol: "||", Form: "infix", Precedence: precConcat, Doc: "Both sides as text, joined; null when either is null.", SQL: "({0} || {1})"},
	{Symbol: "+", Form: "infix", Precedence: precAdd, Doc: "Addition.", SQL: "({0} + {1})"},
	{Symbol: "-", Form: "infix", Precedence: precAdd, Doc: "Subtraction.", SQL: "({0} - {1})"},
	{Symbol: "*", Form: "infix", Precedence: precMul, Doc: "Multiplication.", SQL: "({0} * {1})"},
	{Symbol: "/", Form: "infix", Precedence: precMul, Doc: "Division; int / int is decimal, and dividing by zero gives null.", SQL: "({0} / nullif({1}, 0))"},
	{Symbol: "%", Form: "infix", Precedence: precMul, Doc: "Remainder of int or decimal division; by zero gives null.", SQL: "({0} % nullif({1}, 0))"},
	{Symbol: "-", Form: "prefix", Precedence: precNeg, Doc: "Negation.", SQL: "(-{0})"},
}

// Operators returns the operator table, loosest binding first.
func Operators() []Operator { return slices.Clone(operators) }

// OperatorSQL returns the SQL template of an operator node: unary for one argument, binary for two.
func OperatorSQL(n *Node) string {
	form := "infix"
	if n.Kind == KindUnary {
		form = "prefix"
		if strings.HasPrefix(n.Value, "is ") {
			form = "postfix"
		}
	}
	for _, o := range operators {
		if o.Symbol == n.Value && o.Form == form {
			return o.SQL
		}
	}
	return ""
}

func binaryPrec(op string) int {
	for _, o := range operators {
		if o.Symbol == op && o.Form == "infix" {
			return o.Precedence
		}
	}
	return 0
}

// Coercion returns the SQL template that converts a value of type from to type to, with {0} for
// the value and {tz} for the report time zone, and false when no conversion is needed or exists.
func Coercion(from, to Type) (string, bool) {
	switch {
	case from == to || from == Null || from == Unknown:
		return "", false
	case to == String:
		return "({0})::text", true
	case from == Int && to == Decimal:
		return "({0})::numeric", true
	case from.Numeric() && to == Float:
		return "({0})::float8", true
	case from == Epoch && to == Timestamp:
		return "to_timestamp({0})", true
	case from == Date && to == Timestamp:
		return "(({0})::timestamp AT TIME ZONE {tz})", true
	default:
		return "", false
	}
}
