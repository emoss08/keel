package format

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"golang.org/x/text/language"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/value"
)

// Options are the language and time zone a Formatter writes in.
type Options struct {
	// Locale decides the decimal and grouping separators, the group sizes, the digits, the
	// minus and percent signs, and the currency symbol and its placement. The zero tag is
	// American English.
	Locale language.Tag
	// Location is the time zone dates are written in. Nil is UTC.
	Location *time.Location
}

// Formatter renders the values of one column through its resolved display. Build one per
// column with New; it keeps scratch buffers, so it is not safe for concurrent use, and it
// allocates nothing per value once warm except for numbers too large for an int64.
type Formatter struct {
	t        value.Type
	d        Resolved
	loc      *time.Location
	l        *locale
	currency affix
	rules    []compiledRule
	banded   bool
	width    num
	edges    []num
	dig      []byte
	scaled   []byte
	x, y, r  big.Int
}

// compiledRule is a Rule with its thresholds read as exact decimals.
type compiledRule struct {
	op           RuleOp
	value, upper num
	tone         Tone
}

// New returns a Formatter for a column of type t shown through d.
func New(t value.Type, d Resolved, opts Options) *Formatter {
	f := &Formatter{t: t, d: d, loc: opts.Location, l: localeFor(opts.Locale)}
	if f.loc == nil {
		f.loc = time.UTC
	}
	f.d.Decimals = min(max(d.Decimals, 0), MaxDecimals)
	f.currency = f.l.currencyAffix(d.Currency)
	for _, r := range d.Rules {
		f.rules = append(f.rules, compiledRule{
			op: r.Op, value: numFromFloatThreshold(r.Value), upper: numFromFloatThreshold(r.Upper), tone: r.Tone,
		})
	}
	if d.Band.usable() {
		f.banded = true
		f.width = numFromFloatThreshold(d.Band.Width)
		for _, e := range d.Band.Edges {
			f.edges = append(f.edges, numFromFloatThreshold(e))
		}
	}
	return f
}

// Append appends the rendering of row i of v and returns the tone the column's rules give it.
func (f *Formatter) Append(dst []byte, v *value.Vector, i int) ([]byte, Tone) {
	if v.Null(i) {
		return append(dst, f.d.NullText...), ToneNone
	}
	switch f.t {
	case value.Int, value.Epoch:
		return f.appendNumber(dst, f.intNum(v.Int64[i]))
	case value.Decimal:
		return f.appendNumeric(dst, v.Numeric[i])
	case value.Float:
		return f.appendFloat(dst, v.Float64[i])
	case value.Bool:
		return f.appendBool(dst, v.Bool[i]), ToneNone
	case value.Time:
		return f.appendTime(dst, v.Time[i]), ToneNone
	case value.JSON:
		return f.appendJSON(dst, v.JSON[i]), ToneNone
	}
	return f.appendText(dst, v.String[i]), ToneNone
}

// Tone returns the tone the column's rules give row i of v, without rendering it.
func (f *Formatter) Tone(v *value.Vector, i int) Tone {
	if len(f.rules) == 0 || v.Null(i) {
		return ToneNone
	}
	switch f.t {
	case value.Int, value.Epoch:
		return f.tone(f.intNum(v.Int64[i]))
	case value.Decimal:
		if value.DecimalFinite(v.Numeric[i]) {
			return f.tone(f.numericNum(v.Numeric[i]))
		}
	case value.Float:
		if finite(v.Float64[i]) {
			return f.tone(f.floatNum(v.Float64[i]))
		}
	}
	return ToneNone
}

// Column renders rows 0 to n-1 of v into c, after the cells c already holds.
func (f *Formatter) Column(c *Cells, v *value.Vector, n int) {
	for i := range n {
		var tone Tone
		c.buf, tone = f.Append(c.buf, v, i)
		c.ends = append(c.ends, len(c.buf))
		c.tones = append(c.tones, tone)
	}
}

// Format renders one Go value: nil, an integer, a keel.Decimal, a float, a bool, a string, a
// time.Time or raw JSON bytes. In a numeric or epoch column a string holding a decimal is read
// as that number, so decimals can travel as exact text, and in a bool column "true" and "false"
// are booleans.
func (f *Formatter) Format(x any) (string, Tone) {
	text, tone := f.appendAny(nil, x)
	return string(text), tone
}

// appendAny renders one Go value.
func (f *Formatter) appendAny(dst []byte, x any) ([]byte, Tone) {
	switch x := x.(type) {
	case nil:
		return append(dst, f.d.NullText...), ToneNone
	case int:
		return f.appendNumber(dst, f.intNum(int64(x)))
	case int32:
		return f.appendNumber(dst, f.intNum(int64(x)))
	case int64:
		return f.appendNumber(dst, f.intNum(x))
	case keel.Decimal:
		if x == "" {
			return append(dst, f.d.NullText...), ToneNone
		}
		return f.appendNumeric(dst, x)
	case float64:
		return f.appendFloat(dst, x)
	case float32:
		return f.appendFloat(dst, float64(x))
	case bool:
		return f.appendBool(dst, x), ToneNone
	case time.Time:
		return f.appendTime(dst, x), ToneNone
	case []byte:
		return f.appendJSON(dst, x), ToneNone
	case string:
		return f.appendString(dst, x)
	}
	return f.appendText(dst, fmt.Sprint(x)), ToneNone
}

// appendString renders a string, read as a number in a numeric column when it holds one.
func (f *Formatter) appendString(dst []byte, s string) ([]byte, Tone) {
	if f.t.Numeric() || f.t == value.Epoch {
		if n, err := value.ParseDecimal(s); err == nil && s != "" {
			return f.appendNumeric(dst, n)
		}
	}
	if b, err := strconv.ParseBool(s); err == nil && f.t == value.Bool && (s == "true" || s == "false") {
		return f.appendBool(dst, b), ToneNone
	}
	return f.appendText(dst, s), ToneNone
}

// appendNumeric renders a numeric; NaN and the infinities are shown as words.
func (f *Formatter) appendNumeric(dst []byte, n keel.Decimal) ([]byte, Tone) {
	if special, ok := nonFinite(n); ok {
		return append(dst, special...), ToneNone
	}
	return f.appendNumber(dst, f.numericNum(n))
}

// nonFinite returns the text of a numeric that is not finite.
func nonFinite(n keel.Decimal) (string, bool) {
	if value.DecimalFinite(n) {
		return "", false
	}
	return string(n), true
}

// appendFloat renders a float; NaN and the infinities are shown as words.
func (f *Formatter) appendFloat(dst []byte, x float64) ([]byte, Tone) {
	switch {
	case math.IsNaN(x):
		return append(dst, "NaN"...), ToneNone
	case math.IsInf(x, 1):
		return append(dst, "Infinity"...), ToneNone
	case math.IsInf(x, -1):
		return append(dst, "-Infinity"...), ToneNone
	}
	return f.appendNumber(dst, f.floatNum(x))
}

// appendNumber renders a number in the column's style: raw, as the range of a band, as a date
// when the column shows dates, or in its numeric style.
func (f *Formatter) appendNumber(dst []byte, n num) ([]byte, Tone) {
	tone := f.tone(n)
	switch {
	case f.d.Style == StyleAuto:
		return appendPlain(dst, n), tone
	case f.banded:
		return f.appendBand(dst, n), tone
	case f.d.Style == StyleDate:
		if sec, ok := truncInt(n); ok {
			return f.appendDate(dst, time.Unix(sec, 0)), tone
		}
		return appendPlain(dst, n), tone
	}
	return f.appendStyled(dst, n, true), tone
}

// tone returns the tone of the first rule n matches.
func (f *Formatter) tone(n num) Tone {
	if len(f.rules) == 0 {
		return ToneNone
	}
	if f.d.Style == StylePercent {
		n.exp += 2
	}
	for i := range f.rules {
		if f.rules[i].matches(n) {
			return f.rules[i].tone
		}
	}
	return ToneNone
}

// matches reports whether n satisfies the rule.
func (r *compiledRule) matches(n num) bool {
	c := cmpNum(n, r.value)
	switch r.op {
	case RuleGreater:
		return c > 0
	case RuleGreaterEqual:
		return c >= 0
	case RuleLess:
		return c < 0
	case RuleLessEqual:
		return c <= 0
	case RuleEqual:
		return c == 0
	case RuleBetween:
		return c >= 0 && cmpNum(n, r.upper) <= 0
	}
	return false
}

// compactSteps are the compact notation's powers of ten and letters, largest first.
var compactSteps = [...]struct {
	power  int
	letter string
}{{12, "T"}, {9, "B"}, {6, "M"}, {3, "K"}}

// appendStyled renders a number in the number, currency, percent or duration style. The
// suffix is left off the lower end of a band's range.
func (f *Formatter) appendStyled(dst []byte, n num, withSuffix bool) []byte {
	if f.d.Style == StyleDuration {
		return f.appendDuration(dst, n, withSuffix)
	}
	percent := f.d.Style == StylePercent
	currency := f.d.Style == StyleCurrency
	mag := n
	if percent {
		mag.exp += 2
	}
	letter := ""
	if f.d.Notation == NotationCompact && !mag.zero() {
		for _, step := range compactSteps {
			if len(mag.digits)+mag.exp > step.power {
				mag.exp -= step.power
				letter = step.letter
				break
			}
		}
	}
	f.scaled = roundScaled(f.scaled[:0], mag, f.d.Decimals)
	neg := n.neg && len(f.scaled) > 0
	dst = f.openSign(dst, neg)
	dst = append(dst, f.d.Prefix...)
	if currency {
		dst = append(dst, f.currency.prefix...)
	}
	if percent {
		dst = append(dst, f.l.percent.prefix...)
	}
	dst = f.appendDigits(dst, f.scaled, f.d.Decimals, f.d.Grouping)
	dst = append(dst, letter...)
	if percent {
		dst = append(dst, f.l.percent.suffix...)
	}
	if currency {
		dst = append(dst, f.currency.suffix...)
	}
	if withSuffix {
		dst = append(dst, f.d.Suffix...)
	}
	return f.closeSign(dst, neg)
}

// openSign writes the start of a negative number's sign.
func (f *Formatter) openSign(dst []byte, neg bool) []byte {
	switch {
	case !neg:
		return dst
	case f.d.Negative == NegativeParentheses:
		return append(dst, '(')
	}
	return append(dst, f.l.minus...)
}

// closeSign writes the end of a negative number's sign.
func (f *Formatter) closeSign(dst []byte, neg bool) []byte {
	if neg && f.d.Negative == NegativeParentheses {
		return append(dst, ')')
	}
	return dst
}

// appendDigits writes scaled, the value times 10^places, with the locale's decimal separator,
// grouping and digits.
func (f *Formatter) appendDigits(dst, scaled []byte, places int, grouping bool) []byte {
	whole, frac := []byte("0"), scaled
	if len(scaled) > places {
		whole, frac = scaled[:len(scaled)-places], scaled[len(scaled)-places:]
	}
	dst = f.appendWhole(dst, whole, grouping)
	if places == 0 {
		return dst
	}
	dst = append(dst, f.l.decimal...)
	for range places - len(frac) {
		dst = f.appendDigit(dst, '0')
	}
	for _, c := range frac {
		dst = f.appendDigit(dst, c)
	}
	return dst
}

// appendWhole writes the whole part with the locale's grouping: the last group of the primary size,
// the others of the secondary size.
func (f *Formatter) appendWhole(dst, whole []byte, grouping bool) []byte {
	primary, secondary := f.l.primary, f.l.secondary
	if !grouping || primary == 0 || len(whole) <= primary {
		for _, c := range whole {
			dst = f.appendDigit(dst, c)
		}
		return dst
	}
	rest := len(whole) - primary
	first := rest % secondary
	if first == 0 {
		first = secondary
	}
	for i, c := range whole {
		if i >= first && i <= rest && (i-first)%secondary == 0 {
			dst = append(dst, f.l.group...)
		}
		dst = f.appendDigit(dst, c)
	}
	return dst
}

// appendDigit writes an ASCII digit as the locale writes it.
func (f *Formatter) appendDigit(dst []byte, c byte) []byte {
	if f.l.digits == nil {
		return append(dst, c)
	}
	return append(dst, f.l.digits[c-'0']...)
}

// appendBand renders the range a banded value stands for, each end in the column's display; a
// trailing suffix is written once, after the range.
func (f *Formatter) appendBand(dst []byte, n num) []byte {
	lower, upper, bounded := f.bounds(n)
	if !bounded {
		return append(f.appendStyled(dst, lower, true), '+')
	}
	dst = f.appendStyled(dst, lower, false)
	dst = append(dst, " – "...)
	return f.appendStyled(dst, upper, true)
}

// bounds returns the range a banded value stands for, and false for the open-ended top range.
func (f *Formatter) bounds(n num) (num, num, bool) {
	if len(f.edges) == 0 {
		return n, addNum(n, f.width), true
	}
	index := 0
	for i := range f.edges {
		if cmpNum(n, f.edges[i]) >= 0 {
			index = i
		}
	}
	if index == len(f.edges)-1 {
		return f.edges[index], num{}, false
	}
	return f.edges[index], f.edges[index+1], true
}

// appendBool renders a boolean in the column's boolean style.
func (f *Formatter) appendBool(dst []byte, b bool) []byte {
	if f.d.Style == StyleAuto {
		return strconv.AppendBool(dst, b)
	}
	yes, no := boolWords(f.d.BoolStyle)
	if b {
		return append(dst, yes...)
	}
	return append(dst, no...)
}

// boolWords returns the words of a boolean style.
func boolWords(style BoolStyle) (string, string) {
	switch style {
	case BoolStyleTrueFalse:
		return "True", "False"
	case BoolStyleOnOff:
		return "On", "Off"
	case BoolStyleCheck:
		return "✓", "✗"
	case BoolStyleOneZero:
		return "1", "0"
	}
	return "Yes", "No"
}

// appendText renders text: as stored in a raw display, otherwise through the enum labels with
// the prefix and suffix, and the empty-value text for an empty string.
func (f *Formatter) appendText(dst []byte, s string) []byte {
	if f.d.Style == StyleAuto {
		return append(dst, s...)
	}
	if label, ok := f.d.Labels[s]; ok {
		s = label
	}
	if s == "" {
		return append(dst, f.d.NullText...)
	}
	dst = append(dst, f.d.Prefix...)
	dst = append(dst, s...)
	return append(dst, f.d.Suffix...)
}

// appendJSON renders raw JSON text as text is rendered, without enum labels.
func (f *Formatter) appendJSON(dst, raw []byte) []byte {
	switch {
	case f.d.Style == StyleAuto:
		return append(dst, raw...)
	case len(raw) == 0:
		return append(dst, f.d.NullText...)
	}
	dst = append(dst, f.d.Prefix...)
	dst = append(dst, raw...)
	return append(dst, f.d.Suffix...)
}

// intNum reads i into the formatter's digit buffer.
func (f *Formatter) intNum(i int64) num {
	n := numFromInt(f.dig, i)
	f.dig = n.digits[:0]
	return n
}

// numericNum reads a finite numeric into the formatter's digit buffer.
func (f *Formatter) numericNum(x keel.Decimal) num {
	n := numFromNumeric(f.dig, x)
	f.dig = n.digits[:0]
	return n
}

// floatNum reads a finite float into the formatter's digit buffer.
func (f *Formatter) floatNum(x float64) num {
	n := numFromFloat(f.dig, x)
	f.dig = n.digits[:0]
	return n
}
