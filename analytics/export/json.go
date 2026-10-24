package export

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// JSON writes the result as the rows envelope, a document a program reads back (ReadJSON) to
// compare runs or hand them to an agent:
//
//	{"meta": {...}, "schema": [...], "rows": [[...], ...], "levels": [[row, level], ...],
//	 "totals": [...], "summary": {"rowCount": n, "truncated": false}}
//
// Values are raw, never formatted, so what is read back is what was written: decimals are exact
// strings ("2840.125000"), integers and floats are numbers (non-finite floats are the strings
// "NaN", "Infinity" and "-Infinity"), instants are RFC 3339 strings in UTC, epochs are numbers
// of seconds and JSON columns are embedded as they are. Rows include subtotal rows, listed with
// their level in "levels"; the grand total is "totals", absent when the result has none.
// "summary" says how many detail rows were written and whether, and why, the result was cut
// short ("truncatedBy": "rows" or "bytes").
func JSON(ctx context.Context, w io.Writer, r table.Reader, opts Options) (Stats, error) {
	jw := &jsonWriter{opts: opts, cols: r.Columns(), count: &counter{w: w}}
	jw.out = bufio.NewWriterSize(jw.count, 64<<10)
	err := jw.run(ctx, r)
	return Stats{Rows: jw.rows, Bytes: jw.count.n, Truncated: jw.truncated}, err
}

// jsonWriter is one JSON call: the output, the held total row and the subtotal levels.
type jsonWriter struct {
	opts      Options
	cols      []table.Column
	count     *counter
	out       *bufio.Writer
	line      []byte
	total     []byte
	levels    []byte
	written   int
	rows      int64
	truncated Truncation
}

// envelopeMeta is the "meta" object of the rows envelope.
type envelopeMeta struct {
	// Title is the report's name.
	Title string `json:"title"`
	// Description is the report's description.
	Description string `json:"description,omitempty"`
	// GeneratedAt is when the run started.
	GeneratedAt time.Time `json:"generatedAt"`
	// Timezone is the IANA name of the run's time zone.
	Timezone string `json:"timezone,omitempty"`
	// RequestedBy names who asked for the run.
	RequestedBy string `json:"requestedBy,omitempty"`
	// Params are the run's parameter values in declared order.
	Params []Param `json:"params,omitempty"`
}

// envelopeSummary is the "summary" object of the rows envelope.
type envelopeSummary struct {
	// RowCount is the number of detail rows written.
	RowCount int64 `json:"rowCount"`
	// Truncated reports that the run was cut short.
	Truncated bool `json:"truncated"`
	// TruncatedBy says why: "rows" or "bytes".
	TruncatedBy string `json:"truncatedBy,omitempty"`
}

// run writes the head, every batch and the tail.
func (jw *jsonWriter) run(ctx context.Context, r table.Reader) error {
	if err := jw.head(); err != nil {
		return err
	}
	for {
		b, err := r.Next(ctx)
		if errors.Is(err, io.EOF) {
			if r.Truncated() {
				jw.truncated = TruncatedRows
			}
			break
		}
		if err != nil {
			return err
		}
		stop, err := jw.batch(b)
		if err != nil {
			return err
		}
		if stop {
			jw.truncated = TruncatedBytes
			break
		}
	}
	return jw.tail()
}

// head writes the meta, the schema and the opening of the rows.
func (jw *jsonWriter) head() error {
	m := jw.opts.Meta
	meta, err := json.Marshal(envelopeMeta{Title: m.Title, Description: m.Description, GeneratedAt: m.GeneratedAt,
		Timezone: jw.opts.location().String(), RequestedBy: m.RequestedBy, Params: m.Params})
	if err != nil {
		return err
	}
	schema, err := json.Marshal(jw.cols)
	if err != nil {
		return err
	}
	jw.line = append(jw.line, `{"meta":`...)
	jw.line = append(jw.line, meta...)
	jw.line = append(jw.line, `,"schema":`...)
	jw.line = append(jw.line, schema...)
	jw.line = append(jw.line, `,"rows":[`...)
	return jw.flushLine()
}

// batch writes one batch and reports whether the byte limit stopped it.
func (jw *jsonWriter) batch(b *table.Batch) (bool, error) {
	for row := range b.Len {
		level := b.Level(row)
		if level == table.Total {
			jw.total = jw.appendRow(jw.total[:0], b, row)
			continue
		}
		if jw.written > 0 {
			jw.line = append(jw.line, ',')
		}
		jw.line = jw.appendRow(jw.line, b, row)
		if level.IsSubtotal() {
			jw.levels = appendLevel(jw.levels, jw.written, level)
		} else {
			jw.rows++
		}
		jw.written++
		if err := jw.flushLine(); err != nil {
			return false, err
		}
		if jw.opts.over(jw.count.n + int64(jw.out.Buffered())) {
			return true, nil
		}
	}
	return false, nil
}

// appendLevel appends a [row, level] pair.
func appendLevel(dst []byte, row int, level table.Level) []byte {
	if len(dst) > 0 {
		dst = append(dst, ',')
	}
	dst = append(dst, '[')
	dst = strconv.AppendInt(dst, int64(row), 10)
	dst = append(dst, ',')
	dst = strconv.AppendInt(dst, int64(level), 10)
	return append(dst, ']')
}

// appendRow appends a row as a JSON array of raw values.
func (jw *jsonWriter) appendRow(dst []byte, b *table.Batch, row int) []byte {
	dst = append(dst, '[')
	for c, col := range jw.cols {
		if c > 0 {
			dst = append(dst, ',')
		}
		dst = appendJSONValue(dst, col.Type, &b.Columns[c], row)
	}
	return append(dst, ']')
}

// tail closes the rows and writes the levels, the totals and the summary.
func (jw *jsonWriter) tail() error {
	jw.line = append(jw.line, ']')
	if len(jw.levels) > 0 {
		jw.line = append(jw.line, `,"levels":[`...)
		jw.line = append(jw.line, jw.levels...)
		jw.line = append(jw.line, ']')
	}
	if len(jw.total) > 0 {
		jw.line = append(jw.line, `,"totals":`...)
		jw.line = append(jw.line, jw.total...)
	}
	summary, err := json.Marshal(envelopeSummary{RowCount: jw.rows, Truncated: jw.truncated != NotTruncated,
		TruncatedBy: jw.truncated.String()})
	if err != nil {
		return err
	}
	jw.line = append(jw.line, `,"summary":`...)
	jw.line = append(jw.line, summary...)
	jw.line = append(jw.line, '}')
	if err := jw.flushLine(); err != nil {
		return err
	}
	return jw.out.Flush()
}

// flushLine writes the pending bytes.
func (jw *jsonWriter) flushLine() error {
	_, err := jw.out.Write(jw.line)
	jw.line = jw.line[:0]
	return err
}

// appendJSONValue appends row i of v as a raw JSON value.
func appendJSONValue(dst []byte, t value.Type, v *value.Vector, i int) []byte {
	if v.Null(i) {
		return append(dst, "null"...)
	}
	switch t {
	case value.Int, value.Epoch:
		return strconv.AppendInt(dst, v.Int64[i], 10)
	case value.Float:
		if special, ok := floatSpecial(v.Float64[i]); ok {
			return appendQuoted(dst, special)
		}
		return appendFloat(dst, v.Float64[i])
	case value.Bool:
		return strconv.AppendBool(dst, v.Bool[i])
	case value.JSON:
		if len(v.JSON[i]) == 0 {
			return append(dst, "null"...)
		}
		return append(dst, v.JSON[i]...)
	case value.Decimal:
		dst = append(dst, '"')
		dst = value.AppendNumeric(dst, v.Numeric[i])
		return append(dst, '"')
	case value.Time:
		dst = append(dst, '"')
		dst = v.Time[i].UTC().AppendFormat(dst, time.RFC3339Nano)
		return append(dst, '"')
	}
	return appendQuoted(dst, v.String[i])
}

// hexDigits are the digits of a \u escape.
const hexDigits = "0123456789abcdef"

// appendQuoted appends s as a JSON string. Invalid UTF-8 becomes U+FFFD, and U+2028 and U+2029
// are escaped so the document is also valid JavaScript.
func appendQuoted(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			dst = appendASCII(dst, c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			dst = append(dst, `�`...)
		case r == ' ' || r == ' ':
			dst = append(dst, `\u202`...)
			dst = append(dst, hexDigits[r&0xF])
		default:
			dst = append(dst, s[i:i+size]...)
		}
		i += size
	}
	return append(dst, '"')
}

// appendASCII appends an ASCII byte escaped for a JSON string.
func appendASCII(dst []byte, c byte) []byte {
	switch {
	case c == '"' || c == '\\':
		return append(dst, '\\', c)
	case c == '\n':
		return append(dst, '\\', 'n')
	case c == '\r':
		return append(dst, '\\', 'r')
	case c == '\t':
		return append(dst, '\\', 't')
	case c < 0x20:
		return append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
	}
	return append(dst, c)
}
