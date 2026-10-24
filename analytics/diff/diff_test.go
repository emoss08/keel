package diff_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics/diff"
	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/analytics/format"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

func schema() []table.Column {
	return []table.Column{
		{ID: "customer", Label: "Customer", Type: value.String},
		{ID: "status", Label: "Status", Type: value.Enum},
		{ID: "revenue", Label: "Revenue", Type: value.Decimal},
		{ID: "shipments", Label: "Shipments", Type: value.Int},
	}
}

func run(t *testing.T, rows ...[]any) *table.Table {
	t.Helper()
	b := table.NewBuilder(schema())
	for _, r := range rows {
		if err := b.Append(r...); err != nil {
			t.Fatal(err)
		}
	}
	return b.Table()
}

func compare(t *testing.T, before, after *table.Table, opts diff.Options) *diff.Result {
	t.Helper()
	res, err := diff.Compare(context.Background(), before.Reader(), after.Reader(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func changeFor(t *testing.T, res *diff.Result, customer string) diff.Change {
	t.Helper()
	for _, ch := range res.Changes {
		if ch.Key[0] == customer {
			return ch
		}
	}
	t.Fatalf("no change for %q in %+v", customer, res.Changes)
	return diff.Change{}
}

func TestClassifiesEveryKindOfChange(t *testing.T) {
	before := run(t, []any{"ACME", "Billed", "1000.00", 10}, []any{"Globex", "Billed", "500.00", 5},
		[]any{"Initech", "Billed", "250.00", 2})
	after := run(t, []any{"ACME", "Billed", "1250.50", 12}, []any{"Initech", "Billed", "250.00", 2},
		[]any{"Umbrella", "Billed", "80.00", 1})
	res := compare(t, before, after, diff.Options{})
	want := diff.Summary{Added: 1, Removed: 1, Changed: 1, Unchanged: 1}
	if res.Summary != want || len(res.Changes) != 3 {
		t.Errorf("summary %+v, %d changes", res.Summary, len(res.Changes))
	}
	if got := changeFor(t, res, "Umbrella"); got.Kind != diff.Added || got.Measures[0] != (diff.Measure{
		Column: "revenue", Label: "Revenue", Before: "0", After: "80", Delta: "80"}) {
		t.Errorf("added %+v", got)
	}
	if got := changeFor(t, res, "Globex"); got.Kind != diff.Removed || got.Measures[0].Delta != "-500" || got.Measures[0].After != "0" {
		t.Errorf("removed %+v", got)
	}
}

func TestIsExactOnDecimals(t *testing.T) {
	res := compare(t, run(t, []any{"ACME", "Billed", "0.10", 1}), run(t, []any{"ACME", "Billed", "0.30", 1}), diff.Options{})
	ch := changeFor(t, res, "ACME")
	if len(ch.Measures) != 1 || ch.Measures[0].Column != "revenue" || ch.Measures[0].Delta != "0.2" {
		t.Errorf("change %+v", ch)
	}
}

func TestMatchesByKeyNotPosition(t *testing.T) {
	res := compare(t,
		run(t, []any{"ACME", "Billed", "100.00", 1}, []any{"Globex", "Billed", "200.00", 2}),
		run(t, []any{"Globex", "Billed", "200.00", 2}, []any{"ACME", "Billed", "100.0", 1}), diff.Options{})
	if res.Summary.Unchanged != 2 || res.Summary.Changed != 0 || len(res.Changes) != 0 {
		t.Errorf("result %+v", res)
	}
}

func TestReportsDuplicateKeys(t *testing.T) {
	res := compare(t, run(t, []any{"ACME", "Billed", "100.00", 1}),
		run(t, []any{"ACME", "Billed", "60.00", 1}, []any{"ACME", "Billed", "40.00", 1}), diff.Options{})
	if res.Summary != (diff.Summary{Duplicate: 1}) || len(res.Changes) != 1 || res.Changes[0].Kind != diff.Duplicate {
		t.Errorf("result %+v", res)
	}
}

func TestReportsADuplicateInTheEarlierRun(t *testing.T) {
	res := compare(t, run(t, []any{"ACME", "Billed", "60.00", 1}, []any{"ACME", "Billed", "40.00", 1}),
		run(t, []any{"Globex", "Billed", "10.00", 1}), diff.Options{})
	if res.Summary != (diff.Summary{Duplicate: 1, Added: 1}) {
		t.Errorf("summary %+v", res.Summary)
	}
}

func TestOrdersByRisk(t *testing.T) {
	res := compare(t,
		run(t, []any{"Gone", "Billed", "10.00", 1}, []any{"Small", "Billed", "10.00", 1}, []any{"Big", "Billed", "10.00", 1},
			[]any{"Dup", "Billed", "1", 1}, []any{"Dup", "Billed", "1", 1}),
		run(t, []any{"New", "Billed", "10.00", 1}, []any{"Small", "Billed", "11.00", 1}, []any{"Big", "Billed", "900.00", 1},
			[]any{"Newer", "Billed", "500", 1}), diff.Options{})
	var got []string
	for _, ch := range res.Changes {
		got = append(got, ch.Key[0])
	}
	if strings.Join(got, ",") != "Dup,Gone,Newer,New,Big,Small" {
		t.Errorf("order %v", got)
	}
}

func TestTotalsEachMeasureOnBothSides(t *testing.T) {
	res := compare(t,
		run(t, []any{"ACME", "Billed", "100.25", 2}, []any{"Globex", "Billed", "50.25", 3}),
		run(t, []any{"ACME", "Billed", "200.50", 4}, []any{"Globex", "Billed", "50.25", 3}), diff.Options{})
	want := []diff.Measure{
		{Column: "revenue", Label: "Revenue", Before: "150.5", After: "250.75", Delta: "100.25"},
		{Column: "shipments", Label: "Shipments", Before: "5", After: "7", Delta: "2"},
	}
	if len(res.Totals) != 2 || res.Totals[0] != want[0] || res.Totals[1] != want[1] {
		t.Errorf("totals %+v", res.Totals)
	}
}

func TestRefusesMismatchedSchemas(t *testing.T) {
	one := run(t, []any{"ACME", "Billed", "1.00", 1})
	wider := table.NewBuilder(append(schema(), table.Column{ID: "margin", Label: "Margin", Type: value.Decimal})).Table()
	renamed := table.NewBuilder(schema()).Table()
	renamed.Columns[0].ID = "account"
	retyped := table.NewBuilder(schema()).Table()
	retyped.Columns[2].Type = value.String
	for name, c := range map[string]struct {
		other *table.Table
		says  string
	}{"wider": {wider, "5 columns"}, "renamed": {renamed, "account"}, "retyped": {retyped, "revenue"}} {
		_, err := diff.Compare(context.Background(), c.other.Reader(), one.Reader(), diff.Options{})
		if !errors.Is(err, diff.ErrSchemaMismatch) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRefusesWhatCannotBeCompared(t *testing.T) {
	onlyMeasure := table.NewBuilder([]table.Column{{ID: "revenue", Label: "Revenue", Type: value.Decimal}}).Table()
	onlyKey := table.NewBuilder([]table.Column{{ID: "customer", Label: "Customer", Type: value.String}}).Table()
	one := run(t, []any{"ACME", "Billed", "1.00", 1})
	cases := []struct {
		a    *table.Table
		opts diff.Options
		want error
	}{
		{onlyMeasure, diff.Options{}, diff.ErrNoKeys},
		{onlyKey, diff.Options{}, diff.ErrNoMeasures},
		{one, diff.Options{Keys: []string{"nope"}}, diff.ErrUnknownColumn},
		{one, diff.Options{Measures: []string{"nope"}}, diff.ErrUnknownColumn},
	}
	for i, c := range cases {
		if _, err := diff.Compare(context.Background(), c.a.Reader(), c.a.Reader(), c.opts); !errors.Is(err, c.want) {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestHonoursChosenKeysAndMeasures(t *testing.T) {
	res := compare(t,
		run(t, []any{"ACME", "Billed", "100.00", 1}, []any{"Globex", "Billed", "100.00", 1}),
		run(t, []any{"ACME", "Billed", "150.00", 1}, []any{"Globex", "Billed", "100.00", 1}),
		diff.Options{Keys: []string{"status"}, Measures: []string{"revenue"}})
	if strings.Join(res.Keys, ",") != "status" || strings.Join(res.Measures, ",") != "revenue" || res.Summary.Duplicate != 1 {
		t.Errorf("result %+v", res)
	}
}

func TestKeysCannotCollideAcrossColumns(t *testing.T) {
	both := run(t, []any{"A", "B", "1.00", 1}, []any{"A\x1fB", "", "2.00", 1}, []any{"A\x00\x01B", nil, "3", 1})
	res := compare(t, both, both, diff.Options{})
	if res.Summary.Duplicate != 0 || res.Summary.Unchanged != 3 {
		t.Errorf("summary %+v", res.Summary)
	}
}

func TestANullKeyIsNotTheEmptyString(t *testing.T) {
	res := compare(t, run(t, []any{nil, "Billed", "1.00", 1}), run(t, []any{"", "Billed", "1.00", 1}), diff.Options{})
	if res.Summary != (diff.Summary{Added: 1, Removed: 1}) {
		t.Errorf("summary %+v", res.Summary)
	}
}

func TestTreatsNullMeasuresAsZero(t *testing.T) {
	res := compare(t, run(t, []any{"ACME", "Billed", nil, 1}), run(t, []any{"ACME", "Billed", "0", 1}), diff.Options{})
	if res.Summary.Unchanged != 1 || res.Totals[0].Delta != "0" {
		t.Errorf("result %+v", res)
	}
}

func TestCarriesTruncation(t *testing.T) {
	before := run(t, []any{"ACME", "Billed", "1.00", 1})
	before.Truncated = true
	if res := compare(t, before, run(t, []any{"ACME", "Billed", "1.00", 1}), diff.Options{}); !res.Truncated {
		t.Error("truncation is carried")
	}
}

func TestBoundsTheChangeList(t *testing.T) {
	var rows [][]any
	for i := range 50 {
		rows = append(rows, []any{string(rune('a'+i%26)) + string(rune('a'+i/26)), "Billed", "1.00", 1})
	}
	empty, full := run(t), run(t, rows...)
	if res := compare(t, empty, full, diff.Options{Limit: 10}); len(res.Changes) != 10 || res.Summary.Added != 50 {
		t.Errorf("limited: %d changes, %+v", len(res.Changes), res.Summary)
	}
	if res := compare(t, empty, full, diff.Options{Limit: -1}); len(res.Changes) != 50 {
		t.Errorf("unlimited: %d changes", len(res.Changes))
	}
}

func TestCanKeepUnchangedRows(t *testing.T) {
	one := run(t, []any{"ACME", "Billed", "1.00", 1})
	res := compare(t, one, one, diff.Options{IncludeUnchanged: true})
	if len(res.Changes) != 1 || res.Changes[0].Kind != diff.Unchanged {
		t.Errorf("changes %+v", res.Changes)
	}
	if res := compare(t, one, one, diff.Options{}); res.Changes == nil {
		t.Error("an empty change list is empty, not null")
	}
}

func TestIgnoresSummaryRows(t *testing.T) {
	b := table.NewBuilder(schema())
	for _, r := range [][]any{{"ACME", "Billed", "1.00", 1}, {"Globex", "Billed", "2.00", 1}} {
		if err := b.Append(r...); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AppendLevel(table.Subtotal(1), "ACME", nil, "1.00", 1); err != nil {
		t.Fatal(err)
	}
	if err := b.AppendLevel(table.Total, nil, nil, "3.00", 2); err != nil {
		t.Fatal(err)
	}
	res := compare(t, b.Table(), run(t, []any{"ACME", "Billed", "1.00", 1}, []any{"Globex", "Billed", "2.00", 1}), diff.Options{})
	if res.Summary != (diff.Summary{Unchanged: 2}) || res.Totals[0].Before != "3" {
		t.Errorf("result %+v", res)
	}
}

func TestComparesStoredEnvelopes(t *testing.T) {
	stored := func(rows ...[]any) *export.Envelope {
		var buf bytes.Buffer
		tbl := run(t, rows...)
		for i := range tbl.Columns {
			tbl.Columns[i].Display = format.Resolve(tbl.Columns[i].Type, format.Hint{}, nil)
		}
		if _, err := export.JSON(context.Background(), &buf, tbl.Reader(), export.Options{}); err != nil {
			t.Fatal(err)
		}
		env, err := export.ReadJSON(&buf)
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	before := stored([]any{"ACME", "Billed", "2840.125000", 12})
	after := stored([]any{"ACME", "Billed", "3102.625", 12})
	res, err := diff.Compare(context.Background(), before.Table.Reader(), after.Table.Reader(), diff.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"keys":["customer","status"],"measures":["revenue","shipments"],` +
		`"summary":{"added":0,"removed":0,"changed":1,"unchanged":0,"duplicate":0},` +
		`"changes":[{"kind":"changed","key":["ACME","Billed"],"measures":[{"column":"revenue","label":"Revenue","before":"2840.125","after":"3102.625","delta":"262.5"}]}],` +
		`"totals":[{"column":"revenue","label":"Revenue","before":"2840.125","after":"3102.625","delta":"262.5"},` +
		`{"column":"shipments","label":"Shipments","before":"12","after":"12","delta":"0"}],"truncated":false}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}
