package analytics_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/table"
)

func TestPolicyDenies(t *testing.T) {
	w := seedWorld(t)
	cat := catalog(t)
	p := &fleetPolicy{asked: map[string]int{}}
	hazmat := def(t, `{"entity": "shipment", "columns": [{"id": "h", "dimension": {"field": "hazmat"}},
		{"id": "n", "measure": {"agg": "count"}}]}`)
	_, err := analytics.Compile(w.ctx, cat, p, hazmat, nil, analytics.CompileOptions{})
	var ae *analytics.Error
	if !errors.As(err, &ae) || ae.Code != analytics.CodeDenied || ae.Reason != "needs_safety" || ae.Pointer != "/columns/0/dimension/field" {
		t.Fatalf("denied field: %v", err)
	}
	filtered := def(t, `{"entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}],
		"filters": {"field": "billTo.name", "op": "eq", "value": "x"}}`)
	if _, err := analytics.Compile(w.ctx, cat, p, filtered, nil, analytics.CompileOptions{}); !errors.Is(err, &analytics.Error{Code: analytics.CodeDenied}) {
		t.Fatalf("denied filter field: %v", err)
	}
	stops := def(t, `{"entity": "stop", "columns": [{"id": "n", "measure": {"agg": "count"}}]}`)
	if _, err := analytics.Compile(w.ctx, cat, p, stops, nil, analytics.CompileOptions{}); analytics.ErrorCode(err) != analytics.CodeDenied {
		t.Fatalf("denied entity: %v", err)
	}
	invalid := def(t, `{"entity": "shipment", "columns": [{"id": "n", "dimension": {"field": "nope"}}]}`)
	_, err = analytics.Compile(w.ctx, cat, nil, invalid, nil, analytics.CompileOptions{})
	if !errors.As(err, &ae) || ae.Code != analytics.CodeInvalid || len(ae.Details) != 1 {
		t.Fatalf("invalid definition: %v", err)
	}
}

func TestRowRulesApplyToEveryReference(t *testing.T) {
	w := seedWorld(t)
	p := &fleetPolicy{asked: map[string]int{}, rows: true}
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "n", "measure": {"agg": "count"}},
		{"id": "moves", "measure": {"field": "moves.id", "agg": "count"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "share", "calc": "[revenue] / 2164"}
	], "sort": [{"column": "customer"}]}`)
	r := runner(t, analytics.RunnerOptions{})
	res, err := r.Preview(w.ctx, analytics.Request{Definition: d, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	got := rows(t, res)
	for i := range got {
		got[i] = got[i][:strings.LastIndex(got[i], " | ")]
	}
	want(t, got, "Acme | 3 | 3 | 610.25", "Initech | 4 | 4 | 882.5", "NULL | 4 | 5 | 570.75")
	if res.Columns()[3].Sensitivity != table.Confidential || res.Columns()[4].Sensitivity != table.Confidential ||
		res.Columns()[1].Sensitivity != table.Public {
		t.Fatalf("sensitivities %+v", res.Columns())
	}
	if p.asked["rows customer"] != 1 || p.asked["rows shipment"] != 1 {
		t.Fatalf("row rules asked %v", p.asked)
	}
	comp, err := r.Compile(w.ctx, analytics.Request{Definition: def(t, lanes), Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err := comp.ToSQL(w.ctx, db(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(sql, `."pro_number" <> $`) != 1 {
		t.Fatalf("the base table's rule:\n%s", sql)
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	a := seedWorld(t)
	b := seedRows(t, fixture[:2])
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "n", "measure": {"agg": "count"}},
		{"id": "stops", "measure": {"field": "moves.stops.id", "agg": "count"}},
		{"id": "cities", "measure": {"field": "moves.stops.location.city", "agg": "count_distinct"}}
	], "sort": [{"column": "customer"}]}`)
	want(t, preview(t, a, d, nil, nil), "Acme | 4 | 10 | 3", "Globex | 4 | 10 | 3", "Initech | 4 | 8 | 2")
	want(t, preview(t, b, d, nil, nil), "Acme | 2 | 6 | 3")
	comp := must[*analytics.Compiled](t)(analytics.Compile(b.ctx, catalog(t), nil, d, nil, analytics.CompileOptions{}))
	sql, args, err := comp.ToSQL(b.ctx, db(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{`"shipments" s`, `"customers" c`, `"moves" m`, `"stops" s`, `"locations" l`} {
		at := strings.Index(sql, table)
		if at < 0 || !strings.Contains(sql[at:min(len(sql), at+300)], `."organization_id" = $`) {
			t.Fatalf("%s is not scoped:\n%s", table, sql)
		}
	}
	if args[0] != b.org && args[1] != b.org {
		t.Fatalf("args %v", args)
	}
}
