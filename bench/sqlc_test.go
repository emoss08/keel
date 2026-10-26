package bench

import (
	"context"
	"time"

	"github.com/emoss08/keel/bench/sqlcdb"
	"github.com/jackc/pgx/v5"
)

func openSqlc(e *env) (*contender, error) {
	pool := e.pgx
	q := sqlcdb.New(pool)
	org, bu := string(e.org), string(e.bu)
	customerRow := func(name string, now int64) sqlcdb.InsertCustomerParams {
		return sqlcdb.InsertCustomerParams{ID: "cus_" + rid(), OrganizationID: org, BusinessUnitID: bu,
			Name: name, Code: rid(), CreatedAt: now, UpdatedAt: now}
	}
	return &contender{
		name: "sqlc",
		get: func(ctx context.Context, id string) error {
			_, err := q.GetShipment(ctx, sqlcdb.GetShipmentParams{OrganizationID: org, BusinessUnitID: bu, ID: id})
			return err
		},
		list: func(ctx context.Context, n int) error {
			rows, err := q.ListShipmentsByStatus(ctx, sqlcdb.ListShipmentsByStatusParams{
				OrganizationID: org, BusinessUnitID: bu, Status: sqlcdb.ShipmentStatusAssigned, Limit: int32(n),
			})
			if err != nil {
				return err
			}
			return checkLen("list", len(rows), n)
		},
		relations: func(ctx context.Context) error {
			ships, err := q.ListShipments(ctx, sqlcdb.ListShipmentsParams{OrganizationID: org, BusinessUnitID: bu, Limit: 50})
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
			customers, err := q.CustomersByID(ctx, sqlcdb.CustomersByIDParams{OrganizationID: org, BusinessUnitID: bu, Ids: customerIDs})
			if err != nil {
				return err
			}
			moves, err := q.MovesByShipment(ctx, sqlcdb.MovesByShipmentParams{OrganizationID: org, BusinessUnitID: bu, Ids: shipIDs})
			if err != nil {
				return err
			}
			byID := make(map[string]*sqlcdb.Customer, len(customers))
			for i := range customers {
				byID[customers[i].ID] = &customers[i]
			}
			movesOf := make(map[string][]sqlcdb.Move, len(ships))
			for _, m := range moves {
				movesOf[m.ShipmentID] = append(movesOf[m.ShipmentID], m)
			}
			return checkRelations(len(ships), len(movesOf[ships[0].ID]), byID[ships[0].CustomerID] != nil)
		},
		scoped: func(ctx context.Context, id string) error {
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			qtx := q.WithTx(tx)
			if err := qtx.SetOrg(ctx, org); err != nil {
				return err
			}
			if _, err := qtx.GetShipment(ctx, sqlcdb.GetShipmentParams{OrganizationID: org, BusinessUnitID: bu, ID: id}); err != nil {
				return err
			}
			return tx.Commit(ctx)
		},
		page: func(ctx context.Context) error {
			if _, err := q.CountShipments(ctx, sqlcdb.CountShipmentsParams{OrganizationID: org, BusinessUnitID: bu}); err != nil {
				return err
			}
			rows, err := q.PageShipmentsAfter(ctx, sqlcdb.PageShipmentsAfterParams{
				OrganizationID: org, BusinessUnitID: bu, AfterCreatedAt: e.cursor.createdAt, AfterID: e.cursor.id, Limit: 50,
			})
			if err != nil {
				return err
			}
			return checkLen("page", len(rows), 50)
		},
		create: func(ctx context.Context) error {
			return q.InsertCustomer(ctx, customerRow("bench", time.Now().Unix()))
		},
		createMany: func(ctx context.Context, n int) error {
			now := time.Now().Unix()
			rows := make([]sqlcdb.CopyCustomersParams, n)
			for i := range rows {
				rows[i] = sqlcdb.CopyCustomersParams(customerRow("bench", now))
			}
			_, err := q.CopyCustomers(ctx, rows)
			return err
		},
		save: func(ctx context.Context) (func(ctx context.Context) error, error) {
			c := customerRow("save", time.Now().Unix())
			if err := q.InsertCustomer(ctx, c); err != nil {
				return nil, err
			}
			version := c.Version
			return func(ctx context.Context) error {
				row, err := q.SaveCustomer(ctx, sqlcdb.SaveCustomerParams{
					OrganizationID: org, BusinessUnitID: bu, ID: c.ID, Name: rid(),
					UpdatedAt: time.Now().Unix(), Version: version,
				})
				if err != nil {
					return err
				}
				version = row.Version
				return nil
			}, nil
		},
	}, nil
}
