package export

import (
	"archive/zip"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// maxSheetRows is the number of data rows a sheet holds below its header: Excel's 1,048,576
// rows less one.
var maxSheetRows = 1_048_575

// The XLSX writer's fixed sizes.
const (
	widthRows     = 100
	flushBytes    = 64 << 10
	checkEvery    = 512
	maxCellRunes  = 32767
	minColWidth   = 8
	maxColWidth   = 60
	maxSheetRunes = 31
)

const (
	xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	nsMain    = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsRel     = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsPkgRel  = "http://schemas.openxmlformats.org/package/2006/relationships"
)

// XLSX writes the result as an Excel workbook, streaming: rows are written into the sheet's zip
// entry as they arrive, so memory does not grow with the result.
//
// Numbers, currencies and percentages are native numbers carrying the column's number format,
// dates are native dates in the requested time zone, and booleans shown as True/False (or raw)
// are native booleans; every other display (durations, compact notation, bands, quarters,
// other boolean styles, text) is written as the rendered text. Header, subtotal and total rows
// are bold, tones are cell fills, column widths are estimated from the header and the first
// 100 rows, and a result longer than a sheet continues on "Data (2)", "Data (3)", …. When Meta
// is set, a first sheet lists the title, description, run time, requester and the parameters
// in their declared order.
func XLSX(ctx context.Context, w io.Writer, r table.Reader, opts Options) (Stats, error) {
	x := newXLSX(w, r.Columns(), opts)
	err := x.run(ctx, r)
	return Stats{Rows: x.rows, Bytes: x.count.n, Truncated: x.truncated}, err
}

// cellKind is how a column's cells are written.
type cellKind uint8

const (
	kindText cellKind = iota
	kindNumber
	kindDate
	kindBool
)

// xcell is one cell of a row being written: its type ('n', 'b', 's', or 0 for none), its
// style and its value's bytes in the row's data.
type xcell struct {
	typ        byte
	style      int
	start, end int
}

// xrow is a row being written.
type xrow struct {
	cells []xcell
	data  []byte
}

// reset empties the row to n absent cells.
func (r *xrow) reset(n int) {
	r.cells = r.cells[:0]
	r.data = r.data[:0]
	for range n {
		r.cells = append(r.cells, xcell{})
	}
}

// set sets cell c, copying its value.
func (r *xrow) set(c int, typ byte, style int, value []byte) {
	start := len(r.data)
	r.data = append(r.data, value...)
	r.cells[c] = xcell{typ: typ, style: style, start: start, end: len(r.data)}
}

// xlsxWriter is one XLSX call: the zip, the styles, the current sheet and the row being built.
type xlsxWriter struct {
	opts      Options
	text      Text
	loc       *time.Location
	cols      []table.Column
	fmts      []*format.Formatter
	kinds     []cellKind
	toned     []bool
	styles    *styleSheet
	refs      []string
	count     *counter
	zw        *zip.Writer
	fw        *flate.Writer
	entry     io.Writer
	modified  time.Time
	sheets    []string
	dataStart int
	sheetRow  int
	buf       []byte
	widths    []int
	sized     bool
	cells     []format.Cells
	scratch   []byte
	rowNum    []byte
	row       xrow
	total     *xrow
	rows      int64
	unchecked int
	truncated Truncation
}

// newXLSX prepares a workbook for the columns: formatters, cell kinds, styles and the zip writer,
// which reuses one compressor for every part.
func newXLSX(w io.Writer, cols []table.Column, opts Options) *xlsxWriter {
	x := &xlsxWriter{opts: opts, text: opts.Text.withDefaults(), loc: opts.location(), cols: cols,
		fmts: opts.formatters(cols), count: &counter{w: w}, modified: opts.Meta.GeneratedAt,
		cells: make([]format.Cells, len(cols)), widths: make([]int, len(cols))}
	if x.modified.IsZero() {
		x.modified = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	codes := make([]string, len(cols))
	native := make([]bool, len(cols))
	for c, col := range cols {
		code, ok := x.fmts[c].Excel()
		codes[c], native[c] = code, ok
		x.kinds = append(x.kinds, kindOf(col, ok))
		x.toned = append(x.toned, len(col.Display.Rules) > 0)
		x.widths[c] = utf8.RuneCountInString(col.Label)
	}
	for c := range max(len(cols), 1) {
		x.refs = append(x.refs, columnRef(c))
	}
	x.styles = newStyleSheet(codes, native)
	x.zw = zip.NewWriter(x.count)
	x.zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		if x.fw != nil {
			x.fw.Reset(out)
			return x.fw, nil
		}
		fw, err := flate.NewWriter(out, flate.BestSpeed)
		x.fw = fw
		return fw, err
	})
	return x
}

// kindOf decides how a column's cells are written.
func kindOf(col table.Column, native bool) cellKind {
	d := col.Display
	switch {
	case col.Type == value.Bool && (d.Style == format.StyleAuto ||
		(d.Style == format.StyleBool && d.BoolStyle == format.BoolStyleTrueFalse)):
		return kindBool
	case native && d.Style == format.StyleDate:
		return kindDate
	case native:
		return kindNumber
	}
	return kindText
}

// columnRef returns the letters of column c: A … Z, AA, ….
func columnRef(c int) string {
	var b []byte
	for c++; c > 0; c = (c - 1) / 26 {
		b = append([]byte{byte('A' + (c-1)%26)}, b...)
	}
	return string(b)
}

// run writes the fixed parts, the info sheet, every batch and the closing parts.
func (x *xlsxWriter) run(ctx context.Context, r table.Reader) error {
	if err := x.writeStatic(); err != nil {
		return err
	}
	if err := x.writeInfo(); err != nil {
		return err
	}
	x.dataStart = len(x.sheets)
	if err := x.startSheet(); err != nil {
		return err
	}
	for {
		b, err := r.Next(ctx)
		if errors.Is(err, io.EOF) {
			if r.Truncated() {
				x.truncated = TruncatedRows
			}
			break
		}
		if err != nil {
			return err
		}
		stop, err := x.batch(b)
		if err != nil {
			return err
		}
		if stop {
			x.truncated = TruncatedBytes
			break
		}
	}
	return x.finish()
}

// create starts a zip entry.
func (x *xlsxWriter) create(name string) error {
	w, err := x.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: x.modified})
	x.entry = w
	return err
}

// part writes a whole zip entry.
func (x *xlsxWriter) part(name string, body []byte) error {
	if err := x.create(name); err != nil {
		return err
	}
	_, err := x.entry.Write(body)
	return err
}

// writeStatic writes the parts known before any row: the content types (every .xml part not
// listed is a worksheet, so the sheets need no entry of their own), the package relationships,
// the core properties and the styles.
func (x *xlsxWriter) writeStatic() error {
	parts := []struct {
		name string
		body []byte
	}{
		{"[Content_Types].xml", []byte(xmlHeader + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
			`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>` +
			`</Types>`)},
		{"_rels/.rels", []byte(xmlHeader + `<Relationships xmlns="` + nsPkgRel + `">` +
			`<Relationship Id="rId1" Type="` + nsRel + `/officeDocument" Target="xl/workbook.xml"/>` +
			`<Relationship Id="rId2" Type="` + nsPkgRel + `/metadata/core-properties" Target="docProps/core.xml"/>` +
			`</Relationships>`)},
		{"docProps/core.xml", x.coreXML()},
		{"xl/styles.xml", x.styles.xml()},
	}
	for _, p := range parts {
		if err := x.part(p.name, p.body); err != nil {
			return err
		}
	}
	return nil
}

// coreXML returns docProps/core.xml: the title, creator and creation time.
func (x *xlsxWriter) coreXML() []byte {
	b := []byte(xmlHeader + `<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" ` +
		`xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><dc:title>`)
	b = appendEscaped(b, []byte(x.opts.Meta.Title), false)
	b = append(b, `</dc:title><dc:creator>`...)
	b = appendEscaped(b, []byte(x.opts.Meta.RequestedBy), false)
	b = append(b, `</dc:creator><dcterms:created xsi:type="dcterms:W3CDTF">`...)
	b = x.modified.UTC().AppendFormat(b, "2006-01-02T15:04:05Z")
	return append(b, `</dcterms:created></cp:coreProperties>`...)
}

// writeInfo writes the sheet describing the run, when there is anything to describe.
func (x *xlsxWriter) writeInfo() error {
	m := x.opts.Meta
	if m.Title == "" && m.Description == "" && m.GeneratedAt.IsZero() && m.RequestedBy == "" && len(m.Params) == 0 {
		return nil
	}
	rows := [][2]string{{x.text.Report, m.Title}}
	if m.Description != "" {
		rows = append(rows, [2]string{x.text.Description, m.Description})
	}
	if !m.GeneratedAt.IsZero() {
		rows = append(rows, [2]string{x.text.GeneratedAt, m.GeneratedAt.In(x.loc).Format("2006-01-02 15:04:05 MST")})
	}
	if m.RequestedBy != "" {
		rows = append(rows, [2]string{x.text.RequestedBy, m.RequestedBy})
	}
	for _, p := range m.Params {
		rows = append(rows, [2]string{x.text.Parameter + ": " + p.Name, paramText(p.Value, x.loc)})
	}
	b := []byte(xmlHeader + `<worksheet xmlns="` + nsMain + `" xmlns:r="` + nsRel + `">` +
		`<cols><col min="1" max="1" width="24" customWidth="1"/><col min="2" max="2" width="60" customWidth="1"/></cols><sheetData>`)
	for i, row := range rows {
		n := strconv.Itoa(i + 1)
		b = append(b, `<row r="`+n+`">`...)
		b = appendInlineCell(b, "A"+n, x.styles.header, []byte(row[0]))
		b = appendInlineCell(b, "B"+n, 0, []byte(row[1]))
		b = append(b, `</row>`...)
	}
	b = append(b, `</sheetData></worksheet>`...)
	x.sheets = append(x.sheets, sheetName(x.text.InfoSheet, ""))
	return x.part(sheetPath(len(x.sheets)), b)
}

// paramText writes a parameter value for the info sheet.
func paramText(v any, loc *time.Location) string {
	switch v := v.(type) {
	case nil:
		return ""
	case time.Time:
		return v.In(loc).Format("2006-01-02 15:04:05 MST")
	case string:
		return v
	}
	return fmt.Sprint(v)
}

// sheetPath returns the part name of sheet n.
func sheetPath(n int) string { return "xl/worksheets/sheet" + strconv.Itoa(n) + ".xml" }

// sheetName makes a valid sheet name: without the characters Excel forbids and at most 31
// characters including the suffix.
func sheetName(base, suffix string) string {
	base = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return -1
		}
		return r
	}, base)
	if base == "" {
		base = "Sheet"
	}
	room := maxSheetRunes - utf8.RuneCountInString(suffix)
	if utf8.RuneCountInString(base) > room {
		base = string([]rune(base)[:room])
	}
	return base + suffix
}

// startSheet opens the next data sheet. The first sheet's head waits until the column widths
// are settled; later sheets reuse them.
func (x *xlsxWriter) startSheet() error {
	suffix := ""
	if n := len(x.sheets) - x.dataStart; n > 0 {
		suffix = " (" + strconv.Itoa(n+1) + ")"
	}
	x.sheets = append(x.sheets, sheetName(x.text.DataSheet, suffix))
	if err := x.create(sheetPath(len(x.sheets))); err != nil {
		return err
	}
	x.sheetRow = 1
	if x.sized {
		x.buf = x.sheetHead(x.buf[:0])
	}
	return nil
}

// sheetHead appends a data sheet's opening: the frozen header pane, the column widths and the
// header row.
func (x *xlsxWriter) sheetHead(b []byte) []byte {
	b = append(b, xmlHeader+`<worksheet xmlns="`+nsMain+`" xmlns:r="`+nsRel+`"><sheetViews><sheetView`...)
	if len(x.sheets)-1 == x.dataStart {
		b = append(b, ` tabSelected="1"`...)
	}
	b = append(b, ` workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>`+
		`</sheetView></sheetViews><sheetFormatPr defaultRowHeight="15"/>`...)
	if len(x.cols) > 0 {
		b = append(b, `<cols>`...)
		for c, w := range x.widths {
			n := strconv.Itoa(c + 1)
			b = append(b, `<col min="`+n+`" max="`+n+`" width="`...)
			b = strconv.AppendInt(b, int64(min(max(w+2, minColWidth), maxColWidth)), 10)
			b = append(b, `" customWidth="1"/>`...)
		}
		b = append(b, `</cols>`...)
	}
	b = append(b, `<sheetData><row r="1">`...)
	for c, col := range x.cols {
		b = appendInlineCell(b, x.refs[c]+"1", x.styles.header, []byte(col.Label))
	}
	return append(b, `</row>`...)
}

// settle fixes the column widths and writes the first sheet's head before the rows it held.
func (x *xlsxWriter) settle() error {
	x.sized = true
	rows := x.buf
	x.buf = x.sheetHead(make([]byte, 0, len(rows)+4096))
	x.buf = append(x.buf, rows...)
	return x.flush()
}

// flush writes the pending sheet XML into the entry, once the widths are settled.
func (x *xlsxWriter) flush() error {
	if !x.sized {
		return nil
	}
	_, err := x.entry.Write(x.buf)
	x.buf = x.buf[:0]
	return err
}

// endSheet closes the current sheet.
func (x *xlsxWriter) endSheet() error {
	if !x.sized {
		if err := x.settle(); err != nil {
			return err
		}
	}
	x.buf = append(x.buf, `</sheetData></worksheet>`...)
	return x.flush()
}

// batch writes one batch and reports whether the byte limit stopped it.
func (x *xlsxWriter) batch(b *table.Batch) (bool, error) {
	for c, k := range x.kinds {
		if k == kindText {
			x.cells[c].Reset()
			x.fmts[c].Column(&x.cells[c], &b.Columns[c], b.Len)
		}
	}
	for row := range b.Len {
		level := b.Level(row)
		x.buildRow(b, row, level)
		if level == table.Total {
			x.total = &xrow{cells: append([]xcell(nil), x.row.cells...), data: append([]byte(nil), x.row.data...)}
			continue
		}
		if level == table.Detail {
			x.rows++
		}
		if err := x.emit(&x.row); err != nil {
			return false, err
		}
		stop, err := x.checkBytes()
		if err != nil || stop {
			return stop, err
		}
	}
	return false, nil
}

// checkBytes compares the compressed output with MaxBytes every few hundred rows, flushing the
// compressor so the count is current.
func (x *xlsxWriter) checkBytes() (bool, error) {
	if x.opts.MaxBytes <= 0 || !x.sized {
		return false, nil
	}
	if x.unchecked++; x.unchecked < checkEvery {
		return false, nil
	}
	x.unchecked = 0
	if err := x.flush(); err != nil {
		return false, err
	}
	if err := x.fw.Flush(); err != nil {
		return false, err
	}
	if err := x.zw.Flush(); err != nil {
		return false, err
	}
	return x.opts.over(x.count.n), nil
}

// buildRow builds row i of b: labels for a summary row, the null text for a null cell, and every
// other cell from its value.
func (x *xlsxWriter) buildRow(b *table.Batch, row int, level table.Level) {
	x.row.reset(len(x.cols))
	bold := level != table.Detail
	label := -1
	if bold {
		label = labelColumn(b, row)
	}
	for c := range x.cols {
		v := &b.Columns[c]
		switch {
		case c == label && level == table.Total:
			x.row.set(c, 's', x.styles.style(c, true, format.ToneNone), []byte(x.text.Total))
		case c == label:
			x.row.set(c, 's', x.styles.style(c, true, format.ToneNone), []byte(x.text.Subtotal))
		case v.Null(row):
			if nt := x.cols[c].Display.NullText; nt != "" {
				x.row.set(c, 's', x.styles.style(c, bold, format.ToneNone), []byte(nt))
			}
		default:
			x.cell(c, v, row, bold)
		}
		if !x.sized && level == table.Detail {
			x.measure(c, v, row)
		}
	}
}

// cell sets column c of the row from row i of v.
func (x *xlsxWriter) cell(c int, v *value.Vector, i int, bold bool) {
	tone := format.ToneNone
	if x.kinds[c] == kindText {
		x.row.set(c, 's', x.styles.style(c, bold, x.cells[c].Tone(i)), x.cells[c].Bytes(i))
		return
	}
	if x.toned[c] {
		tone = x.fmts[c].Tone(v, i)
	}
	style := x.styles.style(c, bold, tone)
	x.scratch = x.scratch[:0]
	switch x.kinds[c] {
	case kindBool:
		b := byte('0')
		if v.Bool[i] {
			b = '1'
		}
		x.row.set(c, 'b', style, []byte{b})
	case kindDate:
		if sec, ok := x.seconds(c, v, i); ok {
			x.scratch = appendSerial(x.scratch, sec, x.loc)
			x.row.set(c, 'n', style, x.scratch)
			return
		}
		x.textCell(c, v, i, style)
	default:
		if num, ok := x.number(c, v, i); ok {
			x.scratch = num
			x.row.set(c, 'n', style, num)
			return
		}
		x.textCell(c, v, i, style)
	}
}

// textCell sets a cell to the rendered text of row i.
func (x *xlsxWriter) textCell(c int, v *value.Vector, i int, style int) {
	x.scratch, _ = x.fmts[c].Append(x.scratch[:0], v, i)
	x.row.set(c, 's', style, x.scratch)
}

// number returns the native number text of row i, or false for a value Excel cannot hold.
func (x *xlsxWriter) number(c int, v *value.Vector, i int) ([]byte, bool) {
	switch x.cols[c].Type {
	case value.Int, value.Epoch:
		return strconv.AppendInt(x.scratch, v.Int64[i], 10), true
	case value.Decimal:
		if !value.NumericFinite(v.Numeric[i]) {
			return nil, false
		}
		return value.AppendNumeric(x.scratch, v.Numeric[i]), true
	case value.Float:
		if _, special := floatSpecial(v.Float64[i]); special {
			return nil, false
		}
		return appendFloat(x.scratch, v.Float64[i]), true
	}
	return nil, false
}

// seconds returns the instant of row i as seconds since the Unix epoch.
func (x *xlsxWriter) seconds(c int, v *value.Vector, i int) (int64, bool) {
	switch x.cols[c].Type {
	case value.Int, value.Epoch:
		return v.Int64[i], true
	case value.Time:
		return v.Time[i].Unix(), true
	case value.Float:
		f := v.Float64[i]
		if _, special := floatSpecial(f); special || f > 1e15 || f < -1e15 {
			return 0, false
		}
		return int64(f), true
	case value.Decimal:
		return numericSeconds(v.Numeric[i])
	}
	return 0, false
}

// numericSeconds returns a decimal number of seconds truncated to whole seconds.
func numericSeconds(n pgtype.Numeric) (int64, bool) {
	if !value.NumericFinite(n) {
		return 0, false
	}
	text := value.AppendNumeric(nil, n)
	whole, _, _ := strings.Cut(string(text), ".")
	sec, err := strconv.ParseInt(whole, 10, 64)
	return sec, err == nil
}

// appendSerial appends an instant as an Excel serial date: days since 1899-12-30 of its wall
// clock in loc, since a spreadsheet has no time zones.
func appendSerial(dst []byte, sec int64, loc *time.Location) []byte {
	t := time.Unix(sec, 0).In(loc)
	wall := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	return strconv.AppendFloat(dst, float64(wall.Unix())/86400+25569, 'f', -1, 64)
}

// measure widens column c to fit the rendering of row i, for the first rows of the result.
func (x *xlsxWriter) measure(c int, v *value.Vector, i int) {
	var n int
	if x.kinds[c] == kindText {
		n = utf8.RuneCount(x.cells[c].Bytes(i))
	} else {
		x.scratch, _ = x.fmts[c].Append(x.scratch[:0], v, i)
		n = utf8.RuneCount(x.scratch)
	}
	x.widths[c] = max(x.widths[c], n)
}

// emit writes a row on the current sheet, starting the next sheet when this one is full.
func (x *xlsxWriter) emit(r *xrow) error {
	if x.sheetRow > maxSheetRows {
		if err := x.endSheet(); err != nil {
			return err
		}
		if err := x.startSheet(); err != nil {
			return err
		}
	}
	x.sheetRow++
	x.rowNum = strconv.AppendInt(x.rowNum[:0], int64(x.sheetRow), 10)
	x.buf = append(x.buf, `<row r="`...)
	x.buf = append(x.buf, x.rowNum...)
	x.buf = append(x.buf, `">`...)
	for c, cell := range r.cells {
		if cell.typ != 0 {
			x.buf = appendCell(x.buf, x.refs[c], x.rowNum, cell, r.data[cell.start:cell.end])
		}
	}
	x.buf = append(x.buf, `</row>`...)
	switch {
	case !x.sized && x.sheetRow > widthRows:
		return x.settle()
	case x.sized && len(x.buf) >= flushBytes:
		return x.flush()
	}
	return nil
}

// appendCell appends a cell element.
func appendCell(b []byte, ref string, row []byte, cell xcell, data []byte) []byte {
	if cell.typ == 's' {
		return appendInline(b, ref, row, cell.style, data)
	}
	b = appendCellOpen(b, ref, row, cell.style)
	if cell.typ == 'b' {
		b = append(b, ` t="b"`...)
	}
	b = append(b, `><v>`...)
	b = append(b, data...)
	return append(b, `</v></c>`...)
}

// appendCellOpen appends a cell's start tag up to its type attribute.
func appendCellOpen(b []byte, ref string, row []byte, style int) []byte {
	b = append(b, `<c r="`...)
	b = append(b, ref...)
	b = append(b, row...)
	b = append(b, '"')
	if style == 0 {
		return b
	}
	b = append(b, ` s="`...)
	b = strconv.AppendInt(b, int64(style), 10)
	return append(b, '"')
}

// appendInlineCell appends an inline-string cell at a full reference.
func appendInlineCell(b []byte, ref string, style int, text []byte) []byte {
	return appendInline(b, ref, nil, style, text)
}

// appendInline appends an inline-string cell, preserving leading and trailing space.
func appendInline(b []byte, ref string, row []byte, style int, text []byte) []byte {
	b = appendCellOpen(b, ref, row, style)
	b = append(b, ` t="inlineStr"><is><t`...)
	if len(text) > 0 && (isSpace(text[0]) || isSpace(text[len(text)-1])) {
		b = append(b, ` xml:space="preserve"`...)
	}
	b = append(b, '>')
	b = appendEscaped(b, text, false)
	return append(b, `</t></is></c>`...)
}

// isSpace reports whether c is XML whitespace.
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// appendEscaped appends text escaped for XML, without the characters XML cannot hold (invalid
// UTF-8 becomes U+FFFD) and cut at Excel's 32,767 characters per cell.
func appendEscaped(b, text []byte, attr bool) []byte {
	for n := 0; len(text) > 0 && n < maxCellRunes; n++ {
		r, size := utf8.DecodeRune(text)
		text = text[size:]
		switch {
		case r == '&':
			b = append(b, "&amp;"...)
		case r == '<':
			b = append(b, "&lt;"...)
		case r == '>':
			b = append(b, "&gt;"...)
		case r == '"' && attr:
			b = append(b, "&quot;"...)
		case r == utf8.RuneError && size == 1:
			b = utf8.AppendRune(b, utf8.RuneError)
		case r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || r >= 0x10000:
			b = utf8.AppendRune(b, r)
		}
	}
	return b
}

// finish writes the held total, the notice, the end of the sheet and the workbook parts.
func (x *xlsxWriter) finish() error {
	if x.total != nil {
		if err := x.emit(x.total); err != nil {
			return err
		}
	}
	if x.truncated != NotTruncated {
		x.row.reset(max(len(x.cols), 1))
		x.row.set(0, 's', 0, []byte(x.text.notice(x.truncated)))
		if err := x.emit(&x.row); err != nil {
			return err
		}
	}
	if err := x.endSheet(); err != nil {
		return err
	}
	if err := x.part("xl/workbook.xml", x.workbookXML()); err != nil {
		return err
	}
	if err := x.part("xl/_rels/workbook.xml.rels", x.workbookRels()); err != nil {
		return err
	}
	return x.zw.Close()
}

// workbookXML returns xl/workbook.xml, with the first data sheet active.
func (x *xlsxWriter) workbookXML() []byte {
	b := []byte(xmlHeader + `<workbook xmlns="` + nsMain + `" xmlns:r="` + nsRel + `"><bookViews><workbookView activeTab="`)
	b = strconv.AppendInt(b, int64(x.dataStart), 10)
	b = append(b, `"/></bookViews><sheets>`...)
	for i, name := range x.sheets {
		n := strconv.Itoa(i + 1)
		b = append(b, `<sheet name="`...)
		b = appendEscaped(b, []byte(name), true)
		b = append(b, `" sheetId="`+n+`" r:id="rId`+n+`"/>`...)
	}
	return append(b, `</sheets></workbook>`...)
}

// workbookRels returns the workbook's relationships to its sheets and styles.
func (x *xlsxWriter) workbookRels() []byte {
	b := []byte(xmlHeader + `<Relationships xmlns="` + nsPkgRel + `">`)
	for i := range x.sheets {
		n := strconv.Itoa(i + 1)
		b = append(b, `<Relationship Id="rId`+n+`" Type="`+nsRel+`/worksheet" Target="worksheets/sheet`+n+`.xml"/>`...)
	}
	n := strconv.Itoa(len(x.sheets) + 1)
	b = append(b, `<Relationship Id="rId`+n+`" Type="`+nsRel+`/styles" Target="styles.xml"/>`...)
	return append(b, `</Relationships>`...)
}
