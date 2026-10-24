package keel_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

// leaveFixture creates two leaves and five entries; e3 names l1 but belongs to another worker,
// so it is not an entry of l1, and e5 draws on no leave. IDs end with the tenant (see uid), so
// tenants never share keys, as with generated IDs.
func leaveFixture(t *testing.T) (context.Context, *keel.DB) {
	t.Helper()
	d := db(t)
	ctx := newTenant()
	org, _ := tenantOf(ctx)
	id := func(s string) ids.ID { return uid(s, org) }
	ok(t, fleetdb.Leaves.CreateMany(ctx, d, []*fleet.Leave{
		{ID: id("l1"), WorkerID: "w1", Days: 10}, {ID: id("l2"), WorkerID: "w2", Days: 2},
	}))
	ok(t, fleetdb.LeaveEntries.CreateMany(ctx, d, []*fleet.LeaveEntry{
		{ID: id("e1"), WorkerID: "w1", SourceLeaveID: id("l1"), Hours: 8, PostedAt: 1},
		{ID: id("e2"), WorkerID: "w1", SourceLeaveID: id("l1"), Hours: 4, PostedAt: 2},
		{ID: id("e3"), WorkerID: "w2", SourceLeaveID: id("l1"), Hours: 8, PostedAt: 3},
		{ID: id("e4"), WorkerID: "w2", SourceLeaveID: id("l2"), Hours: 2, PostedAt: 4},
		{ID: id("e5"), WorkerID: "w1", Hours: 1, PostedAt: 5},
	}))
	return ctx, d
}

// uid is the ID s of the tenant org.
func uid(s string, org ids.ID) ids.ID { return ids.ID(s + "_" + string(org)) }

// short is an ID without its tenant suffix.
func short(id ids.ID) string {
	s, _, _ := strings.Cut(string(id), "_")
	return s
}

// entryIDs joins the short IDs of entries in order.
func entryIDs(es []*fleet.LeaveEntry) string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = short(e.ID)
	}
	return strings.Join(out, ",")
}

func TestRelationKeysBelongsTo(t *testing.T) {
	ctx, d := leaveFixture(t)
	e := fleetdb.LeaveEntries
	rows := must(e.Load(e.Source()).Order(e.ID.Asc()).All(ctx, d))(t)
	var sources []string
	for _, r := range rows {
		s := "-"
		if r.Source != nil {
			s = short(r.Source.ID)
		}
		sources = append(sources, s)
	}
	eq(t, strings.Join(sources, ","), "l1,l1,-,l2,-")

	eq(t, entryIDs(must(e.Where(e.Source().Days.Eq(10)).Order(e.ID.Asc()).All(ctx, d))(t)), "e1,e2")
	sql, _ := must2(e.Where(e.Source().Days.Eq(10)).ToSQL(ctx, d))(t)
	if !strings.Contains(sql, `s."id" = le."source_leave_id" AND s."worker_id" = le."worker_id"`) {
		t.Fatalf("join misses a key pair: %s", sql)
	}

	n := must(e.Where(e.Source().Days.Eq(10)).Update(ctx, d, e.Hours.To(1)))(t)
	eq(t, n, 2)
	n = must(e.Where(e.Source().Days.Eq(2)).Delete(ctx, d))(t)
	eq(t, n, 1)
}

func TestRelationKeysToMany(t *testing.T) {
	ctx, d := leaveFixture(t)
	l := fleetdb.Leaves
	en := l.Entries()
	rows := must(l.Load(en.Order(en.PostedAt.Asc())).Order(l.ID.Asc()).All(ctx, d))(t)
	eq(t, len(rows), 2)
	eq(t, entryIDs(rows[0].Entries), "e1,e2")
	eq(t, entryIDs(rows[1].Entries), "e4")

	limited := must(l.Load(en.Order(en.PostedAt.Desc()).Limit(1)).Order(l.ID.Asc()).All(ctx, d))(t)
	eq(t, entryIDs(limited[0].Entries), "e2")
	eq(t, entryIDs(limited[1].Entries), "e4")

	eq(t, must(l.Where(en.Any(en.PostedAt.Eq(3))).Count(ctx, d))(t), 0)
	eq(t, must(l.Where(en.Any(en.Hours.Eq(8))).Count(ctx, d))(t), 1)
	eq(t, must(l.Where(en.None()).Count(ctx, d))(t), 0)
}

func TestRelationKeysPick(t *testing.T) {
	ctx, d := leaveFixture(t)
	l := fleetdb.Leaves
	rows := must(l.Load(l.LastEntry()).Order(l.ID.Asc()).All(ctx, d))(t)
	eq(t, short(rows[0].LastEntry.ID), "e2")
	eq(t, short(rows[1].LastEntry.ID), "e4")

	org, _ := tenantOf(ctx)
	one := must(l.Load(l.LastEntry()).Where(l.ID.Eq(uid("l1", org))).One(ctx, d))(t)
	eq(t, short(one.LastEntry.ID), "e2")

	eq(t, must(l.Where(l.LastEntry().Hours.Eq(4)).Count(ctx, d))(t), 1)
	eq(t, must(l.Where(l.LastEntry().Any(l.LastEntry().PostedAt.Eq(3))).Count(ctx, d))(t), 0)
}

func TestRelationKeysSystemScope(t *testing.T) {
	ctx, d := leaveFixture(t)
	org, bu := tenantOf(ctx)
	sys := systemCtx("relation keys")
	l := fleetdb.Leaves
	rows := must(l.AllTenants().Where(l.OrganizationID.Eq(org), l.BusinessUnitID.Eq(bu)).
		Load(l.Entries(), l.LastEntry()).Order(l.ID.Asc()).All(sys, d))(t)
	eq(t, entryIDs(rows[0].Entries), "e1,e2")
	eq(t, short(rows[0].LastEntry.ID), "e2")
}

func TestRelationKeysDynamic(t *testing.T) {
	ctx, d := leaveFixture(t)
	reg := must(keel.NewRegistry(fleetdb.LeavesSchema(), fleetdb.LeaveEntriesSchema()))(t)

	q := reg.Query("leave_entries")
	q.Select(q.Path("id"), q.Path("source", "days")).Order(q.Path("id").Asc())
	res := must(q.Run(ctx, d))(t)
	days := res.Column("source.days")
	eq(t, days.IsNull(2), true)
	eq(t, days.Int64[0], int64(10))

	lv := reg.Query("leaves")
	ents := lv.Rel("entries")
	lv.Select(lv.Path("id")).Where(ents.Any(ents.Col("posted_at").Eq(3)))
	eq(t, must(lv.Count(ctx, d))(t), 0)

	for _, s := range []keel.PreAggStrategy{keel.PreAggCTE, keel.PreAggLateral} {
		lv := reg.Query("leaves")
		pa := lv.Rel("entries").PreAggregate(s)
		n := pa.Measure(keel.DynCount().As("entries"))
		lv.Select(lv.Path("id"), n).Where(lv.Path("days").Gt(0)).Order(lv.Path("id").Asc())
		res := must(lv.Run(ctx, d))(t)
		got := []string{}
		for i := range res.Rows {
			got = append(got, short(ids.ID(res.Columns[0].String[i]))+"="+string(rune('0'+res.Columns[1].Int64[i])))
		}
		sort.Strings(got)
		eq(t, strings.Join(got, ","), "l1=2,l2=1")
	}
}

func TestRelationKeysTypedIDs(t *testing.T) {
	ctx, d := leaveFixture(t)
	e := fleetdb.LeaveEntries
	got := must(e.Where(e.SourceLeaveID.Eq(ids.ID(""))).Order(e.ID.Asc()).All(ctx, d))(t)
	eq(t, entryIDs(got), "e5")
}
