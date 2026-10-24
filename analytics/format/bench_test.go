package format_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/text/language"

	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/value"
)

const benchRows = 100_000

func benchVectors() map[string]struct {
	typ  value.Type
	hint format.Hint
	vec  *value.Vector
} {
	money := &value.Vector{Numeric: make([]pgtype.Numeric, benchRows)}
	quotients := &value.Vector{Numeric: make([]pgtype.Numeric, benchRows)}
	ints := &value.Vector{Int64: make([]int64, benchRows)}
	floats := &value.Vector{Float64: make([]float64, benchRows)}
	times := &value.Vector{Time: make([]time.Time, benchRows)}
	strs := &value.Vector{String: make([]string, benchRows)}
	big20, _ := new(big.Int).SetString("10820672643987443142", 10)
	for i := range benchRows {
		money.Numeric[i] = pgtype.Numeric{Int: big.NewInt(int64(i*137 - 500000)), Exp: -2, Valid: true}
		quotients.Numeric[i] = pgtype.Numeric{Int: new(big.Int).Add(big20, big.NewInt(int64(i))), Exp: -16, Valid: true}
		ints.Int64[i] = int64(i * 31)
		floats.Float64[i] = float64(i) / 7
		times.Time[i] = time.Unix(1772587800+int64(i)*60, 0)
		strs.String[i] = "ACME Logistics"
	}
	return map[string]struct {
		typ  value.Type
		hint format.Hint
		vec  *value.Vector
	}{
		"money":     {value.Decimal, format.Hint{Kind: format.Money}, money},
		"quotients": {value.Decimal, format.Hint{Kind: format.Money}, quotients},
		"counts":    {value.Int, format.Hint{Kind: format.Count}, ints},
		"durations": {value.Int, format.Hint{Kind: format.Duration}, ints},
		"floats":    {value.Float, format.Hint{Kind: format.Percent}, floats},
		"dates":     {value.Time, format.Hint{}, times},
		"text":      {value.String, format.Hint{}, strs},
	}
}

// BenchmarkColumn formats 100,000 values of one column per iteration.
func BenchmarkColumn(b *testing.B) {
	chicago, _ := time.LoadLocation("America/Chicago")
	for name, c := range benchVectors() {
		b.Run(name, func(b *testing.B) {
			f := format.New(c.typ, format.Resolve(c.typ, c.hint, nil), format.Options{Locale: language.German, Location: chicago})
			var cells format.Cells
			b.ReportAllocs()
			for b.Loop() {
				cells.Reset()
				f.Column(&cells, c.vec, benchRows)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchRows), "ns/value")
		})
	}
}
