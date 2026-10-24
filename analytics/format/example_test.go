package format_test

import (
	"fmt"
	"time"

	"golang.org/x/text/language"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/value"
)

// A column's display starts from its type and the kind its model declares, then takes the
// author's overrides.
func ExampleResolve() {
	d := format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, &format.Spec{Currency: "jpy"})
	fmt.Println(d.Style, d.Decimals, d.Currency, d.Negative)
	// Output: currency 0 JPY parentheses
}

// The same display renders in each reader's language.
func ExampleNew() {
	d := format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, &format.Spec{Currency: "EUR"})
	for _, tag := range []language.Tag{language.AmericanEnglish, language.German, language.Dutch} {
		text, _ := format.New(value.Decimal, d, format.Options{Locale: tag}).Format("-1234.5")
		fmt.Printf("%s %q\n", tag, text)
	}
	// Output:
	// en-US "(€1,234.50)"
	// de "(1.234,50\u00a0€)"
	// nl "(€\u00a01.234,50)"
}

// Rules give numeric cells a tone, first match wins; a percent column compares on the scale it
// shows.
func ExampleFormatter_Format() {
	d := format.Resolve(value.Float, format.Hint{Kind: format.Percent}, &format.Spec{Rules: []format.Rule{
		{Op: format.RuleGreater, Value: 12, Tone: format.ToneNegative},
		{Op: format.RuleBetween, Value: 8, Upper: 12, Tone: format.ToneWarning},
	}})
	f := format.New(value.Float, d, format.Options{})
	for _, v := range []float64{0.15, 0.1, 0.02} {
		text, tone := f.Format(v)
		fmt.Printf("%s %q\n", text, tone)
	}
	// Output:
	// 15.0% "negative"
	// 10.0% "warning"
	// 2.0% ""
}

// Column renders a whole vector into reusable cells; dates are written in the time zone given.
func ExampleFormatter_Column() {
	chicago, _ := time.LoadLocation("America/Chicago")
	d := format.Resolve(value.Epoch, format.Hint{Bucket: format.DateStyleMonthYear}, nil)
	f := format.New(value.Epoch, d, format.Options{Location: chicago})
	v := &value.Vector{Int64: []int64{1772352000, 1775026800, 0}, Nulls: value.NewBitmap(3)}
	v.Nulls.Set(2)
	var cells format.Cells
	f.Column(&cells, v, 3)
	for i := range cells.Len() {
		fmt.Printf("%q\n", cells.String(i))
	}
	// Output:
	// "Mar 2026"
	// "Apr 2026"
	// ""
}

// Excel returns the number format a spreadsheet cell carries.
func ExampleFormatter_Excel() {
	d := format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, nil)
	fmt.Println(format.New(value.Decimal, d, format.Options{}).Excel())
	// Output: "$"#,##0.00;("$"#,##0.00) true
}

// Validate reports an option that cannot apply, with the JSON path of the field and a code to
// translate.
func ExampleSpec_Validate() {
	err := (&format.Spec{Style: format.StyleCurrency}).Validate(value.String)
	fmt.Println(err)
	// Output: format: style: this display style does not apply to the column's type
}

// Rules publishes the vocabulary a catalog offers its client.
func ExampleRules() {
	for _, ts := range format.Rules().Types[:3] {
		fmt.Println(ts.Type, ts.Default, ts.Styles)
	}
	// Output:
	// string text [text]
	// int number [number currency percent duration date text]
	// decimal number [number currency percent duration date text]
}
