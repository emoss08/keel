package format

import (
	"fmt"
	"strconv"
	"unicode/utf8"

	"golang.org/x/text/currency"

	"github.com/emoss08/keel/analytics/value"
)

// The codes of validation errors. An application translates the code it shows; Error's text
// is the English fallback.
const (
	CodeUnknownStyle         = "unknown_style"
	CodeStyleNotAllowed      = "style_not_allowed"
	CodeUnknownNegative      = "unknown_negative"
	CodeUnknownNotation      = "unknown_notation"
	CodeUnknownDateStyle     = "unknown_date_style"
	CodeUnknownBoolStyle     = "unknown_bool_style"
	CodeUnknownDurationUnit  = "unknown_duration_unit"
	CodeUnknownDurationStyle = "unknown_duration_style"
	CodeDecimalsRange        = "decimals_range"
	CodeUnknownCurrency      = "unknown_currency"
	CodeAffixTooLong         = "affix_too_long"
	CodeNullTextTooLong      = "null_text_too_long"
	CodeTooManyRules         = "too_many_rules"
	CodeUnknownOp            = "unknown_op"
	CodeUnknownTone          = "unknown_tone"
	CodeNotFinite            = "not_finite"
	CodeUpperBelowLower      = "upper_below_lower"
	CodeBandWidthAndEdges    = "band_width_and_edges"
	CodeBandWidth            = "band_width"
	CodeBandTooFewEdges      = "band_too_few_edges"
	CodeBandTooManyEdges     = "band_too_many_edges"
	CodeBandNotAscending     = "band_not_ascending"
)

// messages are the English texts of the validation codes.
var messages = map[string]string{
	CodeUnknownStyle:         "unknown display style",
	CodeStyleNotAllowed:      "this display style does not apply to the column's type",
	CodeUnknownNegative:      "unknown negative style",
	CodeUnknownNotation:      "unknown notation",
	CodeUnknownDateStyle:     "unknown date style",
	CodeUnknownBoolStyle:     "unknown boolean style",
	CodeUnknownDurationUnit:  "unknown duration unit",
	CodeUnknownDurationStyle: "unknown duration style",
	CodeDecimalsRange:        "decimals must be between 0 and " + strconv.Itoa(MaxDecimals),
	CodeUnknownCurrency:      "not an ISO 4217 currency code",
	CodeAffixTooLong:         "prefix and suffix are limited to " + strconv.Itoa(MaxAffix) + " characters",
	CodeNullTextTooLong:      "the empty-value text is limited to " + strconv.Itoa(MaxNullText) + " characters",
	CodeTooManyRules:         "a column can carry at most " + strconv.Itoa(MaxRules) + " formatting rules",
	CodeUnknownOp:            "unknown comparison",
	CodeUnknownTone:          "unknown tone",
	CodeNotFinite:            "must be a finite number",
	CodeUpperBelowLower:      "the upper bound must not be below the lower bound",
	CodeBandWidthAndEdges:    "a band takes either a width or a list of boundaries, not both",
	CodeBandWidth:            "the band width must be a positive, finite number",
	CodeBandTooFewEdges:      "a band needs at least two boundaries",
	CodeBandTooManyEdges:     "a band can carry at most " + strconv.Itoa(MaxBandEdges) + " boundaries",
	CodeBandNotAscending:     "band boundaries must ascend, with no repeats",
}

// Error is a display spec that fails validation: the JSON path of the offending field
// ("rules[2].upper") and a stable code.
type Error struct {
	// Field is the JSON path of the field, relative to the spec.
	Field string
	// Code is one of the Code constants.
	Code string
}

// Error returns the English message.
func (e *Error) Error() string {
	return fmt.Sprintf("format: %s: %s", e.Field, messages[e.Code])
}

// Validate reports the first option of the spec that cannot apply to a column of type t, or
// nil. A nil spec is valid.
func (s *Spec) Validate(t value.Type) error {
	if s == nil {
		return nil
	}
	if field, code := s.problem(t); code != "" {
		return &Error{Field: field, Code: code}
	}
	return nil
}

// problem returns the field and code of the spec's first problem for type t, or empty strings.
func (s *Spec) problem(t value.Type) (string, string) {
	checks := []struct {
		field string
		ok    bool
		code  string
	}{
		{"style", s.Style == StyleAuto || contains(allStyles, s.Style), CodeUnknownStyle},
		{"style", s.Style == StyleAuto || contains(stylesFor(t), s.Style), CodeStyleNotAllowed},
		{"negative", s.Negative == NegativeAuto || contains(allNegatives, s.Negative), CodeUnknownNegative},
		{"notation", s.Notation == NotationAuto || contains(allNotations, s.Notation), CodeUnknownNotation},
		{"dateStyle", s.DateStyle == DateStyleAuto || contains(allDateStyles, s.DateStyle), CodeUnknownDateStyle},
		{"boolStyle", s.BoolStyle == BoolStyleAuto || contains(allBoolStyles, s.BoolStyle), CodeUnknownBoolStyle},
		{"durationUnit", s.DurationUnit == DurationUnitAuto || contains(allDurationUnits, s.DurationUnit),
			CodeUnknownDurationUnit},
		{"durationStyle", s.DurationStyle == DurationStyleAuto || contains(allDurationStyles, s.DurationStyle),
			CodeUnknownDurationStyle},
		{"decimals", s.Decimals == nil || (*s.Decimals >= 0 && *s.Decimals <= MaxDecimals), CodeDecimalsRange},
		{"currency", s.Currency == "" || knownCurrency(s.Currency), CodeUnknownCurrency},
		{"prefix", utf8.RuneCountInString(s.Prefix) <= MaxAffix, CodeAffixTooLong},
		{"suffix", utf8.RuneCountInString(s.Suffix) <= MaxAffix, CodeAffixTooLong},
		{"nullText", utf8.RuneCountInString(s.NullText) <= MaxNullText, CodeNullTextTooLong},
		{"rules", len(s.Rules) <= MaxRules, CodeTooManyRules},
	}
	for _, c := range checks {
		if !c.ok {
			return c.field, c.code
		}
	}
	for i := range s.Rules {
		if field, code := s.Rules[i].validate(); code != "" {
			return "rules[" + strconv.Itoa(i) + "]." + field, code
		}
	}
	return "", ""
}

// knownCurrency reports whether code is an ISO 4217 currency CLDR knows.
func knownCurrency(code string) bool {
	if len(code) != 3 {
		return false
	}
	_, err := currency.ParseISO(code)
	return err == nil
}

// contains reports whether v is in list.
func contains[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
