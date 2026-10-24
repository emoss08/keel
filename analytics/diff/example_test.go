package diff_test

import (
	"context"
	"fmt"
	"log"

	"github.com/emoss08/keel/analytics/diff"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// Compare matches rows by key and states every change exactly, the most worth reading first.
func ExampleCompare() {
	week := func(rows ...[]any) table.Reader {
		b := table.NewBuilder([]table.Column{
			{ID: "lane", Label: "Lane", Type: value.String},
			{ID: "revenue", Label: "Revenue", Type: value.Decimal},
		})
		for _, r := range rows {
			if err := b.Append(r...); err != nil {
				log.Fatal(err)
			}
		}
		return b.Table().Reader()
	}
	before := week([]any{"DAL-ATL", "2840.00"}, []any{"HOU-MEM", "910.10"}, []any{"CHI-DET", "400"})
	after := week([]any{"HOU-MEM", "910.30"}, []any{"DAL-ATL", "3102.50"}, []any{"PHX-LAX", "75"})
	res, err := diff.Compare(context.Background(), before, after, diff.Options{})
	if err != nil {
		log.Fatal(err)
	}
	for _, ch := range res.Changes {
		m := ch.Measures[0]
		fmt.Printf("%-7s %s %s -> %s (%s)\n", ch.Kind, ch.Key[0], m.Before, m.After, m.Delta)
	}
	fmt.Printf("total %s -> %s\n", res.Totals[0].Before, res.Totals[0].After)
	// Output:
	// removed CHI-DET 400 -> 0 (-400)
	// added   PHX-LAX 0 -> 75 (75)
	// changed DAL-ATL 2840 -> 3102.5 (262.5)
	// changed HOU-MEM 910.1 -> 910.3 (0.2)
	// total 4150.1 -> 4087.8
}
