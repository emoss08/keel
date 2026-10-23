package console

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/emoss08/keel/keelcli/internal/render"
)

type cursor struct {
	pos    int
	offset int
}

func (c cursor) move(delta, n int) cursor {
	c.pos = clamp(c.pos+delta, 0, n-1)
	return c
}

func (c cursor) to(pos, n int) cursor {
	c.pos = clamp(pos, 0, n-1)
	return c
}

func (c cursor) visible(height, n int) cursor {
	if height <= 0 {
		return c
	}
	c.pos = clamp(c.pos, 0, n-1)
	if c.pos < c.offset {
		c.offset = c.pos
	}
	if c.pos >= c.offset+height {
		c.offset = c.pos - height + 1
	}
	c.offset = clamp(c.offset, 0, max(n-height, 0))
	return c
}

const gutter = 2

func (m Model) rows(styled, plain []string, pos int) []string {
	out := make([]string, len(styled))
	for i := range styled {
		if i == pos {
			out[i] = m.theme.Accent.Render("▌ ") + m.theme.Selected.Width(m.width-gutter).Render(render.Fit(plain[i], m.width-gutter))
			continue
		}
		out[i] = "  " + styled[i]
	}
	return out
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return max(lo, min(v, hi))
}

func window(lines []string, offset, height int) []string {
	if height <= 0 {
		return nil
	}
	offset = clamp(offset, 0, max(len(lines)-height, 0))
	end := min(offset+height, len(lines))
	out := append([]string(nil), lines[offset:end]...)
	for len(out) < height {
		out = append(out, "")
	}
	return out
}

func fitLines(lines []string, width int) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if ansi.StringWidth(l) > width {
			l = ansi.Truncate(l, width, "…")
		}
		out[i] = l
	}
	return out
}

func block(lines []string, width, height int) string {
	return strings.Join(window(fitLines(lines, width), 0, height), "\n")
}
