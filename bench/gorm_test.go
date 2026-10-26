package bench

import (
	"context"
	"database/sql"
	"time"

	"github.com/emoss08/keel/bench/benchjson"
	"github.com/lib/pq"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type gormCustomer struct {
	ID             string `gorm:"primaryKey"`
	OrganizationID string `gorm:"primaryKey"`
	BusinessUnitID string `gorm:"primaryKey"`
	Name           string
	Code           string
	Email          *string
	Version        int64
	CreatedAt      int64 `gorm:"autoCreateTime"`
	UpdatedAt      int64 `gorm:"autoUpdateTime"`
}

func (gormCustomer) TableName() string { return "customers" }

type gormMove struct {
	ID             string `gorm:"primaryKey"`
	OrganizationID string `gorm:"primaryKey"`
	BusinessUnitID string `gorm:"primaryKey"`
	ShipmentID     string
	Sequence       int32
	Status         string
	Loaded         bool
	CreatedAt      int64
}

func (gormMove) TableName() string { return "moves" }

type gormShipment struct {
	ID             string `gorm:"primaryKey"`
	OrganizationID string `gorm:"primaryKey"`
	BusinessUnitID string `gorm:"primaryKey"`
	CustomerID     string
	BillToID       *string
	ProNumber      string
	Status         string
	Priority       int16
	Weight         *int64
	Total          string
	Hazmat         bool
	Labels         pq.StringArray `gorm:"type:text[]"`
	Attributes     benchjson.Map  `gorm:"type:jsonb"`
	StageRank      int16          `gorm:"->"`
	CanceledAt     *int64
	Version        int64
	CreatedAt      int64
	UpdatedAt      int64

	Customer *gormCustomer `gorm:"foreignKey:CustomerID,OrganizationID,BusinessUnitID;references:ID,OrganizationID,BusinessUnitID"`
	Moves    []gormMove    `gorm:"foreignKey:ShipmentID,OrganizationID,BusinessUnitID;references:ID,OrganizationID,BusinessUnitID"`
}

func (gormShipment) TableName() string { return "shipments" }

func openGORM(e *env) (*contender, error) {
	sqldb, err := openSQL(e.dsn)
	if err != nil {
		return nil, err
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqldb}), &gorm.Config{
		Logger:                 logger.Discard,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, err
	}
	org, bu := string(e.org), string(e.bu)
	tenant := func(tx *gorm.DB) *gorm.DB {
		return tx.Where("organization_id = ? AND business_unit_id = ?", org, bu)
	}
	dry := db.Session(&gorm.Session{DryRun: true})
	return &contender{
		name: "gorm",
		get: func(ctx context.Context, id string) error {
			var s gormShipment
			return db.WithContext(ctx).Scopes(tenant).Where("id = ?", id).First(&s).Error
		},
		list: func(ctx context.Context, n int) error {
			var rows []gormShipment
			err := db.WithContext(ctx).Scopes(tenant).Where("status = ?", "Assigned").
				Order("pro_number").Limit(n).Find(&rows).Error
			if err != nil {
				return err
			}
			return checkLen("list", len(rows), n)
		},
		relations: func(ctx context.Context) error {
			var rows []gormShipment
			err := db.WithContext(ctx).Scopes(tenant).
				Preload("Customer", tenant).
				Preload("Moves", func(tx *gorm.DB) *gorm.DB { return tenant(tx).Order("sequence") }).
				Order("pro_number").Limit(50).Find(&rows).Error
			if err != nil {
				return err
			}
			return checkRelations(len(rows), len(rows[0].Moves), rows[0].Customer != nil)
		},
		scoped: func(ctx context.Context, id string) error {
			return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec("SELECT set_config('app.org', ?, true)", org).Error; err != nil {
					return err
				}
				var s gormShipment
				return tx.Scopes(tenant).Where("id = ?", id).First(&s).Error
			}, &sql.TxOptions{ReadOnly: true})
		},
		page: func(ctx context.Context) error {
			var rows []gormShipment
			var total int64
			if err := db.WithContext(ctx).Model(&gormShipment{}).Scopes(tenant).Count(&total).Error; err != nil {
				return err
			}
			err := db.WithContext(ctx).Scopes(tenant).
				Where("(created_at, id) < (?, ?)", e.cursor.createdAt, e.cursor.id).
				Order("created_at DESC, id DESC").Limit(50).Find(&rows).Error
			if err != nil {
				return err
			}
			return checkLen("page", len(rows), 50)
		},
		create: func(ctx context.Context) error {
			c := gormCustomer{ID: "cus_" + rid(), OrganizationID: org, BusinessUnitID: bu, Name: "bench", Code: rid()}
			return db.WithContext(ctx).Create(&c).Error
		},
		createMany: func(ctx context.Context, n int) error {
			rows := make([]gormCustomer, n)
			for i := range rows {
				rows[i] = gormCustomer{ID: "cus_" + rid(), OrganizationID: org, BusinessUnitID: bu, Name: "bench", Code: rid()}
			}
			return db.WithContext(ctx).Create(&rows).Error
		},
		save: func(ctx context.Context) (func(ctx context.Context) error, error) {
			c := gormCustomer{ID: "cus_" + rid(), OrganizationID: org, BusinessUnitID: bu, Name: "save", Code: rid()}
			if err := db.WithContext(ctx).Create(&c).Error; err != nil {
				return nil, err
			}
			key := gormCustomer{ID: c.ID, OrganizationID: org, BusinessUnitID: bu}
			version := c.Version
			return func(ctx context.Context) error {
				row := key
				res := db.WithContext(ctx).Model(&row).Where("version = ?", version).Updates(map[string]any{
					"name": rid(), "version": version + 1, "updated_at": time.Now().Unix(),
				})
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					return errConflict
				}
				version++
				return nil
			}, nil
		},
		build: func(ctx context.Context) error {
			var rows []gormShipment
			stmt := dry.WithContext(ctx).Scopes(tenant).
				Where("status IN ?", []string{"New", "Assigned"}).Where("pro_number LIKE ?", "%x%").
				Order("created_at DESC").Limit(50).Find(&rows).Statement
			_ = stmt.SQL.String()
			return nil
		},
		close: func() { _ = sqldb.Close() },
	}, nil
}
