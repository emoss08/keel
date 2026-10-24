package analytics_test

import (
	"testing"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/schema"
)

// TestEdgesJoinEveryKeyColumn reports across relations keyed by two columns: an entry that names
// a leave of another worker is not that leave's entry, through the belongs edge, the to-many edge
// and the pick.
func TestEdgesJoinEveryKeyColumn(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	org := string(tenantOf(ctx).org)
	id := func(s string) ids.ID { return ids.ID(s + "_" + org) }
	ok(t, fleetdb.Leaves.CreateMany(ctx, d, []*fleet.Leave{
		{ID: id("l1"), WorkerID: "w1", Days: 10}, {ID: id("l2"), WorkerID: "w2", Days: 2},
	}))
	ok(t, fleetdb.LeaveEntries.CreateMany(ctx, d, []*fleet.LeaveEntry{
		{ID: id("e1"), WorkerID: "w1", SourceLeaveID: id("l1"), Hours: 8, PostedAt: 1},
		{ID: id("e2"), WorkerID: "w1", SourceLeaveID: id("l1"), Hours: 4, PostedAt: 2},
		{ID: id("e3"), WorkerID: "w2", SourceLeaveID: id("l1"), Hours: 8, PostedAt: 3},
		{ID: id("e4"), WorkerID: "w2", SourceLeaveID: id("l2"), Hours: 2, PostedAt: 4},
	}))
	cat := must[*analytics.Catalog](t)(analytics.Build([]*schema.Table{fleetdb.LeavesSchema(),
		fleetdb.LeaveEntriesSchema()}, analytics.Options{}))
	r := must[*analytics.Runner](t)(analytics.NewRunner(d, cat, analytics.RunnerOptions{PreviewRows: 100}))
	run := func(js string) []string {
		res, err := r.Preview(ctx, analytics.Request{Definition: def(t, js)})
		if err != nil {
			t.Fatal(err)
		}
		return rows(t, res)
	}
	want(t, run(`{"entity": "leave_entry", "columns": [
		{"id": "worker", "dimension": {"field": "source.workerID"}},
		{"id": "n", "measure": {"agg": "count"}}], "sort": [{"column": "worker"}]}`),
		"w1 | 2", "w2 | 1", "NULL | 1")
	want(t, run(`{"entity": "leave", "columns": [
		{"id": "worker", "dimension": {"field": "workerID"}},
		{"id": "hours", "measure": {"field": "entries.hours", "agg": "sum"}},
		{"id": "last", "measure": {"field": "lastEntry.hours", "agg": "max"}}], "sort": [{"column": "worker"}]}`),
		"w1 | 12 | 4", "w2 | 2 | 2")
}
