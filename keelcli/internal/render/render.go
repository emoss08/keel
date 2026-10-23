// Package render turns keelcli's results into text: tables that fit a width, one line per run
// event, and JSON. Text is styled through a Painter, so the same table prints plain in CI and
// coloured in a terminal.
package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Tone is the meaning of a piece of text, which a Painter turns into a style.
type Tone uint8

// The tones.
const (
	Normal Tone = iota
	Muted
	Faint
	Header
	Strong
	Accent
	OK
	Warn
	Fail
	Info
)

// Painter styles text by tone.
type Painter interface {
	Paint(t Tone, s string) string
}

// Plain is the Painter that leaves text unstyled.
var Plain Painter = plain{}

type plain struct{}

// Paint returns s unchanged.
func (plain) Paint(_ Tone, s string) string { return s }

// IsPlain reports whether p leaves text unstyled.
func IsPlain(p Painter) bool {
	_, ok := p.(plain)
	return ok
}

// Duration formats a duration compactly: <1ms, 42ms, 3s, 1.8s, 1m, 4m12s, 2h, 2h05m, 3d.
func Duration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0ms"
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	case d < time.Minute && d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	case d < time.Hour && int(d.Seconds())%60 == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 72*time.Hour && int(d.Minutes())%60 == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 72*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// Count formats n with thousands separators.
func Count(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Size formats a byte count: 512 B, 48 MB, 1.2 GB.
func Size(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	suffix := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	if v >= 10 {
		return strconv.FormatFloat(v, 'f', 0, 64) + " " + suffix
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + suffix
}

// Time formats an instant as local minutes, or "" when it is zero.
func Time(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

// Plural returns n with the singular or plural noun.
func Plural(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return Count(int64(n)) + " " + plural
}

// OneLine collapses whitespace so a query fits on one line.
func OneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Wrap breaks unstyled text into lines of at most width cells at spaces, cutting words longer
// than a line.
func Wrap(s string, width int) []string {
	width = max(width, 1)
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		for ansi.StringWidth(word) > width {
			if line != "" {
				lines, line = append(lines, line), ""
			}
			head := ansi.Truncate(word, width, "")
			lines = append(lines, head)
			word = word[len(head):]
		}
		switch {
		case line == "":
			line = word
		case ansi.StringWidth(line)+1+ansi.StringWidth(word) <= width:
			line += " " + word
		default:
			lines, line = append(lines, line), word
		}
	}
	if line != "" || len(lines) == 0 {
		lines = append(lines, line)
	}
	return lines
}
