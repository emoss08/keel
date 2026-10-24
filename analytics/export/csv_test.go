package export_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

func writeCSV(t *testing.T, r table.Reader, opts export.CSVOptions) (string, export.Stats) {
	t.Helper()
	var buf bytes.Buffer
	stats, err := export.CSV(context.Background(), &buf, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Bytes != int64(buf.Len()) {
		t.Errorf("stats report %d bytes, wrote %d", stats.Bytes, buf.Len())
	}
	return buf.String(), stats
}

func TestCSVFormatted(t *testing.T) {
	got, stats := writeCSV(t, sampleTable(t).Reader(), export.CSVOptions{Options: export.Options{Location: chicago}})
	want := "Customer,Status,Revenue,Loads,Share,Delivered,Hazmat,Shipped,Transit,Extra\n" +
		"\"ACME, Inc.\",In Transit,\"$1,082.07\",12,18.3%,2026-03-03,Yes,2026-03-03 19:30,26:10,\"{\"\"a\"\":1}\"\n" +
		"\"'=HYPERLINK(\"\"x\"\")\",Delivered,($12.50),3,5.0%,2026-03-05,No,2026-03-03 22:53,0:10,\n" +
		"Globex,,,,,,,,,\n" +
		"Globex,Subtotal,$100.00,1,,,,,,\n" +
		"Total,,\"$1,169.57\",16,,,,,,\n" +
		"Results were truncated at the row limit; narrow your filters or use a larger export format.,,,,,,,,,\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if stats.Rows != 3 || stats.Truncated != export.TruncatedRows {
		t.Errorf("stats %+v", stats)
	}
}

func TestCSVRaw(t *testing.T) {
	got, stats := writeCSV(t, sampleTable(t).Reader(), export.CSVOptions{Raw: true})
	want := "customer,status,revenue,loads,share,delivered,hazmat,shipped,transit,extra\n" +
		"\"ACME, Inc.\",InTransit,1082.0672643987443142,12,0.1834,2026-03-04T01:30:00Z,true,1772587800,94200,\"{\"\"a\"\":1}\"\n" +
		"\"'=HYPERLINK(\"\"x\"\")\",Delivered,-12.50,3,0.05,2026-03-06T01:30:00Z,false,1772600000,600,\n" +
		"Globex,,,,,,,,,\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if stats.Rows != 3 || stats.Truncated != export.TruncatedRows {
		t.Errorf("stats %+v", stats)
	}
}

func TestCSVEscapesFormulas(t *testing.T) {
	cols := []table.Column{
		{ID: "text", Label: "Text", Type: value.String, Display: format.Resolve(value.String, format.Hint{}, nil)},
		{ID: "amount", Label: "Amount", Type: value.Decimal, Display: format.Resolve(value.Decimal, format.Hint{},
			&format.Spec{Style: format.StyleCurrency, Negative: format.NegativeMinus, NullText: "-"})},
	}
	b := table.NewBuilder(cols)
	cases := []struct {
		text   any
		amount any
		want   string
	}{
		{"=1+1", "-5", "'=1+1,-$5.00"},
		{"+1", nil, "'+1,-"},
		{"@SUM(A1)", "5", "'@SUM(A1),$5.00"},
		{"-2+3", "-1234.5", "'-2+3,\"-$1,234.50\""},
		{"\tx", "0", "\"'\tx\",$0.00"},
		{"\rx", "0", "\"'\rx\",$0.00"},
		{"-", "0", "-,$0.00"},
		{"a=b", "0", "a=b,$0.00"},
		{"plain", "-0.001", "plain,$0.00"},
	}
	for _, c := range cases {
		if err := b.Append(c.text, c.amount); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := writeCSV(t, b.Table().Reader(), export.CSVOptions{})
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if lines[0] != "Text,Amount" {
		t.Errorf("header %q", lines[0])
	}
	for i, c := range cases {
		if lines[i+1] != c.want {
			t.Errorf("%q: got %q, want %q", c.text, lines[i+1], c.want)
		}
	}
	prefixed := table.NewBuilder([]table.Column{{ID: "n", Label: "N", Type: value.Decimal,
		Display: format.Resolve(value.Decimal, format.Hint{}, &format.Spec{Prefix: "2+cmd|' /C calc'!A0"})}})
	if err := prefixed.Append("-5"); err != nil {
		t.Fatal(err)
	}
	got, _ = writeCSV(t, prefixed.Table().Reader(), export.CSVOptions{})
	if !strings.HasSuffix(got, "\n\"'-2+cmd|' /C calc'!A05.00\"\n") && !strings.HasSuffix(got, "\n'-2+cmd|' /C calc'!A05.00\n") {
		t.Errorf("an author prefix after a minus is escaped: %q", got)
	}
}

func TestCSVDelimiterAndBOM(t *testing.T) {
	cols := []table.Column{
		{ID: "a", Label: "A;1", Type: value.String, Display: format.Resolved{}},
		{ID: "b", Label: "B", Type: value.Decimal, Display: format.Resolved{}},
	}
	b := table.NewBuilder(cols)
	if err := b.Append("x;y", "1.5"); err != nil {
		t.Fatal(err)
	}
	got, _ := writeCSV(t, b.Table().Reader(), export.CSVOptions{Delimiter: ';', BOM: true})
	if want := "\ufeff\"A;1\";B\n\"x;y\";1.5\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := export.CSV(context.Background(), &bytes.Buffer{}, b.Table().Reader(), export.CSVOptions{Delimiter: '"'}); err == nil {
		t.Error("a quote delimiter is refused")
	}
}

func TestCSVStopsAtTheByteLimit(t *testing.T) {
	got, stats := writeCSV(t, newGenReader(10_000, 1000), export.CSVOptions{Options: export.Options{MaxBytes: 4096}})
	if stats.Truncated != export.TruncatedBytes || stats.Rows == 0 || stats.Rows >= 10_000 {
		t.Fatalf("stats %+v", stats)
	}
	if !strings.HasSuffix(got, "Results were truncated at the size limit; narrow your filters or choose fewer columns.,,,,,\n") {
		t.Errorf("no notice: %q", got[len(got)-200:])
	}
	if len(got) > 4096+200 {
		t.Errorf("wrote %d bytes for a 4096-byte limit", len(got))
	}
}
