package export_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

func writeXLSX(t testing.TB, r table.Reader, opts export.Options) ([]byte, export.Stats) {
	t.Helper()
	var buf bytes.Buffer
	stats, err := export.XLSX(context.Background(), &buf, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), stats
}

func openXLSX(t *testing.T, data []byte, raw bool) *excelize.File {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{RawCellValue: raw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// unzipTest checks the archive with unzip -t, when unzip is installed.
func unzipTest(t *testing.T, data []byte) {
	t.Helper()
	unzip, err := exec.LookPath("unzip")
	if err != nil {
		t.Log("unzip is not installed; skipping unzip -t")
		return
	}
	path := filepath.Join(t.TempDir(), "out.xlsx")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(unzip, "-t", path).CombinedOutput(); err != nil {
		t.Fatalf("unzip -t: %v\n%s", err, out)
	}
}

func cell(t *testing.T, f *excelize.File, sheet, ref string) string {
	t.Helper()
	v, err := f.GetCellValue(sheet, ref)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestXLSXReadsBack(t *testing.T) {
	data, stats := writeXLSX(t, sampleTable(t).Reader(), export.Options{Meta: runMeta, Location: chicago})
	unzipTest(t, data)
	if stats.Rows != 3 || stats.Truncated != export.TruncatedRows || stats.Bytes != int64(len(data)) {
		t.Errorf("stats %+v", stats)
	}
	f := openXLSX(t, data, false)
	if got := f.GetSheetList(); len(got) != 2 || got[0] != "Report" || got[1] != "Data" {
		t.Fatalf("sheets %v", got)
	}
	if f.GetActiveSheetIndex() != 1 {
		t.Errorf("active sheet %d", f.GetActiveSheetIndex())
	}
	info := [][2]string{
		{"Report", "Margin by lane"}, {"Generated At", "2026-10-01 07:00:00 CDT"},
		{"Requested By", "dispatch@example.com"}, {"Parameter: window", "30"}, {"Parameter: customer", "ACME"},
	}
	for i, row := range info {
		n := strconv.Itoa(i + 1)
		if a, b := cell(t, f, "Report", "A"+n), cell(t, f, "Report", "B"+n); a != row[0] || b != row[1] {
			t.Errorf("info row %d: %q %q", i+1, a, b)
		}
	}
	formatted := map[string]string{
		"A1": "Customer", "J1": "Extra",
		"A2": "ACME, Inc.", "B2": "In Transit", "C2": "$1,082.07", "D2": "12", "E2": "18.3%",
		"F2": "2026-03-03", "G2": "Yes", "H2": "2026-03-03 19:30", "I2": "26:10", "J2": `{"a":1}`,
		"A3": `=HYPERLINK("x")`, "C3": "($12.50)",
		"A5": "Globex", "B5": "Subtotal", "C5": "$100.00",
		"A6": "Total", "C6": "$1,169.57", "D6": "16",
		"A7": "Results were truncated at the row limit; narrow your filters or use a larger export format.",
	}
	for ref, want := range formatted {
		if got := cell(t, f, "Data", ref); got != want {
			t.Errorf("Data!%s: got %q, want %q", ref, got, want)
		}
	}
	raw := openXLSX(t, data, true)
	natives := map[string]string{"C2": "1082.0672643987443142", "D2": "12", "E2": "0.1834", "F2": "46084.8125", "C3": "-12.50"}
	for ref, want := range natives {
		if got := cell(t, raw, "Data", ref); got != want {
			t.Errorf("raw Data!%s: got %q, want %q", ref, got, want)
		}
	}
	for ref, want := range map[string]excelize.CellType{"C2": excelize.CellTypeUnset, "A2": excelize.CellTypeInlineString,
		"G2": excelize.CellTypeInlineString, "I2": excelize.CellTypeInlineString} {
		if got, _ := f.GetCellType("Data", ref); got != want {
			t.Errorf("type of Data!%s: %v, want %v", ref, got, want)
		}
	}
	checkStyles(t, f)
	if w, _ := f.GetColWidth("Data", "A"); w < 12 || w > 62 {
		t.Errorf("column A width %v", w)
	}
}

func checkStyles(t *testing.T, f *excelize.File) {
	t.Helper()
	style := func(ref string) *excelize.Style {
		id, err := f.GetCellStyle("Data", ref)
		if err != nil {
			t.Fatal(err)
		}
		s, err := f.GetStyle(id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := style("A1"); s.Font == nil || !s.Font.Bold {
		t.Error("the header is bold")
	}
	if s := style("C2"); s.Fill.Type != "pattern" || s.Fill.Pattern != 1 {
		t.Errorf("a toned cell has a solid fill: %+v", s.Fill)
	}
	if s := style("C3"); s.Fill.Pattern != 0 {
		t.Errorf("an untoned cell has no fill: %+v", s.Fill)
	}
	if s := style("C6"); s.Font == nil || !s.Font.Bold || s.CustomNumFmt == nil || *s.CustomNumFmt != `"$"#,##0.00;("$"#,##0.00)` {
		t.Errorf("the total is bold and keeps its number format: %+v", s)
	}
	if s := style("B5"); s.Font == nil || !s.Font.Bold {
		t.Error("the subtotal label is bold")
	}
}

func TestXLSXNativeBooleans(t *testing.T) {
	cols := []table.Column{
		{ID: "raw", Label: "Raw", Type: value.Bool},
		{ID: "tf", Label: "TF", Type: value.Bool, Display: format.Resolve(value.Bool, format.Hint{},
			&format.Spec{BoolStyle: format.BoolStyleTrueFalse})},
		{ID: "check", Label: "Check", Type: value.Bool, Display: format.Resolve(value.Bool, format.Hint{},
			&format.Spec{BoolStyle: format.BoolStyleCheck})},
	}
	b := table.NewBuilder(cols)
	if err := b.Append(true, false, true); err != nil {
		t.Fatal(err)
	}
	data, _ := writeXLSX(t, b.Table().Reader(), export.Options{})
	f := openXLSX(t, data, false)
	for ref, want := range map[string]string{"A2": "TRUE", "B2": "FALSE", "C2": "✓"} {
		if got := cell(t, f, "Data", ref); got != want {
			t.Errorf("Data!%s: got %q, want %q", ref, got, want)
		}
	}
	if typ, _ := f.GetCellType("Data", "A2"); typ != excelize.CellTypeBool {
		t.Errorf("a raw boolean is native: %v", typ)
	}
}

func TestXLSXSplitsSheets(t *testing.T) {
	defer export.SetMaxSheetRows(4)()
	data, stats := writeXLSX(t, newGenReader(10, 3), export.Options{})
	unzipTest(t, data)
	f := openXLSX(t, data, true)
	if got := f.GetSheetList(); len(got) != 3 || got[0] != "Data" || got[1] != "Data (2)" || got[2] != "Data (3)" {
		t.Fatalf("sheets %v", got)
	}
	if stats.Rows != 10 {
		t.Errorf("rows %d", stats.Rows)
	}
	for sheet, cells := range map[string]map[string]string{
		"Data":     {"A1": "ID", "A2": "0", "A5": "3", "A6": ""},
		"Data (2)": {"A1": "ID", "A2": "4", "A5": "7", "A6": ""},
		"Data (3)": {"A1": "ID", "A2": "8", "A3": "9", "A4": ""},
	} {
		for ref, want := range cells {
			if got := cell(t, f, sheet, ref); got != want {
				t.Errorf("%s!%s: got %q, want %q", sheet, ref, got, want)
			}
		}
	}
}

func TestXLSXStopsAtTheByteLimit(t *testing.T) {
	data, stats := writeXLSX(t, newGenReader(200_000, 1024), export.Options{MaxBytes: 64 << 10})
	unzipTest(t, data)
	if stats.Truncated != export.TruncatedBytes || stats.Rows == 0 || stats.Rows >= 200_000 {
		t.Fatalf("stats %+v", stats)
	}
	if len(data) > 160<<10 {
		t.Errorf("wrote %d bytes for a 64 KiB limit", len(data))
	}
	f := openXLSX(t, data, true)
	last := "A" + strconv.Itoa(int(stats.Rows)+2)
	if got := cell(t, f, "Data", last); got != "Results were truncated at the size limit; narrow your filters or choose fewer columns." {
		t.Errorf("Data!%s = %q", last, got)
	}
}

func TestXLSXEmptyResult(t *testing.T) {
	data, _ := writeXLSX(t, table.NewBuilder(sampleColumns()).Table().Reader(), export.Options{})
	unzipTest(t, data)
	f := openXLSX(t, data, false)
	rows, err := f.GetRows("Data")
	if err != nil || len(rows) != 1 || rows[0][0] != "Customer" {
		t.Errorf("rows %v %v", rows, err)
	}
}

// TestXLSXStreamsInFlatMemory writes two million rows, across two sheets, and checks that the
// live heap after the last row is no larger than after the first hundred thousand.
func TestXLSXStreamsInFlatMemory(t *testing.T) {
	const rows = 2_000_000
	g := newGenReader(rows, 1024)
	probe := &heapProbe{r: g, first: 100_000}
	stats, err := export.XLSX(context.Background(), io.Discard, probe, export.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Rows != rows {
		t.Fatalf("wrote %d rows", stats.Rows)
	}
	early, late := probe.heap[0], probe.heap[1]
	t.Logf("live heap after 100k rows: %d KiB; after 2M rows: %d KiB; output %d MiB", early>>10, late>>10, stats.Bytes>>20)
	if late > early+4<<20 {
		t.Errorf("the heap grew from %d to %d bytes while streaming", early, late)
	}
}

// heapProbe measures the live heap once the reader it wraps has yielded its first rows, and
// again after its last.
type heapProbe struct {
	r     *genReader
	first int
	heap  []uint64
}

func (p *heapProbe) Columns() []table.Column { return p.r.Columns() }

func (p *heapProbe) Truncated() bool { return false }

func (p *heapProbe) Next(ctx context.Context) (*table.Batch, error) {
	if (len(p.heap) == 0 && p.r.next >= p.first) || (len(p.heap) == 1 && p.r.next >= p.r.n) {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		p.heap = append(p.heap, m.HeapAlloc)
	}
	return p.r.Next(ctx)
}
