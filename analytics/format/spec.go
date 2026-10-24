package format

// Style is how a column's values are shown. The zero Style is "auto" in a Spec (keep the
// derived style) and "raw" in a Resolved display (write values as they are).
type Style string

// The display styles.
const (
	// StyleAuto keeps the style derived from the column's type and format kind.
	StyleAuto Style = ""
	// StyleNumber shows a number with places, grouping and an optional compact notation.
	StyleNumber Style = "number"
	// StyleCurrency shows a number with the currency's symbol, placed as the locale places it.
	StyleCurrency Style = "currency"
	// StylePercent shows a fraction times 100 with the locale's percent sign.
	StylePercent Style = "percent"
	// StyleDuration shows a number of seconds, minutes, hours or days as a duration.
	StyleDuration Style = "duration"
	// StyleDate shows an instant, or a number of seconds since the Unix epoch, as a date.
	StyleDate Style = "date"
	// StyleBool shows a boolean in a boolean style.
	StyleBool Style = "bool"
	// StyleText shows the value as text with the prefix and suffix.
	StyleText Style = "text"
)

// Negative is how a negative number is marked.
type Negative string

// The negative styles.
const (
	// NegativeAuto keeps the derived negative style.
	NegativeAuto Negative = ""
	// NegativeMinus writes the locale's minus sign before the value.
	NegativeMinus Negative = "minus"
	// NegativeParentheses wraps the value in parentheses, as accountants do.
	NegativeParentheses Negative = "parentheses"
)

// Notation is how the magnitude of a number is written.
type Notation string

// The notations.
const (
	// NotationAuto keeps the derived notation.
	NotationAuto Notation = ""
	// NotationStandard writes every digit.
	NotationStandard Notation = "standard"
	// NotationCompact divides by a thousand, million, billion or trillion and writes K, M, B or T.
	NotationCompact Notation = "compact"
)

// DateStyle is how a date is written.
type DateStyle string

// The date styles. Each names the layout used in every locale.
const (
	// DateStyleAuto keeps the derived date style.
	DateStyleAuto DateStyle = ""
	// DateStyleDate is "2026-03-04".
	DateStyleDate DateStyle = "date"
	// DateStyleDateLong is "Mar 4, 2026".
	DateStyleDateLong DateStyle = "dateLong"
	// DateStyleDateTime is "2026-03-04 19:30".
	DateStyleDateTime DateStyle = "dateTime"
	// DateStyleTime is "19:30".
	DateStyleTime DateStyle = "time"
	// DateStyleMonthYear is "Mar 2026".
	DateStyleMonthYear DateStyle = "monthYear"
	// DateStyleQuarter is "Q1 2026".
	DateStyleQuarter DateStyle = "quarter"
	// DateStyleYear is "2026".
	DateStyleYear DateStyle = "year"
	// DateStyleWeekday is "Wed 2026-03-04".
	DateStyleWeekday DateStyle = "weekday"
	// DateStyleISO is RFC 3339 with the zone offset: "2026-03-04T19:30:00-06:00".
	DateStyleISO DateStyle = "iso"
)

// BoolStyle is how a boolean is written.
type BoolStyle string

// The boolean styles.
const (
	// BoolStyleAuto keeps the derived boolean style.
	BoolStyleAuto BoolStyle = ""
	// BoolStyleYesNo is "Yes" and "No".
	BoolStyleYesNo BoolStyle = "yesNo"
	// BoolStyleTrueFalse is "True" and "False".
	BoolStyleTrueFalse BoolStyle = "trueFalse"
	// BoolStyleOnOff is "On" and "Off".
	BoolStyleOnOff BoolStyle = "onOff"
	// BoolStyleCheck is "✓" and "✗".
	BoolStyleCheck BoolStyle = "check"
	// BoolStyleOneZero is "1" and "0".
	BoolStyleOneZero BoolStyle = "oneZero"
)

// DurationUnit is what one unit of a duration value is.
type DurationUnit string

// The duration units.
const (
	// DurationUnitAuto keeps the derived unit.
	DurationUnitAuto DurationUnit = ""
	// DurationUnitSeconds reads a value as seconds.
	DurationUnitSeconds DurationUnit = "seconds"
	// DurationUnitMinutes reads a value as minutes.
	DurationUnitMinutes DurationUnit = "minutes"
	// DurationUnitHours reads a value as hours.
	DurationUnitHours DurationUnit = "hours"
	// DurationUnitDays reads a value as days.
	DurationUnitDays DurationUnit = "days"
)

// DurationStyle is how a duration is written.
type DurationStyle string

// The duration styles.
const (
	// DurationStyleAuto keeps the derived duration style.
	DurationStyleAuto DurationStyle = ""
	// DurationStyleClock is hours and minutes: "26:10".
	DurationStyleClock DurationStyle = "clock"
	// DurationStyleCompact is "1d 2h 10m".
	DurationStyleCompact DurationStyle = "compact"
	// DurationStyleVerbose is "1 day 2 hours 10 minutes".
	DurationStyleVerbose DurationStyle = "verbose"
	// DurationStyleDecimalHours is hours with places: "26.2 h".
	DurationStyleDecimalHours DurationStyle = "decimalHours"
)

// Kind is what a column's numbers measure, which decides their default display. A model
// declares it once per Go type with a KeelFormat method, or per field with the format= tag
// option.
type Kind string

// The format kinds.
const (
	// KindNone has no default beyond the value type's.
	KindNone Kind = ""
	// Money is a currency amount: currency style, two places, parentheses for negatives.
	Money Kind = "money"
	// Weight is pounds: no places, " lbs".
	Weight Kind = "weight"
	// Percent is a fraction shown as a percentage with one place.
	Percent Kind = "percent"
	// Duration is a number of seconds shown as a clock duration.
	Duration Kind = "duration"
	// Distance is miles: one place, " mi".
	Distance Kind = "distance"
	// Count is a whole count: no places.
	Count Kind = "count"
)

// The limits a display spec is validated against.
const (
	// MaxDecimals is the most places a number may show.
	MaxDecimals = 10
	// MaxAffix is the most characters a prefix or suffix may hold.
	MaxAffix = 12
	// MaxNullText is the most characters the empty-value text may hold.
	MaxNullText = 24
)

// Spec is an author's display override for one column. Every field is optional: an unset
// field keeps the display derived from the column's type and format kind.
type Spec struct {
	// Style restyles the column. Restyling resets what the new style implies (a currency has
	// two places and parentheses for negatives) before the other fields apply.
	Style Style `json:"style,omitempty"`
	// Decimals is the number of places, 0 to MaxDecimals.
	Decimals *int `json:"decimals,omitempty"`
	// Grouping groups the whole part by the locale's grouping rule.
	Grouping *bool `json:"grouping,omitempty"`
	// Currency is the ISO 4217 code of a currency column, in any case.
	Currency string `json:"currency,omitempty"`
	// Negative is how negative numbers are marked.
	Negative Negative `json:"negative,omitempty"`
	// Notation is standard or compact.
	Notation Notation `json:"notation,omitempty"`
	// Prefix is written before the value.
	Prefix string `json:"prefix,omitempty"`
	// Suffix is written after the value.
	Suffix string `json:"suffix,omitempty"`
	// DateStyle is how dates are written.
	DateStyle DateStyle `json:"dateStyle,omitempty"`
	// BoolStyle is how booleans are written.
	BoolStyle BoolStyle `json:"boolStyle,omitempty"`
	// DurationUnit is what one unit of a duration value is.
	DurationUnit DurationUnit `json:"durationUnit,omitempty"`
	// DurationStyle is how durations are written.
	DurationStyle DurationStyle `json:"durationStyle,omitempty"`
	// NullText is what an empty value shows.
	NullText string `json:"nullText,omitempty"`
	// Rules give numeric cells a tone; they are read in order and the first match wins.
	Rules []Rule `json:"rules,omitempty"`
}

// IsEmpty reports whether the spec overrides nothing, so a caller need not store it.
func (s *Spec) IsEmpty() bool {
	if s == nil {
		return true
	}
	return s.Style == StyleAuto && s.Decimals == nil && s.Grouping == nil && s.Currency == "" &&
		s.Negative == NegativeAuto && s.Notation == NotationAuto && s.Prefix == "" &&
		s.Suffix == "" && s.DateStyle == DateStyleAuto && s.BoolStyle == BoolStyleAuto &&
		s.DurationUnit == DurationUnitAuto && s.DurationStyle == DurationStyleAuto &&
		s.NullText == "" && len(s.Rules) == 0
}

// Resolved is a column's complete display: every field set, the derived defaults and the
// author's overrides applied. It travels with the column to every writer and to the client,
// and is what a Formatter renders. A zero Resolved (style "") writes values raw.
type Resolved struct {
	// Style is the display style, or "" to write values raw.
	Style Style `json:"style"`
	// Decimals is the number of places.
	Decimals int `json:"decimals"`
	// Grouping groups the whole part.
	Grouping bool `json:"grouping"`
	// Currency is the ISO 4217 code of a currency column.
	Currency string `json:"currency"`
	// Negative is how negative numbers are marked.
	Negative Negative `json:"negative"`
	// Notation is standard or compact.
	Notation Notation `json:"notation"`
	// Prefix is written before the value.
	Prefix string `json:"prefix"`
	// Suffix is written after the value.
	Suffix string `json:"suffix"`
	// DateStyle is how dates are written.
	DateStyle DateStyle `json:"dateStyle"`
	// BoolStyle is how booleans are written.
	BoolStyle BoolStyle `json:"boolStyle"`
	// DurationUnit is what one unit of a duration value is.
	DurationUnit DurationUnit `json:"durationUnit"`
	// DurationStyle is how durations are written.
	DurationStyle DurationStyle `json:"durationStyle"`
	// NullText is what an empty value shows.
	NullText string `json:"nullText"`
	// Rules give numeric cells a tone, first match wins.
	Rules []Rule `json:"rules,omitempty"`
	// Band is set when the column groups a numeric dimension into ranges; each value is the
	// lower bound of its range and is shown as the range.
	Band *Band `json:"band,omitempty"`
	// Labels maps an enum column's stored values to the labels shown for them.
	Labels map[string]string `json:"labels,omitempty"`
}
