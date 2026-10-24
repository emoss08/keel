package keel_test

// The examples in this file are compiled but not run: none has an Output comment. They are the
// compiled counterparts of the snippets in README.md and docs/guide.md, written against the fleet
// test models (internal/testmodels/fleet) and their generated package fleetdb.
//
// They share the scaffolding below, which stands in for an application's own setup: appDB is the
// *keel.DB the application opens once at startup, and requestContext returns the context of a
// request authenticated for one tenant.

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/migrate"
	"github.com/emoss08/keel/schema"
	"github.com/jackc/pgx/v5/pgxpool"
)

// appDB is the application's database, opened once at startup (see ExampleOpen).
var appDB *keel.DB

// scope is what the application's authentication middleware puts on a request context.
type scope struct {
	OrganizationID ids.ID
	BusinessUnitID ids.ID
	// SystemReason is set for work that is not done for one tenant: sweeps, migrations, reports.
	SystemReason string
}

type scopeKey struct{}

func withTenant(ctx context.Context, org, bu ids.ID) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{OrganizationID: org, BusinessUnitID: bu})
}

func withSystem(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{SystemReason: reason})
}

// requestContext returns the context of a request authenticated for one tenant.
func requestContext() context.Context {
	return withTenant(context.Background(), "org_acme", "bu_dallas")
}

func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// slowQueryLog is an Observer that logs statements slower than threshold.
type slowQueryLog struct{ threshold time.Duration }

func (slowQueryLog) BeforeQuery(ctx context.Context, _ *keel.QueryEvent) context.Context {
	return ctx
}

func (l slowQueryLog) AfterQuery(_ context.Context, e *keel.QueryEvent) {
	if e.Duration >= l.threshold {
		log.Printf("slow %s on %s (%d args, %d rows): %v: %s",
			e.Op, e.Table, e.Args, e.Rows, e.Duration, e.SQL)
	}
}

// traceKey carries a tracingObserver's span on a statement's context.
type traceKey struct{}

// statementSpan is the span of one statement.
type statementSpan struct {
	name  string
	start time.Time
}

// tracingObserver starts a span in BeforeQuery and ends the span it finds on the statement's
// context in AfterQuery, as a tracing library's observer would.
type tracingObserver struct{}

func (tracingObserver) BeforeQuery(ctx context.Context, e *keel.QueryEvent) context.Context {
	return context.WithValue(ctx, traceKey{}, &statementSpan{name: e.Op + " " + e.Table, start: time.Now()})
}

func (tracingObserver) AfterQuery(ctx context.Context, e *keel.QueryEvent) {
	sp, ok := ctx.Value(traceKey{}).(*statementSpan)
	if !ok {
		return
	}
	log.Printf("span %s: %v, %d rows, error %v", sp.name, time.Since(sp.start), e.Rows, e.Err)
}

// appConfig is the configuration of a multi-tenant application.
func appConfig(dsn string) keel.Config {
	return keel.Config{
		DSN:           dsn,
		TenantColumns: []string{"organization_id", "business_unit_id"},
		Tenant: func(ctx context.Context) ([]any, bool) {
			sc, ok := ctx.Value(scopeKey{}).(scope)
			if !ok || sc.SystemReason != "" {
				return nil, false
			}
			return []any{sc.OrganizationID, sc.BusinessUnitID}, true
		},
		System: func(ctx context.Context) (string, bool) {
			sc, ok := ctx.Value(scopeKey{}).(scope)
			return sc.SystemReason, ok && sc.SystemReason != ""
		},
		OnBegin: func(ctx context.Context, tx keel.TxInfo) ([]keel.Statement, error) {
			sc, _ := ctx.Value(scopeKey{}).(scope)
			return []keel.Statement{{
				SQL:  "SELECT set_config('app.organization_id', $1, true)",
				Args: []any{string(sc.OrganizationID)},
			}}, nil
		},
		NewID:     newID,
		Observers: []keel.Observer{slowQueryLog{threshold: 200 * time.Millisecond}},
	}
}

type tenantCtxKey struct{}

func Example_quickStart() {
	ctx := context.Background()
	db, err := keel.Open(ctx, keel.Config{
		DSN:           os.Getenv("DATABASE_URL"),
		TenantColumns: []string{"organization_id", "business_unit_id"},
		Tenant: func(ctx context.Context) ([]any, bool) {
			t, ok := ctx.Value(tenantCtxKey{}).([2]ids.ID)
			if !ok {
				return nil, false
			}
			return []any{t[0], t[1]}, true
		},
		NewID: newID,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx = context.WithValue(ctx, tenantCtxKey{}, [2]ids.ID{"org_acme", "bu_dallas"})
	c, s := fleetdb.Customers, fleetdb.Shipments

	cust := &fleet.Customer{Code: "ACME", Name: "Acme Freight"}
	if err := c.Create(ctx, db, cust); err != nil {
		log.Fatal(err)
	}
	sh := &fleet.Shipment{CustomerID: cust.ID, ProNumber: "PRO-1001", Status: fleet.StatusNew}
	if err := s.Create(ctx, db, sh); err != nil {
		log.Fatal(err)
	}

	open, err := s.
		Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned), s.Customer().Code.Eq("ACME")).
		Load(s.Customer()).
		Order(s.CreatedAt.Desc()).
		Limit(50).
		List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(open), open[0].Customer.Name)

	sh.Status = fleet.StatusAssigned
	if err := s.Save(ctx, db, sh); errors.Is(err, keel.ErrConflict) {
		fmt.Println("someone else saved this shipment first")
	} else if err != nil {
		log.Fatal(err)
	}

	err = keel.Write(ctx, db, func(ctx context.Context) error {
		mv := &fleet.Move{ShipmentID: sh.ID, Sequence: 1, Status: fleet.StatusAssigned}
		if err := fleetdb.Moves.Create(ctx, db, mv); err != nil {
			return err
		}
		_, err := s.Where(s.ID.Eq(sh.ID)).Update(ctx, db, s.Status.To(fleet.StatusInTransit))
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleOpen() {
	ctx := context.Background()
	db, err := keel.Open(ctx, appConfig(os.Getenv("DATABASE_URL")))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		log.Fatal(err)
	}
}

func ExampleOpen_pool() {
	ctx := context.Background()
	pc, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	pc.MaxConns = 32
	pc.MaxConnIdleTime = 5 * time.Minute

	cfg := appConfig("")
	cfg.Pool = pc
	cfg.PlanCache = keel.PlanCacheCustom
	cfg.StatementCacheCapacity = 1024
	db, err := keel.Open(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
}

func ExampleHandle_Create() {
	ctx, db := requestContext(), appDB
	sh := &fleet.Shipment{
		CustomerID: "cus_01",
		ProNumber:  "PRO-1001",
		Status:     fleet.StatusNew,
		Total:      fleet.Money{Cents: 125_000},
		Labels:     []string{"expedite"},
		Attributes: map[string]any{"trailer": map[string]any{"type": "reefer"}},
	}
	if err := fleetdb.Shipments.Create(ctx, db, sh); err != nil {
		log.Fatal(err)
	}
	fmt.Println(sh.ID, sh.OrganizationID, sh.CreatedAt, sh.StageRank)
}

func ExampleHandle_CreateMany() {
	ctx, db := requestContext(), appDB
	stops := []*fleet.Stop{
		{MoveID: "mov_01", LocationID: "loc_chicago", Sequence: 1, Kind: "pickup"},
		{MoveID: "mov_01", LocationID: "loc_dallas", Sequence: 2, Kind: "delivery"},
	}
	if err := fleetdb.Stops.CreateMany(ctx, db, stops); err != nil {
		log.Fatal(err)
	}
}

func ExampleHandle_Copy() {
	ctx, db := requestContext(), appDB
	rows := make([]*fleet.Shipment, 0, 50_000)
	for i := range 50_000 {
		rows = append(rows, &fleet.Shipment{
			CustomerID: "cus_01",
			ProNumber:  fmt.Sprintf("IMPORT-%06d", i),
			Status:     fleet.StatusNew,
		})
	}
	n, err := fleetdb.Shipments.Copy(ctx, db, rows)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n, "shipments imported")
}

func ExampleHandle_Save() {
	ctx, db := requestContext(), appDB
	c := fleetdb.Customers
	cust, err := c.Get(ctx, db, "cus_01")
	if err != nil {
		log.Fatal(err)
	}
	cust.Name = "Acme Freight"
	err = c.Save(ctx, db, cust)
	var conflict *keel.ConflictError
	switch {
	case errors.As(err, &conflict):
		fmt.Printf("%s changed since version %d; reload and retry\n", conflict.Table, conflict.Version)
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Println("saved version", cust.Version)
	}
}

func ExampleHandle_Save_columns() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	sh, err := s.Get(ctx, db, "shp_01")
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now().Unix()
	sh.Status = fleet.StatusCanceled
	sh.CanceledAt = &now
	if err := s.Save(ctx, db, sh, s.Status, s.CanceledAt); err != nil {
		log.Fatal(err)
	}
}

func ExampleHandle_DeleteRow() {
	ctx, db := requestContext(), appDB
	st, err := fleetdb.Stops.Get(ctx, db, "stp_01")
	if err != nil {
		log.Fatal(err)
	}
	if err := fleetdb.Stops.DeleteRow(ctx, db, st); errors.Is(err, keel.ErrNotFound) {
		fmt.Println("already deleted")
	} else if err != nil {
		log.Fatal(err)
	}
}

func ExampleQuery_Update() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	now := time.Now().Unix()
	n, err := s.
		Where(s.Status.Eq(fleet.StatusNew), s.Customer().Code.Eq("ACME")).
		Update(ctx, db, s.Status.To(fleet.StatusCanceled), s.CanceledAt.To(now), s.Priority.Add(1))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n, "shipments canceled")
}

func ExampleQuery_Delete() {
	ctx, db := requestContext(), appDB
	st := fleetdb.Stops
	n, err := st.Where(st.MoveID.Eq("mov_01"), st.ArrivedAt.IsNull()).Delete(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n, "stops deleted")
}

func ExampleHandle_Upsert() {
	ctx, db := requestContext(), appDB
	c := fleetdb.Customers
	rows := []*fleet.Customer{
		{Code: "ACME", Name: "Acme Freight"},
		{Code: "GLOBEX", Name: "Globex Logistics"},
	}
	inserted, err := c.Upsert(ctx, db, rows,
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Update(c.Name))
	if err != nil {
		log.Fatal(err)
	}
	for i, r := range rows {
		fmt.Println(r.Code, "inserted:", inserted[i], "version:", r.Version)
	}
}

func ExampleConflict_Set() {
	ctx, db := requestContext(), appDB
	c := fleetdb.Customers
	rows := []*fleet.Customer{{Code: "ACME", Name: "Acme Freight"}}
	_, err := c.Upsert(ctx, db, rows,
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Set(
			c.Name.ToExpr(c.Name.Excluded()),
			c.Email.ToExpr(keel.Raw("coalesce(?, ?)", c.Email, c.Email.Excluded())),
			c.Version.Add(10),
		))
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleHandle_CreateOrSkip() {
	ctx, db := requestContext(), appDB
	c := fleetdb.Customers
	rows := []*fleet.Customer{{Code: "ACME", Name: "Acme"}, {Code: "INITECH", Name: "Initech"}}
	n, err := c.CreateOrSkip(ctx, db, rows, c.OrganizationID, c.BusinessUnitID, c.Code)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n, "new customers")
}

func ExampleHandle_InsertFrom() {
	ctx, db := requestContext(), appDB
	tg, c := fleetdb.Tags, fleetdb.Customers
	n, err := tg.InsertFrom(ctx, db,
		c.Pick(keel.Raw("'tag_' || ?", c.ID), c.Name).Where(c.Email.IsNotNull()),
		tg.ID, tg.Name)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n, "tags created")
}

func ExampleQuery_One() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	byID, err := s.Get(ctx, db, "shp_01")
	if errors.Is(err, keel.ErrNotFound) {
		fmt.Println("no such shipment")
	} else if err != nil {
		log.Fatal(err)
	}
	sh, err := s.Where(s.ProNumber.Eq("PRO-1001")).One(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	newest, err := s.Where(s.CustomerID.Eq(sh.CustomerID)).Order(s.CreatedAt.Desc()).First(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	recent, err := s.Where(s.Status.Eq(fleet.StatusNew)).Order(s.CreatedAt.Desc()).Limit(50).List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	tags, err := fleetdb.Tags.Query().All(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(byID, newest.ProNumber, len(recent), len(tags))
}

func ExampleQuery_Count() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	open := s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned))

	n, err := open.Count(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	upTo, err := open.CountUpTo(ctx, db, 1000)
	if err != nil {
		log.Fatal(err)
	}
	exists, err := open.Exists(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n, upTo, exists)
}

func Example_operators() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	since := time.Now().Add(-24 * time.Hour)

	n, err := s.Where(
		s.Status.In(fleet.StatusNew, fleet.StatusAssigned),
		s.Priority.Gte(2),
		s.Weight.Between(10_000, 40_000),
		s.BillToID.IsNull(),
		s.ProNumber.HasPrefix("PRO-"),
		s.Hazmat.IsFalse(),
		s.Total.Gt(fleet.Money{Cents: 50_000}),
		s.Labels.Has("expedite"),
		s.Attributes.Contains(map[string]any{"temperature": "frozen"}),
		s.Attributes.Text("trailer", "type").Eq("reefer"),
		s.SearchVector.Matches("dallas reefer"),
		keel.Or(s.CanceledAt.IsNull(), s.CanceledAt.Gt(since.Unix())),
		keel.Not(s.Customer().Name.ContainsFold("test")),
	).Count(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	st := fleetdb.Stops
	late, err := st.Where(st.ArrivedAt.Gte(since), st.Kind.Eq("delivery")).Count(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n, late)
}

// shipmentFilter is a search request with optional fields.
type shipmentFilter struct {
	Search     string
	Statuses   []fleet.Status
	CustomerID ids.ID
	HazmatOnly bool
}

func ExampleWhen() {
	ctx, db := requestContext(), appDB
	f := shipmentFilter{Search: "PRO-10", Statuses: []fleet.Status{fleet.StatusNew}}
	s := fleetdb.Shipments

	rows, err := s.Where(
		keel.When(f.Search != "", s.ProNumber.ContainsFold(f.Search)),
		keel.When(len(f.Statuses) > 0, s.Status.In(f.Statuses...)),
		keel.When(f.CustomerID != "", s.CustomerID.Eq(f.CustomerID)),
		keel.When(f.HazmatOnly, s.Hazmat.IsTrue()),
	).Order(s.CreatedAt.Desc()).Limit(50).List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func Example_relationFilters() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	m := s.Moves()
	st := m.Stops()

	rows, err := s.Where(
		s.Customer().Code.Eq("ACME"),
		keel.Or(s.BillToID.IsNull(), s.BillTo().Code.Ne("ACME")),
		m.Any(m.Status.Eq(fleet.StatusInTransit), m.Loaded.IsTrue()),
		m.Any(st.Any(st.Kind.Eq("delivery"), st.Location().City.Eq("Dallas"))),
		s.Tags().None(s.Tags().Name.Eq("on-hold")),
	).Limit(100).List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func ExampleQuery_Join() {
	ctx, db := requestContext(), appDB
	w := fleetdb.Workers
	mgr := w.As("mgr")

	reports, err := w.
		Join(mgr, mgr.ID.EqCol(w.ManagerID)).
		Where(mgr.LastName.Eq("Hopper")).
		Order(w.LastName.Asc()).
		All(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	s, c := fleetdb.Shipments, fleetdb.Customers
	rows, err := s.
		LeftJoin(c, c.ID.EqCol(s.BillToID)).
		Where(keel.Or(c.ID.IsNull(), c.Email.IsNotNull())).
		Limit(100).
		List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(reports), len(rows))
}

func ExampleExists() {
	ctx, db := requestContext(), appDB
	c, s := fleetdb.Customers, fleetdb.Shipments
	cutoff := time.Now().AddDate(0, -3, 0).Unix()

	idle, err := c.Where(keel.NotExists(
		s.Where(s.CustomerID.EqCol(c.ID), s.CreatedAt.Gte(cutoff)),
	)).All(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	hazmatShippers, err := c.Where(
		c.ID.InQuery(s.Pick(s.CustomerID).Where(s.Hazmat.IsTrue())),
	).Count(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(idle), hazmatShippers)
}

func ExampleQuery_LeftJoinLateral() {
	ctx, db := requestContext(), appDB
	m := fleetdb.Moves
	last := fleetdb.Stops.As("last_stop")

	type moveEnd struct {
		Move     ids.ID
		Location ids.ID
		Sequence int32
	}
	rows, err := keel.Into[moveEnd](ctx, db, m.
		LeftJoinLateral(last.
			Pick(last.LocationID, last.Sequence).
			Where(last.MoveID.EqCol(m.ID)).
			Order(last.Sequence.Desc()).
			Limit(1)).
		Pick(m.ID, last.LocationID, last.Sequence).
		Where(m.ShipmentID.Eq("shp_01")))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(rows)
}

func ExampleCTE() {
	ctx, db := requestContext(), appDB
	s, c := fleetdb.Shipments, fleetdb.Customers

	busy := keel.CTE("busy", []string{"customer_id", "loads"},
		s.Group(s.CustomerID).Pick(s.CustomerID, keel.Count()).Where(s.Status.Eq(fleet.StatusInTransit)))

	customers, err := c.With(busy).
		Join(busy, busy.Col("customer_id").EqCol(c.ID)).
		Where(busy.Col("loads").Gte(10)).
		Order(c.Name.Asc()).
		All(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	type load struct {
		Customer ids.ID
		Loads    int64
	}
	top, err := keel.Into[load](ctx, db, busy.Query().With(busy).
		Pick(busy.Col("customer_id"), busy.Col("loads")).
		Order(busy.Col("loads").Desc()).
		Limit(5))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(customers), top)
}

func ExampleRecursiveCTE() {
	ctx, db := requestContext(), appDB
	w := fleetdb.Workers

	chain := keel.RecursiveCTE("chain", []string{"id", "depth"},
		w.Pick(w.ID, keel.Raw("0")).Where(w.ID.Eq("wrk_dispatch_lead")))
	sub := w.As("sub")
	chain.Recurse(sub.
		Pick(sub.ID, keel.Raw("? + 1", chain.Col("depth"))).
		Join(chain, chain.Col("id").EqCol(sub.ManagerID)))

	type node struct {
		ID    ids.ID
		Depth int32
	}
	tree, err := keel.Into[node](ctx, db, chain.Query().With(chain).
		Pick(chain.Col("id"), chain.Col("depth")).
		Order(chain.Col("depth").Asc()))
	if err != nil {
		log.Fatal(err)
	}

	team, err := w.With(chain).Where(w.ID.InQuery(chain.Query().Pick(chain.Col("id")))).All(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(tree), len(team))
}

func ExampleUnionAll() {
	ctx, db := requestContext(), appDB
	s, c := fleetdb.Shipments, fleetdb.Customers

	codes, err := keel.Into[string](ctx, db, keel.UnionAll(
		s.Pick(s.ProNumber).Where(s.Hazmat.IsTrue()),
		c.Pick(c.Code).Where(c.Email.IsNull()),
	))
	if err != nil {
		log.Fatal(err)
	}

	flagged := keel.CTE("flagged", []string{"customer_id"}, keel.Union(
		s.Pick(s.CustomerID).Where(s.Hazmat.IsTrue()),
		s.Pick(s.CustomerID).Where(s.Status.Eq(fleet.StatusCanceled)),
	))
	n, err := c.With(flagged).
		Where(c.ID.InQuery(flagged.Query().Pick(flagged.Col("customer_id")))).
		Count(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(codes), n)
}

func ExampleInto() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	type statusTotal struct {
		Status  fleet.Status
		Loads   int64
		Hazmat  int64
		Revenue fleet.Money
	}
	rows, err := keel.Into[statusTotal](ctx, db, s.
		Group(s.Status).
		Pick(s.Status, keel.Count(), keel.Count().Filter(s.Hazmat.IsTrue()), s.Total.Sum()).
		Having(keel.Count().Gt(10)).
		Order(keel.Count().Desc()))
	if err != nil {
		log.Fatal(err)
	}

	heaviest, err := keel.IntoOne[*int64](ctx, db, s.Pick(s.Weight.Max()).Where(s.Status.Eq(fleet.StatusNew)))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(rows, heaviest)
}

func ExampleRowNumber() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	type ranked struct {
		Customer ids.ID
		Pro      string
		Rank     int64
	}
	rows, err := keel.Into[ranked](ctx, db, s.
		Pick(s.CustomerID, s.ProNumber,
			keel.RowNumber().Over([]keel.Expr{s.CustomerID}, s.CreatedAt.Desc())).
		Where(s.Status.Eq(fleet.StatusDelivered)).
		Limit(500))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func ExampleQuery_DistinctOn() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	latest, err := s.
		DistinctOn(s.CustomerID).
		Order(s.CustomerID.Asc(), s.CreatedAt.Desc()).
		All(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(latest), "customers")
}

func ExampleQuery_Lock() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	err := keel.Write(ctx, db, func(ctx context.Context) error {
		batch, err := s.
			Where(s.Status.Eq(fleet.StatusNew)).
			Order(s.CreatedAt.Asc()).
			Limit(20).
			Lock(keel.ForUpdate, keel.SkipLocked).
			List(ctx, db)
		if err != nil {
			return err
		}
		for _, sh := range batch {
			sh.Status = fleet.StatusAssigned
			if err := s.Save(ctx, db, sh, s.Status); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleQuery_Timeout() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	n, err := s.Where(s.SearchVector.Matches("reefer")).Timeout(2*time.Second).Count(ctx, db)
	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Println("search took too long")
	} else if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n)
}

func ExampleQuery_CustomPlan() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	rows, err := s.
		Where(s.Status.Eq(fleet.StatusDelivered), s.CustomerID.Eq("cus_01")).
		CustomPlan().
		Order(s.CreatedAt.Desc()).
		Limit(100).
		List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func ExampleQuery_ToSQL() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	sql, args, err := s.
		Pick(s.ID, s.ProNumber).
		Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned), s.Customer().Code.Eq("ACME")).
		Order(s.CreatedAt.Desc()).
		Limit(20).
		ToSQL(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(sql)
	fmt.Println(args)
}

func ExampleRaw() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	n, err := s.Where(
		keel.Raw("length(?) > ?", s.ProNumber, 8),
		keel.Raw("? ?? 'trailer'", s.Attributes),
	).Count(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n)
}

func ExampleQuery_Load() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	m := s.Moves()
	cu := s.Customer()

	rows, err := s.
		Where(s.Status.Eq(fleet.StatusInTransit)).
		Load(
			cu.Pick(cu.Name, cu.Code),
			s.BillTo(),
			m.Where(m.Status.Ne(fleet.StatusCanceled)).
				Order(m.Sequence.Asc()).
				Load(m.Stops().Location()),
			s.Tags(),
		).
		Order(s.CreatedAt.Desc()).
		Limit(50).
		List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	for _, sh := range rows {
		fmt.Println(sh.ProNumber, sh.Customer.Name, len(sh.Moves), len(sh.Tags))
		for _, mv := range sh.Moves {
			for _, st := range mv.Stops {
				fmt.Println("  ", st.Sequence, st.Location.City)
			}
		}
	}
}

func ExampleQuery_Load_limit() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	m := s.Moves()

	rows, err := s.
		Load(m.Order(m.Sequence.Desc()).Limit(1)).
		Order(s.CreatedAt.Desc()).
		Limit(50).
		List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	for _, sh := range rows {
		if len(sh.Moves) > 0 {
			fmt.Println(sh.ProNumber, "last move", sh.Moves[0].ID)
		}
	}
}

func ExampleQuery_Load_pick() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	origin := s.OriginStop()

	rows, err := s.
		Where(origin.Location().City.Eq("Austin")).
		Load(origin.Location(), s.DestinationStop().Location()).
		Order(s.CreatedAt.Desc()).
		Limit(50).
		List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	for _, sh := range rows {
		if sh.DestinationStop != nil {
			fmt.Println(sh.ProNumber, sh.OriginStop.Location.City, "→", sh.DestinationStop.Location.City)
		}
	}
}

func Example_metadata() {
	t := fleetdb.ShipmentsSchema()
	fmt.Println(t.Label, t.Plural, t.Report.Category, t.Doc)
	for _, c := range t.Columns {
		if c.Report == nil || !c.Report.Included {
			continue
		}
		fmt.Println(c.Label, c.Report.Kind == schema.Measure, c.Report.Format, c.Doc)
		for _, v := range c.Enum {
			fmt.Println("  ", v.Value, v.Label)
		}
	}
}

func ExampleQuery_Page() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	cursor := ""

	page, err := s.
		Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned)).
		Load(s.Customer()).
		Order(s.CreatedAt.Desc()).
		Page(ctx, db, keel.After(cursor, 50).Total(10_000))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(page.Items), page.Total, page.TotalCapped)
	if page.Next != "" {
		fmt.Println("next page:", page.Next)
	}
}

func ExampleQuery_Each() {
	ctx, db := withSystem(context.Background(), "nightly hazmat audit"), appDB
	s := fleetdb.Shipments

	err := s.AllTenants().Where(s.Hazmat.IsTrue()).Each(ctx, db, 500,
		func(ctx context.Context, batch []*fleet.Shipment) error {
			for _, sh := range batch {
				fmt.Println(sh.OrganizationID, sh.ProNumber)
			}
			return nil
		})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleBatch() {
	ctx, db := requestContext(), appDB
	type statusCount struct {
		Status fleet.Status
		Loads  int64
	}
	s, c := fleetdb.Shipments, fleetdb.Customers
	open := s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned))

	var rows []*fleet.Shipment
	var total int64
	var byStatus []statusCount
	var cust *fleet.Customer
	err := keel.Batch(ctx, db,
		keel.ListInto(open.Clone().Order(s.CreatedAt.Desc()).Limit(50), &rows),
		keel.CountInto(open.Clone(), &total),
		keel.IntoSlice(s.Group(s.Status).Pick(s.Status, keel.Count()), &byStatus),
		keel.OneInto(c.Where(c.ID.Eq("cus_01")), &cust),
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows), total, len(byStatus), cust.Name)
}

func ExampleWrite() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	err := keel.Write(ctx, db, func(ctx context.Context) error {
		sh, err := s.Where(s.ID.Eq("shp_01")).Lock(keel.ForUpdate, keel.NoWait).One(ctx, db)
		if err != nil {
			return err
		}
		sh.Status = fleet.StatusAssigned
		if err := s.Save(ctx, db, sh, s.Status); err != nil {
			return err
		}
		mv := &fleet.Move{ShipmentID: sh.ID, Sequence: 1, Status: fleet.StatusAssigned}
		if err := fleetdb.Moves.Create(ctx, db, mv); err != nil {
			return err
		}
		keel.AfterCommit(ctx, func(ctx context.Context) {
			log.Printf("shipment %s assigned", sh.ID)
		})
		return nil
	}, keel.LockTimeout(2*time.Second))
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleRead() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	var open, late int64

	err := keel.Read(ctx, db, func(ctx context.Context) error {
		var err error
		if open, err = s.Where(s.Status.Ne(fleet.StatusDelivered)).Count(ctx, db); err != nil {
			return err
		}
		late, err = s.Where(s.Status.Eq(fleet.StatusInTransit), s.Priority.Gte(2)).Count(ctx, db)
		return err
	}, keel.RepeatableRead())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(open, late)
}

func ExampleReadValue() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	sh, err := keel.ReadValue(ctx, db, func(ctx context.Context) (*fleet.Shipment, error) {
		return s.Where(s.ProNumber.Eq("PRO-1001")).Load(s.Customer(), s.Moves()).One(ctx, db)
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(sh.Customer.Name)
}

func ExampleWriteValue() {
	ctx, db := requestContext(), appDB

	id, err := keel.WriteValue(ctx, db, func(ctx context.Context) (ids.ID, error) {
		c := &fleet.Customer{Code: "UMBRELLA", Name: "Umbrella Transport"}
		if err := fleetdb.Customers.Create(ctx, db, c); err != nil {
			return "", err
		}
		sh := &fleet.Shipment{CustomerID: c.ID, ProNumber: "PRO-2001", Status: fleet.StatusNew}
		if err := fleetdb.Shipments.Create(ctx, db, sh); err != nil {
			return "", err
		}
		return sh.ID, nil
	}, keel.Serializable(), keel.Retry(3))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(id)
}

func ExampleSavepoint() {
	ctx, db := requestContext(), appDB
	c := fleetdb.Customers

	err := keel.Write(ctx, db, func(ctx context.Context) error {
		for _, code := range []string{"ACME", "GLOBEX", "ACME"} {
			err := keel.Savepoint(ctx, db, func(ctx context.Context) error {
				return c.Create(ctx, db, &fleet.Customer{Code: code, Name: code})
			})
			if keel.IsUnique(err, "uq_customers_code") {
				continue
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleLockKey() {
	ctx, db := requestContext(), appDB

	err := keel.Write(ctx, db, func(ctx context.Context) error {
		if err := keel.LockKey(ctx, db, "invoice-run:org_acme"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	err = keel.Write(ctx, db, func(ctx context.Context) error {
		got, err := keel.TryLockKey(ctx, db, "rate-refresh")
		if err != nil {
			return err
		}
		if !got {
			fmt.Println("another process is refreshing rates")
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleQuery_AllTenants() {
	ctx, db := withSystem(context.Background(), "purge canceled shipments"), appDB
	s := fleetdb.Shipments
	m := s.Moves()
	cutoff := time.Now().AddDate(-1, 0, 0).Unix()

	stale, err := s.AllTenants().
		Where(s.Status.Eq(fleet.StatusCanceled), s.CanceledAt.Lt(cutoff), m.None(m.Loaded.IsTrue())).
		Load(s.Customer(), m.Stops()).
		Limit(1000).
		List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	n, err := s.AllTenants().
		Where(s.Status.Eq(fleet.StatusCanceled), s.CanceledAt.Lt(cutoff)).
		Delete(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(stale), n)
}

func Example_tenantErrors() {
	db := appDB
	s := fleetdb.Shipments

	_, err := s.Query().Limit(10).List(context.Background(), db)
	fmt.Println(errors.Is(err, keel.ErrNoScope))

	sys := withSystem(context.Background(), "report")
	_, err = s.Query().Limit(10).List(sys, db)
	fmt.Println(errors.Is(err, keel.ErrAllTenantsRequired))

	_, err = s.AllTenants().Limit(10).List(requestContext(), db)
	fmt.Println(errors.Is(err, keel.ErrSystemScopeRequired))

	err = fleetdb.Customers.Create(requestContext(), db,
		&fleet.Customer{OrganizationID: "org_other", BusinessUnitID: "bu_other", Code: "X", Name: "X"})
	fmt.Println(errors.Is(err, keel.ErrTenantMismatch))

	plant, err := fleetdb.Plants.Get(context.Background(), db, "DAL1")
	fmt.Println(plant, err)
}

func ExampleOnCreate() {
	db := appDB
	s := fleetdb.Shipments

	keel.OnCreate(db, s, keel.BeforeWrite, func(ctx context.Context, m *keel.Mutation[fleet.Shipment]) error {
		if m.Row.ProNumber == "" {
			return errors.New("shipment needs a PRO number")
		}
		if m.Row.Attributes == nil {
			m.Row.Attributes = map[string]any{}
		}
		return nil
	})
}

func ExampleOnUpdate() {
	db := appDB
	s := fleetdb.Shipments

	keel.OnUpdate(db, s, keel.AfterWrite, func(ctx context.Context, m *keel.Mutation[fleet.Shipment]) error {
		if m.Old == nil || !m.Changed(s.Status) || m.Old.Status == m.Row.Status {
			return nil
		}
		log.Printf("shipment %s: %s -> %s", m.Row.ID, m.Old.Status, m.Row.Status)
		return nil
	}, keel.Needs(s.Status))
}

func ExampleOnWrite() {
	db := appDB
	c := fleetdb.Customers

	keel.OnWrite(db, c, keel.Committed, func(ctx context.Context, m *keel.Mutation[fleet.Customer]) error {
		log.Printf("customer %s %s; refresh the search index", m.Row.ID, m.Op)
		return nil
	})
}

type portalKey struct{}

func ExampleDB_Intercept() {
	db := appDB
	s := fleetdb.Shipments

	db.Intercept(s, func(ctx context.Context, q *keel.Intercepted) error {
		customers, ok := ctx.Value(portalKey{}).([]ids.ID)
		if !ok {
			return nil
		}
		if q.Op == keel.OpDelete {
			return errors.New("portal users cannot delete shipments")
		}
		q.Where(s.CustomerID.In(customers...))
		return nil
	})

	ctx := context.WithValue(requestContext(), portalKey{}, []ids.ID{"cus_01"})
	rows, err := s.Query().Limit(20).List(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func ExampleObserver() {
	cfg := appConfig(os.Getenv("DATABASE_URL"))
	cfg.Observers = append(cfg.Observers, tracingObserver{})
	db, err := keel.Open(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
}

func Example_modelHooks() {
	ctx, db := requestContext(), appDB
	w := fleetdb.Workers

	grace := &fleet.Worker{FirstName: "  Grace ", LastName: "Hopper"}
	if err := w.Create(ctx, db, grace); err != nil {
		log.Fatal(err)
	}
	fmt.Println(grace.FirstName, grace.WholeName, grace.HiredAt)

	got, err := w.Get(ctx, db, grace.ID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(got.Initials)
}

func ExampleIsUnique() {
	ctx, db := requestContext(), appDB

	err := fleetdb.Shipments.Create(ctx, db, &fleet.Shipment{
		CustomerID: "cus_01", ProNumber: "PRO-1001", Status: fleet.StatusNew,
	})
	switch {
	case keel.IsUnique(err, "uq_shipments_pro"):
		fmt.Println("PRO number already used")
	case keel.IsForeignKey(err, "fk_shipments_customer"):
		fmt.Println("no such customer")
	case err != nil:
		fmt.Println("SQLSTATE", keel.Code(err), "constraint", keel.Constraint(err))
	}
}

//go:embed testdata/migrations/*.sql
var migrationFiles embed.FS

func Example_migrations() {
	ctx := context.Background()
	dir, err := fs.Sub(migrationFiles, "testdata/migrations")
	if err != nil {
		log.Fatal(err)
	}
	sources := migrate.Sources(migrate.Source{Name: "core", FS: dir})
	m, err := migrate.Open(ctx, os.Getenv("KEEL_DSN"), sources, migrate.Options{
		After: []string{"ANALYZE"},
		Observer: func(e migrate.Event) {
			switch e.Kind {
			case migrate.Retry:
				log.Printf("%s: lock timeout, attempt %d, blocked by %v", e.Migration.ID(), e.Attempt, e.Blockers)
			case migrate.MigrationEnd:
				log.Printf("%s in %s", e.Migration.ID(), e.Duration)
			}
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close()

	res, err := m.Up(ctx, migrate.UpOptions{})
	switch {
	case errors.Is(err, migrate.ErrChanged):
		log.Fatal("an applied migration was edited; add a new migration instead")
	case errors.Is(err, migrate.ErrOutOfOrder):
		log.Fatal("a migration is older than the newest applied one; give it a newer version")
	case err != nil:
		log.Fatal(err)
	}
	fmt.Println("applied", len(res.Applied))

	status, err := m.Status(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, s := range status {
		fmt.Println(s.ID(), s.State, s.Directives.NoTx)
	}

	if err := migrate.Require(ctx, m.Conn(), sources.Version()); errors.Is(err, migrate.ErrBehind) {
		log.Fatal(err)
	}
}

func ExampleOpen_replicas() {
	ctx := context.Background()
	cfg := appConfig(os.Getenv("DATABASE_URL"))
	cfg.Replicas = []keel.ReplicaConfig{
		{DSN: os.Getenv("REPLICA_A_URL")},
		{DSN: os.Getenv("REPLICA_B_URL"), Weight: 2},
	}
	cfg.MaxReplicaLag = 2 * time.Second
	db, err := keel.Open(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	for _, r := range db.Replicas() {
		fmt.Println(r.Name, r.Healthy, r.Lag, r.Position, r.InFlight, r.Reads, r.Err)
	}
	fmt.Println("replica reads that ran on the primary:", db.ReplicaFallbacks())
}

func ExampleOnReplica() {
	ctx, db := keel.OnReplica(requestContext()), appDB
	s := fleetdb.Shipments

	page, err := s.Where(s.Status.Eq(fleet.StatusNew)).
		Order(s.CreatedAt.Desc()).
		Page(ctx, db, keel.After("", 50))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(page.Items))

	sh := &fleet.Shipment{CustomerID: "cus_01", ProNumber: "PRO-3001", Status: fleet.StatusNew}
	if err := s.Create(ctx, db, sh); err != nil {
		log.Fatal(err)
	}
	got, err := s.Get(ctx, db, sh.ID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(got.ProNumber)
}

func ExampleCommitPosition() {
	ctx, db := keel.OnReplica(requestContext()), appDB
	s := fleetdb.Shipments

	sh := &fleet.Shipment{CustomerID: "cus_01", ProNumber: "PRO-3002", Status: fleet.StatusNew}
	if err := s.Create(ctx, db, sh); err != nil {
		log.Fatal(err)
	}
	cookie := keel.CommitPosition(ctx, db).String()

	pos, err := keel.ParsePosition(cookie)
	if err != nil {
		log.Fatal(err)
	}
	next := keel.OnReplicaAfter(requestContext(), db, pos)
	n, err := s.Where(s.Status.Eq(fleet.StatusNew)).Count(next, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n)
}

func ExampleReplicaTx() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	err := keel.Read(ctx, db, func(ctx context.Context) error {
		return s.Where(s.Status.Eq(fleet.StatusDelivered)).Each(ctx, db, 1000,
			func(ctx context.Context, batch []*fleet.Shipment) error {
				for _, sh := range batch {
					fmt.Println(sh.ProNumber, sh.Total)
				}
				return nil
			})
	}, keel.ReplicaTx(), keel.RepeatableRead())
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleOpen_dataVersions() {
	ctx := context.Background()
	cfg := appConfig(os.Getenv("DATABASE_URL"))
	cfg.DataVersions = keel.DataVersionsConfig{
		Enabled:  true,
		Interval: time.Second,
		OnError:  func(err error) { log.Printf("data versions: %v", err) },
	}
	db, err := keel.Open(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
}

func ExampleDB_DataVersions() {
	ctx, db := requestContext(), appDB
	tenant := db.TenantKey(ctx)

	versions, err := db.DataVersions(ctx, tenant, "shipments", "customers")
	if err != nil {
		log.Fatal(err)
	}
	cacheKey := fmt.Sprintf("open-shipments:%s:%d:%d", tenant, versions["shipments"], versions["customers"])
	fmt.Println(cacheKey)
}

func ExampleTenantKeyOf() {
	ctx, db := withSystem(context.Background(), "warm report caches"), appDB

	tenant := keel.TenantKeyOf(ids.ID("org_acme"), ids.ID("bu_dallas"))
	versions, err := db.DataVersions(ctx, tenant, "shipments")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(tenant, versions["shipments"])
}

func ExampleMarkChanged() {
	ctx, db := requestContext(), appDB

	err := keel.Write(ctx, db, func(ctx context.Context) error {
		_, err := keel.Exec(ctx, db, `UPDATE shipments SET priority = priority + 1
		 WHERE organization_id = $1 AND business_unit_id = $2 AND status = 'New'`, "org_acme", "bu_dallas")
		if err != nil {
			return err
		}
		keel.MarkChanged(ctx, db, "shipments")
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleQuery_Rollup() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	code := s.Customer().Code
	level := keel.Grouping(code, s.Status)

	type line struct {
		Customer *string
		Status   *fleet.Status
		Loads    int64
		Revenue  fleet.Money
		Level    int32
	}
	rows, err := keel.Into[line](ctx, db, s.
		Pick(code, s.Status, keel.Count(), s.Total.Sum(), level).
		Rollup(code, s.Status).
		Order(level.Asc(), code.Asc(), s.Status.Asc()))
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range rows {
		switch r.Level {
		case 0:
			fmt.Println(*r.Customer, *r.Status, r.Loads)
		case 1:
			fmt.Println(*r.Customer, "subtotal", r.Loads)
		default:
			fmt.Println("total", r.Loads)
		}
	}
}

func ExampleQuery_GroupingSets() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	code := s.Customer().Code
	level := keel.Grouping(code, s.Status)

	type total struct {
		Customer *string
		Status   *fleet.Status
		Loads    int64
	}
	rows, err := keel.Into[total](ctx, db, s.
		Pick(code, s.Status, keel.Count()).
		GroupingSets([]keel.Expr{code}, []keel.Expr{s.Status}, nil).
		Having(level.Gt(0)))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func ExampleAgg_OverWindow() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	byDay := keel.PartitionBy(s.CustomerID).OrderBy(s.CreatedAt.Asc())

	type trend struct {
		Pro     string
		Running fleet.Money
		Moving  float64
		Share   float64
		Rank    int64
		Change  *fleet.Money
	}
	rows, err := keel.Into[trend](ctx, db, s.Pick(
		s.ProNumber,
		keel.Sum(s.Total).OverWindow(byDay.Rows(keel.UnboundedPreceding, keel.CurrentRow)),
		keel.Avg(s.Total).OverWindow(byDay.Rows(keel.Preceding(2), keel.CurrentRow)),
		keel.Raw("? * 100.0 / ?", s.Total, keel.Sum(s.Total).OverWindow(keel.Window{})),
		keel.Rank().OverWindow(keel.PartitionBy(s.CustomerID).OrderBy(s.Total.Desc())),
		keel.Raw("? - ?", s.Total, keel.Lag(s.Total, 1, nil).OverWindow(byDay)),
	).Order(s.CustomerID.Asc(), s.CreatedAt.Asc()).Limit(1000))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func ExampleQuery_Window() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	type numbered struct {
		Pro     string
		N       int64
		Running fleet.Money
		Tile    int32
	}
	rows, err := keel.Into[numbered](ctx, db, s.
		Pick(s.ProNumber,
			keel.RowNumber().OverWindow(keel.NamedWindow("per_customer")),
			keel.Sum(s.Total).OverWindow(keel.NamedWindow("per_customer").
				Rows(keel.UnboundedPreceding, keel.CurrentRow)),
			keel.Ntile(4).OverWindow(keel.OrderBy(s.Total.Desc()))).
		Window("per_customer", keel.PartitionBy(s.CustomerID).OrderBy(s.CreatedAt.Asc())).
		Limit(500))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func Example_sharedParameters() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments

	type monthly struct {
		Month   time.Time
		Revenue fleet.Money
	}
	month := keel.Raw("date_trunc(?, to_timestamp(?))", "month", s.CreatedAt)
	rows, err := keel.Into[monthly](ctx, db, s.
		Pick(month, s.Total.Sum()).
		Group(month).
		Order(keel.Asc(month)))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(rows))
}

func ExampleQuery_Explain() {
	ctx, db := requestContext(), appDB
	s := fleetdb.Shipments
	q := s.Where(s.Status.Eq(fleet.StatusNew)).Order(s.CreatedAt.Desc()).Limit(50)

	plan, err := q.Explain(ctx, db, keel.ExplainOptions{})
	if err != nil {
		log.Fatal(err)
	}
	if plan.TotalCost > 100_000 {
		fmt.Println("too expensive for an interactive request")
	}

	analyzed, err := q.Explain(ctx, db, keel.ExplainOptions{Analyze: true})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(analyzed.Rows, analyzed.ActualRows, analyzed.ExecutionTime)
}

// reportTables is the registry of the tables reports can start from.
var reportTables, _ = keel.NewRegistry(fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema(),
	fleetdb.MovesSchema(), fleetdb.StopsSchema(), fleetdb.TagsSchema(), fleetdb.LocationsSchema())

func ExampleRegistry() {
	ctx, db := requestContext(), appDB
	search := "acme"

	q := reportTables.Query("shipments")
	pro := q.Path("pro_number")
	customer := q.Path("customer", "name")
	q.Select(pro, customer, q.Path("status"), q.Path("total")).
		Where(q.Path("status").In([]string{"New", "Assigned"}), customer.ContainsFold(search)).
		Order(pro.Desc()).
		Limit(100)
	res, err := q.Run(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	total := res.Column("total")
	for i := range res.Rows {
		fmt.Println(res.Columns[0].String[i], res.Column("customer.name").String[i], total.Numeric[i])
	}
}

func ExampleDynQuery_Rollup() {
	ctx, db := requestContext(), appDB
	q := reportTables.Query("shipments")

	code := q.Path("customer", "code")
	month := keel.DynFunc("date_trunc", "month", keel.DynFunc("to_timestamp", q.Path("created_at"))).
		As("month")
	revenue := q.Path("total").Sum()
	level := keel.DynOf(keel.Grouping(code, month))
	q.Select(code, month, revenue.As("revenue"),
		keel.DynCount().Filter(q.Path("hazmat").IsTrue()).As("hazmat loads"),
		revenue.Sum().Over(keel.Window{}).As("all revenue"),
		keel.DynOf(keel.Rank().OverWindow(keel.OrderBy(revenue.Desc()))).As("rank"),
		level.As("level")).
		Rollup(code, month).
		Order(level.Asc(), code.Asc(), month.Asc())
	res, err := q.Run(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Rows)
}

func ExampleDynRel_PreAggregate() {
	ctx, db := requestContext(), appDB
	q := reportTables.Query("shipments")
	code := q.Path("customer", "code")

	moves := q.Rel("moves")
	stops := q.Rel("moves", "stops")
	pa := moves.PreAggregate(keel.PreAggCTE)
	legs := pa.Measure(keel.DynCount().As("legs"))
	loaded := pa.Measure(keel.DynCount().Filter(moves.Col("loaded").IsTrue()).As("loaded legs"))
	q.Select(code, q.Path("total").Sum().As("revenue"), legs.Sum().As("legs"),
		loaded.Sum().As("loaded legs")).
		Where(moves.Any(moves.Col("status").Eq("New")),
			stops.None(stops.Col("location", "city").Eq("Dallas"))).
		Group(code)
	res, err := q.Run(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Rows)
}

func ExampleDynQuery_With() {
	ctx, db := requestContext(), appDB
	sub := reportTables.Query("shipments")
	sub.Select(sub.Path("customer_id").As("customer"), sub.Path("total").Sum().As("revenue")).
		Group(sub.Path("customer_id"))

	c := reportTables.Query("customers")
	totals := c.With(sub)
	c.Join(totals, totals.Col("customer").Eq(c.Path("id"))).
		Select(c.Path("code"), totals.Col("revenue")).
		Where(totals.Col("revenue").Gt(10_000))
	res, err := c.Run(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	outer := keel.DynFromQuery(sub)
	outer.Select(keel.DynCount().As("customers"), outer.Path("revenue").Avg().As("average"))
	summary, err := outer.Run(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Rows, summary.Column("average").Numeric[0])
}

func ExampleDynUnionAll() {
	ctx, db := requestContext(), appDB
	names := func(table string) *keel.DynQuery {
		q := reportTables.Query(table)
		return q.Select(q.Path("name").As("name"))
	}
	all := keel.DynUnionAll(names("customers"), names("locations"), names("tags"))
	all.Order(all.Path("name").Asc())
	res, err := all.Run(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Column("name").String)
}

func ExampleDynQuery_Page() {
	ctx, db := requestContext(), appDB
	q := reportTables.Query("shipments")
	pro := q.Path("pro_number")
	q.Select(pro, q.Path("customer", "code")).Order(pro.Desc())

	cursor := ""
	for {
		res, err := q.Page(ctx, db, keel.After(cursor, 50))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(res.Column("pro_number").String)
		if res.Next == "" {
			break
		}
		cursor = res.Next
	}
}

func ExampleDynQuery_Stream() {
	ctx, db := requestContext(), appDB
	q := reportTables.Query("shipments")
	q.Select(q.Path("pro_number"), q.Path("customer", "name"), q.Path("total"), q.Path("weight"))

	for batch, err := range q.Stream(ctx, db, 5000) {
		if err != nil {
			log.Fatal(err)
		}
		weight := batch.Column("weight")
		for i := range batch.Rows {
			if weight.IsNull(i) {
				continue
			}
			fmt.Println(batch.Columns[0].String[i], weight.Int64[i])
		}
	}
}

func ExampleDynInto() {
	ctx, db := requestContext(), appDB
	q := reportTables.Query("shipments")
	q.Select(q.Path("status"), keel.DynCount().As("loads")).Group(q.Path("status"))

	statuses := reportTables.Query("shipments")
	statuses.Select(statuses.Path("status")).Distinct()

	var report, pivot *keel.DynResult
	var rows int64
	err := keel.Batch(ctx, db, keel.DynInto(q, &report), keel.DynInto(statuses, &pivot),
		keel.DynCountInto(q, &rows))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(report.Rows, pivot.Rows, rows)
}

func ExampleDynQuery_Explain() {
	ctx, db := requestContext(), appDB
	q := reportTables.Query("shipments")
	q.Select(q.Path("customer", "code"), q.Path("total").Sum().As("revenue")).
		Group(q.Path("customer", "code"))

	plan, err := q.Explain(ctx, db, keel.ExplainOptions{})
	if err != nil {
		log.Fatal(err)
	}
	if plan.TotalCost > 1_000_000 || plan.Rows > 100_000 {
		log.Fatal("report too expensive")
	}
	fmt.Println(plan.TotalCost)
}
