// Package bench compares keel with Bun (Trenova's fork), raw pgx, GORM, ent, sqlc, Bob, Jet and
// sqlx on the same schema and data. Each library runs the operations the way its documentation
// writes them, with the tenant filter written by hand where the library has no tenancy.
//
//	cd bench && go test -bench . -benchmem -count 6 | tee new.txt
package bench

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/emoss08/keel"
	"github.com/emoss08/keel/bench/benchjson"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/migrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

type bunCustomer struct {
	bun.BaseModel  `bun:"table:customers,alias:cus"`
	ID             string  `bun:"id,pk"`
	OrganizationID string  `bun:"organization_id,pk"`
	BusinessUnitID string  `bun:"business_unit_id,pk"`
	Name           string  `bun:"name"`
	Code           string  `bun:"code"`
	Email          *string `bun:"email"`
	Version        int64   `bun:"version"`
	CreatedAt      int64   `bun:"created_at"`
	UpdatedAt      int64   `bun:"updated_at"`
}

type bunMove struct {
	bun.BaseModel  `bun:"table:moves,alias:mv"`
	ID             string `bun:"id,pk"`
	OrganizationID string `bun:"organization_id,pk"`
	BusinessUnitID string `bun:"business_unit_id,pk"`
	ShipmentID     string `bun:"shipment_id"`
	Sequence       int32  `bun:"sequence"`
	Status         string `bun:"status"`
	Loaded         bool   `bun:"loaded"`
	CreatedAt      int64  `bun:"created_at"`
}

type bunShipment struct {
	bun.BaseModel  `bun:"table:shipments,alias:sp"`
	ID             string        `bun:"id,pk"`
	OrganizationID string        `bun:"organization_id,pk"`
	BusinessUnitID string        `bun:"business_unit_id,pk"`
	CustomerID     string        `bun:"customer_id"`
	BillToID       *string       `bun:"bill_to_id"`
	ProNumber      string        `bun:"pro_number"`
	Status         string        `bun:"status"`
	Priority       int16         `bun:"priority"`
	Weight         *int64        `bun:"weight"`
	Total          fleet.Money   `bun:"total"`
	Hazmat         bool          `bun:"hazmat"`
	Labels         []string      `bun:"labels,array"`
	Attributes     benchjson.Map `bun:"attributes,type:jsonb"`
	StageRank      int16         `bun:"stage_rank,scanonly"`
	CanceledAt     *int64        `bun:"canceled_at"`
	Version        int64         `bun:"version"`
	CreatedAt      int64         `bun:"created_at"`
	UpdatedAt      int64         `bun:"updated_at"`

	Customer *bunCustomer `bun:"rel:belongs-to,join:customer_id=id,join:organization_id=organization_id,join:business_unit_id=business_unit_id"`
	Moves    []*bunMove   `bun:"rel:has-many,join:id=shipment_id,join:organization_id=organization_id,join:business_unit_id=business_unit_id"`
}

type env struct {
	keel   *keel.DB
	scope  *keel.DB
	cached *keel.DB
	bun    *bun.DB
	pgx    *pgxpool.Pool
	ctx    context.Context
	dsn    string
	org    ids.ID
	bu     ids.ID
	ship   []*fleet.Shipment
	others []*contender

	// cursor and keelCursor are the keyset position after the first page of 50 shipments by
	// created_at descending, id descending.
	cursor     pageCursor
	keelCursor string
}

var (
	envOnce sync.Once
	envVal  *env
	envErr  error
	dbName  string
	admin   = os.Getenv("KEEL_TEST_DSN")
)

type tenantKey struct{}

func dsnFor(name string) string {
	cfg, err := pgx.ParseConfig(admin)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

func keelConfig(dsn string) keel.Config {
	return keel.Config{
		DSN:           dsn,
		TenantColumns: []string{"organization_id", "business_unit_id"},
		Tenant: func(ctx context.Context) ([]any, bool) {
			v, ok := ctx.Value(tenantKey{}).([2]ids.ID)
			if !ok {
				return nil, false
			}
			return []any{v[0], v[1]}, true
		},
		JSON: &keel.JSONCodec{Marshal: sonic.Marshal, Unmarshal: sonic.Unmarshal},
		NewID: func(prefix string) string {
			b := make([]byte, 10)
			_, _ = rand.Read(b)
			return prefix + hex.EncodeToString(b)
		},
	}
}

func setup(b *testing.B) *env {
	b.Helper()
	envOnce.Do(func() { envVal, envErr = build() })
	if envErr != nil {
		b.Skipf("postgres unavailable: %v", envErr)
	}
	return envVal
}

func build() (*env, error) {
	if admin == "" {
		admin = "postgres://keel:keel@localhost:55432/keel?sslmode=disable"
	}
	ctx := context.Background()
	a, err := pgx.Connect(ctx, admin)
	if err != nil {
		return nil, err
	}
	defer a.Close(ctx)
	bb := make([]byte, 4)
	_, _ = rand.Read(bb)
	dbName = "keel_bench_" + hex.EncodeToString(bb)
	if _, err := a.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		return nil, err
	}
	dsn := dsnFor(dbName)
	m, err := migrate.Open(ctx, dsn, migrate.Sources(migrate.Source{Name: "testdata", FS: os.DirFS("../testdata/migrations")}),
		migrate.Options{})
	if err != nil {
		return nil, err
	}
	defer m.Close()
	if _, err := m.Up(ctx, migrate.UpOptions{}); err != nil {
		return nil, err
	}

	e := &env{org: "org_bench", bu: "bu_bench", dsn: dsn}
	e.ctx = context.WithValue(ctx, tenantKey{}, [2]ids.ID{e.org, e.bu})
	if e.keel, err = keel.Open(ctx, keelConfig(dsn)); err != nil {
		return nil, err
	}
	sc := keelConfig(dsn)
	sc.OnBegin = func(ctx context.Context, _ keel.TxInfo) ([]keel.Statement, error) {
		return []keel.Statement{{SQL: "SELECT set_config('app.org', $1, true)", Args: []any{"org_bench"}}}, nil
	}
	if e.scope, err = keel.Open(ctx, sc); err != nil {
		return nil, err
	}
	cc := keelConfig(dsn)
	cc.DataVersions = keel.DataVersionsConfig{Enabled: true}
	cc.Cache = &keel.CacheConfig{Local: keel.MemoryCache(64 << 20)}
	if e.cached, err = keel.Open(ctx, cc); err != nil {
		return nil, err
	}
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	sqldb.SetMaxOpenConns(8)
	e.bun = bun.NewDB(sqldb, pgdialect.New())
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pc.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		conn.TypeMap().RegisterType(&pgtype.Type{Name: "jsonb", OID: pgtype.JSONBOID,
			Codec: &pgtype.JSONBCodec{Marshal: sonic.Marshal, Unmarshal: sonic.Unmarshal}})
		return nil
	}
	if e.pgx, err = pgxpool.NewWithConfig(ctx, pc); err != nil {
		return nil, err
	}

	customers := make([]*fleet.Customer, 10)
	for i := range customers {
		customers[i] = &fleet.Customer{Name: fmt.Sprintf("Customer %d", i), Code: fmt.Sprintf("C%d", i)}
	}
	if err := fleetdb.Customers.CreateMany(e.ctx, e.keel, customers); err != nil {
		return nil, err
	}
	ship := make([]*fleet.Shipment, 2000)
	for i := range ship {
		w := int64(i)
		ship[i] = &fleet.Shipment{
			CustomerID: customers[i%10].ID, ProNumber: fmt.Sprintf("PRO-%06d", i),
			Status: fleet.StatusAssigned, Weight: &w, Total: fleet.Money{Cents: int64(i * 100)},
			Labels: []string{"a", "b"}, Attributes: map[string]any{"i": i},
		}
	}
	if err := fleetdb.Shipments.CreateMany(e.ctx, e.keel, ship); err != nil {
		return nil, err
	}
	var moves []*fleet.Move
	for _, s := range ship[:200] {
		for seq := range 3 {
			moves = append(moves, &fleet.Move{ShipmentID: s.ID, Sequence: int32(seq), Status: fleet.StatusAssigned})
		}
	}
	if err := fleetdb.Moves.CreateMany(e.ctx, e.keel, moves); err != nil {
		return nil, err
	}
	e.ship = ship
	if _, err := e.pgx.Exec(ctx, "ANALYZE"); err != nil {
		return nil, err
	}
	s := fleetdb.Shipments
	first, err := s.Order(s.CreatedAt.Desc(), s.ID.Desc()).Page(e.ctx, e.keel, keel.After("", 50))
	if err != nil {
		return nil, err
	}
	last := first.Items[len(first.Items)-1]
	e.keelCursor = first.Next
	e.cursor = pageCursor{createdAt: last.CreatedAt, id: string(last.ID)}
	for _, open := range contenders {
		c, err := open(e)
		if err != nil {
			return nil, err
		}
		e.others = append(e.others, c)
	}
	return e, nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	if envVal != nil {
		for _, c := range envVal.others {
			if c.close != nil {
				c.close()
			}
		}
		envVal.keel.Close()
		envVal.scope.Close()
		envVal.cached.Close()
		envVal.pgx.Close()
		_ = envVal.bun.Close()
		ctx := context.Background()
		if a, err := pgx.Connect(ctx, admin); err == nil {
			_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
			a.Close(ctx)
		}
	}
	os.Exit(code)
}

// --- get one row by key, tenant filtered ---

func BenchmarkGetByID(b *testing.B) {
	e := setup(b)
	id := e.ship[42].ID
	b.Run("keel", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fleetdb.Shipments.Get(e.ctx, e.keel, id); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			s := new(bunShipment)
			err := e.bun.NewSelect().Model(s).
				Where("sp.organization_id = ?", e.org).Where("sp.business_unit_id = ?", e.bu).
				Where("sp.id = ?", string(id)).Scan(e.ctx)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("pgx", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			rows, err := e.pgx.Query(e.ctx, `SELECT id, organization_id, business_unit_id, customer_id, bill_to_id,
				pro_number, status, priority, weight, total, hazmat, labels, attributes, stage_rank, canceled_at,
				version, created_at, updated_at FROM shipments WHERE organization_id = $1 AND business_unit_id = $2
				AND id = $3`, e.org, e.bu, id)
			if err != nil {
				b.Fatal(err)
			}
			s, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByPos[pgxShipment])
			if err != nil || s == nil {
				b.Fatal(err)
			}
		}
	})
	runContenders(b, e, func(c *contender) func(context.Context) error {
		if c.get == nil {
			return nil
		}
		return func(ctx context.Context) error { return c.get(ctx, string(id)) }
	})
}

type pgxShipment struct {
	ID, OrganizationID, BusinessUnitID, CustomerID string
	BillToID                                       *string
	ProNumber, Status                              string
	Priority                                       int16
	Weight                                         *int64
	Total                                          fleet.Money
	Hazmat                                         bool
	Labels                                         []string
	Attributes                                     map[string]any
	StageRank                                      int16
	CanceledAt                                     *int64
	Version, CreatedAt, UpdatedAt                  int64
}

// --- list rows ---

func benchList(b *testing.B, n int) {
	e := setup(b)
	b.Run("keel", func(b *testing.B) {
		b.ReportAllocs()
		s := fleetdb.Shipments
		for b.Loop() {
			rows, err := s.Where(s.Status.Eq(fleet.StatusAssigned)).Order(s.ProNumber.Asc()).Limit(n).List(e.ctx, e.keel)
			if err != nil || len(rows) != n {
				b.Fatal(err, len(rows))
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var rows []*bunShipment
			err := e.bun.NewSelect().Model(&rows).
				Where("sp.organization_id = ?", e.org).Where("sp.business_unit_id = ?", e.bu).
				Where("sp.status = ?", "Assigned").Order("sp.pro_number ASC").Limit(int64(n)).Scan(e.ctx)
			if err != nil || len(rows) != n {
				b.Fatal(err, len(rows))
			}
		}
	})
	b.Run("pgx", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			rows, err := e.pgx.Query(e.ctx, `SELECT id, organization_id, business_unit_id, customer_id, bill_to_id,
				pro_number, status, priority, weight, total, hazmat, labels, attributes, stage_rank, canceled_at,
				version, created_at, updated_at FROM shipments WHERE organization_id = $1 AND business_unit_id = $2
				AND status = $3 ORDER BY pro_number LIMIT $4`, e.org, e.bu, "Assigned", n)
			if err != nil {
				b.Fatal(err)
			}
			out, err := pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[pgxShipment])
			if err != nil || len(out) != n {
				b.Fatal(err)
			}
		}
	})
	runContenders(b, e, func(c *contender) func(context.Context) error {
		if c.list == nil {
			return nil
		}
		return func(ctx context.Context) error { return c.list(ctx, n) }
	})
}

func BenchmarkList100(b *testing.B)  { benchList(b, 100) }
func BenchmarkList1000(b *testing.B) { benchList(b, 1000) }

// --- relations: 50 shipments with their customer (to-one) and moves (to-many) ---

func BenchmarkLoadRelations(b *testing.B) {
	e := setup(b)
	b.Run("keel", func(b *testing.B) {
		b.ReportAllocs()
		s := fleetdb.Shipments
		for b.Loop() {
			rows, err := s.Load(s.Customer(), s.Moves()).Order(s.ProNumber.Asc()).Limit(50).List(e.ctx, e.keel)
			if err != nil || len(rows) != 50 || len(rows[0].Moves) != 3 {
				b.Fatal(err)
			}
		}
	})
	b.Run("keel-snapshot", func(b *testing.B) {
		b.ReportAllocs()
		s := fleetdb.Shipments
		for b.Loop() {
			rows, err := s.Load(s.Customer(), s.Moves()).Order(s.ProNumber.Asc()).Limit(50).List(e.ctx, e.scope)
			if err != nil || len(rows) != 50 || len(rows[0].Moves) != 3 {
				b.Fatal(err)
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var rows []*bunShipment
			err := e.bun.NewSelect().Model(&rows).
				Where("sp.organization_id = ?", e.org).Where("sp.business_unit_id = ?", e.bu).
				Relation("Customer").Relation("Moves").Order("sp.pro_number ASC").Limit(50).Scan(e.ctx)
			if err != nil || len(rows) != 50 || len(rows[0].Moves) != 3 {
				b.Fatal(err)
			}
		}
	})
	runContenders(b, e, func(c *contender) func(context.Context) error { return c.relations })
}

// --- a read in a scoped transaction (row-level security set_config), as Trenova runs every read ---

func BenchmarkScopedRead(b *testing.B) {
	e := setup(b)
	id := e.ship[7].ID
	b.Run("keel", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fleetdb.Shipments.Get(e.ctx, e.scope, id); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			err := e.bun.RunInTx(e.ctx, &sql.TxOptions{ReadOnly: true}, func(ctx context.Context, tx bun.Tx) error {
				if _, err := tx.ExecContext(ctx, "SELECT set_config('app.org', ?, true)", "org_bench"); err != nil {
					return err
				}
				s := new(bunShipment)
				return tx.NewSelect().Model(s).
					Where("sp.organization_id = ?", e.org).Where("sp.business_unit_id = ?", e.bu).
					Where("sp.id = ?", string(id)).Scan(ctx)
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	runContenders(b, e, func(c *contender) func(context.Context) error {
		if c.scoped == nil {
			return nil
		}
		return func(ctx context.Context) error { return c.scoped(ctx, string(id)) }
	})
}

func BenchmarkCachedRead(b *testing.B) {
	e := setup(b)
	id := e.ship[11].ID
	s := fleetdb.Shipments
	b.Run("keel", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.Get(e.ctx, e.cached, id); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("keel-cached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.Cache(keel.TTL(time.Minute)).Where(s.ID.Eq(id)).One(e.ctx, e.cached); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// --- writes ---

func BenchmarkCreate(b *testing.B) {
	e := setup(b)
	b.Run("keel", func(b *testing.B) {
		resetWrites(b, e)
		b.ReportAllocs()
		for b.Loop() {
			c := &fleet.Customer{Name: "bench", Code: rid()}
			if err := fleetdb.Customers.Create(e.ctx, e.keel, c); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		resetWrites(b, e)
		b.ReportAllocs()
		for b.Loop() {
			now := time.Now().Unix()
			c := &bunCustomer{ID: "cus_" + rid(), OrganizationID: string(e.org), BusinessUnitID: string(e.bu),
				Name: "bench", Code: rid(), CreatedAt: now, UpdatedAt: now}
			if _, err := e.bun.NewInsert().Model(c).Exec(e.ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	runWriteContenders(b, e, func(c *contender) func(context.Context) error { return c.create })
}

func BenchmarkCreateMany1000(b *testing.B) {
	e := setup(b)
	b.Run("keel", func(b *testing.B) {
		resetWrites(b, e)
		b.ReportAllocs()
		for b.Loop() {
			rows := make([]*fleet.Customer, 1000)
			for i := range rows {
				rows[i] = &fleet.Customer{Name: "bench", Code: rid()}
			}
			if err := fleetdb.Customers.CreateMany(e.ctx, e.keel, rows); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		resetWrites(b, e)
		b.ReportAllocs()
		for b.Loop() {
			now := time.Now().Unix()
			rows := make([]*bunCustomer, 1000)
			for i := range rows {
				rows[i] = &bunCustomer{ID: "cus_" + rid(), OrganizationID: string(e.org), BusinessUnitID: string(e.bu),
					Name: "bench", Code: rid(), CreatedAt: now, UpdatedAt: now}
			}
			if _, err := e.bun.NewInsert().Model(&rows).Exec(e.ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("keel-copy", func(b *testing.B) {
		resetWrites(b, e)
		b.ReportAllocs()
		for b.Loop() {
			rows := make([]*fleet.Customer, 1000)
			for i := range rows {
				rows[i] = &fleet.Customer{Name: "bench", Code: rid()}
			}
			if _, err := fleetdb.Customers.Copy(e.ctx, e.keel, rows); err != nil {
				b.Fatal(err)
			}
		}
	})
	runWriteContenders(b, e, func(c *contender) func(context.Context) error {
		if c.createMany == nil {
			return nil
		}
		return func(ctx context.Context) error { return c.createMany(ctx, 1000) }
	})
}

func BenchmarkSave(b *testing.B) {
	e := setup(b)
	b.Run("keel", func(b *testing.B) {
		resetWrites(b, e)
		c := &fleet.Customer{Name: "save", Code: rid()}
		if err := fleetdb.Customers.Create(e.ctx, e.keel, c); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			c.Name = rid()
			if err := fleetdb.Customers.Save(e.ctx, e.keel, c); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		resetWrites(b, e)
		now := time.Now().Unix()
		c := &bunCustomer{ID: "cus_" + rid(), OrganizationID: string(e.org), BusinessUnitID: string(e.bu),
			Name: "save", Code: rid(), CreatedAt: now, UpdatedAt: now}
		if _, err := e.bun.NewInsert().Model(c).Exec(e.ctx); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			c.Name = rid()
			ov := c.Version
			c.Version++
			c.UpdatedAt = time.Now().Unix()
			res, err := e.bun.NewUpdate().Model(c).WherePK().Where("version = ?", ov).Returning("*").Exec(e.ctx)
			if err != nil {
				b.Fatal(err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				b.Fatal("conflict")
			}
		}
	})
	for _, c := range e.others {
		if c.save == nil {
			continue
		}
		b.Run(c.name, func(b *testing.B) {
			resetWrites(b, e)
			save, err := c.save(e.ctx)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := save(e.ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// --- a keyset page of 50 after a cursor, with the total count ---

func BenchmarkPage(b *testing.B) {
	e := setup(b)
	b.Run("keel", func(b *testing.B) {
		s := fleetdb.Shipments
		b.ReportAllocs()
		for b.Loop() {
			page, err := s.Order(s.CreatedAt.Desc(), s.ID.Desc()).
				Page(e.ctx, e.keel, keel.After(e.keelCursor, 50).Total(10_000))
			if err != nil || len(page.Items) != 50 || page.Total == 0 {
				b.Fatal(err, len(page.Items))
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var rows []*bunShipment
			total, err := e.bun.NewSelect().Model((*bunShipment)(nil)).
				Where("sp.organization_id = ?", e.org).Where("sp.business_unit_id = ?", e.bu).Count(e.ctx)
			if err != nil || total == 0 {
				b.Fatal(err)
			}
			err = e.bun.NewSelect().Model(&rows).
				Where("sp.organization_id = ?", e.org).Where("sp.business_unit_id = ?", e.bu).
				Where("(sp.created_at, sp.id) < (?, ?)", e.cursor.createdAt, e.cursor.id).
				Order("sp.created_at DESC", "sp.id DESC").Limit(50).Scan(e.ctx)
			if err != nil || len(rows) != 50 {
				b.Fatal(err, len(rows))
			}
		}
	})
	b.Run("pgx", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var total int64
			err := e.pgx.QueryRow(e.ctx, `SELECT count(*) FROM shipments
				WHERE organization_id = $1 AND business_unit_id = $2`, e.org, e.bu).Scan(&total)
			if err != nil || total == 0 {
				b.Fatal(err)
			}
			rows, err := e.pgx.Query(e.ctx, `SELECT id, organization_id, business_unit_id, customer_id, bill_to_id,
				pro_number, status, priority, weight, total, hazmat, labels, attributes, stage_rank, canceled_at,
				version, created_at, updated_at FROM shipments WHERE organization_id = $1 AND business_unit_id = $2
				AND (created_at, id) < ($3, $4) ORDER BY created_at DESC, id DESC LIMIT 50`,
				e.org, e.bu, e.cursor.createdAt, e.cursor.id)
			if err != nil {
				b.Fatal(err)
			}
			out, err := pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[pgxShipment])
			if err != nil || len(out) != 50 {
				b.Fatal(err, len(out))
			}
		}
	})
	runContenders(b, e, func(c *contender) func(context.Context) error { return c.page })
}

// --- statement building only, no database ---

func BenchmarkBuildOnly(b *testing.B) {
	e := setup(b)
	b.Run("keel", func(b *testing.B) {
		s := fleetdb.Shipments
		b.ReportAllocs()
		for b.Loop() {
			q := s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned), s.ProNumber.Contains("x")).
				Order(s.CreatedAt.Desc()).Limit(50)
			if _, _, err := q.ToSQL(e.ctx, e.keel); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bun", func(b *testing.B) {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector()), pgdialect.New())
		b.ReportAllocs()
		for b.Loop() {
			var rows []*bunShipment
			q := db.NewSelect().Model(&rows).
				Where("sp.organization_id = ?", "org").Where("sp.business_unit_id = ?", "bu").
				Where("sp.status IN (?)", bun.List([]string{"New", "Assigned"})).
				Where("sp.pro_number LIKE ?", "%x%").Order("sp.created_at DESC").Limit(50)
			_ = q.String()
		}
	})
	runContenders(b, e, func(c *contender) func(context.Context) error { return c.build })
}

func rid() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
