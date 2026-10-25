package keel_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// prepared renders the statements item's first round trip sends.
func prepared(t *testing.T, ctx context.Context, d *keel.DB, item keel.BatchItem) []keel.Statement {
	t.Helper()
	stmts, err := keel.PreparedStatements(ctx, d, item)
	ok(t, err)
	return stmts
}

// contains fails the test unless sql holds part.
func contains(t *testing.T, sql, part string) {
	t.Helper()
	if !strings.Contains(sql, part) {
		t.Fatalf("statement\n%s\ndoes not contain\n%s", sql, part)
	}
}

// renderDBWith is renderDB with an edited configuration.
func renderDBWith(t *testing.T, edit func(cfg *keel.Config)) *keel.DB {
	t.Helper()
	cfg := testConfig("postgres://keel:keel@127.0.0.1:1/none")
	edit(&cfg)
	d, err := keel.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func TestPipelinedLoadKeysRepeatTheParentStatement(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	m := s.Moves()
	var rows []*fleet.Shipment
	stmts := prepared(t, ctx, d, keel.ListInto(s.
		Where(s.ProNumber.Eq("PRO-1")).
		Load(m.Order(m.Sequence.Asc()).Load(m.Stops())).
		Order(s.ProNumber.Asc()).
		Limit(10), &rows))
	eq(t, len(stmts), 3)
	if !strings.HasSuffix(stmts[0].SQL, `FROM "shipments" s WHERE s."organization_id" = $1 AND `+
		`s."business_unit_id" = $2 AND s."pro_number" = $3 ORDER BY s."pro_number" ASC, s."id" LIMIT 10`) {
		t.Fatalf("root statement without its primary key tie-break:\n%s", stmts[0].SQL)
	}
	if !strings.HasSuffix(stmts[1].SQL, `FROM "moves" m WHERE m."organization_id" = $1 AND `+
		`m."business_unit_id" = $2 AND m."shipment_id" IN (SELECT s."id" FROM "shipments" s `+
		`WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND s."pro_number" = $3 `+
		`ORDER BY s."pro_number" ASC, s."id" LIMIT 10) ORDER BY m."sequence" ASC`) {
		t.Fatalf("moves statement:\n%s", stmts[1].SQL)
	}
	if !strings.HasSuffix(stmts[2].SQL, `FROM "stops" s WHERE s."organization_id" = $1 AND `+
		`s."business_unit_id" = $2 AND s."move_id" IN (SELECT m."id" FROM "moves" m `+
		`WHERE m."organization_id" = $1 AND m."business_unit_id" = $2 AND m."shipment_id" IN `+
		`(SELECT s2."id" FROM "shipments" s2 WHERE s2."organization_id" = $1 AND `+
		`s2."business_unit_id" = $2 AND s2."pro_number" = $3 ORDER BY s2."pro_number" ASC, s2."id" LIMIT 10))`) {
		t.Fatalf("stops statement:\n%s", stmts[2].SQL)
	}
	for _, st := range stmts[1:] {
		eq(t, fmt.Sprint(st.Args), "[org_1 bu_1 PRO-1]")
		if strings.Contains(st.SQL, "ANY(") || strings.Contains(st.SQL, "unnest(") {
			t.Fatalf("pipelined load binds keys:\n%s", st.SQL)
		}
	}
}

func TestPipelinedLoadKeysUnderASystemScopeCarryTheTenant(t *testing.T) {
	d := renderDB(t)
	s := fleetdb.Shipments
	m := s.Moves()
	var rows []*fleet.Shipment
	stmts := prepared(t, systemCtx("report"), d, keel.AllInto(s.AllTenants().Load(m.Stops()), &rows))
	eq(t, len(stmts), 3)
	if !strings.HasSuffix(stmts[1].SQL, `FROM "moves" m WHERE (m."organization_id", m."business_unit_id", `+
		`m."shipment_id") IN (SELECT s."organization_id", s."business_unit_id", s."id" FROM "shipments" s)`) {
		t.Fatalf("moves statement:\n%s", stmts[1].SQL)
	}
	if !strings.HasSuffix(stmts[2].SQL, `FROM "stops" s WHERE (s."organization_id", s."business_unit_id", `+
		`s."move_id") IN (SELECT m."organization_id", m."business_unit_id", m."id" FROM "moves" m `+
		`WHERE (m."organization_id", m."business_unit_id", m."shipment_id") IN `+
		`(SELECT s2."organization_id", s2."business_unit_id", s2."id" FROM "shipments" s2))`) {
		t.Fatalf("stops statement:\n%s", stmts[2].SQL)
	}
	eq(t, len(stmts[1].Args), 0)
}

func TestPipelinedManyToManyLimitAndPickLoads(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	m := s.Moves()
	var rows []*fleet.Shipment
	rootKeys := `(SELECT s."id" FROM "shipments" s WHERE s."organization_id" = $1 AND s."business_unit_id" = $2)`
	distinctKeys := `FROM (SELECT DISTINCT * FROM ` + rootKeys + ` kq) AS k(k) CROSS JOIN LATERAL (`

	tags := prepared(t, ctx, d, keel.AllInto(s.Load(s.Tags()).Order(s.ProNumber.Asc()), &rows))
	eq(t, len(tags), 2)
	contains(t, tags[1].SQL, `."shipment_id" IN `+rootKeys)

	limited := prepared(t, ctx, d, keel.AllInto(s.Load(m.Order(m.Sequence.Desc()).Limit(1)).Order(s.ProNumber.Asc()), &rows))
	eq(t, len(limited), 2)
	contains(t, limited[1].SQL, distinctKeys)
	contains(t, limited[1].SQL, `ORDER BY m."sequence" DESC LIMIT 1) lat`)

	pick := prepared(t, ctx, d, keel.AllInto(s.Load(s.OriginStop()).Order(s.ProNumber.Asc()), &rows))
	eq(t, len(pick), 2)
	if !strings.HasPrefix(pick[1].SQL, `SELECT k.k, os."id"`) {
		t.Fatalf("pick statement:\n%s", pick[1].SQL)
	}
	contains(t, pick[1].SQL, distinctKeys)

	deeper := prepared(t, ctx, d, keel.AllInto(s.
		Load(m.Order(m.Sequence.Desc()).Limit(1).Load(m.Stops())).
		Order(s.ProNumber.Asc()), &rows))
	eq(t, len(deeper), 3)
	contains(t, deeper[1].SQL, `ORDER BY m."sequence" DESC, m."id" LIMIT 1) lat`)
	contains(t, deeper[2].SQL, `s."move_id" IN (SELECT lat.* FROM (SELECT DISTINCT * FROM `+
		`(SELECT s2."id" FROM "shipments" s2 WHERE s2."organization_id" = $1 AND s2."business_unit_id" = $2) kq) `+
		`AS k(k) CROSS JOIN LATERAL (SELECT m."id" FROM "moves" m WHERE m."organization_id" = $1 AND `+
		`m."business_unit_id" = $2 AND m."shipment_id" = k.k ORDER BY m."sequence" DESC, m."id" LIMIT 1) lat)`)
}

func TestPipelinedLoadsFallBackPerLevel(t *testing.T) {
	d := renderDB(t)
	ctx := tenantCtx("org_1", "bu_1")
	s := fleetdb.Shipments
	m := s.Moves()
	var rows []*fleet.Shipment
	var one *fleet.Shipment
	var page keel.Page[*fleet.Shipment]
	cases := []struct {
		name  string
		item  keel.BatchItem
		stmts int
	}{
		{"First without an order", keel.FirstInto(s.Load(m), &one), 1},
		{"Limit without an order", keel.ListInto(s.Load(m).Limit(5), &rows), 1},
		{"Offset without an order", keel.AllInto(s.Load(m).Offset(5), &rows), 1},
		{"a lock", keel.AllInto(s.Load(m).Lock(keel.ForUpdate), &rows), 1},
		{"DISTINCT ON", keel.AllInto(s.Load(m).DistinctOn(s.CustomerID).Order(s.CustomerID.Asc()), &rows), 1},
		{"the query opts out", keel.AllInto(s.LoadStrategy(keel.LoadPerLevel).Load(m), &rows), 1},
		{"the load opts out", keel.AllInto(s.Load(m.LoadStrategy(keel.LoadPerLevel)), &rows), 1},
		{"below a per-parent Limit without an order", keel.AllInto(s.Load(m.Limit(1).Load(m.Stops())), &rows), 2},
		{"One without an order", keel.OneInto(s.Where(s.ID.Eq("shp_1")).Load(m), &one), 2},
		{"Limit with an order", keel.ListInto(s.Load(m).Order(s.Status.Asc()).Limit(5), &rows), 2},
		{"a page", keel.PageInto(s.Order(s.ProNumber.Asc()).Load(m), keel.After("", 5), &page), 2},
		{"an opt-in below an opt-out", keel.AllInto(s.LoadStrategy(keel.LoadPerLevel).
			Load(m.LoadStrategy(keel.LoadPipelined).Load(m.Stops())), &rows), 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eq(t, len(prepared(t, ctx, d, c.item)), c.stmts)
		})
	}

	perLevel := renderDBWith(t, func(cfg *keel.Config) { cfg.LoadStrategy = keel.LoadPerLevel })
	eq(t, len(prepared(t, ctx, perLevel, keel.AllInto(s.Load(m), &rows))), 1)
	eq(t, len(prepared(t, ctx, perLevel, keel.AllInto(s.LoadStrategy(keel.LoadPipelined).Load(m), &rows))), 2)

	pg := prepared(t, ctx, d, keel.PageInto(s.Order(s.ProNumber.Asc()).Load(m), keel.After("", 5), &page))
	contains(t, pg[1].SQL, `ORDER BY s."pro_number" ASC, s."id" ASC, s."organization_id" ASC, `+
		`s."business_unit_id" ASC LIMIT 6)`)
}

// shipmentTree describes rows and every relation loaded into them, with to-many relations in key
// order, so two reads that loaded the same tree describe it the same way.
func shipmentTree(rows []*fleet.Shipment) string {
	var b strings.Builder
	for _, s := range rows {
		fmt.Fprintf(&b, "%s/%s", s.OrganizationID, s.ProNumber)
		if s.Customer != nil {
			fmt.Fprintf(&b, " customer=%s", s.Customer.Code)
		}
		if s.BillTo != nil {
			fmt.Fprintf(&b, " billto=%s", s.BillTo.Code)
		}
		if s.OriginStop != nil {
			fmt.Fprintf(&b, " origin=%s", s.OriginStop.ID)
		}
		moves := slices.Clone(s.Moves)
		slices.SortFunc(moves, func(a, b *fleet.Move) int { return strings.Compare(string(a.ID), string(b.ID)) })
		for _, mv := range moves {
			fmt.Fprintf(&b, " move=%s/%d/%s", mv.ID, mv.Sequence, mv.ShipmentID)
			if mv.FirstStop != nil {
				fmt.Fprintf(&b, " first=%s", mv.FirstStop.ID)
			}
			stops := slices.Clone(mv.Stops)
			slices.SortFunc(stops, func(a, b *fleet.Stop) int { return strings.Compare(string(a.ID), string(b.ID)) })
			for _, st := range stops {
				fmt.Fprintf(&b, " stop=%s/%s", st.ID, st.MoveID)
				if st.Location != nil {
					fmt.Fprintf(&b, "@%s", st.Location.City)
				}
			}
		}
		var tags []string
		for _, tg := range s.Tags {
			tags = append(tags, string(tg.ID))
		}
		slices.Sort(tags)
		fmt.Fprintf(&b, " tags=%v\n", tags)
	}
	return b.String()
}

// compareStrategies reads build's query pipelined and per level and fails unless both load the
// same tree and the pipelined read takes trips round trips.
func compareStrategies(t *testing.T, ctx context.Context, d *keel.DB, rec *recorder, trips int,
	build func() *keel.Query[*fleet.Shipment],
) []*fleet.Shipment {
	t.Helper()
	rec.reset()
	piped := must(build().All(ctx, d))(t)
	eq(t, rec.trips(), trips)
	pipedStmts := rec.statements()
	rec.reset()
	perLevel := must(build().LoadStrategy(keel.LoadPerLevel).All(ctx, d))(t)
	eq(t, rec.statements(), pipedStmts)
	eq(t, shipmentTree(piped), shipmentTree(perLevel))
	return piped
}

func TestPipelinedLoadsMatchPerLevel(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	w := seed(t, 8)
	s := fleetdb.Shipments
	m := s.Moves()

	rows := compareStrategies(t, w.ctx, d, rec, 1, func() *keel.Query[*fleet.Shipment] {
		return s.Load(s.Customer(), s.BillTo(), m.Order(m.Sequence.Desc()).Load(m.FirstStop()),
			m.Stops().Location(), s.Tags(), s.OriginStop()).Order(s.ProNumber.Asc())
	})
	eq(t, len(rows), 8)
	eq(t, len(rows[0].Moves), 2)
	eq(t, rows[0].Moves[0].Sequence, int32(1))
	eq(t, len(rows[0].Moves[0].Stops), 2)
	eq(t, rows[0].Moves[0].Stops[0].Location != nil, true)
	eq(t, len(rows[0].Tags), 2)
	eq(t, rows[0].OriginStop != nil, true)
	eq(t, len(rows[7].Moves), 0)

	limited := compareStrategies(t, w.ctx, d, rec, 1, func() *keel.Query[*fleet.Shipment] {
		return s.Load(m.Stops()).Order(s.Status.Asc(), s.ProNumber.Asc()).Limit(5)
	})
	eq(t, len(limited), 5)

	compareStrategies(t, w.ctx, d, rec, 1, func() *keel.Query[*fleet.Shipment] {
		return s.Where(s.Hazmat.IsFalse()).
			Load(m.Where(m.Loaded.IsTrue()).Order(m.Sequence.Desc()).Limit(1).Load(m.Stops())).
			Order(s.ProNumber.Desc()).Offset(1).Limit(4)
	})

	compareStrategies(t, w.ctx, d, rec, 2, func() *keel.Query[*fleet.Shipment] {
		return s.Load(m.Where(m.Sequence.Eq(0)).Limit(1).Load(m.Stops().Location())).Order(s.ProNumber.Asc())
	})

	compareStrategies(t, w.ctx, d, rec, 2, func() *keel.Query[*fleet.Shipment] {
		return s.Where(s.ProNumber.In("PRO-0000", "PRO-0001")).Load(m.Stops()).Limit(3)
	})
}

func TestPipelinedLoadsRunInOneSnapshot(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	w := seed(t, 4)
	s := fleetdb.Shipments
	m := s.Moves()
	rr := keel.Raw("current_setting('transaction_isolation') = ?", "repeatable read")

	rec.reset()
	rows := must(s.Where(rr).Load(m.Where(rr), m.Stops()).Order(s.ProNumber.Asc()).All(w.ctx, d))(t)
	eq(t, len(rows), 4)
	eq(t, len(rows[0].Moves), 2)
	eq(t, len(rows[0].Moves[0].Stops), 2)
	eq(t, rec.trips(), 1)
	eq(t, rec.statements(), 3)

	ok(t, keel.Read(w.ctx, d, func(ctx context.Context) error {
		rec.reset()
		got := must(s.Load(m.Stops()).Order(s.ProNumber.Asc()).All(ctx, d))(t)
		eq(t, len(got[0].Moves[0].Stops), 2)
		eq(t, rec.trips(), 1)
		return nil
	}, keel.RepeatableRead))

	ok(t, keel.Read(w.ctx, d, func(ctx context.Context) error {
		rec.reset()
		got := must(s.Load(m.Stops()).Order(s.ProNumber.Asc()).All(ctx, d))(t)
		eq(t, len(got[0].Moves[0].Stops), 2)
		eq(t, rec.trips(), 3)
		return nil
	}, keel.ReadCommitted))

	ok(t, keel.Write(w.ctx, d, func(ctx context.Context) error {
		rec.reset()
		got := must(s.Load(m.Stops()).Order(s.ProNumber.Asc()).All(ctx, d))(t)
		eq(t, len(got[0].Moves), 2)
		eq(t, rec.trips(), 3)
		return nil
	}))

	ok(t, keel.Write(w.ctx, d, func(ctx context.Context) error {
		rec.reset()
		got := must(s.Load(m.Stops()).Order(s.ProNumber.Asc()).All(ctx, d))(t)
		eq(t, len(got[0].Moves), 2)
		eq(t, rec.trips(), 1)
		return nil
	}, keel.Serializable))

	repeatable := openDB(t, func(cfg *keel.Config) {
		cfg.Observers = []keel.Observer{rec}
		cfg.ReadIsolation = keel.RepeatableRead
	})
	ok(t, keel.Read(w.ctx, repeatable, func(ctx context.Context) error {
		rec.reset()
		_ = must(s.Load(m.Stops()).Order(s.ProNumber.Asc()).All(ctx, repeatable))(t)
		eq(t, rec.trips(), 1)
		return nil
	}))
}

func TestPipelinedLoadsInAnImplicitTransactionWithOnBegin(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	w := seed(t, 3)
	s := fleetdb.Shipments
	m := s.Moves()
	org := string(w.shipments[0].OrganizationID)
	inTx := keel.And(
		keel.Raw("current_setting('keel.org') = ?", org),
		keel.Raw("current_setting('transaction_isolation') = ?", "repeatable read"),
		keel.Raw("current_setting('transaction_read_only') = ?", "on"),
	)
	rec.reset()
	rows := must(s.Where(inTx).Load(m.Where(inTx), m.Stops()).Order(s.ProNumber.Asc()).All(w.ctx, d))(t)
	eq(t, len(rows), 3)
	eq(t, len(rows[0].Moves), 2)
	eq(t, rec.trips(), 1)
}

func TestPipelinedLoadsUnderASystemScope(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	a, b := seed(t, 4), seed(t, 4)
	s := fleetdb.Shipments
	m := s.Moves()
	sys := systemCtx("report")
	orgA, orgB := a.shipments[0].OrganizationID, b.shipments[0].OrganizationID
	rows := compareStrategies(t, sys, d, rec, 1, func() *keel.Query[*fleet.Shipment] {
		return s.AllTenants().
			Where(keel.Or(s.OrganizationID.Eq(orgA), s.OrganizationID.Eq(orgB))).
			Load(s.Customer(), m.Order(m.Sequence.Asc()).Load(m.FirstStop()), m.Stops().Location(), s.Tags(),
				s.OriginStop()).
			Order(s.ProNumber.Asc(), s.OrganizationID.Asc())
	})
	eq(t, len(rows), 8)
	for _, r := range rows {
		for _, mv := range r.Moves {
			eq(t, mv.OrganizationID, r.OrganizationID)
			eq(t, mv.ShipmentID, r.ID)
			for _, st := range mv.Stops {
				eq(t, st.OrganizationID, r.OrganizationID)
			}
		}
		for _, tg := range r.Tags {
			eq(t, tg.OrganizationID, r.OrganizationID)
		}
	}
	eq(t, len(rows[0].Moves), 2)
	eq(t, len(rows[0].Tags), 2)
}

func TestPipelinedBatchAndPage(t *testing.T) {
	rec := &recorder{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{rec} })
	w := seed(t, 8)
	s, mv := fleetdb.Shipments, fleetdb.Moves
	m := s.Moves()

	var ships []*fleet.Shipment
	var moves []*fleet.Move
	var first *fleet.Shipment
	rec.reset()
	ok(t, keel.Batch(w.ctx, d,
		keel.AllInto(s.Load(m.Stops(), s.Tags()).Order(s.ProNumber.Asc()), &ships),
		keel.ListInto(mv.Where(mv.Sequence.Eq(0)).Load(mv.Stops(), mv.Shipment()).Order(mv.ID.Asc()).Limit(3), &moves),
		keel.FirstInto(s.Load(m).Order(s.ProNumber.Desc()), &first),
	))
	eq(t, rec.trips(), 1)
	eq(t, rec.statements(), 8)
	eq(t, len(ships[0].Moves[0].Stops), 2)
	eq(t, len(ships[0].Tags), 2)
	eq(t, len(moves), 3)
	for _, x := range moves {
		eq(t, len(x.Stops), 2)
		eq(t, x.Shipment != nil, true)
	}
	eq(t, first.ProNumber, "PRO-0007")

	var pages, perLevel []string
	for _, strategy := range []keel.LoadStrategy{keel.LoadPipelined, keel.LoadPerLevel} {
		cursor := ""
		var got []string
		for {
			rec.reset()
			p := must(s.Order(s.Status.Asc()).Load(m.Stops()).LoadStrategy(strategy).
				Page(w.ctx, d, keel.After(cursor, 3)))(t)
			if strategy == keel.LoadPipelined {
				eq(t, rec.trips(), 1)
			}
			got = append(got, shipmentTree(p.Items))
			if p.Next == "" {
				break
			}
			cursor = p.Next
		}
		if strategy == keel.LoadPipelined {
			pages = got
		} else {
			perLevel = got
		}
	}
	eq(t, strings.Join(pages, "|"), strings.Join(perLevel, "|"))
	eq(t, len(pages), 3)
}
