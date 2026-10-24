package export_test

import (
	"context"
	"io"
	"testing"

	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/analytics/table"
)

// BenchmarkWrite writes 100,000 rows of six columns per iteration in each format.
func BenchmarkWrite(b *testing.B) {
	writers := map[string]func(context.Context, io.Writer, table.Reader) (export.Stats, error){
		"csv": func(ctx context.Context, w io.Writer, r table.Reader) (export.Stats, error) {
			return export.CSV(ctx, w, r, export.CSVOptions{})
		},
		"csv-raw": func(ctx context.Context, w io.Writer, r table.Reader) (export.Stats, error) {
			return export.CSV(ctx, w, r, export.CSVOptions{Raw: true})
		},
		"json": func(ctx context.Context, w io.Writer, r table.Reader) (export.Stats, error) {
			return export.JSON(ctx, w, r, export.Options{})
		},
		"xlsx": func(ctx context.Context, w io.Writer, r table.Reader) (export.Stats, error) {
			return export.XLSX(ctx, w, r, export.Options{})
		},
		"arrow": func(ctx context.Context, w io.Writer, r table.Reader) (export.Stats, error) {
			return export.Arrow(ctx, w, r, export.Options{})
		},
	}
	for name, write := range writers {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			var bytes int64
			for b.Loop() {
				stats, err := write(context.Background(), io.Discard, newGenReader(100_000, 1024))
				if err != nil {
					b.Fatal(err)
				}
				bytes = stats.Bytes
			}
			b.ReportMetric(float64(bytes)/(1<<20), "MiB")
		})
	}
}
