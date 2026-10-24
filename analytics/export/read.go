package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// ErrEnvelope reports a rows envelope that cannot be read back.
var ErrEnvelope = errors.New("export: malformed rows envelope")

// Envelope is a rows envelope read back.
type Envelope struct {
	// Meta describes the run.
	Meta Meta
	// Location is the time zone the run was made in.
	Location *time.Location
	// Table holds the schema and the rows; a grand total is its last row, at level Total.
	Table *table.Table
	// RowCount is the number of detail rows the summary reports.
	RowCount int64
	// Truncated says why the run was cut short, if it was.
	Truncated Truncation
}

// ReadJSON reads a rows envelope written by JSON. It refuses an envelope without a schema or
// with a row whose width or values do not match it, since every reader of the rows indexes
// them by column.
func ReadJSON(r io.Reader) (*Envelope, error) {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	rd := &envelopeReader{dec: dec, env: &Envelope{Location: time.UTC}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrEnvelope, err)
		}
		key, _ := tok.(string)
		if err := rd.field(key); err != nil {
			return nil, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, err
	}
	return rd.finish()
}

// envelopeReader is one ReadJSON call.
type envelopeReader struct {
	dec    *json.Decoder
	env    *Envelope
	build  *table.Builder
	cols   []table.Column
	levels [][2]int
	totals []json.RawMessage
	vals   []any
}

// field reads the value of one top-level key; unknown keys are skipped.
func (rd *envelopeReader) field(key string) error {
	switch key {
	case "meta":
		return rd.meta()
	case "schema":
		return rd.schema()
	case "rows":
		return rd.rows()
	case "levels":
		return rd.decode(&rd.levels)
	case "totals":
		return rd.decode(&rd.totals)
	case "summary":
		var s envelopeSummary
		if err := rd.decode(&s); err != nil {
			return err
		}
		rd.env.RowCount = s.RowCount
		rd.env.Truncated = truncationOf(s)
		return nil
	}
	var skip json.RawMessage
	return rd.decode(&skip)
}

// truncationOf reads why a run was cut short from its summary.
func truncationOf(s envelopeSummary) Truncation {
	switch {
	case s.TruncatedBy == "bytes":
		return TruncatedBytes
	case s.Truncated:
		return TruncatedRows
	}
	return NotTruncated
}

// decode reads the next value into v.
func (rd *envelopeReader) decode(v any) error {
	if err := rd.dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrEnvelope, err)
	}
	return nil
}

// meta reads the meta object.
func (rd *envelopeReader) meta() error {
	var m envelopeMeta
	if err := rd.decode(&m); err != nil {
		return err
	}
	rd.env.Meta = Meta{Title: m.Title, Description: m.Description, GeneratedAt: m.GeneratedAt,
		RequestedBy: m.RequestedBy, Params: m.Params}
	if m.Timezone != "" {
		loc, err := time.LoadLocation(m.Timezone)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrEnvelope, err)
		}
		rd.env.Location = loc
	}
	return nil
}

// schema reads the schema and starts the table.
func (rd *envelopeReader) schema() error {
	if err := rd.decode(&rd.cols); err != nil {
		return err
	}
	if len(rd.cols) == 0 {
		return fmt.Errorf("%w: the schema has no columns", ErrEnvelope)
	}
	rd.build = table.NewBuilder(rd.cols)
	rd.vals = make([]any, len(rd.cols))
	return nil
}

// rows reads the rows one by one into the table.
func (rd *envelopeReader) rows() error {
	if rd.build == nil {
		return fmt.Errorf("%w: rows come before the schema", ErrEnvelope)
	}
	if err := expectDelim(rd.dec, '['); err != nil {
		return err
	}
	var row []json.RawMessage
	for i := 0; rd.dec.More(); i++ {
		row = row[:0]
		if err := rd.decode(&row); err != nil {
			return err
		}
		if err := rd.appendRow(row, table.Detail, "row "+strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return expectDelim(rd.dec, ']')
}

// appendRow checks a row against the schema and adds it at a level.
func (rd *envelopeReader) appendRow(row []json.RawMessage, level table.Level, where string) error {
	if len(row) != len(rd.cols) {
		return fmt.Errorf("%w: %s has %d values for %d columns", ErrEnvelope, where, len(row), len(rd.cols))
	}
	for c, raw := range row {
		x, err := decodeValue(rd.cols[c].Type, raw)
		if err != nil {
			return fmt.Errorf("%w: %s, column %q: %w", ErrEnvelope, where, rd.cols[c].ID, err)
		}
		rd.vals[c] = x
	}
	return rd.build.AppendLevel(level, rd.vals...)
}

// finish applies the subtotal levels, appends the totals row and returns the envelope.
func (rd *envelopeReader) finish() (*Envelope, error) {
	if rd.build == nil {
		return nil, fmt.Errorf("%w: no schema", ErrEnvelope)
	}
	t := rd.build.Table()
	for _, at := range rd.levels {
		row, level := at[0], at[1]
		if row < 0 || row >= t.Len() || level <= int(table.Detail) || level >= int(table.Total) {
			return nil, fmt.Errorf("%w: level %d for row %d", ErrEnvelope, level, row)
		}
		b := t.Batches[row/table.BatchRows]
		if b.Levels == nil {
			b.Levels = make([]table.Level, b.Len)
		}
		b.Levels[row%table.BatchRows] = table.Level(level)
	}
	if rd.totals != nil {
		if err := rd.appendRow(rd.totals, table.Total, "the totals row"); err != nil {
			return nil, err
		}
	}
	t.Truncated = rd.env.Truncated != NotTruncated
	rd.env.Table = t
	return rd.env, nil
}

// expectDelim reads the next token, which must be want.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEnvelope, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("%w: expected %q, found %v", ErrEnvelope, want, tok)
	}
	return nil
}

// decodeValue reads one raw JSON value of a column of type t into the Go value Builder takes.
func decodeValue(t value.Type, raw json.RawMessage) (any, error) {
	if bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	switch t {
	case value.Int, value.Epoch:
		return strconv.ParseInt(string(raw), 10, 64)
	case value.Float:
		return decodeFloat(raw)
	case value.Bool:
		var b bool
		err := json.Unmarshal(raw, &b)
		return b, err
	case value.JSON:
		return bytes.Clone(raw), nil
	case value.Decimal:
		return decodeDecimal(raw)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if t == value.Time {
		return time.Parse(time.RFC3339Nano, s)
	}
	return s, nil
}

// decodeFloat reads a float written as a number, or NaN or an infinity written as a string.
func decodeFloat(raw json.RawMessage) (any, error) {
	if raw[0] != '"' {
		return strconv.ParseFloat(string(raw), 64)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s != "NaN" && s != "Infinity" && s != "-Infinity" {
		return nil, fmt.Errorf("invalid float %s", raw)
	}
	return strconv.ParseFloat(s, 64)
}

// decodeDecimal reads a decimal written as a string or a number.
func decodeDecimal(raw json.RawMessage) (any, error) {
	s := string(raw)
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
	}
	return value.ParseDecimal(s)
}
