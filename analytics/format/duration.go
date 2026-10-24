package format

import (
	"math"
	"strconv"
)

// unitSeconds returns the seconds in one duration unit.
func unitSeconds(u DurationUnit) int64 {
	switch u {
	case DurationUnitMinutes:
		return 60
	case DurationUnitHours:
		return 3600
	case DurationUnitDays:
		return 86_400
	}
	return 1
}

// appendDuration renders a number of duration units. Decimal hours are rounded half away from
// zero; the other styles truncate to whole seconds and show days, hours and minutes. The sign
// is dropped when what is shown is zero.
func (f *Formatter) appendDuration(dst []byte, n num, withSuffix bool) []byte {
	var body []byte
	var zero bool
	if f.d.DurationStyle == DurationStyleDecimalHours {
		body, zero = f.decimalHours(f.scaled[:0], n)
	} else {
		seconds := f.wholeSeconds(n)
		zero = seconds < 60
		body = appendClock(f.scaled[:0], seconds, f.d.DurationStyle)
	}
	f.scaled = body
	neg := n.neg && !zero
	dst = f.openSign(dst, neg)
	dst = append(dst, f.d.Prefix...)
	dst = append(dst, body...)
	if withSuffix {
		dst = append(dst, f.d.Suffix...)
	}
	return f.closeSign(dst, neg)
}

// coefficientSeconds sets f.x to the magnitude's coefficient times the unit's seconds, so the
// duration in seconds is f.x × 10^n.exp.
func (f *Formatter) coefficientSeconds(n num) {
	setDigits(&f.x, n.digits)
	f.y.SetInt64(unitSeconds(f.d.DurationUnit))
	f.x.Mul(&f.x, &f.y)
}

// wholeSeconds returns the duration's whole seconds, truncated toward zero and saturated at the
// largest uint64.
func (f *Formatter) wholeSeconds(n num) uint64 {
	f.coefficientSeconds(n)
	if n.exp >= 0 {
		f.x.Mul(&f.x, pow10(n.exp))
	} else {
		f.x.Quo(&f.x, pow10(-n.exp))
	}
	if !f.x.IsUint64() {
		return math.MaxUint64
	}
	return f.x.Uint64()
}

// decimalHours appends the duration in hours with at least one place, grouped as numbers are,
// followed by " h", and reports whether the rounded hours are zero.
func (f *Formatter) decimalHours(dst []byte, n num) ([]byte, bool) {
	places := max(f.d.Decimals, 1)
	f.coefficientSeconds(n)
	shift := n.exp + places
	f.y.SetInt64(3600)
	if shift >= 0 {
		f.x.Mul(&f.x, pow10(shift))
	} else {
		f.y.Mul(&f.y, pow10(-shift))
	}
	f.x.QuoRem(&f.x, &f.y, &f.r)
	f.r.Lsh(&f.r, 1)
	if f.r.Cmp(&f.y) >= 0 {
		f.x.Add(&f.x, bigOne)
	}
	digits := f.x.Append(f.dig[:0], 10)
	zero := f.x.Sign() == 0
	if zero {
		digits = digits[:0]
	}
	f.dig = digits
	dst = f.appendDigits(dst, digits, places, f.d.Grouping)
	return append(dst, " h"...), zero
}

// appendClock writes whole seconds as "26:10", "1d 2h 10m" or "1 day 2 hours 10 minutes".
func appendClock(dst []byte, seconds uint64, style DurationStyle) []byte {
	switch style {
	case DurationStyleCompact, DurationStyleVerbose:
		return appendParts(dst, seconds, style == DurationStyleVerbose)
	}
	dst = strconv.AppendUint(dst, seconds/3600, 10)
	dst = append(dst, ':')
	minutes := (seconds % 3600) / 60
	if minutes < 10 {
		dst = append(dst, '0')
	}
	return strconv.AppendUint(dst, minutes, 10)
}

// durationParts name days, hours and minutes in the compact and verbose styles.
var durationParts = [...]struct {
	short, long string
}{{"d", "day"}, {"h", "hour"}, {"m", "minute"}}

// appendParts writes the non-zero days, hours and minutes of a duration.
func appendParts(dst []byte, seconds uint64, verbose bool) []byte {
	counts := [...]uint64{seconds / 86_400, (seconds % 86_400) / 3600, (seconds % 3600) / 60}
	start := len(dst)
	for i, count := range counts {
		if count == 0 {
			continue
		}
		if len(dst) > start {
			dst = append(dst, ' ')
		}
		dst = strconv.AppendUint(dst, count, 10)
		if !verbose {
			dst = append(dst, durationParts[i].short...)
			continue
		}
		dst = append(dst, ' ')
		dst = append(dst, durationParts[i].long...)
		if count != 1 {
			dst = append(dst, 's')
		}
	}
	switch {
	case len(dst) > start:
		return dst
	case verbose:
		return append(dst, "0 minutes"...)
	}
	return append(dst, "0m"...)
}
