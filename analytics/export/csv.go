package export

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// CSVOptions configure the CSV writer.
type CSVOptions struct {
	Options
	// Delimiter separates fields; zero is a comma.
	Delimiter rune
	// BOM starts the file with a UTF-8 byte order mark, which Excel needs to read UTF-8.
	BOM bool
	// Raw writes values for a program instead of a person: the header is the column IDs,
	// values are exact (decimals keep every digit, instants are RFC 3339 in UTC), and only
	// detail rows are written, with no totals, labels or notice.
	Raw bool
}

// CSV writes the result as CSV. A field is quoted when it holds the delimiter, a quote or a
// line break. A field that a spreadsheet would read as a formula, one starting with =, +, @, a
// tab or a carriage return, or with - followed by anything but a number, is prefixed with a
// single quote, so opening the file cannot run anything a user typed into the data.
func CSV(ctx context.Context, w io.Writer, r table.Reader, opts CSVOptions) (Stats, error) {
	delim := opts.Delimiter
	if delim == 0 {
		delim = ','
	}
	if delim == '"' || delim == '\r' || delim == '\n' || !utf8.ValidRune(delim) || delim == utf8.RuneError {
		return Stats{}, fmt.Errorf("export: invalid CSV delimiter %q", delim)
	}
	cw := &csvWriter{opts: opts, text: opts.Text.withDefaults(), cols: r.Columns(),
		delim: utf8.AppendRune(nil, delim)}
	cw.count = &counter{w: w}
	cw.out = bufio.NewWriterSize(cw.count, 64<<10)
	if !opts.Raw {
		cw.fmts = opts.formatters(cw.cols)
		cw.cells = make([]format.Cells, len(cw.cols))
	}
	err := cw.run(ctx, r)
	return Stats{Rows: cw.rows, Bytes: cw.count.n, Truncated: cw.truncated}, err
}

// csvWriter is one CSV call: the options, the formatters and the line being written.
type csvWriter struct {
	opts      CSVOptions
	text      Text
	cols      []table.Column
	fmts      []*format.Formatter
	cells     []format.Cells
	delim     []byte
	count     *counter
	out       *bufio.Writer
	line      []byte
	cell      []byte
	total     []byte
	rows      int64
	truncated Truncation
}

// run writes the header, every batch, then the total and the notice.
func (cw *csvWriter) run(ctx context.Context, r table.Reader) error {
	if cw.opts.BOM {
		cw.line = append(cw.line, 0xEF, 0xBB, 0xBF)
	}
	for i, c := range cw.cols {
		head := c.Label
		if cw.opts.Raw {
			head = c.ID
		}
		cw.field(i, []byte(head), fieldText)
	}
	if err := cw.endLine(); err != nil {
		return err
	}
	for {
		b, err := r.Next(ctx)
		if errors.Is(err, io.EOF) {
			if r.Truncated() {
				cw.truncated = TruncatedRows
			}
			break
		}
		if err != nil {
			return err
		}
		if stop, err := cw.batch(b); err != nil || stop {
			if err != nil {
				return err
			}
			cw.truncated = TruncatedBytes
			break
		}
	}
	return cw.finish()
}

// batch writes one batch and reports whether the byte limit stopped it.
func (cw *csvWriter) batch(b *table.Batch) (bool, error) {
	if !cw.opts.Raw {
		for c := range cw.cols {
			cw.cells[c].Reset()
			cw.fmts[c].Column(&cw.cells[c], &b.Columns[c], b.Len)
		}
	}
	for row := range b.Len {
		level := b.Level(row)
		if cw.opts.Raw && level != table.Detail {
			continue
		}
		if cw.opts.Raw {
			cw.rawRow(b, row)
		} else {
			cw.formattedRow(b, row, level)
		}
		if level == table.Total {
			cw.total = append(cw.total[:0], cw.line...)
			cw.line = cw.line[:0]
			continue
		}
		if level == table.Detail {
			cw.rows++
		}
		if err := cw.endLine(); err != nil {
			return false, err
		}
		if cw.opts.over(cw.count.n + int64(cw.out.Buffered())) {
			return true, nil
		}
	}
	return false, nil
}

// rawRow writes a row's exact values.
func (cw *csvWriter) rawRow(b *table.Batch, row int) {
	for c, col := range cw.cols {
		v := &b.Columns[c]
		cw.cell = cw.cell[:0]
		if !v.Null(row) {
			cw.cell = appendRawText(cw.cell, col.Type, v, row)
		}
		kind := fieldText
		if numberLike(col.Type) {
			kind = fieldExact
		}
		cw.field(c, cw.cell, kind)
	}
}

// formattedRow writes a row's rendered cells, with the label of a subtotal or total row.
func (cw *csvWriter) formattedRow(b *table.Batch, row int, level table.Level) {
	label := -1
	if level != table.Detail {
		label = labelColumn(b, row)
	}
	for c, col := range cw.cols {
		text := cw.cells[c].Bytes(row)
		switch {
		case c == label && level == table.Total:
			text = []byte(cw.text.Total)
		case c == label:
			text = []byte(cw.text.Subtotal)
		}
		kind := fieldText
		if numberLike(col.Type) && !b.Columns[c].Null(row) {
			kind = fieldNumber
		}
		cw.field(c, text, kind)
	}
}

// finish writes the held total row and the truncation notice, then flushes.
func (cw *csvWriter) finish() error {
	if !cw.opts.Raw && len(cw.total) > 0 {
		cw.line = append(cw.line, cw.total...)
		if err := cw.endLine(); err != nil {
			return err
		}
	}
	if !cw.opts.Raw && cw.truncated != NotTruncated {
		cw.field(0, []byte(cw.text.notice(cw.truncated)), fieldText)
		for c := 1; c < len(cw.cols); c++ {
			cw.field(c, nil, fieldText)
		}
		if err := cw.endLine(); err != nil {
			return err
		}
	}
	return cw.out.Flush()
}

// endLine ends the line and writes it.
func (cw *csvWriter) endLine() error {
	cw.line = append(cw.line, '\n')
	_, err := cw.out.Write(cw.line)
	cw.line = cw.line[:0]
	return err
}

// fieldKind is where a field's text comes from, which decides how it is escaped.
type fieldKind uint8

const (
	// fieldText is text a user or author wrote, escaped against formula injection.
	fieldText fieldKind = iota
	// fieldNumber is a rendered number, whose leading minus is a sign.
	fieldNumber
	// fieldExact is a number's exact text, which cannot be a formula.
	fieldExact
)

// field appends one field to the line, escaped against formula injection and quoted as needed.
func (cw *csvWriter) field(i int, text []byte, kind fieldKind) {
	if i > 0 {
		cw.line = append(cw.line, cw.delim...)
	}
	escape := kind != fieldExact && formulaRisk(text, kind == fieldNumber)
	if !cw.needsQuotes(text) {
		if escape {
			cw.line = append(cw.line, '\'')
		}
		cw.line = append(cw.line, text...)
		return
	}
	cw.line = append(cw.line, '"')
	if escape {
		cw.line = append(cw.line, '\'')
	}
	for _, c := range text {
		if c == '"' {
			cw.line = append(cw.line, '"')
		}
		cw.line = append(cw.line, c)
	}
	cw.line = append(cw.line, '"')
}

// needsQuotes reports whether a field must be quoted: it holds the delimiter, a quote or a line
// break, or starts with a space or tab.
func (cw *csvWriter) needsQuotes(text []byte) bool {
	if len(text) == 0 {
		return false
	}
	if text[0] == ' ' || text[0] == '\t' {
		return true
	}
	for i, c := range text {
		if c == '"' || c == '\r' || c == '\n' || (c == cw.delim[0] && bytes.HasPrefix(text[i:], cw.delim)) {
			return true
		}
	}
	return false
}

// formulaRisk reports whether a spreadsheet would read the field as a formula. A lone minus is
// text, and a minus that starts a rendered number (digits, separators, a percent or currency
// sign) is a sign; anything else after it is escaped.
func formulaRisk(text []byte, number bool) bool {
	if len(text) == 0 {
		return false
	}
	switch text[0] {
	case '=', '+', '@', '\t', '\r':
		return true
	case '-':
		return len(text) > 1 && !(number && plainNumber(text[1:]))
	}
	return false
}

// plainNumber reports whether the text after a minus is a rendered number.
func plainNumber(text []byte) bool {
	digit := false
	for len(text) > 0 {
		r, size := utf8.DecodeRune(text)
		text = text[size:]
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r == '.', r == ',', r == ' ', r == '%', r == '\u00a0', r == '\u202f', r == '’', r == '\'':
		case unicode.Is(unicode.Sc, r):
		default:
			return false
		}
	}
	return digit
}

// appendRawText appends the exact text of a non-null value.
func appendRawText(dst []byte, t value.Type, v *value.Vector, i int) []byte {
	switch t {
	case value.Int, value.Epoch:
		return strconv.AppendInt(dst, v.Int64[i], 10)
	case value.Decimal:
		return value.AppendNumeric(dst, v.Numeric[i])
	case value.Float:
		return appendFloat(dst, v.Float64[i])
	case value.Bool:
		return strconv.AppendBool(dst, v.Bool[i])
	case value.Time:
		return v.Time[i].UTC().AppendFormat(dst, time.RFC3339Nano)
	case value.JSON:
		return append(dst, v.JSON[i]...)
	}
	return append(dst, v.String[i]...)
}

// appendFloat appends a float's shortest decimal form, in exponent notation only when it is
// very large or very small, or NaN, Infinity or -Infinity.
func appendFloat(dst []byte, f float64) []byte {
	if special, ok := floatSpecial(f); ok {
		return append(dst, special...)
	}
	if a := math.Abs(f); a != 0 && (a < 1e-6 || a >= 1e21) {
		return strconv.AppendFloat(dst, f, 'g', -1, 64)
	}
	return strconv.AppendFloat(dst, f, 'f', -1, 64)
}

// floatSpecial returns the text of a float that is not finite.
func floatSpecial(f float64) (string, bool) {
	switch {
	case math.IsNaN(f):
		return "NaN", true
	case math.IsInf(f, 1):
		return "Infinity", true
	case math.IsInf(f, -1):
		return "-Infinity", true
	}
	return "", false
}
