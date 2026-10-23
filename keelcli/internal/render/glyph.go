package render

import (
	"strings"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

// Glyph is a one-cell symbol with its tone.
type Glyph struct {
	Symbol string
	Tone   Tone
}

// The glyphs shared by every view.
var (
	GlyphOK      = Glyph{"✓", OK}
	GlyphFail    = Glyph{"✗", Fail}
	GlyphWarn    = Glyph{"!", Warn}
	GlyphPending = Glyph{"•", Accent}
	GlyphQueued  = Glyph{"·", Faint}
	GlyphRetry   = Glyph{"↻", Warn}
	GlyphStopped = Glyph{"⊘", Muted}
	GlyphSkipped = Glyph{"–", Faint}
	GlyphUnknown = Glyph{"?", Fail}
	GlyphInfo    = Glyph{"i", Info}
	GlyphPartial = Glyph{"◐", Warn}
)

// MigrationGlyph is the glyph of a migration state.
func MigrationGlyph(s migrateport.State) Glyph {
	switch s {
	case migrateport.Pending:
		return GlyphPending
	case migrateport.Changed:
		return GlyphWarn
	case migrateport.RepeatableDue:
		return Glyph{"↻", Info}
	case migrateport.Missing:
		return GlyphUnknown
	default:
		return GlyphOK
	}
}

// SeedGlyph is the glyph of a seed state.
func SeedGlyph(s seedport.State) Glyph {
	switch s {
	case seedport.Due:
		return GlyphPending
	case seedport.Partial:
		return GlyphPartial
	case seedport.Orphaned:
		return Glyph{"?", Muted}
	default:
		return GlyphOK
	}
}

// ReasonTone is the tone of a seed reason: accented when it runs, muted when it does not, failing
// when it is refused.
func ReasonTone(r seedport.Reason) Tone {
	switch {
	case r.Kind == seedport.Refused:
		return Fail
	case r.Runs():
		return Accent
	default:
		return Muted
	}
}

// Reason describes a seed reason with its changed inputs.
func Reason(r seedport.Reason) string {
	if len(r.Inputs) == 0 {
		return r.Kind.String()
	}
	return r.Kind.String() + " (" + strings.Join(r.Inputs, ", ") + ")"
}

// Directives lists a migration's directives as their file lines spell them.
func Directives(d migrateport.Directives) []string {
	var out []string
	if d.NoTx {
		out = append(out, "notx")
	}
	if d.LockTimeout > 0 {
		out = append(out, "lock_timeout "+d.LockTimeout.String())
	}
	if d.StatementTimeout > 0 {
		out = append(out, "statement_timeout "+d.StatementTimeout.String())
	}
	if d.NoRetry {
		out = append(out, "no_retry")
	}
	return out
}

// MigrationDetail is a migration's kind, when it is not a plain versioned file, and directives.
func MigrationDetail(m migrateport.Migration) string {
	var parts []string
	if m.Kind != migrateport.Versioned {
		parts = append(parts, m.Kind.String())
	}
	parts = append(parts, Directives(m.Directives)...)
	return strings.Join(parts, " · ")
}

// StateDetail explains a migration state beyond its name.
func StateDetail(s migrateport.Status) string {
	switch s.State {
	case migrateport.Changed:
		return "checksum differs"
	case migrateport.Missing:
		return "no file in any source"
	case migrateport.RepeatableDue:
		return "checksum changed"
	default:
		return MigrationDetail(s.Migration)
	}
}
