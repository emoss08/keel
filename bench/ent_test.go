package bench

import (
	"context"
	"database/sql"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/emoss08/keel/bench/ent"
	"github.com/emoss08/keel/bench/ent/customer"
	"github.com/emoss08/keel/bench/ent/move"
	"github.com/emoss08/keel/bench/ent/shipment"
)

func openEnt(e *env) (*contender, error) {
	sqldb, err := openSQL(e.dsn)
	if err != nil {
		return nil, err
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqldb)))
	org, bu := string(e.org), string(e.bu)
	shipments := func(c *ent.ShipmentClient) *ent.ShipmentQuery {
		return c.Query().Where(shipment.OrganizationID(org), shipment.BusinessUnitID(bu))
	}
	newCustomer := func(c *ent.CustomerClient, name string, now int64) *ent.CustomerCreate {
		return c.Create().SetID("cus_" + rid()).SetOrganizationID(org).SetBusinessUnitID(bu).
			SetName(name).SetCode(rid()).SetVersion(0).SetCreatedAt(now).SetUpdatedAt(now)
	}
	return &contender{
		name: "ent",
		get: func(ctx context.Context, id string) error {
			_, err := shipments(client.Shipment).Where(shipment.ID(id)).Only(ctx)
			return err
		},
		list: func(ctx context.Context, n int) error {
			rows, err := shipments(client.Shipment).Where(shipment.Status("Assigned")).
				Order(shipment.ByProNumber()).Limit(n).All(ctx)
			if err != nil {
				return err
			}
			return checkLen("list", len(rows), n)
		},
		relations: func(ctx context.Context) error {
			rows, err := shipments(client.Shipment).
				WithCustomer(func(q *ent.CustomerQuery) {
					q.Where(customer.OrganizationID(org), customer.BusinessUnitID(bu))
				}).
				WithMoves(func(q *ent.MoveQuery) {
					q.Where(move.OrganizationID(org), move.BusinessUnitID(bu)).Order(move.BySequence())
				}).
				Order(shipment.ByProNumber()).Limit(50).All(ctx)
			if err != nil {
				return err
			}
			return checkRelations(len(rows), len(rows[0].Edges.Moves), rows[0].Edges.Customer != nil)
		},
		scoped: func(ctx context.Context, id string) error {
			tx, err := client.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "SELECT set_config('app.org', $1, true)", org); err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := shipments(tx.Shipment).Where(shipment.ID(id)).Only(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}
			return tx.Commit()
		},
		page: func(ctx context.Context) error {
			if _, err := shipments(client.Shipment).Count(ctx); err != nil {
				return err
			}
			rows, err := shipments(client.Shipment).
				Where(shipment.Or(
					shipment.CreatedAtLT(e.cursor.createdAt),
					shipment.And(shipment.CreatedAt(e.cursor.createdAt), shipment.IDLT(e.cursor.id)),
				)).
				Order(shipment.ByCreatedAt(entsql.OrderDesc()), shipment.ByID(entsql.OrderDesc())).
				Limit(50).All(ctx)
			if err != nil {
				return err
			}
			return checkLen("page", len(rows), 50)
		},
		create: func(ctx context.Context) error {
			return newCustomer(client.Customer, "bench", time.Now().Unix()).Exec(ctx)
		},
		createMany: func(ctx context.Context, n int) error {
			now := time.Now().Unix()
			rows := make([]*ent.CustomerCreate, n)
			for i := range rows {
				rows[i] = newCustomer(client.Customer, "bench", now)
			}
			return client.Customer.CreateBulk(rows...).Exec(ctx)
		},
		save: func(ctx context.Context) (func(ctx context.Context) error, error) {
			c, err := newCustomer(client.Customer, "save", time.Now().Unix()).Save(ctx)
			if err != nil {
				return nil, err
			}
			version := c.Version
			return func(ctx context.Context) error {
				n, err := client.Customer.Update().
					Where(customer.ID(c.ID), customer.OrganizationID(org), customer.BusinessUnitID(bu),
						customer.Version(version)).
					SetName(rid()).SetVersion(version + 1).SetUpdatedAt(time.Now().Unix()).
					Save(ctx)
				if err != nil {
					return err
				}
				if n != 1 {
					return errConflict
				}
				version++
				return nil
			}, nil
		},
		close: func() { _ = client.Close() },
	}, nil
}
