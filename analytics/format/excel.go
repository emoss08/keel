package format

import (
	"strings"

	"github.com/emoss08/keel/analytics/value"
)

// Excel returns the number format code a spreadsheet cell of the column carries and whether the
// column's values are written as native numbers or dates. A display Excel cannot express
// natively (a band, compact notation, a duration, a quarter, booleans, text) is written as its
// rendered text, so the sheet reads like every other export; a raw numeric display is a native
// number in the General format (code ""). The currency symbol and its placement and the percent
// sign follow the formatter's locale; separators are left to the spreadsheet's own locale.
func (f *Formatter) Excel() (code string, native bool) {
	numeric := f.t.Numeric() || f.t == value.Epoch
	switch {
	case f.banded:
		return "", false
	case f.d.Style == StyleAuto:
		return "", numeric
	case f.d.Style == StyleDate:
		if f.d.DateStyle == DateStyleQuarter || !(numeric || f.t == value.Time) {
			return "", false
		}
		return excelDate(f.d.DateStyle), true
	case f.d.Style == StyleNumber, f.d.Style == StyleCurrency, f.d.Style == StylePercent:
		if f.d.Notation == NotationCompact || !numeric {
			return "", false
		}
		return f.excelNumber(), true
	}
	return "", false
}

// excelNumber returns the number format code of a number, currency or percent column.
func (f *Formatter) excelNumber() string {
	var b strings.Builder
	b.WriteString(excelLiteral(f.d.Prefix))
	if f.d.Style == StyleCurrency {
		b.WriteString(excelLiteral(f.currency.prefix))
	}
	if f.d.Style == StylePercent {
		b.WriteString(excelPercent(f.l.percent.prefix))
	}
	if f.d.Grouping && f.l.primary > 0 {
		b.WriteString("#,##0")
	} else {
		b.WriteString("0")
	}
	if f.d.Decimals > 0 {
		b.WriteByte('.')
		b.WriteString(strings.Repeat("0", f.d.Decimals))
	}
	if f.d.Style == StylePercent {
		b.WriteString(excelPercent(f.l.percent.suffix))
	}
	if f.d.Style == StyleCurrency {
		b.WriteString(excelLiteral(f.currency.suffix))
	}
	b.WriteString(excelLiteral(f.d.Suffix))
	positive := b.String()
	if f.d.Negative == NegativeParentheses {
		return positive + ";(" + positive + ")"
	}
	return positive
}

// excelDate returns the number format code of a date style.
func excelDate(style DateStyle) string {
	switch style {
	case DateStyleDate:
		return "yyyy-mm-dd"
	case DateStyleDateLong:
		return `mmm d", "yyyy`
	case DateStyleTime:
		return "hh:mm"
	case DateStyleMonthYear:
		return "mmm yyyy"
	case DateStyleYear:
		return "yyyy"
	case DateStyleWeekday:
		return "ddd yyyy-mm-dd"
	case DateStyleISO:
		return `yyyy-mm-dd"T"hh:mm:ss`
	}
	return "yyyy-mm-dd hh:mm"
}

// excelLiteral quotes text for a number format code; Excel has no escape for a quote inside a
// literal, so quotes are dropped.
func excelLiteral(text string) string {
	text = strings.ReplaceAll(text, `"`, "")
	if text == "" {
		return ""
	}
	return `"` + text + `"`
}

// excelPercent quotes a percent affix, leaving its percent sign bare so Excel scales the value.
func excelPercent(affix string) string {
	parts := strings.Split(affix, "%")
	for i, part := range parts {
		parts[i] = excelLiteral(part)
	}
	return strings.Join(parts, "%")
}
