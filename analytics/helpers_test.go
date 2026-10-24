package analytics_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// def decodes a definition written as JSON, adding the version.
func def(t testing.TB, js string) *ir.Definition {
	t.Helper()
	d, err := ir.Decode([]byte(`{"version": 2, ` + strings.TrimPrefix(strings.TrimSpace(js), "{")))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// rows reads a reader to the end and writes each row as its cells joined by " | ", subtotal and
// total rows marked with their level.
func rows(t testing.TB, r table.Reader) []string {
	t.Helper()
	var out []string
	cols := r.Columns()
	for {
		b, err := r.Next(context.Background())
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		for i := range b.Len {
			cells := make([]string, len(cols))
			for c := range cols {
				cells[c] = cell(&b.Columns[c], cols[c].Type, i)
			}
			line := strings.Join(cells, " | ")
			switch l := b.Level(i); {
			case l == table.Total:
				line = "TOTAL " + line
			case l.IsSubtotal():
				line = fmt.Sprintf("SUB%d %s", l, line)
			}
			out = append(out, line)
		}
	}
}

// cell writes one value as text: NULL, a decimal as written, a time in RFC 3339 UTC.
func cell(v *value.Vector, t value.Type, i int) string {
	if v.Null(i) {
		return "NULL"
	}
	switch t {
	case value.Int, value.Epoch:
		return fmt.Sprint(v.Int64[i])
	case value.Decimal:
		return string(value.NormalizeDecimal(v.Numeric[i]))
	case value.Float:
		return fmt.Sprintf("%.4g", v.Float64[i])
	case value.Bool:
		return fmt.Sprint(v.Bool[i])
	case value.Time:
		return v.Time[i].UTC().Format(time.RFC3339)
	case value.JSON:
		return string(v.JSON[i])
	}
	return v.String[i]
}

// runner returns a runner over the test database with options.
func runner(t testing.TB, opts analytics.RunnerOptions) *analytics.Runner {
	t.Helper()
	r, err := analytics.NewRunner(db(t), catalog(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// preview previews a definition for a world and returns its rows.
func preview(t testing.TB, w *world, d *ir.Definition, params map[string]any, loc *time.Location) []string {
	t.Helper()
	r := runner(t, analytics.RunnerOptions{PreviewRows: 1000})
	res, err := r.Preview(w.ctx, analytics.Request{Definition: d, Params: params, Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	return rows(t, res)
}

// want compares rows with the expected lines.
func want(t testing.TB, got []string, lines ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(lines, "\n"))
	}
}

// location loads a time zone.
func location(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
