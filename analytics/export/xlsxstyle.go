package export

import (
	"strconv"

	"github.com/emoss08/keel/analytics/format"
)

// toneColors are the fill and font colours of each tone, light enough to read on Excel's
// white sheet and close to the on-screen palette.
var toneColors = [...]struct {
	tone       format.Tone
	fill, font string
}{
	{format.TonePositive, "FFDCFCE7", "FF166534"},
	{format.ToneWarning, "FFFEF3C7", "FF92400E"},
	{format.ToneNegative, "FFFEE2E2", "FF991B1B"},
	{format.ToneNeutral, "FFF4F4F5", "FF3F3F46"},
}

// toneIndex returns a tone's position in toneColors plus one, or 0 for no tone.
func toneIndex(t format.Tone) int {
	for i, c := range toneColors {
		if c.tone == t {
			return i + 1
		}
	}
	return 0
}

// xf is one cell format: a number format, a font and a fill.
type xf struct {
	numFmt, font, fill int
}

// styleSheet collects the cell formats a workbook uses, derived from its columns' displays
// before any row is written.
type styleSheet struct {
	codes   []string
	codeIDs map[string]int
	xfs     []xf
	xfIDs   map[xf]int
	header  int
	// cell[col][bold][tone] is the cellXfs index of a column's cell.
	cell [][2][len(toneColors) + 1]int
}

// The fonts: regular, bold, then each tone's colour regular and bold.
const (
	fontRegular = 0
	fontBold    = 1
)

// toneFont returns the font of a tone (1-based), regular or bold.
func toneFont(tone int, bold bool) int {
	f := 2 + (tone-1)*2
	if bold {
		f++
	}
	return f
}

// newStyleSheet registers the formats of every column, given each column's number format code
// and whether it is written natively.
func newStyleSheet(codes []string, native []bool) *styleSheet {
	s := &styleSheet{codeIDs: map[string]int{}, xfIDs: map[xf]int{}}
	s.xfID(xf{})
	s.header = s.xfID(xf{font: fontBold})
	s.cell = make([][2][len(toneColors) + 1]int, len(codes))
	for c := range codes {
		numFmt := 0
		if native[c] && codes[c] != "" {
			numFmt = s.numFmt(codes[c])
		}
		for bold := range 2 {
			font := fontRegular + bold
			s.cell[c][bold][0] = s.xfID(xf{numFmt: numFmt, font: font})
			for tone := 1; tone <= len(toneColors); tone++ {
				s.cell[c][bold][tone] = s.xfID(xf{numFmt: numFmt, font: toneFont(tone, bold == 1), fill: tone + 1})
			}
		}
	}
	return s
}

// numFmt returns the ID of a custom number format, registering it on first use.
func (s *styleSheet) numFmt(code string) int {
	if id, ok := s.codeIDs[code]; ok {
		return id
	}
	id := 164 + len(s.codes)
	s.codes = append(s.codes, code)
	s.codeIDs[code] = id
	return id
}

// xfID returns the index of a cell format, registering it on first use.
func (s *styleSheet) xfID(x xf) int {
	if id, ok := s.xfIDs[x]; ok {
		return id
	}
	id := len(s.xfs)
	s.xfs = append(s.xfs, x)
	s.xfIDs[x] = id
	return id
}

// style returns the cellXfs index of a cell of column c.
func (s *styleSheet) style(c int, bold bool, tone format.Tone) int {
	b := 0
	if bold {
		b = 1
	}
	return s.cell[c][b][toneIndex(tone)]
}

// xml writes xl/styles.xml.
func (s *styleSheet) xml() []byte {
	b := []byte(xmlHeader + `<styleSheet xmlns="` + nsMain + `">`)
	if len(s.codes) > 0 {
		b = append(b, `<numFmts count="`...)
		b = strconv.AppendInt(b, int64(len(s.codes)), 10)
		b = append(b, `">`...)
		for i, code := range s.codes {
			b = append(b, `<numFmt numFmtId="`...)
			b = strconv.AppendInt(b, int64(164+i), 10)
			b = append(b, `" formatCode="`...)
			b = appendEscaped(b, []byte(code), true)
			b = append(b, `"/>`...)
		}
		b = append(b, `</numFmts>`...)
	}
	b = append(b, `<fonts count="`...)
	b = strconv.AppendInt(b, int64(2+2*len(toneColors)), 10)
	b = append(b, `"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font>`...)
	for _, c := range toneColors {
		b = append(b, `<font><color rgb="`+c.font+`"/><sz val="11"/><name val="Calibri"/></font>`...)
		b = append(b, `<font><b/><color rgb="`+c.font+`"/><sz val="11"/><name val="Calibri"/></font>`...)
	}
	b = append(b, `</fonts><fills count="`...)
	b = strconv.AppendInt(b, int64(2+len(toneColors)), 10)
	b = append(b, `"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill>`...)
	for _, c := range toneColors {
		b = append(b, `<fill><patternFill patternType="solid"><fgColor rgb="`+c.fill+`"/><bgColor indexed="64"/></patternFill></fill>`...)
	}
	b = append(b, `</fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>`...)
	b = append(b, `<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="`...)
	b = strconv.AppendInt(b, int64(len(s.xfs)), 10)
	b = append(b, `">`...)
	for _, x := range s.xfs {
		b = appendXF(b, x)
	}
	b = append(b, `</cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`...)
	return b
}

// appendXF appends a cellXfs entry.
func appendXF(b []byte, x xf) []byte {
	b = append(b, `<xf numFmtId="`...)
	b = strconv.AppendInt(b, int64(x.numFmt), 10)
	b = append(b, `" fontId="`...)
	b = strconv.AppendInt(b, int64(x.font), 10)
	b = append(b, `" fillId="`...)
	b = strconv.AppendInt(b, int64(x.fill), 10)
	b = append(b, `" borderId="0" xfId="0"`...)
	if x.numFmt != 0 {
		b = append(b, ` applyNumberFormat="1"`...)
	}
	if x.font != 0 {
		b = append(b, ` applyFont="1"`...)
	}
	if x.fill != 0 {
		b = append(b, ` applyFill="1"`...)
	}
	return append(b, `/>`...)
}
