package format

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/currency"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/text/number"
)

// currencyPattern is the text before and after the number in a currency pattern, "¤" standing
// for the symbol.
type currencyPattern struct {
	prefix, suffix string
}

// locale is what formatting numbers in one language needs, read once from x/text's CLDR data by
// formatting probe numbers and taking them apart, so no separator or sign is written down here.
type locale struct {
	tag       language.Tag
	decimal   string
	group     string
	primary   int
	secondary int
	minus     string
	percent   affix
	digits    *[10]string
	currency  currencyPattern
}

// affix is the text written before and after a number.
type affix struct {
	prefix, suffix string
}

// locales caches each language's formatting data.
var locales struct {
	sync.Mutex
	m map[language.Tag]*locale
}

// localeFor returns the formatting data of tag, deriving it on first use. The zero tag is
// American English.
func localeFor(tag language.Tag) *locale {
	if tag == language.Und {
		tag = language.AmericanEnglish
	}
	locales.Lock()
	defer locales.Unlock()
	if l, ok := locales.m[tag]; ok {
		return l
	}
	if locales.m == nil {
		locales.m = map[language.Tag]*locale{}
	}
	l := deriveLocale(tag)
	locales.m[tag] = l
	return l
}

// deriveLocale reads a language's formatting data from x/text and the currency patterns.
func deriveLocale(tag language.Tag) *locale {
	p := message.NewPrinter(tag)
	l := &locale{tag: tag, currency: patternFor(tag)}
	l.digits = nativeDigits(p.Sprint(number.Decimal(1234567890, number.NoSeparator())))
	l.separators(l.ascii(p.Sprint(number.Decimal(1234567890.5, number.Scale(1)))))
	l.minus, _ = l.around(p.Sprint(number.Decimal(-1)), "1")
	l.percent.prefix, l.percent.suffix = l.around(p.Sprint(number.Percent(0.5)), "50")
	return l
}

// nativeDigits maps ASCII digits to the locale's from the digits of 1234567890, or returns nil
// when the locale writes ASCII digits.
func nativeDigits(s string) *[10]string {
	if s == "1234567890" || utf8.RuneCountInString(s) != 10 {
		return nil
	}
	var d [10]string
	i := 1
	for _, r := range s {
		d[i%10] = string(r)
		i++
	}
	return &d
}

// ascii replaces the locale's digits in s with ASCII ones.
func (l *locale) ascii(s string) string {
	if l.digits == nil {
		return s
	}
	for i, d := range l.digits {
		s = strings.ReplaceAll(s, d, string(rune('0'+i)))
	}
	return s
}

// separators reads the decimal separator, the grouping separator and the group sizes from the
// locale's rendering of 1234567890.5.
func (l *locale) separators(s string) {
	var runs []int
	var seps []string
	for s != "" {
		n := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
		if n < 0 {
			n = len(s)
		}
		runs = append(runs, n)
		s = s[n:]
		m := strings.IndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' })
		if m < 0 {
			break
		}
		seps = append(seps, s[:m])
		s = s[m:]
	}
	l.decimal = seps[len(seps)-1]
	whole := runs[:len(runs)-1]
	if len(whole) < 2 {
		return
	}
	l.group = seps[0]
	l.primary = whole[len(whole)-1]
	l.secondary = whole[len(whole)-2]
}

// around returns the text before and after the locale's rendering of the ASCII digits num in s.
func (l *locale) around(s, num string) (string, string) {
	before, after, _ := strings.Cut(s, l.native(num))
	return before, after
}

// native writes ASCII digits in the locale's digits.
func (l *locale) native(s string) string {
	if l.digits == nil {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		b.WriteString(l.digits[r-'0'])
	}
	return b.String()
}

// patternFor returns the currency pattern of tag or its nearest listed ancestor.
func patternFor(tag language.Tag) currencyPattern {
	for {
		if p, ok := currencyPatterns[tag.String()]; ok {
			return p
		}
		if tag == language.Und {
			return currencyPattern{prefix: "¤"}
		}
		tag = tag.Parent()
	}
}

// currencyAffix returns the text written around a number in the currency with ISO code, the
// symbol as the locale writes it and placed as its pattern places it. A symbol ending (or
// starting) in a letter is kept off the digits with a no-break space, as CLDR's currency
// spacing rule does: "CHF 5.00", not "CHF5.00". No code means US dollars.
func (l *locale) currencyAffix(code string) affix {
	if code == "" {
		code = "USD"
	}
	symbol := code
	if unit, err := currency.ParseISO(code); err == nil {
		symbol = message.NewPrinter(l.tag).Sprint(currency.Symbol(unit))
	}
	prefix := strings.ReplaceAll(l.currency.prefix, "¤", symbol)
	suffix := strings.ReplaceAll(l.currency.suffix, "¤", symbol)
	if strings.HasSuffix(l.currency.prefix, "¤") {
		if r, _ := utf8.DecodeLastRuneInString(symbol); !unicode.Is(unicode.S, r) {
			prefix += "\u00a0"
		}
	}
	if strings.HasPrefix(l.currency.suffix, "¤") {
		if r, _ := utf8.DecodeRuneInString(symbol); !unicode.Is(unicode.S, r) {
			suffix = "\u00a0" + suffix
		}
	}
	return affix{prefix: prefix, suffix: suffix}
}
