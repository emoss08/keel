package value

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/emoss08/keel"
)

// The text of the decimals that are not finite numbers, as Postgres writes them.
const (
	nanText    = "NaN"
	posInfText = "Infinity"
	negInfText = "-Infinity"
)

// ErrNotFinite reports arithmetic on NaN or an infinity.
var ErrNotFinite = errors.New("value: decimal is not finite")

// DecimalFromInt returns i as a Decimal value.
func DecimalFromInt(i int64) keel.Decimal { return keel.Decimal(strconv.FormatInt(i, 10)) }

// DecimalFromFloat returns f as a Decimal value, read from its shortest decimal representation,
// so 0.1 is exactly 0.1 rather than the binary fraction nearest to it. NaN and the infinities
// map to Postgres's NaN and infinities.
func DecimalFromFloat(f float64) keel.Decimal {
	switch {
	case math.IsNaN(f):
		return nanText
	case math.IsInf(f, 1):
		return posInfText
	case math.IsInf(f, -1):
		return negInfText
	}
	d, _ := ParseDecimal(strconv.FormatFloat(f, 'g', -1, 64))
	return d
}

// ParseDecimal reads a decimal written in plain or exponent notation ("-12.50", "1.2e3"), or
// "NaN", "Infinity" or "-Infinity", and returns it in the plain text Postgres writes. Surrounding
// spaces are ignored. The scale is kept as written, so "1.50" has two places.
func ParseDecimal(s string) (keel.Decimal, error) {
	s = strings.TrimSpace(s)
	switch s {
	case nanText:
		return nanText, nil
	case posInfText, "+Infinity", "inf", "+inf":
		return posInfText, nil
	case negInfText, "-inf":
		return negInfText, nil
	}
	mantissa, exp, err := splitExponent(s)
	if err != nil {
		return "", err
	}
	neg := false
	if mantissa != "" && (mantissa[0] == '-' || mantissa[0] == '+') {
		neg = mantissa[0] == '-'
		mantissa = mantissa[1:]
	}
	whole, frac, _ := strings.Cut(mantissa, ".")
	digits := whole + frac
	if digits == "" || !allDigits(digits) {
		return "", fmt.Errorf("value: invalid decimal %q", s)
	}
	scale := int64(len(frac)) - int64(exp)
	if scale < -1e6 || scale > 1e6 {
		return "", fmt.Errorf("value: decimal %q is out of range", s)
	}
	return keel.Decimal(plain(neg, digits, int(scale))), nil
}

// allDigits reports whether s holds only ASCII digits.
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// splitExponent splits a decimal into its mantissa and its exponent.
func splitExponent(s string) (string, int, error) {
	at := strings.IndexAny(s, "eE")
	if at < 0 {
		return s, 0, nil
	}
	exp, err := strconv.Atoi(s[at+1:])
	if err != nil {
		return "", 0, fmt.Errorf("value: invalid decimal %q", s)
	}
	return s[:at], exp, nil
}

// plain writes ±digits × 10^-scale as plain decimal text: leading zeros dropped, a negative scale
// written as trailing zeros, negative zero written as zero.
func plain(neg bool, digits string, scale int) string {
	digits = strings.TrimLeft(digits, "0")
	if scale < 0 {
		if digits != "" {
			digits += strings.Repeat("0", -scale)
		}
		scale = 0
	}
	if strings.Trim(digits, "0") == "" {
		neg = false
	}
	var b strings.Builder
	b.Grow(len(digits) + scale + 3)
	if neg {
		b.WriteByte('-')
	}
	if len(digits) <= scale {
		b.WriteByte('0')
		if scale > 0 {
			b.WriteByte('.')
			b.WriteString(strings.Repeat("0", scale-len(digits)))
			b.WriteString(digits)
		}
		return b.String()
	}
	b.WriteString(digits[:len(digits)-scale])
	if scale > 0 {
		b.WriteByte('.')
		b.WriteString(digits[len(digits)-scale:])
	}
	return b.String()
}

// DecimalFinite reports whether d is a number other than NaN or an infinity. The empty Decimal,
// which stands for NULL, is not.
func DecimalFinite(d keel.Decimal) bool {
	return d != "" && d != nanText && d != posInfText && d != negInfText
}

// SplitDecimal returns the parts of a finite decimal in plain text: its sign, the digits before
// the point and the digits after it, without allocating. ok is false for a value that is not
// finite.
func SplitDecimal(d keel.Decimal) (neg bool, whole, frac string, ok bool) {
	if !DecimalFinite(d) {
		return false, "", "", false
	}
	s := string(d)
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, _ = strings.Cut(s, ".")
	return neg, whole, frac, true
}

// NormalizeDecimal returns d without trailing zeros after the point, so 1.50 becomes 1.5 and
// 2.000 becomes 2; negative zero becomes 0. Values that are not finite are returned as they are.
func NormalizeDecimal(d keel.Decimal) keel.Decimal {
	neg, whole, frac, ok := SplitDecimal(d)
	if !ok {
		return d
	}
	frac = strings.TrimRight(frac, "0")
	return keel.Decimal(plain(neg, whole+frac, len(frac)))
}

// fixed is a finite decimal as ±coef × 10^-scale, with the coefficient in an int64 when it fits
// and in a big.Int otherwise.
type fixed struct {
	small int64
	big   *big.Int
	scale int
}

// fixedOf reads a finite decimal.
func fixedOf(d keel.Decimal) (fixed, error) {
	neg, whole, frac, ok := SplitDecimal(d)
	if !ok {
		return fixed{}, ErrNotFinite
	}
	digits := strings.TrimLeft(whole+frac, "0")
	f := fixed{scale: len(frac)}
	if len(digits) <= 18 {
		for i := 0; i < len(digits); i++ {
			f.small = f.small*10 + int64(digits[i]-'0')
		}
		if neg {
			f.small = -f.small
		}
		return f, nil
	}
	f.big, _ = new(big.Int).SetString(digits, 10)
	if neg {
		f.big.Neg(f.big)
	}
	return f, nil
}

// bigInt returns the coefficient as a new big.Int.
func (f fixed) bigInt() *big.Int {
	if f.big != nil {
		return new(big.Int).Set(f.big)
	}
	return big.NewInt(f.small)
}

// rescale returns f at a larger scale, staying in an int64 when the result fits.
func (f fixed) rescale(scale int) fixed {
	up := scale - f.scale
	if up <= 0 {
		return f
	}
	if f.big == nil && up <= 18 {
		p := int64(pow10s[up])
		if hi, lo := mulCheck(f.small, p); hi {
			return fixed{small: lo, scale: scale}
		}
	}
	x := f.bigInt()
	x.Mul(x, new(big.Int).Exp(bigTen, big.NewInt(int64(up)), nil))
	return fixed{big: x, scale: scale}
}

// mulCheck returns a × b and whether it fits in an int64.
func mulCheck(a, b int64) (bool, int64) {
	if a == 0 || b == 0 {
		return true, 0
	}
	c := a * b
	if c/b != a {
		return false, 0
	}
	return true, c
}

// pow10s are the powers of ten that fit in an int64.
var pow10s = func() [19]uint64 {
	var p [19]uint64
	p[0] = 1
	for i := 1; i < len(p); i++ {
		p[i] = p[i-1] * 10
	}
	return p
}()

// bigTen is 10.
var bigTen = big.NewInt(10)

// text writes f as plain decimal text.
func (f fixed) text() keel.Decimal {
	var digits string
	neg := false
	if f.big != nil {
		neg = f.big.Sign() < 0
		digits = new(big.Int).Abs(f.big).String()
	} else {
		u := uint64(f.small)
		if f.small < 0 {
			neg, u = true, -u
		}
		digits = strconv.FormatUint(u, 10)
	}
	return keel.Decimal(plain(neg, digits, f.scale))
}

// aligned reads a and b at their common scale.
func aligned(a, b keel.Decimal) (fixed, fixed, error) {
	x, err := fixedOf(a)
	if err != nil {
		return fixed{}, fixed{}, err
	}
	y, err := fixedOf(b)
	if err != nil {
		return fixed{}, fixed{}, err
	}
	scale := max(x.scale, y.scale)
	return x.rescale(scale), y.rescale(scale), nil
}

// AddDecimal returns a + b exactly. Both must be finite.
func AddDecimal(a, b keel.Decimal) (keel.Decimal, error) {
	x, y, err := aligned(a, b)
	if err != nil {
		return "", err
	}
	if x.big == nil && y.big == nil {
		if s := x.small + y.small; (s > x.small) == (y.small > 0) {
			return fixed{small: s, scale: x.scale}.text(), nil
		}
	}
	return fixed{big: x.bigInt().Add(x.bigInt(), y.bigInt()), scale: x.scale}.text(), nil
}

// SubDecimal returns a - b exactly. Both must be finite.
func SubDecimal(a, b keel.Decimal) (keel.Decimal, error) {
	neg := b
	if DecimalFinite(b) {
		if b[0] == '-' {
			neg = b[1:]
		} else {
			neg = "-" + b
		}
	}
	return AddDecimal(a, neg)
}

// CompareDecimal returns -1, 0 or 1 as a is less than, equal to or greater than b. Both must be
// finite.
func CompareDecimal(a, b keel.Decimal) (int, error) {
	x, y, err := aligned(a, b)
	if err != nil {
		return 0, err
	}
	if x.big == nil && y.big == nil {
		switch {
		case x.small < y.small:
			return -1, nil
		case x.small > y.small:
			return 1, nil
		}
		return 0, nil
	}
	return x.bigInt().Cmp(y.bigInt()), nil
}
