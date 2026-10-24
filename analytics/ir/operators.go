package ir

import (
	"slices"

	"github.com/emoss08/keel/analytics/formula"
)

// Op is the comparison of a filter condition.
type Op string

const (
	// OpEq is equal.
	OpEq Op = "eq"
	// OpNe is not equal.
	OpNe Op = "ne"
	// OpGt is greater than.
	OpGt Op = "gt"
	// OpGte is greater than or equal.
	OpGte Op = "gte"
	// OpLt is less than.
	OpLt Op = "lt"
	// OpLte is less than or equal.
	OpLte Op = "lte"
	// OpBetween is from the first value (inclusive) to the second (exclusive).
	OpBetween Op = "between"
	// OpContains is text containing the value, ignoring case.
	OpContains Op = "contains"
	// OpStartsWith is text starting with the value, ignoring case.
	OpStartsWith Op = "starts_with"
	// OpEndsWith is text ending with the value, ignoring case.
	OpEndsWith Op = "ends_with"
	// OpLike is a SQL LIKE pattern.
	OpLike Op = "like"
	// OpILike is a SQL ILIKE pattern.
	OpILike Op = "ilike"
	// OpIn is any of the values.
	OpIn Op = "in"
	// OpNotIn is none of the values.
	OpNotIn Op = "not_in"
	// OpIsNull is empty.
	OpIsNull Op = "is_null"
	// OpIsNotNull is not empty.
	OpIsNotNull Op = "is_not_null"
	// OpLastNDays is within the value's number of days before now.
	OpLastNDays Op = "last_n_days"
	// OpNextNDays is within the value's number of days after now.
	OpNextNDays Op = "next_n_days"
	// OpToday is today in the report time zone.
	OpToday Op = "today"
	// OpYesterday is yesterday.
	OpYesterday Op = "yesterday"
	// OpTomorrow is tomorrow.
	OpTomorrow Op = "tomorrow"
	// OpThisWeek is this ISO week.
	OpThisWeek Op = "this_week"
	// OpLastWeek is last ISO week.
	OpLastWeek Op = "last_week"
	// OpThisMonth is this month.
	OpThisMonth Op = "this_month"
	// OpLastMonth is last month.
	OpLastMonth Op = "last_month"
	// OpThisQuarter is this quarter.
	OpThisQuarter Op = "this_quarter"
	// OpLastQuarter is last quarter.
	OpLastQuarter Op = "last_quarter"
	// OpThisYear is this year.
	OpThisYear Op = "this_year"
	// OpLastYear is last year.
	OpLastYear Op = "last_year"
)

// Operand is the shape of the value an operator takes.
type Operand string

const (
	// OperandNone takes no value.
	OperandNone Operand = "none"
	// OperandOne takes one value of the field's type.
	OperandOne Operand = "one"
	// OperandList takes a non-empty list of values of the field's type.
	OperandList Operand = "list"
	// OperandRange takes [from, to] of the field's type.
	OperandRange Operand = "range"
	// OperandText takes one text pattern.
	OperandText Operand = "text"
	// OperandDays takes a whole number of days from 1 to MaxDays.
	OperandDays Operand = "days"
)

// MaxDays is the largest day count last_n_days and next_n_days accept.
const MaxDays = 3650

// OpInfo describes an operator: its label and the value it takes.
type OpInfo struct {
	// Op is the operator.
	Op Op `json:"op"`
	// Label names it for a builder.
	Label string `json:"label"`
	// Operand is the shape of the value it takes.
	Operand Operand `json:"operand"`
}

var opInfos = []OpInfo{
	{OpEq, "Equals", OperandOne},
	{OpNe, "Not equals", OperandOne},
	{OpGt, "Greater than", OperandOne},
	{OpGte, "Greater than or equal", OperandOne},
	{OpLt, "Less than", OperandOne},
	{OpLte, "Less than or equal", OperandOne},
	{OpBetween, "Between", OperandRange},
	{OpContains, "Contains", OperandText},
	{OpStartsWith, "Starts with", OperandText},
	{OpEndsWith, "Ends with", OperandText},
	{OpLike, "Matches pattern", OperandText},
	{OpILike, "Matches pattern, any case", OperandText},
	{OpIn, "Is any of", OperandList},
	{OpNotIn, "Is none of", OperandList},
	{OpIsNull, "Is empty", OperandNone},
	{OpIsNotNull, "Is not empty", OperandNone},
	{OpLastNDays, "In the last N days", OperandDays},
	{OpNextNDays, "In the next N days", OperandDays},
	{OpToday, "Today", OperandNone},
	{OpYesterday, "Yesterday", OperandNone},
	{OpTomorrow, "Tomorrow", OperandNone},
	{OpThisWeek, "This week", OperandNone},
	{OpLastWeek, "Last week", OperandNone},
	{OpThisMonth, "This month", OperandNone},
	{OpLastMonth, "Last month", OperandNone},
	{OpThisQuarter, "This quarter", OperandNone},
	{OpLastQuarter, "Last quarter", OperandNone},
	{OpThisYear, "This year", OperandNone},
	{OpLastYear, "Last year", OperandNone},
}

// Ops returns every operator with its description.
func Ops() []OpInfo { return slices.Clone(opInfos) }

// Relative reports whether the operator selects a period relative to the report's now: today,
// last_week, last_n_days and the like.
func (o Op) Relative() bool { return slices.Contains(opsRelative, o) }

// Info returns the operator's description.
func (o Op) Info() (OpInfo, bool) {
	for _, i := range opInfos {
		if i.Op == o {
			return i, true
		}
	}
	return OpInfo{}, false
}

var (
	opsEquality  = []Op{OpEq, OpNe}
	opsOrdering  = []Op{OpGt, OpGte, OpLt, OpLte, OpBetween}
	opsText      = []Op{OpContains, OpStartsWith, OpEndsWith, OpLike, OpILike}
	opsSet       = []Op{OpIn, OpNotIn}
	opsNull      = []Op{OpIsNull, OpIsNotNull}
	opsRelative  = []Op{OpLastNDays, OpNextNDays, OpToday, OpYesterday, OpTomorrow, OpThisWeek, OpLastWeek, OpThisMonth, OpLastMonth, OpThisQuarter, OpLastQuarter, OpThisYear, OpLastYear}
	opsAggregate = []Op{OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpBetween, OpIn, OpNotIn, OpIsNull, OpIsNotNull}
)

// OpsFor returns the operators a condition on a value of type t can use, in the order a builder
// lists them.
func OpsFor(t formula.Type) []Op {
	join := func(groups ...[]Op) []Op { return slices.Concat(groups...) }
	switch t {
	case formula.String:
		return join(opsEquality, opsText, opsOrdering, opsSet, opsNull)
	case formula.Int:
		return join(opsEquality, opsOrdering, opsSet, opsNull)
	case formula.Decimal, formula.Float:
		return join(opsEquality, opsOrdering, opsNull)
	case formula.Date, formula.Timestamp, formula.Epoch:
		return join(opsEquality, opsOrdering, opsRelative, opsNull)
	case formula.Enum, formula.Ref:
		return join(opsEquality, opsSet, opsNull)
	case formula.Bool:
		return join(opsEquality, opsNull)
	case formula.JSON:
		return slices.Clone(opsNull)
	default:
		return allOps()
	}
}

func allOps() []Op {
	out := make([]Op, len(opInfos))
	for i, info := range opInfos {
		out[i] = info.Op
	}
	return out
}

// OpTable returns OpsFor every value type, as published to the client.
func OpTable() map[formula.Type][]Op {
	table := map[formula.Type][]Op{}
	for _, t := range formula.Types() {
		table[t] = OpsFor(t)
	}
	return table
}

func opAllowed(op Op, t formula.Type) bool { return slices.Contains(OpsFor(t), op) }
