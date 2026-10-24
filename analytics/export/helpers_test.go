package export_test

import (
	"context"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

var chicago = mustLocation("America/Chicago")

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// sampleColumns is a schema with every value type.
func sampleColumns() []table.Column {
	col := func(id, label string, t value.Type, hint format.Hint, spec *format.Spec) table.Column {
		return table.Column{ID: id, Label: label, Type: t, Display: format.Resolve(t, hint, spec)}
	}
	return []table.Column{
		col("customer", "Customer", value.String, format.Hint{}, nil),
		col("status", "Status", value.Enum, format.Hint{Labels: map[string]string{"InTransit": "In Transit"}}, nil),
		col("revenue", "Revenue", value.Decimal, format.Hint{Kind: format.Money},
			&format.Spec{Rules: []format.Rule{{Op: format.RuleGreater, Value: 1000, Tone: format.TonePositive}}}),
		col("loads", "Loads", value.Int, format.Hint{Kind: format.Count}, nil),
		col("share", "Share", value.Float, format.Hint{Kind: format.Percent}, nil),
		col("delivered", "Delivered", value.Time, format.Hint{}, &format.Spec{DateStyle: format.DateStyleDate}),
		col("hazmat", "Hazmat", value.Bool, format.Hint{}, nil),
		col("shipped", "Shipped", value.Epoch, format.Hint{}, nil),
		col("transit", "Transit", value.Int, format.Hint{Kind: format.Duration}, nil),
		col("extra", "Extra", value.JSON, format.Hint{}, nil),
	}
}

var delivered = time.Date(2026, 3, 4, 1, 30, 0, 0, time.UTC)

// sampleTable is a small result with a subtotal, a total and a truncation.
func sampleTable(t testing.TB) *table.Table {
	b := table.NewBuilder(sampleColumns())
	rows := [][]any{
		{"ACME, Inc.", "InTransit", "1082.0672643987443142", 12, 0.1834, delivered, true, int64(1772587800), 94200, []byte(`{"a":1}`)},
		{"=HYPERLINK(\"x\")", "Delivered", "-12.50", 3, 0.05, delivered.Add(48 * time.Hour), false, int64(1772600000), 600, nil},
		{"Globex", nil, nil, nil, nil, nil, nil, nil, nil, nil},
	}
	for _, r := range rows {
		if err := b.Append(r...); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AppendLevel(table.Subtotal(1), "Globex", nil, "100", 1, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.AppendLevel(table.Total, nil, nil, "1169.5672643987443142", 16, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	tbl := b.Table()
	tbl.Truncated = true
	return tbl
}

// genReader yields n synthetic rows in batches, reusing its buffers like a database reader.
type genReader struct {
	n, next, size int
	cols          []table.Column
	batch         table.Batch
	ints          []big.Int
}

func newGenReader(n, size int) *genReader {
	g := &genReader{n: n, size: size, cols: []table.Column{
		{ID: "id", Label: "ID", Type: value.Int, Display: format.Resolve(value.Int, format.Hint{Kind: format.Count}, nil)},
		{ID: "customer", Label: "Customer", Type: value.String, Display: format.Resolve(value.String, format.Hint{}, nil)},
		{ID: "amount", Label: "Amount", Type: value.Decimal, Display: format.Resolve(value.Decimal, format.Hint{Kind: format.Money},
			&format.Spec{Rules: []format.Rule{{Op: format.RuleLess, Value: 0, Tone: format.ToneNegative}}})},
		{ID: "at", Label: "At", Type: value.Time, Display: format.Resolve(value.Time, format.Hint{}, nil)},
		{ID: "ok", Label: "OK", Type: value.Bool, Display: format.Resolve(value.Bool, format.Hint{}, nil)},
		{ID: "hours", Label: "Hours", Type: value.Float, Display: format.Resolve(value.Float, format.Hint{}, nil)},
	}}
	g.batch.Columns = []value.Vector{
		{Int64: make([]int64, size)},
		{String: make([]string, size)},
		{Numeric: make([]pgtype.Numeric, size)},
		{Time: make([]time.Time, size)},
		{Bool: make([]bool, size)},
		{Float64: make([]float64, size), Nulls: value.NewBitmap(size)},
	}
	g.ints = make([]big.Int, size)
	return g
}

var customers = []string{"ACME", "Globex", "Initech", "Umbrella", "Hooli", "Stark Industries"}

func (g *genReader) Columns() []table.Column { return g.cols }

func (g *genReader) Truncated() bool { return false }

func (g *genReader) Next(context.Context) (*table.Batch, error) {
	if g.next >= g.n {
		return nil, io.EOF
	}
	n := min(g.size, g.n-g.next)
	cols := g.batch.Columns
	clear(cols[5].Nulls)
	for i := range n {
		id := int64(g.next + i)
		cols[0].Int64[i] = id
		cols[1].String[i] = customers[id%int64(len(customers))]
		g.ints[i].SetInt64(id*137 - 50000)
		cols[2].Numeric[i] = pgtype.Numeric{Int: &g.ints[i], Exp: -2, Valid: true}
		cols[3].Time[i] = delivered.Add(time.Duration(id) * time.Minute)
		cols[4].Bool[i] = id%3 == 0
		cols[5].Float64[i] = float64(id) / 7
		if id%10 == 0 {
			cols[5].Nulls.Set(i)
		}
	}
	g.next += n
	g.batch.Len = n
	return &g.batch, nil
}
