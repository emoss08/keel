// Package format is the display layer of keel/analytics: how a report column's values are
// shown.
//
// A column's display is resolved once, from three sources in order:
//
//   - the value type (value.Decimal shows two places, value.Epoch shows a date);
//   - a Hint from the catalog and the report definition: the format Kind a model declares
//     (Money, Weight, Percent, Duration, Distance, Count), the date style a date bucket
//     implies, a band, enum labels;
//   - the author's Spec, every field optional.
//
// Resolve returns the complete Resolved display, which travels with the column to every writer
// and to the client. Spec.Validate reports options that cannot apply to a column's type with a
// stable code, and Rules publishes the whole vocabulary as data.
//
// A Formatter renders values through a Resolved display in a locale and a time zone:
//
//	f := format.New(value.Decimal, col.Display, format.Options{Locale: language.German, Location: berlin})
//	text, tone := f.Format("1234.5") // "1.234,50 €" when the column is euros
//
// Numbers are exact: decimals are rounded half away from zero on their digits, so 1.005 with
// two places is "1.01", and a float is read from its shortest decimal form. Separators, group
// sizes, digits, the minus and percent signs and currency symbols come from CLDR through
// golang.org/x/text; where a currency symbol goes comes from CLDR's currency patterns, which
// x/text lacks, generated into currency_cldr.go for the same CLDR release. Date layouts,
// compact suffixes (K, M, B, T) and duration words are the same in every locale. Column
// renders a whole vector into Cells without allocating once warm, and Excel returns the
// number format a spreadsheet cell needs.
//
// The behaviour is that of Trenova's reportfmt and its Rust formatter, with these deliberate
// differences:
//
//   - a negative that rounds to zero, or a negative duration shorter than a minute in a style
//     without seconds, is shown without its sign ("$0.00", not "($0.00)");
//   - currency symbols and placement follow the locale's CLDR data, so Canadian dollars are
//     "CA$" in American English and "$" in Canadian English, and an alphabetic symbol is kept
//     off the digits with a no-break space ("CHF 5.00");
//   - a currency's default places are its own (JPY 0, BHD 3) unless the author sets them;
//   - a JSON value is shown as its text rather than as empty;
//   - a raw Time value is RFC 3339 in UTC, and a raw display of a number is a native number in
//     a spreadsheet;
//   - quarters and other displays Excel cannot express natively are written as their text in
//     a spreadsheet, rather than as a year;
//   - decimal hours are rounded once, exactly, rather than to 16 places first.
package format

//go:generate go run ./internal/cldrgen -out currency_cldr.go
