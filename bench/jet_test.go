package bench

import (
	"context"
	"database/sql"
	"time"

	"github.com/emoss08/keel/bench/jet/enum"
	"github.com/emoss08/keel/bench/jet/model"
	. "github.com/emoss08/keel/bench/jet/table"
	. "github.com/go-jet/jet/v2/postgres"
)

func openJet(e *env) (*contender, error) {
	db, err := openSQL(e.dsn)
	if err != nil {
		return nil, err
	}
	org, bu := String(string(e.org)), String(string(e.bu))
	shipmentColumns := Shipments.AllColumns.Except(Shipments.SearchVector)
	tenant := Shipments.OrganizationID.EQ(org).AND(Shipments.BusinessUnitID.EQ(bu))
	newCustomer := func(name string, now int64) model.Customers {
		return model.Customers{ID: "cus_" + rid(), OrganizationID: string(e.org), BusinessUnitID: string(e.bu),
			Name: name, Code: rid(), CreatedAt: now, UpdatedAt: now}
	}
	getStmt := func(id string) SelectStatement {
		return SELECT(shipmentColumns).FROM(Shipments).WHERE(tenant.AND(Shipments.ID.EQ(String(id))))
	}
	return &contender{
		name: "jet",
		get: func(ctx context.Context, id string) error {
			var s model.Shipments
			return getStmt(id).QueryContext(ctx, db, &s)
		},
		list: func(ctx context.Context, n int) error {
			var rows []model.Shipments
			err := SELECT(shipmentColumns).FROM(Shipments).
				WHERE(tenant.AND(Shipments.Status.EQ(enum.ShipmentStatus.Assigned))).
				ORDER_BY(Shipments.ProNumber.ASC()).LIMIT(int64(n)).
				QueryContext(ctx, db, &rows)
			if err != nil {
				return err
			}
			return checkLen("list", len(rows), n)
		},
		relations: func(ctx context.Context) error {
			page := SELECT(shipmentColumns).FROM(Shipments).WHERE(tenant).
				ORDER_BY(Shipments.ProNumber.ASC()).LIMIT(50).AsTable("shipments")
			var rows []struct {
				model.Shipments
				Customer *model.Customers
				Moves    []model.Moves
			}
			err := SELECT(page.AllColumns(), Customers.AllColumns, Moves.AllColumns).
				FROM(page.
					INNER_JOIN(Customers, Customers.ID.EQ(Shipments.CustomerID.From(page)).
						AND(Customers.OrganizationID.EQ(org)).AND(Customers.BusinessUnitID.EQ(bu))).
					LEFT_JOIN(Moves, Moves.ShipmentID.EQ(Shipments.ID.From(page)).
						AND(Moves.OrganizationID.EQ(org)).AND(Moves.BusinessUnitID.EQ(bu)))).
				ORDER_BY(Shipments.ProNumber.From(page).ASC(), Moves.Sequence.ASC()).
				QueryContext(ctx, db, &rows)
			if err != nil {
				return err
			}
			return checkRelations(len(rows), len(rows[0].Moves), rows[0].Customer != nil)
		},
		scoped: func(ctx context.Context, id string) error {
			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "SELECT set_config('app.org', $1, true)", string(e.org)); err != nil {
				_ = tx.Rollback()
				return err
			}
			var s model.Shipments
			if err := getStmt(id).QueryContext(ctx, tx, &s); err != nil {
				_ = tx.Rollback()
				return err
			}
			return tx.Commit()
		},
		page: func(ctx context.Context) error {
			var total struct{ Count int64 }
			if err := SELECT(COUNT(STAR).AS("count")).FROM(Shipments).WHERE(tenant).QueryContext(ctx, db, &total); err != nil {
				return err
			}
			var rows []model.Shipments
			err := SELECT(shipmentColumns).FROM(Shipments).
				WHERE(tenant.AND(ROW(Shipments.CreatedAt, Shipments.ID).LT(
					ROW(Int(e.cursor.createdAt), String(e.cursor.id))))).
				ORDER_BY(Shipments.CreatedAt.DESC(), Shipments.ID.DESC()).LIMIT(50).
				QueryContext(ctx, db, &rows)
			if err != nil {
				return err
			}
			return checkLen("page", len(rows), 50)
		},
		create: func(ctx context.Context) error {
			_, err := Customers.INSERT(Customers.AllColumns).MODEL(newCustomer("bench", time.Now().Unix())).ExecContext(ctx, db)
			return err
		},
		createMany: func(ctx context.Context, n int) error {
			now := time.Now().Unix()
			rows := make([]model.Customers, n)
			for i := range rows {
				rows[i] = newCustomer("bench", now)
			}
			_, err := Customers.INSERT(Customers.AllColumns).MODELS(rows).ExecContext(ctx, db)
			return err
		},
		save: func(ctx context.Context) (func(ctx context.Context) error, error) {
			c := newCustomer("save", time.Now().Unix())
			if _, err := Customers.INSERT(Customers.AllColumns).MODEL(c).ExecContext(ctx, db); err != nil {
				return nil, err
			}
			version := c.Version
			return func(ctx context.Context) error {
				res, err := Customers.UPDATE(Customers.Name, Customers.Version, Customers.UpdatedAt).
					SET(String(rid()), Int(version+1), Int(time.Now().Unix())).
					WHERE(Customers.ID.EQ(String(c.ID)).AND(Customers.OrganizationID.EQ(org)).
						AND(Customers.BusinessUnitID.EQ(bu)).AND(Customers.Version.EQ(Int(version)))).
					ExecContext(ctx, db)
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
		build: func(context.Context) error {
			_, _ = SELECT(shipmentColumns).FROM(Shipments).
				WHERE(Shipments.OrganizationID.EQ(String("org")).AND(Shipments.BusinessUnitID.EQ(String("bu"))).
					AND(Shipments.Status.IN(enum.ShipmentStatus.New, enum.ShipmentStatus.Assigned)).
					AND(Shipments.ProNumber.LIKE(String("%x%")))).
				ORDER_BY(Shipments.CreatedAt.DESC()).LIMIT(50).Sql()
			return nil
		},
		close: func() { _ = db.Close() },
	}, nil
}
