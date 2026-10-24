package analytics_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
)

func TestAveragesOfInstants(t *testing.T) {
	w := seedWorld(t)
	report := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "mean", "measure": {"field": "createdAt", "agg": "avg"}},
		{"id": "earliest", "measure": {"field": "createdAt", "agg": "min"}},
		{"id": "latest", "measure": {"field": "createdAt", "agg": "max"}},
		{"id": "span", "calc": "[latest] - [earliest]"},
		{"id": "sinceFirst", "calc": "[mean] - [earliest]"},
		{"id": "arrived", "measure": {"field": "moves.stops.arrivedAt", "agg": "avg"}},
		{"id": "lag", "calc": "[arrived] - [mean]"}
	], "sort": [{"column": "customer"}]}`)
	res, err := runner(t, analytics.RunnerOptions{}).Preview(w.ctx, analytics.Request{Definition: report})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	err = keel.QueryRows(w.ctx, db(t), func(row pgx.CollectableRow) error {
		var name string
		var mean, earliest, latest int64
		var arrived time.Time
		var lag string
		if err := row.Scan(&name, &mean, &earliest, &latest, &arrived, &lag); err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s | %d | %d | %d | %d | %d | %s | %s", name, mean, earliest, latest,
			latest-earliest, mean-earliest, arrived.UTC().Format(time.RFC3339), lag))
		return nil
	}, `WITH s AS (
		SELECT c.name, round(avg(s.created_at))::bigint AS mean, min(s.created_at) AS earliest, max(s.created_at) AS latest
		  FROM shipments s JOIN customers c ON c.id = s.customer_id AND c.organization_id = s.organization_id
		 WHERE s.organization_id = $1 GROUP BY c.name),
	st AS (
		SELECT c.name, to_timestamp(avg(extract(epoch FROM st.arrived_at))) AS arrived
		  FROM stops st JOIN moves m ON m.id = st.move_id AND m.organization_id = $1
		  JOIN shipments s ON s.id = m.shipment_id AND s.organization_id = $1
		  JOIN customers c ON c.id = s.customer_id
		 WHERE st.organization_id = $1 AND s.organization_id = $1 AND c.organization_id = $1 GROUP BY c.name)
	SELECT s.name, s.mean, s.earliest, s.latest, st.arrived,
	       trim_scale(extract(epoch FROM (st.arrived - to_timestamp(s.mean))))::text
	  FROM s JOIN st USING (name) ORDER BY s.name`, w.org)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected rows: %v", lines)
	}
	want(t, rows(t, res), lines...)
	col := res.Columns()[1]
	if col.Type.String() != "epoch" {
		t.Fatalf("the mean of epochs is %s", col.Type)
	}
}
