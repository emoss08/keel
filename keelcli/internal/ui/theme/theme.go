// Package theme is keelcli's one definition of colour and style. Every view takes its styles from
// a Theme, built for a dark or a light terminal background, so no view names a colour.
package theme

import (
	"image/color"

	"charm.land/bubbles/v2/help"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/emoss08/keel/keelcli/internal/render"
)

// Palette is the theme's colours.
type Palette struct {
	Text      color.Color
	Muted     color.Color
	Faint     color.Color
	Accent    color.Color
	OK        color.Color
	Warn      color.Color
	Fail      color.Color
	Info      color.Color
	Border    color.Color
	Selection color.Color
	OnAccent  color.Color
	Keyword   color.Color
	String    color.Color
	Number    color.Color
}

// Theme is every style keelcli's views use.
type Theme struct {
	Dark    bool
	Palette Palette

	Text   lipgloss.Style
	Muted  lipgloss.Style
	Faint  lipgloss.Style
	Header lipgloss.Style
	Strong lipgloss.Style
	Accent lipgloss.Style
	OK     lipgloss.Style
	Warn   lipgloss.Style
	Fail   lipgloss.Style
	Info   lipgloss.Style

	Title       lipgloss.Style
	TitleMeta   lipgloss.Style
	Tab         lipgloss.Style
	ActiveTab   lipgloss.Style
	Rule        lipgloss.Style
	Selected    lipgloss.Style
	Panel       lipgloss.Style
	WarnPanel   lipgloss.Style
	PanelTitle  lipgloss.Style
	Modal       lipgloss.Style
	Spinner     lipgloss.Style
	Help        help.Styles
	Production  lipgloss.Style
	Environment lipgloss.Style

	SQLKeyword   lipgloss.Style
	SQLString    lipgloss.Style
	SQLNumber    lipgloss.Style
	SQLComment   lipgloss.Style
	SQLDirective lipgloss.Style
	LineNumber   lipgloss.Style
	LineMark     lipgloss.Style
}

// New builds the theme for a dark or light background.
func New(dark bool) Theme {
	ld := lipgloss.LightDark(dark)
	c := lipgloss.Color
	p := Palette{
		Text:      ld(c("#1F1F24"), c("#E4E4EA")),
		Muted:     ld(c("#55555F"), c("#A3A3B0")),
		Faint:     ld(c("#8C8C99"), c("#6B6B78")),
		Accent:    ld(c("#5B4BD6"), c("#9D94FF")),
		OK:        ld(c("#17803D"), c("#5FD38D")),
		Warn:      ld(c("#9A6100"), c("#F2C14E")),
		Fail:      ld(c("#C42B2B"), c("#FF6E6E")),
		Info:      ld(c("#1D63B8"), c("#6CB6FF")),
		Border:    ld(c("#CFCFDA"), c("#3B3B4A")),
		Selection: ld(c("#E8E5FF"), c("#2F2B52")),
		OnAccent:  ld(c("#FFFFFF"), c("#14121F")),
		Keyword:   ld(c("#7A2FBF"), c("#C9A2FF")),
		String:    ld(c("#2E7A12"), c("#B5E48C")),
		Number:    ld(c("#B4541B"), c("#F7A072")),
	}
	s := lipgloss.NewStyle
	t := Theme{
		Dark:    dark,
		Palette: p,
		Text:    s().Foreground(p.Text),
		Muted:   s().Foreground(p.Muted),
		Faint:   s().Foreground(p.Faint),
		Header:  s().Foreground(p.Muted).Bold(true),
		Strong:  s().Foreground(p.Text).Bold(true),
		Accent:  s().Foreground(p.Accent),
		OK:      s().Foreground(p.OK),
		Warn:    s().Foreground(p.Warn),
		Fail:    s().Foreground(p.Fail),
		Info:    s().Foreground(p.Info),

		Title:       s().Foreground(p.OnAccent).Background(p.Accent).Bold(true).Padding(0, 1),
		TitleMeta:   s().Foreground(p.Muted),
		Tab:         s().Foreground(p.Muted).Padding(0, 1),
		ActiveTab:   s().Foreground(p.Accent).Bold(true).Underline(true).Padding(0, 1),
		Rule:        s().Foreground(p.Border),
		Selected:    s().Background(p.Selection),
		Panel:       s().Border(lipgloss.RoundedBorder()).BorderForeground(p.Border).Padding(0, 1),
		WarnPanel:   s().Border(lipgloss.RoundedBorder()).BorderForeground(p.Warn).Padding(0, 1),
		PanelTitle:  s().Foreground(p.Warn).Bold(true),
		Modal:       s().Border(lipgloss.RoundedBorder()).BorderForeground(p.Accent).Padding(1, 2),
		Spinner:     s().Foreground(p.Accent),
		Production:  s().Foreground(p.OnAccent).Background(p.Fail).Bold(true).Padding(0, 1),
		Environment: s().Foreground(p.OK).Bold(true),

		SQLKeyword:   s().Foreground(p.Keyword).Bold(true),
		SQLString:    s().Foreground(p.String),
		SQLNumber:    s().Foreground(p.Number),
		SQLComment:   s().Foreground(p.Faint).Italic(true),
		SQLDirective: s().Foreground(p.Warn).Bold(true),
		LineNumber:   s().Foreground(p.Faint),
		LineMark:     s().Background(p.Selection),
	}
	t.Help = help.Styles{
		ShortKey:       s().Foreground(p.Accent),
		ShortDesc:      s().Foreground(p.Muted),
		ShortSeparator: s().Foreground(p.Border),
		Ellipsis:       s().Foreground(p.Border),
		FullKey:        s().Foreground(p.Accent),
		FullDesc:       s().Foreground(p.Muted),
		FullSeparator:  s().Foreground(p.Border),
	}
	return t
}

// Paint styles s by tone; Theme is a render.Painter.
func (t Theme) Paint(tone render.Tone, s string) string {
	return t.Tone(tone).Render(s)
}

// Tone is the style of a tone.
func (t Theme) Tone(tone render.Tone) lipgloss.Style {
	switch tone {
	case render.Muted:
		return t.Muted
	case render.Faint:
		return t.Faint
	case render.Header:
		return t.Header
	case render.Strong:
		return t.Strong
	case render.Accent:
		return t.Accent
	case render.OK:
		return t.OK
	case render.Warn:
		return t.Warn
	case render.Fail:
		return t.Fail
	case render.Info:
		return t.Info
	default:
		return t.Text
	}
}

// Glyph renders a glyph in its tone.
func (t Theme) Glyph(g render.Glyph) string {
	return t.Tone(g.Tone).Render(g.Symbol)
}

// Huh is the theme for Huh forms.
func (t Theme) Huh() huh.Theme {
	return huh.ThemeFunc(func(dark bool) *huh.Styles {
		th := t
		if dark != t.Dark {
			th = New(dark)
		}
		return th.huhStyles()
	})
}

func (t Theme) huhStyles() *huh.Styles {
	p := t.Palette
	h := huh.ThemeBase(t.Dark)
	s := lipgloss.NewStyle
	f := &h.Focused
	f.Base = f.Base.BorderForeground(p.Accent)
	f.Card = f.Base
	f.Title = s().Foreground(p.Accent).Bold(true)
	f.NoteTitle = s().Foreground(p.Accent).Bold(true).MarginBottom(1)
	f.Description = s().Foreground(p.Muted)
	f.ErrorIndicator = f.ErrorIndicator.Foreground(p.Fail)
	f.ErrorMessage = f.ErrorMessage.Foreground(p.Fail)
	f.SelectSelector = f.SelectSelector.Foreground(p.Accent)
	f.NextIndicator = f.NextIndicator.Foreground(p.Accent)
	f.PrevIndicator = f.PrevIndicator.Foreground(p.Accent)
	f.Option = s().Foreground(p.Text)
	f.SelectedOption = s().Foreground(p.OK)
	f.SelectedPrefix = s().Foreground(p.OK).SetString("✓ ")
	f.UnselectedPrefix = s().Foreground(p.Faint).SetString("• ")
	f.UnselectedOption = s().Foreground(p.Text)
	f.FocusedButton = f.FocusedButton.Foreground(p.OnAccent).Background(p.Accent).Bold(true)
	f.Next = f.FocusedButton
	f.BlurredButton = f.BlurredButton.Foreground(p.Muted).Background(p.Selection)
	f.TextInput.Cursor = s().Foreground(p.Accent)
	f.TextInput.Placeholder = s().Foreground(p.Faint)
	f.TextInput.Prompt = s().Foreground(p.Accent)
	f.TextInput.Text = s().Foreground(p.Text)
	h.Blurred = h.Focused
	h.Blurred.Base = h.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
	h.Blurred.Card = h.Blurred.Base
	h.Blurred.NextIndicator = s()
	h.Blurred.PrevIndicator = s()
	h.Group.Title = f.Title
	h.Group.Description = f.Description
	h.Help = t.Help
	return h
}
