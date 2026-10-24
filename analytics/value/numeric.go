package value

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// bigTen is 10.
var bigTen = big.NewInt(10)

// NumericFromInt returns i as a Decimal value.
func NumericFromInt(i int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(i), Valid: true}
}

// NumericFromFloat returns f as a Decimal value, read from its shortest decimal representation,
// so 0.1 is exactly 0.1 rather than the binary fraction nearest to it. NaN and the infinities
// map to Postgres's NaN and infinities.
func NumericFromFloat(f float64) pgtype.Numeric {
	switch {
	case math.IsNaN(f):
		return pgtype.Numeric{NaN: true, Valid: true}
	case math.IsInf(f, 1):
		return pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}
	case math.IsInf(f, -1):
		return pgtype.Numeric{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
	}
	n, _ := ParseNumeric(strconv.FormatFloat(f, 'g', -1, 64))
	return n
}

// ParseNumeric reads a decimal written in plain or exponent notation ("-12.50", "1.2e3"), or
// "NaN", "Infinity" or "-Infinity". Surrounding spaces are ignored. The scale is kept as
// written, so "1.50" has two places.
func ParseNumeric(s string) (pgtype.Numeric, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "NaN":
		return pgtype.Numeric{NaN: true, Valid: true}, nil
	case "Infinity", "+Infinity", "inf", "+inf":
		return pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}, nil
	case "-Infinity", "-inf":
		return pgtype.Numeric{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, nil
	}
	mantissa, exp, err := splitExponent(s)
	if err != nil {
		return pgtype.Numeric{}, err
	}
	whole, frac, _ := strings.Cut(mantissa, ".")
	digits := whole + frac
	if digits == "" || digits == "-" || digits == "+" || strings.ContainsAny(digits[1:], "+-") {
		return pgtype.Numeric{}, fmt.Errorf("value: invalid decimal %q", s)
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return pgtype.Numeric{}, fmt.Errorf("value: invalid decimal %q", s)
	}
	scale := int64(exp) - int64(len(frac))
	if scale < math.MinInt32 || scale > math.MaxInt32 {
		return pgtype.Numeric{}, fmt.Errorf("value: decimal %q is out of range", s)
	}
	return pgtype.Numeric{Int: n, Exp: int32(scale), Valid: true}, nil
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

// AppendNumeric appends n as plain decimal text, keeping its scale ("1.50", "-0.001", "1200").
// NaN and the infinities are written "NaN", "Infinity" and "-Infinity"; a null value appends
// nothing.
func AppendNumeric(dst []byte, n pgtype.Numeric) []byte {
	if special, ok := numericSpecial(n); ok {
		return append(dst, special...)
	}
	if n.Int == nil {
		return append(dst, '0')
	}
	start := len(dst)
	if n.Int.IsInt64() {
		dst = strconv.AppendInt(dst, n.Int.Int64(), 10)
	} else {
		dst = n.Int.Append(dst, 10)
	}
	neg := len(dst) > start && dst[start] == '-'
	digitsAt := start
	if neg {
		digitsAt++
	}
	if n.Exp >= 0 {
		if n.Int.Sign() == 0 {
			return dst
		}
		for range n.Exp {
			dst = append(dst, '0')
		}
		return dst
	}
	return insertPoint(dst, digitsAt, int(-n.Exp))
}

// insertPoint inserts a decimal point before the last places digits of dst[digitsAt:], padding
// with leading zeros.
func insertPoint(dst []byte, digitsAt, places int) []byte {
	have := len(dst) - digitsAt
	if have <= places {
		pad := places - have + 1
		for range pad {
			dst = append(dst, 0)
		}
		copy(dst[digitsAt+pad:], dst[digitsAt:len(dst)-pad])
		for i := range pad {
			dst[digitsAt+i] = '0'
		}
	}
	dst = append(dst, 0)
	point := len(dst) - 1 - places
	copy(dst[point+1:], dst[point:len(dst)-1])
	dst[point] = '.'
	return dst
}

// numericSpecial returns the text of a null, NaN or infinite numeric.
func numericSpecial(n pgtype.Numeric) (string, bool) {
	switch {
	case !n.Valid:
		return "", true
	case n.NaN:
		return "NaN", true
	case n.InfinityModifier == pgtype.Infinity:
		return "Infinity", true
	case n.InfinityModifier == pgtype.NegativeInfinity:
		return "-Infinity", true
	}
	return "", false
}

// NumericFinite reports whether n is a valid number other than NaN or an infinity.
func NumericFinite(n pgtype.Numeric) bool {
	_, special := numericSpecial(n)
	return !special
}

// NormalizeNumeric returns n without trailing zeros in its coefficient, so 1.50 becomes 1.5 and
// 1200 becomes 12e2. Values that are not finite are returned as they are.
func NormalizeNumeric(n pgtype.Numeric) pgtype.Numeric {
	if !NumericFinite(n) {
		return n
	}
	if n.Int == nil || n.Int.Sign() == 0 {
		return pgtype.Numeric{Int: new(big.Int), Valid: true}
	}
	coef := new(big.Int).Set(n.Int)
	exp := n.Exp
	rem := new(big.Int)
	quo := new(big.Int)
	for {
		quo.QuoRem(coef, bigTen, rem)
		if rem.Sign() != 0 {
			break
		}
		coef, quo = quo, coef
		exp++
	}
	return pgtype.Numeric{Int: coef, Exp: exp, Valid: true}
}

// ErrNotFinite reports arithmetic on NaN or an infinity.
var ErrNotFinite = errors.New("value: decimal is not finite")

// AddNumeric returns a + b exactly. Both must be finite.
func AddNumeric(a, b pgtype.Numeric) (pgtype.Numeric, error) {
	x, y, exp, err := aligned(a, b)
	if err != nil {
		return pgtype.Numeric{}, err
	}
	return pgtype.Numeric{Int: x.Add(x, y), Exp: exp, Valid: true}, nil
}

// SubNumeric returns a - b exactly. Both must be finite.
func SubNumeric(a, b pgtype.Numeric) (pgtype.Numeric, error) {
	x, y, exp, err := aligned(a, b)
	if err != nil {
		return pgtype.Numeric{}, err
	}
	return pgtype.Numeric{Int: x.Sub(x, y), Exp: exp, Valid: true}, nil
}

// CompareNumeric returns -1, 0 or 1 as a is less than, equal to or greater than b. Both must be
// finite.
func CompareNumeric(a, b pgtype.Numeric) (int, error) {
	x, y, _, err := aligned(a, b)
	if err != nil {
		return 0, err
	}
	return x.Cmp(y), nil
}

// aligned returns the coefficients of a and b at their common exponent.
func aligned(a, b pgtype.Numeric) (*big.Int, *big.Int, int32, error) {
	if !NumericFinite(a) || !NumericFinite(b) {
		return nil, nil, 0, ErrNotFinite
	}
	x, y := coefficient(a), coefficient(b)
	exp := min(a.Exp, b.Exp)
	scaleUp(x, int64(a.Exp)-int64(exp))
	scaleUp(y, int64(b.Exp)-int64(exp))
	return x, y, exp, nil
}

// coefficient returns a copy of n's coefficient.
func coefficient(n pgtype.Numeric) *big.Int {
	if n.Int == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(n.Int)
}

// scaleUp multiplies x by 10^places.
func scaleUp(x *big.Int, places int64) {
	if places <= 0 {
		return
	}
	x.Mul(x, new(big.Int).Exp(bigTen, big.NewInt(places), nil))
}
