package format

import (
	"maps"
	"slices"
	"strings"

	"golang.org/x/text/currency"

	"github.com/emoss08/keel/analytics/value"
)

// Hint is what the catalog and the report definition imply about a column's display before an
// author overrides anything.
type Hint struct {
	// Kind is what the column's numbers measure (money, weight, …), from the model.
	Kind Kind
	// Bucket is the date style a date bucket implies ("monthYear" for a month bucket). It
	// applies when the column is shown as a date.
	Bucket DateStyle
	// Band is the ranges a banded dimension groups into.
	Band *Band
	// Labels maps an enum column's stored values to their labels, from the model's constants.
	Labels map[string]string
}

// Resolve returns a column's complete display: the defaults a value of type t and the hint
// imply, then the author's override. Restyling to another style first resets what that style
// implies (a currency has its currency's places and parentheses for negatives; a number drops
// a derived suffix), then the override's other fields apply. Decimals are clamped to
// 0..MaxDecimals and the currency code is upper-cased; Validate reports such values instead of
// clamping them.
func Resolve(t value.Type, hint Hint, override *Spec) Resolved {
	d := baseline(t, hint.Kind)
	if hint.Bucket != DateStyleAuto && d.Style == StyleDate {
		d.DateStyle = hint.Bucket
	}
	if !hint.Band.IsEmpty() {
		d.Band = &Band{Width: hint.Band.Width, Edges: slices.Clone(hint.Band.Edges)}
	}
	if len(hint.Labels) > 0 {
		d.Labels = maps.Clone(hint.Labels)
	}
	if override == nil {
		return d
	}
	if override.Style != StyleAuto && override.Style != d.Style {
		d = restyle(d, override.Style)
	}
	if override.Currency != "" {
		d.Currency = strings.ToUpper(override.Currency)
		if d.Style == StyleCurrency {
			d.Decimals = currencyDigits(d.Currency)
		}
	}
	if override.Decimals != nil {
		d.Decimals = min(max(*override.Decimals, 0), MaxDecimals)
	}
	if override.Grouping != nil {
		d.Grouping = *override.Grouping
	}
	setIf(&d.Negative, override.Negative)
	setIf(&d.Notation, override.Notation)
	setIf(&d.Prefix, override.Prefix)
	setIf(&d.Suffix, override.Suffix)
	setIf(&d.DateStyle, override.DateStyle)
	setIf(&d.BoolStyle, override.BoolStyle)
	setIf(&d.DurationUnit, override.DurationUnit)
	setIf(&d.DurationStyle, override.DurationStyle)
	setIf(&d.NullText, override.NullText)
	if len(override.Rules) > 0 {
		d.Rules = slices.Clone(override.Rules)
	}
	return d
}

// setIf replaces a resolved field with an override that is set.
func setIf[T ~string](field *T, v T) {
	if v != "" {
		*field = v
	}
}

// baseline returns the display a value type and format kind imply.
func baseline(t value.Type, kind Kind) Resolved {
	d := Resolved{
		Style:         StyleText,
		Grouping:      true,
		Currency:      "USD",
		Negative:      NegativeMinus,
		Notation:      NotationStandard,
		DateStyle:     DateStyleDateTime,
		BoolStyle:     BoolStyleYesNo,
		DurationUnit:  DurationUnitSeconds,
		DurationStyle: DurationStyleClock,
	}
	switch t {
	case value.Epoch, value.Time:
		d.Style = StyleDate
	case value.Bool:
		d.Style = StyleBool
	case value.Int, value.Decimal, value.Float:
		d.Style = StyleNumber
		if t != value.Int {
			d.Decimals = 2
		}
		applyKind(&d, kind)
	}
	return d
}

// applyKind applies a format kind to a numeric display.
func applyKind(d *Resolved, kind Kind) {
	switch kind {
	case Money:
		d.Style = StyleCurrency
		d.Decimals = currencyDigits(d.Currency)
		d.Negative = NegativeParentheses
	case Percent:
		d.Style = StylePercent
		d.Decimals = 1
	case Duration:
		d.Style = StyleDuration
		d.Decimals = 0
	case Distance:
		d.Decimals = 1
		d.Suffix = " mi"
	case Weight:
		d.Decimals = 0
		d.Suffix = " lbs"
	case Count:
		d.Decimals = 0
	}
}

// restyle switches a display to another style, resetting what the style implies.
func restyle(d Resolved, style Style) Resolved {
	d.Style = style
	switch style {
	case StyleCurrency:
		d.Decimals = currencyDigits(d.Currency)
		d.Negative = NegativeParentheses
	case StylePercent:
		d.Decimals = 1
	case StyleDuration:
		d.Decimals = 0
	case StyleNumber:
		d.Suffix = ""
	}
	return d
}

// currencyDigits returns the number of places a currency's amounts are written with (2 for
// USD, 0 for JPY, 3 for BHD), from CLDR's currency data, or 2 for an unknown code.
func currencyDigits(code string) int {
	unit, err := currency.ParseISO(code)
	if err != nil {
		return 2
	}
	scale, _ := currency.Standard.Rounding(unit)
	return scale
}
