package format

import "time"

// appendTime renders an instant: RFC 3339 in UTC in a raw display, otherwise as a date in the
// formatter's time zone, whatever the numeric style.
func (f *Formatter) appendTime(dst []byte, t time.Time) []byte {
	if f.d.Style == StyleAuto {
		return t.UTC().AppendFormat(dst, time.RFC3339Nano)
	}
	return f.appendDate(dst, t)
}

// appendDate writes an instant in the date style, in the formatter's time zone. The layouts are
// the same in every locale.
func (f *Formatter) appendDate(dst []byte, t time.Time) []byte {
	t = t.In(f.loc)
	if f.d.DateStyle == DateStyleQuarter {
		dst = append(dst, 'Q', byte('0'+(int(t.Month())-1)/3+1), ' ')
		return t.AppendFormat(dst, "2006")
	}
	return t.AppendFormat(dst, dateLayout(f.d.DateStyle))
}

// dateLayout returns the Go layout of a date style.
func dateLayout(style DateStyle) string {
	switch style {
	case DateStyleDate:
		return "2006-01-02"
	case DateStyleDateLong:
		return "Jan 2, 2006"
	case DateStyleTime:
		return "15:04"
	case DateStyleMonthYear:
		return "Jan 2006"
	case DateStyleYear:
		return "2006"
	case DateStyleWeekday:
		return "Mon 2006-01-02"
	case DateStyleISO:
		return time.RFC3339
	}
	return "2006-01-02 15:04"
}
