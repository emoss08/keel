package format

import (
	"math"
	"math/big"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
)

// num is an exact decimal as the formatter works on it: the magnitude's coefficient as ASCII
// digits without leading zeros (empty for zero) and a power of ten, value = ±digits × 10^exp.
// Its digits usually alias a Formatter's scratch buffer.
type num struct {
	neg    bool
	digits []byte
	exp    int
}

// zero reports whether n is zero.
func (n num) zero() bool { return len(n.digits) == 0 }

// numFromInt reads i into buf.
func numFromInt(buf []byte, i int64) num {
	u := uint64(i)
	if i < 0 {
		u = -u
	}
	if u == 0 {
		return num{digits: buf[:0]}
	}
	return num{neg: i < 0, digits: strconv.AppendUint(buf[:0], u, 10)}
}

// numFromNumeric reads a finite numeric into buf.
func numFromNumeric(buf []byte, n pgtype.Numeric) num {
	if n.Int == nil || n.Int.Sign() == 0 {
		return num{digits: buf[:0]}
	}
	if n.Int.IsInt64() {
		out := numFromInt(buf, n.Int.Int64())
		out.exp = int(n.Exp)
		return out
	}
	digits := n.Int.Append(buf[:0], 10)
	neg := digits[0] == '-'
	if neg {
		digits = digits[1:]
	}
	return num{neg: neg, digits: digits, exp: int(n.Exp)}
}

// numFromFloat reads a finite float from its shortest decimal representation into buf.
func numFromFloat(buf []byte, f float64) num {
	if f == 0 {
		return num{digits: buf[:0]}
	}
	b := strconv.AppendFloat(buf[:0], math.Abs(f), 'e', -1, 64)
	at := len(b) - 1
	for b[at] != 'e' {
		at--
	}
	e := 0
	for _, c := range b[at+2:] {
		e = e*10 + int(c-'0')
	}
	if b[at+1] == '-' {
		e = -e
	}
	b = b[:at]
	frac := 0
	if len(b) > 1 && b[1] == '.' {
		frac = len(b) - 2
		copy(b[1:], b[2:])
		b = b[:len(b)-1]
	}
	return num{neg: f < 0, digits: b, exp: e - frac}
}

// numFromFloatThreshold reads a rule threshold or band boundary into its own buffer.
func numFromFloatThreshold(f float64) num { return numFromFloat(nil, f) }

// trimmed returns n without trailing zeros in its digits.
func (n num) trimmed() num {
	for len(n.digits) > 0 && n.digits[len(n.digits)-1] == '0' {
		n.digits = n.digits[:len(n.digits)-1]
		n.exp++
	}
	return n
}

// cmpNum compares two exact decimals.
func cmpNum(a, b num) int {
	a, b = a.trimmed(), b.trimmed()
	switch {
	case a.zero() && b.zero():
		return 0
	case a.zero():
		return sign(!b.neg) * -1
	case b.zero():
		return sign(!a.neg)
	case a.neg != b.neg:
		return sign(!a.neg)
	}
	m := cmpMagnitude(a, b)
	if a.neg {
		return -m
	}
	return m
}

// sign returns 1 for true and -1 for false.
func sign(positive bool) int {
	if positive {
		return 1
	}
	return -1
}

// cmpMagnitude compares the magnitudes of two non-zero decimals without trailing zeros.
func cmpMagnitude(a, b num) int {
	ma, mb := len(a.digits)+a.exp, len(b.digits)+b.exp
	if ma != mb {
		return sign(ma > mb)
	}
	for i := 0; i < len(a.digits) && i < len(b.digits); i++ {
		if a.digits[i] != b.digits[i] {
			return sign(a.digits[i] > b.digits[i])
		}
	}
	switch {
	case len(a.digits) == len(b.digits):
		return 0
	default:
		return sign(len(a.digits) > len(b.digits))
	}
}

// bigOf returns n's signed coefficient.
func bigOf(n num) *big.Int {
	x := new(big.Int)
	setDigits(x, n.digits)
	if n.neg {
		x.Neg(x)
	}
	return x
}

// addNum returns a + b exactly, in a new buffer.
func addNum(a, b num) num {
	x, y := bigOf(a), bigOf(b)
	exp := min(a.exp, b.exp)
	x.Mul(x, pow10(a.exp-exp))
	y.Mul(y, pow10(b.exp-exp))
	x.Add(x, y)
	if x.Sign() == 0 {
		return num{exp: exp}
	}
	digits := x.Append(nil, 10)
	neg := digits[0] == '-'
	if neg {
		digits = digits[1:]
	}
	return num{neg: neg, digits: digits, exp: exp}
}

var (
	bigOne = big.NewInt(1)
	bigTen = big.NewInt(10)
	powers = func() [40]*big.Int {
		var p [40]*big.Int
		p[0] = big.NewInt(1)
		for i := 1; i < len(p); i++ {
			p[i] = new(big.Int).Mul(p[i-1], bigTen)
		}
		return p
	}()
)

// pow10 returns 10^n for n ≥ 0. Small powers are shared and must not be modified.
func pow10(n int) *big.Int {
	if n < len(powers) {
		return powers[n]
	}
	return new(big.Int).Exp(bigTen, big.NewInt(int64(n)), nil)
}

// setDigits sets z to the decimal digits, without allocating when they fit a uint64.
func setDigits(z *big.Int, digits []byte) {
	if len(digits) > 19 {
		z.SetString(string(digits), 10)
		return
	}
	var u uint64
	for _, c := range digits {
		u = u*10 + uint64(c-'0')
	}
	z.SetUint64(u)
}

// roundScaled appends the digits of |n| × 10^places rounded half away from zero, without
// leading zeros (nothing for zero).
func roundScaled(dst []byte, n num, places int) []byte {
	start := len(dst)
	if n.zero() {
		return dst
	}
	shift := n.exp + places
	if shift >= 0 {
		dst = append(dst, n.digits...)
		for range shift {
			dst = append(dst, '0')
		}
		return dst
	}
	drop := -shift
	if drop > len(n.digits) {
		return dst
	}
	keep := len(n.digits) - drop
	dst = append(dst, n.digits[:keep]...)
	if n.digits[keep] >= '5' {
		dst = increment(dst, start)
	}
	return stripLeadingZeros(dst, start)
}

// increment adds one to the decimal digits in dst[start:].
func increment(dst []byte, start int) []byte {
	for i := len(dst) - 1; i >= start; i-- {
		if dst[i] != '9' {
			dst[i]++
			return dst
		}
		dst[i] = '0'
	}
	dst = append(dst, 0)
	copy(dst[start+1:], dst[start:len(dst)-1])
	dst[start] = '1'
	return dst
}

// stripLeadingZeros removes the leading zeros of dst[start:].
func stripLeadingZeros(dst []byte, start int) []byte {
	i := start
	for i < len(dst) && dst[i] == '0' {
		i++
	}
	if i == start {
		return dst
	}
	return append(dst[:start], dst[i:]...)
}

// truncInt returns the whole part of n, truncated toward zero, and whether it fits an int64.
func truncInt(n num) (int64, bool) {
	var u uint64
	whole := len(n.digits) + n.exp
	for i := range whole {
		d := uint64(0)
		if i < len(n.digits) {
			d = uint64(n.digits[i] - '0')
		}
		if u > (math.MaxInt64-d)/10 {
			return 0, false
		}
		u = u*10 + d
	}
	if n.neg {
		return -int64(u), true
	}
	return int64(u), true
}

// appendPlain appends n as plain decimal text without trailing zeros, as a raw display writes a
// number.
func appendPlain(dst []byte, n num) []byte {
	n = n.trimmed()
	if n.zero() {
		return append(dst, '0')
	}
	if n.neg {
		dst = append(dst, '-')
	}
	if n.exp >= 0 {
		dst = append(dst, n.digits...)
		for range n.exp {
			dst = append(dst, '0')
		}
		return dst
	}
	point := len(n.digits) + n.exp
	if point <= 0 {
		dst = append(dst, '0', '.')
		for range -point {
			dst = append(dst, '0')
		}
		return append(dst, n.digits...)
	}
	dst = append(dst, n.digits[:point]...)
	dst = append(dst, '.')
	return append(dst, n.digits[point:]...)
}
