package table_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

func columns() []table.Column {
	return []table.Column{
		{ID: "name", Label: "Name", Type: value.String},
		{ID: "amount", Label: "Amount", Type: value.Decimal, Sensitivity: table.Restricted,
			Display: format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, nil)},
		{ID: "count", Label: "Count", Type: value.Int},
		{ID: "at", Label: "At", Type: value.Epoch},
		{ID: "ok", Label: "OK", Type: value.Bool},
		{ID: "doc", Label: "Doc", Type: value.JSON},
	}
}

func TestBuilder(t *testing.T) {
	b := table.NewBuilder(columns())
	at := time.Unix(1772587800, 0)
	rows := [][]any{
		{"a", "1.50", 1, at, true, `{"x":1}`},
		{nil, nil, nil, nil, nil, nil},
		{"c", 2.25, int32(3), int64(5), false, []byte(`[]`)},
	}
	for _, r := range rows {
		if err := b.Append(r...); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AppendLevel(table.Total, nil, "3.75", 4, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	tbl := b.Table()
	if tbl.Len() != 4 || len(tbl.Batches) != 1 {
		t.Fatalf("%d rows in %d batches", tbl.Len(), len(tbl.Batches))
	}
	batch := tbl.Batches[0]
	if batch.Level(3) != table.Total || batch.Level(0) != table.Detail {
		t.Error("levels")
	}
	cols := batch.Columns
	if cols[0].String[0] != "a" || !cols[0].Null(1) || cols[3].Int64[0] != 1772587800 || string(cols[5].JSON[0]) != `{"x":1}` {
		t.Error("values")
	}
	for c := range cols {
		if !cols[c].Null(1) {
			t.Errorf("column %d row 1 is null", c)
		}
	}
	if got := string(cols[1].Numeric[2]); got != "2.25" {
		t.Errorf("float into decimal: %s", got)
	}
}

func TestBuilderRefusesWhatDoesNotFit(t *testing.T) {
	b := table.NewBuilder(columns())
	cases := [][]any{
		{1, nil, nil, nil, nil, nil},
		{nil, "abc", nil, nil, nil, nil},
		{nil, nil, "1", nil, nil, nil},
		{nil, nil, nil, nil, "yes", nil},
		{nil, nil, uint64(1 << 63), nil, nil, nil},
		{"short"},
	}
	for i, r := range cases {
		if err := b.Append(r...); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := b.Append("ok", "1", 1, 1, true, nil); err != nil {
		t.Fatal(err)
	}
	batch := b.Table().Batches[0]
	if batch.Len != 1 || len(batch.Columns[0].String) != 1 || batch.Columns[0].Null(0) {
		t.Errorf("a refused row leaves nothing behind: %+v", batch.Columns[0])
	}
}

func TestBuilderStartsNewBatches(t *testing.T) {
	b := table.NewBuilder(columns()[:1])
	for range table.BatchRows + 5 {
		if err := b.Append("x"); err != nil {
			t.Fatal(err)
		}
	}
	tbl := b.Table()
	if len(tbl.Batches) != 2 || tbl.Batches[1].Len != 5 || tbl.Len() != table.BatchRows+5 {
		t.Errorf("%d batches", len(tbl.Batches))
	}
}

// reusingReader yields the same batch twice with different contents, as a database reader that
// reuses its buffers does.
type reusingReader struct {
	n     int
	batch table.Batch
}

func (r *reusingReader) Columns() []table.Column { return columns()[:2] }

func (r *reusingReader) Truncated() bool { return true }

func (r *reusingReader) Next(ctx context.Context) (*table.Batch, error) {
	if r.n == 2 {
		return nil, io.EOF
	}
	r.n++
	if r.batch.Columns == nil {
		r.batch = table.Batch{Len: 1, Columns: []value.Vector{{String: make([]string, 1)}, {Numeric: make([]keel.Decimal, 1)}}}
	}
	r.batch.Columns[0].String[0] = strings.Repeat("x", r.n)
	r.batch.Columns[1].Numeric[0] = value.DecimalFromInt(int64(r.n))
	return &r.batch, nil
}

func TestCollectCopiesBatches(t *testing.T) {
	tbl, err := table.Collect(context.Background(), &reusingReader{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Batches) != 2 || !tbl.Truncated {
		t.Fatalf("%d batches", len(tbl.Batches))
	}
	first := tbl.Batches[0].Columns
	if first[0].String[0] != "x" || first[1].Numeric[0] != "1" {
		t.Errorf("the first batch was overwritten: %q %v", first[0].String[0], first[1].Numeric[0])
	}
	r := tbl.Reader()
	for range 2 {
		if _, err := r.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("after the last batch: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tbl.Reader().Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context: %v", err)
	}
}

func TestColumnJSON(t *testing.T) {
	b, err := json.Marshal(columns()[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), `{"id":"amount","label":"Amount","type":"decimal","display":{"style":"currency"`) ||
		!strings.HasSuffix(string(b), `"sensitivity":"restricted"}`) {
		t.Errorf("%s", b)
	}
	var back table.Column
	if err := json.Unmarshal(b, &back); err != nil || back.Type != value.Decimal || back.Sensitivity != table.Restricted {
		t.Errorf("%+v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"sensitivity":"secret"}`), &back); err == nil {
		t.Error("unknown sensitivity accepted")
	}
}

func TestLevels(t *testing.T) {
	if !table.Subtotal(1).IsSubtotal() || table.Total.IsSubtotal() || table.Detail.IsSubtotal() || table.Subtotal(999) != 254 {
		t.Error("levels")
	}
}
