package render

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Column describes one table column. Min is the narrowest it shrinks to, Max caps its natural
// width (0 is no cap), Flex columns take the shrinking first, and when the table cannot fit, the
// column with the highest Drop is removed first (0 is never removed).
type Column struct {
	Title string
	Min   int
	Max   int
	Right bool
	Flex  bool
	Drop  int
}

// Cell is one table cell. Text is unstyled; the table styles it by Tone after fitting it.
type Cell struct {
	Text string
	Tone Tone
}

// C makes a cell.
func C(text string, tone Tone) Cell { return Cell{Text: text, Tone: tone} }

// Table lays out rows in columns that fit Width; a zero Width never truncates.
type Table struct {
	Columns  []Column
	Rows     [][]Cell
	Width    int
	NoHeader bool
	Indent   int
}

// Lines renders the table, one string per line, without trailing spaces.
func (t Table) Lines(p Painter) []string {
	widths := t.fit()
	pad := strings.Repeat(" ", t.Indent)
	var out []string
	if !t.NoHeader {
		header := make([]Cell, len(t.Columns))
		for i, c := range t.Columns {
			header[i] = Cell{Text: c.Title, Tone: Header}
		}
		out = append(out, pad+t.line(header, widths, p))
	}
	for _, r := range t.Rows {
		out = append(out, pad+t.line(r, widths, p))
	}
	if t.Width > 0 {
		for i, l := range out {
			out[i] = Fit(l, t.Width)
		}
	}
	return out
}

// String renders the table with a newline after each line.
func (t Table) String(p Painter) string {
	lines := t.Lines(p)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func (t Table) line(cells []Cell, widths []int, p Painter) string {
	var b strings.Builder
	last := -1
	for i, w := range widths {
		if w > 0 {
			last = i
		}
	}
	first := true
	for i, w := range widths {
		if w == 0 {
			continue
		}
		var c Cell
		if i < len(cells) {
			c = cells[i]
		}
		text := Fit(c.Text, w)
		gap := w - ansi.StringWidth(text)
		if !first {
			b.WriteString("  ")
		}
		first = false
		if t.Columns[i].Right {
			b.WriteString(strings.Repeat(" ", gap))
			b.WriteString(paint(p, c.Tone, text))
			continue
		}
		b.WriteString(paint(p, c.Tone, text))
		if i != last {
			b.WriteString(strings.Repeat(" ", gap))
		}
	}
	return strings.TrimRight(b.String(), " ")
}

func paint(p Painter, t Tone, s string) string {
	if s == "" {
		return s
	}
	return p.Paint(t, s)
}

func (t Table) natural() []int {
	widths := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		if !t.NoHeader {
			widths[i] = ansi.StringWidth(c.Title)
		}
		for _, r := range t.Rows {
			if i < len(r) {
				widths[i] = max(widths[i], ansi.StringWidth(r[i].Text))
			}
		}
		if c.Max > 0 && t.Width > 0 {
			widths[i] = min(widths[i], c.Max)
		}
	}
	return widths
}

func (t Table) fit() []int {
	widths := t.natural()
	if t.Width <= 0 {
		return widths
	}
	avail := t.Width - t.Indent
	optional := func(c Column) bool { return c.Flex && c.Drop > 0 }
	required := func(c Column) bool { return c.Flex && c.Drop == 0 }
	fixed := func(c Column) bool { return !c.Flex }
	for total(widths) > avail {
		if !t.shrink(widths, avail, optional) && !t.drop(widths) && !t.shrink(widths, avail, required) &&
			!t.shrink(widths, avail, fixed) {
			break
		}
	}
	return widths
}

func (t Table) shrink(widths []int, avail int, eligible func(Column) bool) bool {
	over := total(widths) - avail
	for i := len(t.Columns) - 1; i >= 0; i-- {
		c := t.Columns[i]
		if widths[i] == 0 || !eligible(c) {
			continue
		}
		if room := widths[i] - max(c.Min, 1); room > 0 {
			widths[i] -= min(room, over)
			return true
		}
	}
	return false
}

func (t Table) drop(widths []int) bool {
	best := -1
	for i, c := range t.Columns {
		if widths[i] > 0 && c.Drop > 0 && (best < 0 || c.Drop > t.Columns[best].Drop) {
			best = i
		}
	}
	if best < 0 {
		return false
	}
	widths[best] = 0
	return true
}

func total(widths []int) int {
	sum, n := 0, 0
	for _, w := range widths {
		if w > 0 {
			sum += w
			n++
		}
	}
	if n > 1 {
		sum += 2 * (n - 1)
	}
	return sum
}

// Fit truncates s to width cells with an ellipsis.
func Fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}
