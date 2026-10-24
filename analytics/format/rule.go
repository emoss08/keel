package format

import "math"

// MaxRules is the most tone rules a column may carry. Past a handful of bands, conditional
// emphasis stops making exceptions obvious.
const MaxRules = 8

// Tone is the meaning a matched rule gives a cell, not a colour: each surface (a light or dark
// grid, a spreadsheet) paints it in its own palette.
type Tone string

// The tones.
const (
	// ToneNone is an unmatched cell.
	ToneNone Tone = ""
	// TonePositive marks a good value.
	TonePositive Tone = "positive"
	// ToneWarning marks a value to look at.
	ToneWarning Tone = "warning"
	// ToneNegative marks a bad value.
	ToneNegative Tone = "negative"
	// ToneNeutral sets a value apart without judging it.
	ToneNeutral Tone = "neutral"
)

// RuleOp is the comparison a tone rule makes.
type RuleOp string

// The rule comparisons.
const (
	// RuleGreater matches values above Value.
	RuleGreater RuleOp = "gt"
	// RuleGreaterEqual matches values at or above Value.
	RuleGreaterEqual RuleOp = "gte"
	// RuleLess matches values below Value.
	RuleLess RuleOp = "lt"
	// RuleLessEqual matches values at or below Value.
	RuleLessEqual RuleOp = "lte"
	// RuleEqual matches values equal to Value.
	RuleEqual RuleOp = "eq"
	// RuleBetween matches values from Value to Upper, both included.
	RuleBetween RuleOp = "between"
)

// Rule gives a numeric cell a tone when its value matches. Values and thresholds are compared
// as exact decimals, the threshold read from its shortest decimal form, so a rule on eq 0.3
// matches 0.3. A percent column is compared on the scale it shows: a threshold of 12 means 12%.
type Rule struct {
	// Op is the comparison.
	Op RuleOp `json:"op"`
	// Value is the threshold, or the lower bound of between.
	Value float64 `json:"value"`
	// Upper is the upper bound of between.
	Upper float64 `json:"upper,omitempty"`
	// Tone is what a match earns.
	Tone Tone `json:"tone"`
}

// valid reports whether the comparison is known.
func (o RuleOp) valid() bool {
	switch o {
	case RuleGreater, RuleGreaterEqual, RuleLess, RuleLessEqual, RuleEqual, RuleBetween:
		return true
	}
	return false
}

// valid reports whether the tone is one a rule may give.
func (t Tone) valid() bool {
	switch t {
	case TonePositive, ToneWarning, ToneNegative, ToneNeutral:
		return true
	}
	return false
}

// finite reports whether f is neither NaN nor infinite.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// validate reports the first problem with the rule, as the field and code of a validation
// error.
func (r *Rule) validate() (string, string) {
	switch {
	case !r.Op.valid():
		return "op", CodeUnknownOp
	case !r.Tone.valid():
		return "tone", CodeUnknownTone
	case !finite(r.Value):
		return "value", CodeNotFinite
	case r.Op != RuleBetween:
		return "", ""
	case !finite(r.Upper):
		return "upper", CodeNotFinite
	case r.Upper < r.Value:
		return "upper", CodeUpperBelowLower
	}
	return "", ""
}
