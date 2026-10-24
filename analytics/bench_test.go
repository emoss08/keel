package analytics_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/ir"
)

// typicalReport is a grouped report of the kind a builder makes: two dimensions across a
// relation and a month bucket, measures with a filter and across a to-many path, a formula, a
// window, a total, a filter and a sort.
const typicalReport = `{"entity": "shipment", "columns": [
	{"id": "customer", "dimension": {"field": "customer.name"}},
	{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
	{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
	{"id": "shipments", "measure": {"agg": "count"}},
	{"id": "hazmat", "measure": {"agg": "count", "filter": {"field": "hazmat", "op": "eq", "value": true}}},
	{"id": "moves", "measure": {"field": "moves.id", "agg": "count"}},
	{"id": "perMove", "calc": "round([revenue] / nullif([moves], 0), 2)"},
	{"id": "share", "window": {"of": "revenue", "fn": "percent_of_total", "partition": ["month"]}}
], "filters": {"field": "status", "op": "not_in", "value": ["Canceled"]},
"groupings": {"total": true}, "sort": [{"column": "revenue", "desc": true}]}`

// BenchmarkCompile measures compiling a typical report and rendering its statement.
func BenchmarkCompile(b *testing.B) {
	cat := catalog(b)
	d := def(b, typicalReport)
	render, err := keel.Open(context.Background(), testConfig("postgres://keel:keel@127.0.0.1:1/none"))
	if err != nil {
		b.Fatal(err)
	}
	defer render.Close()
	ctx := tenantCtx("org_1", "bu_1")
	b.Run("compile", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := analytics.Compile(ctx, cat, nil, d, nil, analytics.CompileOptions{MaxRows: 100}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("compile+sql", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			comp, err := analytics.Compile(ctx, cat, nil, d, nil, analytics.CompileOptions{MaxRows: 100})
			if err != nil {
				b.Fatal(err)
			}
			if _, _, err := comp.ToSQL(ctx, render); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// bigWorld is a tenant with 100,000 shipments across 50 customers and two years.
var (
	bigOnce  sync.Once
	bigCtx   context.Context
	bigError error
)

// seedBig creates the 100,000-shipment tenant once.
func seedBig(b testing.TB) context.Context {
	d := db(b)
	bigOnce.Do(func() {
		bigCtx = newTenant()
		tn := tenantOf(bigCtx)
		_, bigError = d.Pool().Exec(context.Background(), `
			INSERT INTO customers (id, organization_id, business_unit_id, name, code, created_at, updated_at)
			SELECT 'cus_' || i, $1, $2, 'Customer ' || i, 'C' || i, 0, 0 FROM generate_series(1, 50) i`, tn.org, tn.bu)
		if bigError != nil {
			return
		}
		_, bigError = d.Pool().Exec(context.Background(), `
			INSERT INTO shipments (id, organization_id, business_unit_id, customer_id, pro_number, status, priority,
				weight, total, hazmat, created_at, updated_at)
			SELECT 'shp_' || i, $1, $2, 'cus_' || (i % 50 + 1), 'P' || i,
				(ARRAY['New','Assigned','InTransit','Delivered','Canceled'])[i % 5 + 1]::shipment_status, i % 3,
				1000 + i % 9000, (i % 100000) / 100.0, i % 7 = 0, 1735689600 + (i * 631)::bigint, 0
			FROM generate_series(1, 100000) i`, tn.org, tn.bu)
		if bigError != nil {
			return
		}
		_, bigError = d.Pool().Exec(context.Background(), `
			INSERT INTO moves (id, organization_id, business_unit_id, shipment_id, sequence, status, loaded, created_at)
			SELECT 'mov_' || i || '_' || k, $1, $2, 'shp_' || i, k, 'Assigned', k = 1, 0
			FROM generate_series(1, 100000) i, generate_series(1, 2) k`, tn.org, tn.bu)
		if bigError != nil {
			return
		}
		_, bigError = d.Pool().Exec(context.Background(), "ANALYZE shipments; ANALYZE moves; ANALYZE customers")
	})
	if bigError != nil {
		b.Fatal(bigError)
	}
	return bigCtx
}

// handWritten is the SQL a person writes for the grouped report in BenchmarkPreview, with the
// moves pre-aggregated as the compiler does.
const handWritten = `
WITH m AS (
	SELECT shipment_id, count(*) AS moves FROM moves
	WHERE organization_id = $1 AND business_unit_id = $2 GROUP BY shipment_id)
SELECT c.name, extract(epoch from date_trunc('month', to_timestamp(s.created_at), 'UTC'))::bigint AS month,
	sum(s.total), count(*), coalesce(sum(m.moves), 0)::bigint
FROM shipments s
LEFT JOIN customers c ON c.id = s.customer_id AND c.organization_id = $1 AND c.business_unit_id = $2
LEFT JOIN m ON m.shipment_id = s.id
WHERE s.organization_id = $1 AND s.business_unit_id = $2 AND s.status <> 'Canceled'
GROUP BY 1, 2 ORDER BY 3 DESC LIMIT 101`

// BenchmarkPreview measures a preview of a grouped report over 100,000 shipments through the
// runner (no cache, no EXPLAIN) and the equivalent hand-written statement through pgx.
func BenchmarkPreview(b *testing.B) {
	ctx := seedBig(b)
	d := def(b, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
		{"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
		{"id": "shipments", "measure": {"agg": "count"}},
		{"id": "moves", "measure": {"field": "moves.id", "agg": "count"}}
	], "filters": {"field": "status", "op": "ne", "value": "Canceled"}, "sort": [{"column": "revenue", "desc": true}]}`)
	r := runner(b, analytics.RunnerOptions{PreviewRows: 100, Primary: true})
	b.Run("analytics", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			res, err := r.Preview(ctx, analytics.Request{Definition: d})
			if err != nil {
				b.Fatal(err)
			}
			if res.Len() != 100 {
				b.Fatalf("%d rows", res.Len())
			}
		}
	})
	tn := tenantOf(ctx)
	b.Run("pgx", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			rows, err := db(b).Pool().Query(context.Background(), handWritten, tn.org, tn.bu)
			if err != nil {
				b.Fatal(err)
			}
			n := 0
			for rows.Next() {
				var name string
				var month, count, moves int64
				var revenue keel.Decimal
				if err := rows.Scan(&name, &month, &revenue, &count, &moves); err != nil {
					b.Fatal(err)
				}
				n++
			}
			if rows.Err() != nil || n != 101 {
				b.Fatalf("%d rows: %v", n, rows.Err())
			}
		}
	})
}

// BenchmarkGuards measures what the guards add to a preview: EXPLAIN on a cold plan cache, and
// a cache hit.
func BenchmarkGuards(b *testing.B) {
	ctx := seedBig(b)
	d := def(b, typicalReport)
	for name, opts := range map[string]analytics.RunnerOptions{
		"explain-cached": {MaxCost: 1e12, PreviewRows: 100, Primary: true},
		"cache-hit":      {Cache: newMemCache(), PreviewRows: 100, Primary: true},
	} {
		r := runner(b, opts)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.Preview(ctx, analytics.Request{Definition: d}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	_ = fmt.Sprint(ir.Version)
}
