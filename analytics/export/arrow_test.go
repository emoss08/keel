package export_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

func writeArrow(t *testing.T, r table.Reader, opts export.Options) ([]byte, export.Stats) {
	t.Helper()
	var buf bytes.Buffer
	stats, err := export.Arrow(context.Background(), &buf, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Bytes != int64(buf.Len()) {
		t.Errorf("stats report %d bytes, wrote %d", stats.Bytes, buf.Len())
	}
	return buf.Bytes(), stats
}

// readArrow reads a stream with arrow-go and returns its schema and records.
func readArrow(t *testing.T, data []byte) (*arrow.Schema, []arrow.RecordBatch) {
	t.Helper()
	rd, err := ipc.NewReader(bytes.NewReader(data), ipc.WithAllocator(memory.NewGoAllocator()))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Release()
	var recs []arrow.RecordBatch
	for rd.Next() {
		rec := rd.RecordBatch()
		rec.Retain()
		recs = append(recs, rec)
	}
	if err := rd.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return rd.Schema(), recs
}

func TestArrowRoundTrip(t *testing.T) {
	data, stats := writeArrow(t, sampleTable(t).Reader(), export.Options{Meta: runMeta})
	if stats.Rows != 3 || stats.Truncated != export.TruncatedRows {
		t.Errorf("stats %+v", stats)
	}
	schema, recs := readArrow(t, data)
	want := []struct {
		name string
		typ  string
	}{
		{"customer", "utf8"}, {"status", "utf8"}, {"revenue", "decimal(38, 16)"}, {"loads", "int64"},
		{"share", "float64"}, {"delivered", "timestamp[us, tz=UTC]"}, {"hazmat", "bool"},
		{"shipped", "timestamp[s, tz=UTC]"}, {"transit", "int64"}, {"extra", "utf8"},
	}
	for i, f := range schema.Fields() {
		if f.Name != want[i].name || f.Type.String() != want[i].typ || !f.Nullable {
			t.Errorf("field %d: %s %s", i, f.Name, f.Type)
		}
	}
	if label, _ := schema.Field(2).Metadata.GetValue("keel.label"); label != "Revenue" {
		t.Errorf("label metadata %q", label)
	}
	if ext, _ := schema.Field(9).Metadata.GetValue("ARROW:extension:name"); ext != "arrow.json" {
		t.Errorf("json extension %q", ext)
	}
	if title, _ := schema.Metadata().GetValue("keel.title"); title != runMeta.Title {
		t.Errorf("title %q", title)
	}
	if len(recs) != 2 || recs[0].NumRows() != 3 || recs[1].NumRows() != 0 {
		t.Fatalf("records %d", len(recs))
	}
	rec := recs[0]
	if got := rec.Column(0).(*array.String).Value(0); got != "ACME, Inc." {
		t.Errorf("customer %q", got)
	}
	if rec.Column(1).IsNull(2) == false || rec.Column(1).(*array.String).Value(0) != "InTransit" {
		t.Error("status")
	}
	rev := rec.Column(2).(*array.Decimal128)
	if got := rev.Value(0).ToString(16); got != "1082.0672643987443142" {
		t.Errorf("revenue %q", got)
	}
	if got := rev.Value(1).ToString(16); got != "-12.5000000000000000" {
		t.Errorf("negative revenue %q", got)
	}
	if !rev.IsNull(2) {
		t.Error("null revenue")
	}
	if got := rec.Column(3).(*array.Int64).Value(1); got != 3 {
		t.Errorf("loads %d", got)
	}
	if got := rec.Column(4).(*array.Float64).Value(0); got != 0.1834 {
		t.Errorf("share %v", got)
	}
	if got := rec.Column(5).(*array.Timestamp).Value(0).ToTime(arrow.Microsecond); !got.Equal(delivered) {
		t.Errorf("delivered %v", got)
	}
	if b := rec.Column(6).(*array.Boolean); !b.Value(0) || b.Value(1) || !b.IsNull(2) {
		t.Error("hazmat")
	}
	if got := rec.Column(7).(*array.Timestamp).Value(0).ToTime(arrow.Second); got.Unix() != 1772587800 {
		t.Errorf("shipped %v", got)
	}
	if got := rec.Column(9).(*array.String).Value(0); got != `{"a":1}` || !rec.Column(9).IsNull(1) {
		t.Errorf("extra %q", got)
	}
	if !bytes.Contains(data, []byte("keel.truncated")) {
		t.Error("no truncation marker")
	}
}

func TestArrowStreamsManyBatches(t *testing.T) {
	data, stats := writeArrow(t, newGenReader(10_000, 1000), export.Options{})
	_, recs := readArrow(t, data)
	total := int64(0)
	for _, r := range recs {
		total += r.NumRows()
	}
	if total != 10_000 || stats.Rows != 10_000 || len(recs) != 10 {
		t.Fatalf("read %d rows in %d records", total, len(recs))
	}
	last := recs[9]
	if got := last.Column(0).(*array.Int64).Value(999); got != 9999 {
		t.Errorf("last id %d", got)
	}
	if got := last.Column(2).(*array.Decimal128).Value(999).ToString(2); got != "13198.63" {
		t.Errorf("last amount %s", got)
	}
	if !last.Column(5).IsNull(990) || last.Column(5).IsNull(991) {
		t.Error("nulls")
	}
	if got := last.Column(3).(*array.Timestamp).Value(0).ToTime(arrow.Microsecond); !got.Equal(delivered.Add(9000 * time.Minute)) {
		t.Errorf("time %v", got)
	}
}

func TestArrowRoundsLaterDecimalsAndRefusesOverflow(t *testing.T) {
	cols := []table.Column{{ID: "d", Label: "D", Type: value.Decimal}}
	b := table.NewBuilder(cols)
	for _, s := range []string{"1.5", "2.25"} {
		if err := b.Append(s); err != nil {
			t.Fatal(err)
		}
	}
	tbl := b.Table()
	more := table.NewBuilder(cols)
	for _, s := range []string{"1.255", "-1.255", "NaN"} {
		if err := more.Append(s); err != nil {
			t.Fatal(err)
		}
	}
	tbl.Batches = append(tbl.Batches, more.Table().Batches...)
	data, _ := writeArrow(t, tbl.Reader(), export.Options{})
	_, recs := readArrow(t, data)
	d := recs[1].Column(0).(*array.Decimal128)
	if d.Value(0).ToString(2) != "1.26" || d.Value(1).ToString(2) != "-1.26" || !d.IsNull(2) {
		t.Errorf("rounded %s %s", d.Value(0).ToString(2), d.Value(1).ToString(2))
	}
	huge := table.NewBuilder(cols)
	if err := huge.Append("1e40"); err != nil {
		t.Fatal(err)
	}
	if _, err := export.Arrow(context.Background(), io.Discard, huge.Table().Reader(), export.Options{}); !errors.Is(err, export.ErrDecimalRange) {
		t.Errorf("overflow: %v", err)
	}
}

func TestArrowEmptyResult(t *testing.T) {
	data, _ := writeArrow(t, table.NewBuilder(sampleColumns()).Table().Reader(), export.Options{})
	schema, recs := readArrow(t, data)
	if len(schema.Fields()) != 10 || len(recs) != 0 {
		t.Errorf("%d fields, %d records", len(schema.Fields()), len(recs))
	}
}

func TestArrowStopsAtTheByteLimit(t *testing.T) {
	data, stats := writeArrow(t, newGenReader(10_000, 500), export.Options{MaxBytes: 32 << 10})
	if stats.Truncated != export.TruncatedBytes || stats.Rows == 0 || stats.Rows >= 10_000 {
		t.Fatalf("stats %+v", stats)
	}
	_, recs := readArrow(t, data)
	rows := int64(0)
	for _, r := range recs {
		rows += r.NumRows()
	}
	if rows != stats.Rows {
		t.Errorf("read %d rows, wrote %d", rows, stats.Rows)
	}
}
