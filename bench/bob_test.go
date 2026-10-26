package bench

import (
	"context"
	"database/sql"
	"time"

	"github.com/aarondl/opt/omit"
	"github.com/emoss08/keel/bench/bob/enums"
	"github.com/emoss08/keel/bench/bob/models"
	"github.com/stephenafamo/bob"
	"github.com/stephenafamo/bob/dialect/psql"
	"github.com/stephenafamo/bob/dialect/psql/dialect"
	"github.com/stephenafamo/bob/dialect/psql/sm"
)

func openBob(e *env) (*contender, error) {
	sqldb, err := openSQL(e.dsn)
	if err != nil {
		return nil, err
	}
	db := bob.NewDB(sqldb)
	org, bu := string(e.org), string(e.bu)
	ships := models.Shipments.Columns
	tenant := []bob.Mod[*dialect.SelectQuery]{
		models.SelectWhere.Shipments.OrganizationID.EQ(org),
		models.SelectWhere.Shipments.BusinessUnitID.EQ(bu),
	}
	shipmentQuery := func(mods ...bob.Mod[*dialect.SelectQuery]) *psql.ViewQuery[*models.Shipment, models.ShipmentSlice] {
		return models.Shipments.Query(append(append([]bob.Mod[*dialect.SelectQuery]{}, tenant...), mods...)...)
	}
	newCustomer := func(name string, now int64) *models.CustomerSetter {
		return &models.CustomerSetter{
			ID: omit.From("cus_" + rid()), OrganizationID: omit.From(org), BusinessUnitID: omit.From(bu),
			Name: omit.From(name), Code: omit.From(rid()), Version: omit.From(int64(0)),
			CreatedAt: omit.From(now), UpdatedAt: omit.From(now),
		}
	}
	return &contender{
		name: "bob",
		get: func(ctx context.Context, id string) error {
			_, err := shipmentQuery(models.SelectWhere.Shipments.ID.EQ(id)).One(ctx, db)
			return err
		},
		list: func(ctx context.Context, n int) error {
			rows, err := shipmentQuery(
				models.SelectWhere.Shipments.Status.EQ(enums.ShipmentStatusAssigned),
				sm.OrderBy(ships.ProNumber), sm.Limit(n),
			).All(ctx, db)
			if err != nil {
				return err
			}
			return checkLen("list", len(rows), n)
		},
		relations: func(ctx context.Context) error {
			rows, err := shipmentQuery(
				models.Preload.Shipment.Customer(),
				models.SelectThenLoad.Shipment.Moves(
					models.SelectWhere.Moves.OrganizationID.EQ(org),
					models.SelectWhere.Moves.BusinessUnitID.EQ(bu),
					sm.OrderBy(models.Moves.Columns.Sequence),
				),
				sm.OrderBy(ships.ProNumber), sm.Limit(50),
			).All(ctx, db)
			if err != nil {
				return err
			}
			return checkRelations(len(rows), len(rows[0].R.Moves), rows[0].R.Customer != nil)
		},
		scoped: func(ctx context.Context, id string) error {
			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "SELECT set_config('app.org', $1, true)", org); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if _, err := shipmentQuery(models.SelectWhere.Shipments.ID.EQ(id)).One(ctx, tx); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			return tx.Commit(ctx)
		},
		page: func(ctx context.Context) error {
			if _, err := shipmentQuery().Count(ctx, db); err != nil {
				return err
			}
			rows, err := shipmentQuery(
				sm.Where(psql.Group(ships.CreatedAt, ships.ID).LT(
					psql.Group(psql.Arg(e.cursor.createdAt), psql.Arg(e.cursor.id)))),
				sm.OrderBy(ships.CreatedAt).Desc(), sm.OrderBy(ships.ID).Desc(), sm.Limit(50),
			).All(ctx, db)
			if err != nil {
				return err
			}
			return checkLen("page", len(rows), 50)
		},
		create: func(ctx context.Context) error {
			_, err := models.Customers.Insert(newCustomer("bench", time.Now().Unix())).Exec(ctx, db)
			return err
		},
		createMany: func(ctx context.Context, n int) error {
			now := time.Now().Unix()
			rows := make([]bob.Mod[*dialect.InsertQuery], n)
			for i := range rows {
				rows[i] = newCustomer("bench", now)
			}
			_, err := models.Customers.Insert(rows...).Exec(ctx, db)
			return err
		},
		save: func(ctx context.Context) (func(ctx context.Context) error, error) {
			c, err := models.Customers.Insert(newCustomer("save", time.Now().Unix())).One(ctx, db)
			if err != nil {
				return nil, err
			}
			version := c.Version
			return func(ctx context.Context) error {
				n, err := models.Customers.Update(
					models.CustomerSetter{
						Name: omit.From(rid()), Version: omit.From(version + 1),
						UpdatedAt: omit.From(time.Now().Unix()),
					}.UpdateMod(),
					models.UpdateWhere.Customers.ID.EQ(c.ID),
					models.UpdateWhere.Customers.OrganizationID.EQ(org),
					models.UpdateWhere.Customers.BusinessUnitID.EQ(bu),
					models.UpdateWhere.Customers.Version.EQ(version),
				).Exec(ctx, db)
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
		build: func(ctx context.Context) error {
			q := psql.Select(
				sm.Columns(models.Shipments.Columns),
				sm.From(models.Shipments.Name()),
				models.SelectWhere.Shipments.OrganizationID.EQ("org"),
				models.SelectWhere.Shipments.BusinessUnitID.EQ("bu"),
				models.SelectWhere.Shipments.Status.In(enums.ShipmentStatusNew, enums.ShipmentStatusAssigned),
				models.SelectWhere.Shipments.ProNumber.Like("%x%"),
				sm.OrderBy(ships.CreatedAt).Desc(), sm.Limit(50),
			)
			_, _, err := q.Build(ctx)
			return err
		},
		close: func() { _ = sqldb.Close() },
	}, nil
}
