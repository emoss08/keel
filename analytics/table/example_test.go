package table_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// A Builder makes a table from Go values; its Reader is what every writer and diff takes.
func ExampleBuilder() {
	b := table.NewBuilder([]table.Column{
		{ID: "customer", Label: "Customer", Type: value.String},
		{ID: "revenue", Label: "Revenue", Type: value.Decimal,
			Display: format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, nil)},
	})
	for _, row := range [][]any{{"ACME", "2840.125"}, {"Globex", nil}} {
		if err := b.Append(row...); err != nil {
			log.Fatal(err)
		}
	}
	if err := b.AppendLevel(table.Total, nil, "2840.125"); err != nil {
		log.Fatal(err)
	}
	r := b.Table().Reader()
	for {
		batch, err := r.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		revenue := &batch.Columns[1]
		for i := range batch.Len {
			fmt.Printf("level %d, null %v, %q\n", batch.Level(i), revenue.Null(i), value.AppendNumeric(nil, revenue.Numeric[i]))
		}
	}
	// Output:
	// level 0, null false, "2840.125"
	// level 0, null true, ""
	// level 255, null false, "2840.125"
}
