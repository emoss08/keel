// Package fleet holds the models keel's tests run against.
package fleet

import (
	"context"
	"strings"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

// Status is the status of a shipment.
type Status string

const (
	StatusNew       Status = "New"
	StatusAssigned  Status = "Assigned"
	StatusInTransit Status = "InTransit"
	StatusDelivered Status = "Delivered"
	StatusCanceled  Status = "Canceled"
)

// Priority is a shipment's priority.
type Priority int16

// Customer is a tenant's customer.
type Customer struct {
	keel.Table `db:"customers"`

	ID             ids.ID  `db:"id,pk,autoid:cus_"`
	OrganizationID ids.ID  `db:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID  `db:"business_unit_id,pk,tenant"`
	Name           string  `db:"name"`
	Code           string  `db:"code"`
	Email          *string `db:"email"`
	Version        int64   `db:"version,version"`
	CreatedAt      int64   `db:"created_at,autocreate"`
	UpdatedAt      int64   `db:"updated_at,autoupdate"`
}

// Shipment is a load moved for a customer.
type Shipment struct {
	keel.Table `db:"shipments"`

	ID             ids.ID         `db:"id,pk,autoid:shp_"`
	OrganizationID ids.ID         `db:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID         `db:"business_unit_id,pk,tenant"`
	CustomerID     ids.ID         `db:"customer_id"`
	BillToID       *ids.ID        `db:"bill_to_id"`
	ProNumber      string         `db:"pro_number"`
	Status         Status         `db:"status"`
	Priority       Priority       `db:"priority"`
	Weight         *int64         `db:"weight"`
	Total          Money          `db:"total"`
	Hazmat         bool           `db:"hazmat"`
	Labels         []string       `db:"labels"`
	Attributes     map[string]any `db:"attributes,jsonb"`
	SearchVector   string         `db:"search_vector,noselect,tsvector"`
	StageRank      int16          `db:"stage_rank,readonly"`
	CanceledAt     *int64         `db:"canceled_at"`
	Version        int64          `db:"version,version"`
	CreatedAt      int64          `db:"created_at,autocreate"`
	UpdatedAt      int64          `db:"updated_at,autoupdate"`

	Customer *Customer `db:"belongs:customer_id"`
	BillTo   *Customer `db:"belongs:bill_to_id"`
	Moves    []*Move   `db:"many:shipment_id"`
	Tags     []*Tag    `db:"m2m:shipment_tags.shipment_id.tag_id"`
}

// Move is one leg of a shipment.
type Move struct {
	keel.Table `db:"moves"`

	ID             ids.ID `db:"id,pk,autoid:mov_"`
	OrganizationID ids.ID `db:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `db:"business_unit_id,pk,tenant"`
	ShipmentID     ids.ID `db:"shipment_id"`
	Sequence       int32  `db:"sequence"`
	Status         Status `db:"status"`
	Loaded         bool   `db:"loaded"`
	CreatedAt      int64  `db:"created_at,autocreate"`

	Shipment *Shipment `db:"belongs:shipment_id"`
	Stops    []*Stop   `db:"many:move_id"`
}

// Stop is a pickup or delivery on a move.
type Stop struct {
	keel.Table `db:"stops"`

	ID             ids.ID     `db:"id,pk,autoid:stp_"`
	OrganizationID ids.ID     `db:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID     `db:"business_unit_id,pk,tenant"`
	MoveID         ids.ID     `db:"move_id"`
	LocationID     ids.ID     `db:"location_id"`
	Sequence       int32      `db:"sequence"`
	Kind           string     `db:"kind"`
	ArrivedAt      *time.Time `db:"arrived_at"`

	Location *Location `db:"belongs:location_id"`
}

// Location is a place stops happen at.
type Location struct {
	keel.Table `db:"locations"`

	ID             ids.ID `db:"id,pk,autoid:loc_"`
	OrganizationID ids.ID `db:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `db:"business_unit_id,pk,tenant"`
	Name           string `db:"name"`
	City           string `db:"city"`
}

// Tag labels shipments.
type Tag struct {
	keel.Table `db:"tags"`

	ID             ids.ID `db:"id,pk,autoid:tag_"`
	OrganizationID ids.ID `db:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `db:"business_unit_id,pk,tenant"`
	Name           string `db:"name"`
}

// Worker drives moves; it reports to another worker.
type Worker struct {
	keel.Table `db:"workers"`

	ID             ids.ID    `db:"id,pk,autoid:wrk_"`
	OrganizationID ids.ID    `db:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID    `db:"business_unit_id,pk,tenant"`
	ManagerID      *ids.ID   `db:"manager_id"`
	FirstName      string    `db:"first_name"`
	LastName       string    `db:"last_name"`
	WholeName      string    `db:"whole_name,readonly"`
	HiredAt        time.Time `db:"hired_at,autocreate"`
	Initials       string    `db:"-"`

	Manager *Worker `db:"belongs:manager_id"`
}

// BeforeSave trims the names before every write.
func (w *Worker) BeforeSave(_ context.Context) error {
	w.FirstName = strings.TrimSpace(w.FirstName)
	w.LastName = strings.TrimSpace(w.LastName)
	return nil
}

// AfterLoad derives the initials of a loaded worker.
func (w *Worker) AfterLoad(_ context.Context) error {
	if w.FirstName != "" && w.LastName != "" {
		w.Initials = w.FirstName[:1] + w.LastName[:1]
	}
	return nil
}

// Plant is a table without tenant columns, shared by every tenant.
type Plant struct {
	keel.Table `db:"plants"`

	Code string `db:"code,pk"`
	Name string `db:"name"`
}
