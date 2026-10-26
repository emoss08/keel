package bench

import (
	"context"
	"database/sql"
	"time"

	"github.com/emoss08/keel/bench/benchjson"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

type sqlxCustomer struct {
	ID             string  `db:"id"`
	OrganizationID string  `db:"organization_id"`
	BusinessUnitID string  `db:"business_unit_id"`
	Name           string  `db:"name"`
	Code           string  `db:"code"`
	Email          *string `db:"email"`
	Version        int64   `db:"version"`
	CreatedAt      int64   `db:"created_at"`
	UpdatedAt      int64   `db:"updated_at"`
}

type sqlxMove struct {
	ID             string `db:"id"`
	OrganizationID string `db:"organization_id"`
	BusinessUnitID string `db:"business_unit_id"`
	ShipmentID     string `db:"shipment_id"`
	Sequence       int32  `db:"sequence"`
	Status         string `db:"status"`
	Loaded         bool   `db:"loaded"`
	CreatedAt      int64  `db:"created_at"`
}

type sqlxShipment struct {
	ID             string         `db:"id"`
	OrganizationID string         `db:"organization_id"`
	BusinessUnitID string         `db:"business_unit_id"`
	CustomerID     string         `db:"customer_id"`
	BillToID       *string        `db:"bill_to_id"`
	ProNumber      string         `db:"pro_number"`
	Status         string         `db:"status"`
	Priority       int16          `db:"priority"`
	Weight         *int64         `db:"weight"`
	Total          string         `db:"total"`
	Hazmat         bool           `db:"hazmat"`
	Labels         pq.StringArray `db:"labels"`
	Attributes     benchjson.Map  `db:"attributes"`
	StageRank      int16          `db:"stage_rank"`
	CanceledAt     *int64         `db:"canceled_at"`
	Version        int64          `db:"version"`
	CreatedAt      int64          `db:"created_at"`
	UpdatedAt      int64          `db:"updated_at"`
}

const sqlxShipmentColumns = `id, organization_id, business_unit_id, customer_id, bill_to_id, pro_number, status,
	priority, weight, total, hazmat, labels, attributes, stage_rank, canceled_at, version, created_at, updated_at`

const sqlxInsertCustomer = `INSERT INTO customers (id, organization_id, business_unit_id, name, code, email, version,
	created_at, updated_at) VALUES (:id, :organization_id, :business_unit_id, :name, :code, :email, :version,
	:created_at, :updated_at)`

func openSqlx(e *env) (*contender, error) {
	sqldb, err := openSQL(e.dsn)
	if err != nil {
		return nil, err
	}
	db := sqlx.NewDb(sqldb, "pgx")
	org, bu := string(e.org), string(e.bu)
	newCustomer := func(name string, now int64) sqlxCustomer {
		return sqlxCustomer{ID: "cus_" + rid(), OrganizationID: org, BusinessUnitID: bu, Name: name, Code: rid(),
			CreatedAt: now, UpdatedAt: now}
	}
	getSQL := `SELECT ` + sqlxShipmentColumns + ` FROM shipments
		WHERE organization_id = $1 AND business_unit_id = $2 AND id = $3`
	return &contender{
		name: "sqlx",
		get: func(ctx context.Context, id string) error {
			var s sqlxShipment
			return db.GetContext(ctx, &s, getSQL, org, bu, id)
		},
		list: func(ctx context.Context, n int) error {
			var rows []sqlxShipment
			err := db.SelectContext(ctx, &rows, `SELECT `+sqlxShipmentColumns+` FROM shipments
				WHERE organization_id = $1 AND business_unit_id = $2 AND status = $3 ORDER BY pro_number LIMIT $4`,
				org, bu, "Assigned", n)
			if err != nil {
				return err
			}
			return checkLen("list", len(rows), n)
		},
		relations: func(ctx context.Context) error {
			var ships []sqlxShipment
			err := db.SelectContext(ctx, &ships, `SELECT `+sqlxShipmentColumns+` FROM shipments
				WHERE organization_id = $1 AND business_unit_id = $2 ORDER BY pro_number LIMIT 50`, org, bu)
			if err != nil {
				return err
			}
			shipIDs := make([]string, len(ships))
			customerIDs := make([]string, 0, len(ships))
			seen := map[string]bool{}
			for i, s := range ships {
				shipIDs[i] = s.ID
				if !seen[s.CustomerID] {
					seen[s.CustomerID] = true
					customerIDs = append(customerIDs, s.CustomerID)
				}
			}
			var customers []sqlxCustomer
			err = db.SelectContext(ctx, &customers, `SELECT id, organization_id, business_unit_id, name, code, email,
				version, created_at, updated_at FROM customers
				WHERE organization_id = $1 AND business_unit_id = $2 AND id = ANY($3)`, org, bu, customerIDs)
			if err != nil {
				return err
			}
			var moves []sqlxMove
			err = db.SelectContext(ctx, &moves, `SELECT id, organization_id, business_unit_id, shipment_id, sequence,
				status, loaded, created_at FROM moves
				WHERE organization_id = $1 AND business_unit_id = $2 AND shipment_id = ANY($3)
				ORDER BY shipment_id, sequence`, org, bu, shipIDs)
			if err != nil {
				return err
			}
			byID := make(map[string]*sqlxCustomer, len(customers))
			for i := range customers {
				byID[customers[i].ID] = &customers[i]
			}
			movesOf := make(map[string][]sqlxMove, len(ships))
			for _, m := range moves {
				movesOf[m.ShipmentID] = append(movesOf[m.ShipmentID], m)
			}
			return checkRelations(len(ships), len(movesOf[ships[0].ID]), byID[ships[0].CustomerID] != nil)
		},
		scoped: func(ctx context.Context, id string) error {
			tx, err := db.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, "SELECT set_config('app.org', $1, true)", org); err != nil {
				return err
			}
			var s sqlxShipment
			if err := tx.GetContext(ctx, &s, getSQL, org, bu, id); err != nil {
				return err
			}
			return tx.Commit()
		},
		page: func(ctx context.Context) error {
			var total int64
			if err := db.GetContext(ctx, &total, `SELECT count(*) FROM shipments
				WHERE organization_id = $1 AND business_unit_id = $2`, org, bu); err != nil {
				return err
			}
			var rows []sqlxShipment
			err := db.SelectContext(ctx, &rows, `SELECT `+sqlxShipmentColumns+` FROM shipments
				WHERE organization_id = $1 AND business_unit_id = $2 AND (created_at, id) < ($3, $4)
				ORDER BY created_at DESC, id DESC LIMIT 50`, org, bu, e.cursor.createdAt, e.cursor.id)
			if err != nil {
				return err
			}
			return checkLen("page", len(rows), 50)
		},
		create: func(ctx context.Context) error {
			_, err := db.NamedExecContext(ctx, sqlxInsertCustomer, newCustomer("bench", time.Now().Unix()))
			return err
		},
		createMany: func(ctx context.Context, n int) error {
			now := time.Now().Unix()
			rows := make([]sqlxCustomer, n)
			for i := range rows {
				rows[i] = newCustomer("bench", now)
			}
			_, err := db.NamedExecContext(ctx, sqlxInsertCustomer, rows)
			return err
		},
		save: func(ctx context.Context) (func(ctx context.Context) error, error) {
			c := newCustomer("save", time.Now().Unix())
			if _, err := db.NamedExecContext(ctx, sqlxInsertCustomer, c); err != nil {
				return nil, err
			}
			version := c.Version
			return func(ctx context.Context) error {
				res, err := db.ExecContext(ctx, `UPDATE customers SET name = $1, version = version + 1, updated_at = $2
					WHERE organization_id = $3 AND business_unit_id = $4 AND id = $5 AND version = $6`,
					rid(), time.Now().Unix(), org, bu, c.ID, version)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n != 1 {
					return errConflict
				}
				version++
				return nil
			}, nil
		},
		close: func() { _ = db.Close() },
	}, nil
}
