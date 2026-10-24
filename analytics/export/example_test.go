package export_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// revenueByCustomer is a small result with a grand total, as the analytics runner returns one.
func revenueByCustomer() *table.Table {
	b := table.NewBuilder([]table.Column{
		{ID: "customer", Label: "Customer", Type: value.String, Display: format.Resolve(value.String, format.Hint{}, nil)},
		{ID: "revenue", Label: "Revenue", Type: value.Decimal, Display: format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, nil)},
	})
	for _, row := range [][]any{{"ACME", "2840.125"}, {"=cmd|' /C calc'!A0", "-12.5"}} {
		if err := b.Append(row...); err != nil {
			log.Fatal(err)
		}
	}
	if err := b.AppendLevel(table.Total, nil, "2827.625"); err != nil {
		log.Fatal(err)
	}
	return b.Table()
}

// A formatted CSV renders every cell through its column's display, labels the total and
// escapes a cell a spreadsheet would run as a formula.
func ExampleCSV() {
	if _, err := export.CSV(context.Background(), os.Stdout, revenueByCustomer().Reader(), export.CSVOptions{}); err != nil {
		log.Fatal(err)
	}
	// Output:
	// Customer,Revenue
	// ACME,"$2,840.13"
	// '=cmd|' /C calc'!A0,($12.50)
	// Total,"$2,827.63"
}

// A raw CSV is for a program: column IDs, exact values and detail rows only.
func ExampleCSV_raw() {
	if _, err := export.CSV(context.Background(), os.Stdout, revenueByCustomer().Reader(), export.CSVOptions{Raw: true}); err != nil {
		log.Fatal(err)
	}
	// Output:
	// customer,revenue
	// ACME,2840.125
	// '=cmd|' /C calc'!A0,-12.5
}

// The JSON envelope stores a run's raw values; ReadJSON reads it back as a table.
func ExampleReadJSON() {
	var stored bytes.Buffer
	meta := export.Meta{Title: "Revenue by customer", GeneratedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	if _, err := export.JSON(context.Background(), &stored, revenueByCustomer().Reader(), export.Options{Meta: meta}); err != nil {
		log.Fatal(err)
	}
	env, err := export.ReadJSON(&stored)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(env.Meta.Title, env.RowCount, env.Table.Len())
	// Output: Revenue by customer 2 3
}

// An XLSX workbook is written as rows arrive, so a result of any size streams in flat memory.
func ExampleXLSX() {
	f, err := os.Create("revenue.xlsx")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	berlin, _ := time.LoadLocation("Europe/Berlin")
	stats, err := export.XLSX(context.Background(), f, revenueByCustomer().Reader(), export.Options{
		Meta:     export.Meta{Title: "Revenue by customer", Params: []export.Param{{Name: "window", Value: 30}}},
		Location: berlin,
		MaxBytes: 50 << 20,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d rows, %d bytes, truncated: %q", stats.Rows, stats.Bytes, stats.Truncated)
}

// An Arrow stream feeds a dataframe: pyarrow.ipc.open_stream(f).read_pandas().
func ExampleArrow() {
	var buf bytes.Buffer
	stats, err := export.Arrow(context.Background(), &buf, revenueByCustomer().Reader(), export.Options{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(stats.Rows)
	// Output: 2
}
