package analytics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/analytics"
)

func TestToSQLIsReadableDeterministicAndBindsValuesOnce(t *testing.T) {
	w := seedWorld(t)
	cat := catalog(t)
	d := def(t, `{"entity": "shipment", "columns": [
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "big", "measure": {"agg": "count", "filter": {"field": "total", "op": "gte", "value": 300}}}
	], "filters": {"field": "total", "op": "gte", "value": 300}}`)
	chicago := location(t, "America/Chicago")
	first := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, cat, nil, d, nil, analytics.CompileOptions{Location: chicago}))
	second := must[*analytics.Compiled](t)(analytics.Compile(w.ctx, cat, nil, d, nil, analytics.CompileOptions{Location: chicago}))
	a, args, err := first.ToSQL(w.ctx, db(t))
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := second.ToSQL(w.ctx, db(t))
	if a != b {
		t.Fatalf("two compilations differ:\n%s\n%s", a, b)
	}
	start := strings.Index(a, "CAST(extract(epoch")
	bucket := a[start : start+strings.Index(a[start:], "AS bigint)")]
	if strings.Count(a, bucket) != 3 || !strings.Contains(a, "GROUP BY "+bucket) || !strings.Contains(a, "ORDER BY "+bucket) {
		t.Fatalf("the bucket is not written once per clause with shared parameters:\n%s", a)
	}
	if len(args) != 5 || args[4] != "America/Chicago" || strings.Count(a, `s."total" >= CAST($3::text AS numeric)`) != 2 {
		t.Fatalf("args %v:\n%s", args, a)
	}
	if !strings.Contains(a, `FROM "shipments" s WHERE s."organization_id" = $1 AND s."business_unit_id" = $2`) {
		t.Fatalf("not tenant scoped:\n%s", a)
	}
}

func TestCompileRefusesWhatItCannotAnswer(t *testing.T) {
	w := seedWorld(t)
	cat := catalog(t)
	for name, js := range map[string]string{
		"row calc over a to-many path": `{"entity": "shipment", "columns": [{"id": "x", "calc": "{moves.sequence} + 1"}]}`,
		"aggregate mixing levels": `{"entity": "shipment", "columns": [{"id": "c", "dimension": {"field": "customer.name"}},
			{"id": "x", "calc": "sum({total} + {moves.sequence})"}]}`,
		"distinct count of an expression across a path": `{"entity": "shipment", "columns": [{"id": "c", "dimension": {"field": "customer.name"}},
			{"id": "x", "calc": "count_distinct(lower({moves.status}))"}]}`,
	} {
		_, err := analytics.Compile(w.ctx, cat, nil, def(t, js), nil, analytics.CompileOptions{})
		if analytics.ErrorCode(err) != analytics.CodeNotAllowed {
			t.Errorf("%s: %v", name, err)
		}
	}
	params := def(t, `{"entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}],
		"filters": {"field": "status", "op": "in", "param": "statuses"},
		"parameters": [{"name": "statuses", "type": "enum", "multi": true, "required": true, "allowed": ["New", "Delivered"]}]}`)
	for name, p := range map[string]map[string]any{
		"missing":     nil,
		"not allowed": {"statuses": []any{"Canceled"}},
		"not a list":  {"statuses": "New"},
		"undeclared":  {"statuses": []any{"New"}, "other": 1},
	} {
		if _, err := analytics.Compile(w.ctx, cat, nil, params, p, analytics.CompileOptions{}); analytics.ErrorCode(err) != analytics.CodeParameter {
			t.Errorf("%s parameter: %v", name, err)
		}
	}
	want(t, preview(t, w, params, map[string]any{"statuses": []string{"New", "Delivered"}}, nil), "7")
}

func TestOptionalParametersDropTheirConditions(t *testing.T) {
	w := seedWorld(t)
	d := def(t, `{"entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}],
		"filters": {"or": [{"field": "customer.code", "op": "eq", "param": "code"}, {"field": "total", "op": "gt", "param": "min"}]},
		"parameters": [{"name": "code", "type": "string"}, {"name": "min", "type": "decimal"}]}`)
	want(t, preview(t, w, d, nil, nil), "12")
	want(t, preview(t, w, d, map[string]any{"code": "G"}, nil), "4")
	want(t, preview(t, w, d, map[string]any{"code": "G", "min": "500"}, nil), "5")
}

func TestRelativeDates(t *testing.T) {
	w := seedWorld(t)
	r := runner(t, analytics.RunnerOptions{})
	count := func(op string, value string, now time.Time, loc *time.Location) string {
		v := ""
		if value != "" {
			v = `, "value": ` + value
		}
		d := def(t, `{"entity": "shipment", "columns": [{"id": "n", "measure": {"agg": "count"}}],
			"filters": {"field": "createdAt", "op": "`+op+`"`+v+`}}`)
		res, err := r.Preview(w.ctx, analytics.Request{Definition: d, Location: loc, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return rows(t, res)[0]
	}
	feb2 := time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC)
	chicago := location(t, "America/Chicago")
	for _, c := range []struct {
		op, value string
		loc       *time.Location
		want      string
	}{
		{"this_month", "", time.UTC, "3"},
		{"this_month", "", chicago, "3"},
		{"last_month", "", time.UTC, "2"},
		{"last_month", "", chicago, "3"},
		{"yesterday", "", time.UTC, "2"},
		{"last_n_days", "3", time.UTC, "3"},
		{"this_year", "", time.UTC, "12"},
		{"this_quarter", "", time.UTC, "9"},
		{"next_n_days", "40", time.UTC, "5"},
	} {
		if got := count(c.op, c.value, feb2, c.loc); got != c.want {
			t.Errorf("%s %s in %s: %s, want %s", c.op, c.value, c.loc, got, c.want)
		}
	}
}
