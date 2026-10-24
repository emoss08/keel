package format

import (
	"slices"

	"github.com/emoss08/keel/analytics/value"
)

var (
	allStyles = []Style{StyleNumber, StyleCurrency, StylePercent, StyleDuration, StyleDate,
		StyleBool, StyleText}
	allNegatives  = []Negative{NegativeMinus, NegativeParentheses}
	allNotations  = []Notation{NotationStandard, NotationCompact}
	allDateStyles = []DateStyle{DateStyleDate, DateStyleDateLong, DateStyleDateTime, DateStyleTime,
		DateStyleMonthYear, DateStyleQuarter, DateStyleYear, DateStyleWeekday, DateStyleISO}
	allBoolStyles = []BoolStyle{BoolStyleYesNo, BoolStyleTrueFalse, BoolStyleOnOff, BoolStyleCheck,
		BoolStyleOneZero}
	allDurationUnits = []DurationUnit{DurationUnitSeconds, DurationUnitMinutes, DurationUnitHours,
		DurationUnitDays}
	allDurationStyles = []DurationStyle{DurationStyleClock, DurationStyleCompact, DurationStyleVerbose,
		DurationStyleDecimalHours}
	allTones = []Tone{TonePositive, ToneWarning, ToneNegative, ToneNeutral}
	allOps   = []RuleOp{RuleGreater, RuleGreaterEqual, RuleLess, RuleLessEqual, RuleEqual, RuleBetween}
	allKinds = []Kind{Money, Weight, Percent, Duration, Distance, Count}

	numberStyles = []Style{StyleNumber, StyleCurrency, StylePercent, StyleDuration, StyleDate, StyleText}
)

// stylesFor returns the styles a column of type t may be shown in. Numbers may be shown as dates
// because an aggregate over an epoch column (an average arrival time) is a number that still
// denotes an instant.
func stylesFor(t value.Type) []Style {
	switch t {
	case value.Int, value.Decimal, value.Float:
		return numberStyles
	case value.Epoch:
		return []Style{StyleDate, StyleNumber, StyleText}
	case value.Time:
		return []Style{StyleDate, StyleText}
	case value.Bool:
		return []Style{StyleBool, StyleText}
	default:
		return []Style{StyleText}
	}
}

// Vocabulary is the display vocabulary as data: what Validate accepts and Resolve derives, for
// an analytics catalog to publish and a client to offer without copying the rules by hand.
type Vocabulary struct {
	// Types lists, for each value type, its derived style and the styles it may be shown in.
	Types []TypeStyles `json:"types"`
	// Kinds are the format kinds a column may declare.
	Kinds []Kind `json:"kinds"`
	// Negatives are the negative styles.
	Negatives []Negative `json:"negatives"`
	// Notations are the notations.
	Notations []Notation `json:"notations"`
	// DateStyles are the date styles.
	DateStyles []DateStyle `json:"dateStyles"`
	// BoolStyles are the boolean styles.
	BoolStyles []BoolStyle `json:"boolStyles"`
	// DurationUnits are the duration units.
	DurationUnits []DurationUnit `json:"durationUnits"`
	// DurationStyles are the duration styles.
	DurationStyles []DurationStyle `json:"durationStyles"`
	// Tones are the tones a rule may give.
	Tones []Tone `json:"tones"`
	// RuleOps are the rule comparisons.
	RuleOps []RuleOp `json:"ruleOps"`
	// MaxDecimals is the most places a number may show.
	MaxDecimals int `json:"maxDecimals"`
	// MaxAffix is the most characters of a prefix or suffix.
	MaxAffix int `json:"maxAffix"`
	// MaxNullText is the most characters of the empty-value text.
	MaxNullText int `json:"maxNullText"`
	// MaxRules is the most rules a column may carry.
	MaxRules int `json:"maxRules"`
	// MaxBandEdges is the most boundaries a band may list.
	MaxBandEdges int `json:"maxBandEdges"`
}

// TypeStyles is the display styles of one value type.
type TypeStyles struct {
	// Type is the value type.
	Type value.Type `json:"type"`
	// Default is the style Resolve derives for the type with no format kind.
	Default Style `json:"default"`
	// Styles are the styles a Spec may choose for the type.
	Styles []Style `json:"styles"`
}

// Rules returns the display vocabulary. The result is a fresh copy the caller may keep.
func Rules() Vocabulary {
	types := make([]TypeStyles, 0, len(value.Types()))
	for _, t := range value.Types() {
		types = append(types, TypeStyles{
			Type:    t,
			Default: Resolve(t, Hint{}, nil).Style,
			Styles:  slices.Clone(stylesFor(t)),
		})
	}
	return Vocabulary{
		Types:          types,
		Kinds:          slices.Clone(allKinds),
		Negatives:      slices.Clone(allNegatives),
		Notations:      slices.Clone(allNotations),
		DateStyles:     slices.Clone(allDateStyles),
		BoolStyles:     slices.Clone(allBoolStyles),
		DurationUnits:  slices.Clone(allDurationUnits),
		DurationStyles: slices.Clone(allDurationStyles),
		Tones:          slices.Clone(allTones),
		RuleOps:        slices.Clone(allOps),
		MaxDecimals:    MaxDecimals,
		MaxAffix:       MaxAffix,
		MaxNullText:    MaxNullText,
		MaxRules:       MaxRules,
		MaxBandEdges:   MaxBandEdges,
	}
}
