// Package fleet holds the models keel's tests run against.
package fleet

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

// Status is the status of a shipment.
type Status string

const (
	// StatusNew is a shipment nobody has planned yet.
	StatusNew Status = "New"
	// StatusAssigned is a shipment with a driver.
	StatusAssigned Status = "Assigned"
	// StatusInTransit is a shipment on the road.
	StatusInTransit Status = "InTransit"
	// StatusDelivered is a shipment at its destination.
	StatusDelivered Status = "Delivered"
	// StatusCanceled is a shipment that will not move.
	StatusCanceled Status = "Canceled"
)

// Priority is a shipment's priority.
type Priority int16

const (
	// PriorityNormal is the default priority.
	PriorityNormal Priority = 0
	// PriorityHigh moves ahead of normal shipments.
	PriorityHigh Priority = 1
	// PriorityUrgent moves first.
	PriorityUrgent Priority = 2
)

// Customer is a tenant's customer.
type Customer struct {
	keel.Table `keel:"customers,report=Sales"`

	ID             ids.ID `keel:"id,pk,autoid:cus_"`
	OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	// Name is the customer's trading name.
	Name string `keel:"name" json:"name"`
	// Code is the customer's short code, unique per tenant.
	Code      string  `keel:"code"`
	Email     *string `keel:"email" json:"email,omitempty"`
	Version   int64   `keel:"version,version"`
	CreatedAt int64   `keel:"created_at,autocreate"`
	UpdatedAt int64   `keel:"updated_at,autoupdate"`
}

// Shipment is a load moved for a customer.
type Shipment struct {
	keel.Table `keel:"shipments,report=Operations"`

	ID             ids.ID  `keel:"id,pk,autoid:shp_"`
	OrganizationID ids.ID  `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID  `keel:"business_unit_id,pk,tenant"`
	CustomerID     ids.ID  `keel:"customer_id"`
	BillToID       *ids.ID `keel:"bill_to_id"`
	// ProNumber is the carrier's progressive number, unique per tenant.
	ProNumber string   `keel:"pro_number,label=PRO Number" json:"proNumber"`
	Status    Status   `keel:"status"`
	Priority  Priority `keel:"priority"`
	// Weight is the shipment's gross weight in pounds.
	Weight       *int64         `keel:"weight,format=weight"`
	Total        Money          `keel:"total,type=decimal"`
	Hazmat       bool           `keel:"hazmat"`
	Labels       []string       `keel:"labels"`
	Attributes   map[string]any `keel:"attributes,jsonb,noreport"`
	SearchVector string         `keel:"search_vector,noselect,readonly,tsvector" json:"-"`
	StageRank    int16          `keel:"stage_rank,readonly,dimension"`
	CanceledAt   *int64         `keel:"canceled_at"`
	Version      int64          `keel:"version,version"`
	CreatedAt    int64          `keel:"created_at,autocreate"`
	UpdatedAt    int64          `keel:"updated_at,autoupdate"`

	Customer *Customer `keel:"belongs:customer_id" json:"customer"`
	BillTo   *Customer `keel:"belongs:bill_to_id,label=Bill-to Customer"`
	Moves    []*Move   `keel:"many:shipment_id"`
	Tags     []*Tag    `keel:"m2m:shipment_tags.shipment_id.tag_id,noreport"`
	// OriginStop is the first stop of the shipment's first move.
	OriginStop *Stop `keel:"pick:moves.stops,first=moves.sequence|stops.sequence"`
	// DestinationStop is the last stop of the shipment's last move.
	DestinationStop *Stop `keel:"pick:moves.stops,last=moves.sequence|stops.sequence"`
}

// Move is one leg of a shipment.
type Move struct {
	keel.Table `keel:"moves,report=Operations"`

	ID             ids.ID `keel:"id,pk,autoid:mov_"`
	OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	ShipmentID     ids.ID `keel:"shipment_id"`
	// Sequence orders the moves of a shipment.
	Sequence  int32  `keel:"sequence,dimension,agg=min|max"`
	Status    Status `keel:"status"`
	Loaded    bool   `keel:"loaded"`
	CreatedAt int64  `keel:"created_at,autocreate"`

	Shipment *Shipment `keel:"belongs:shipment_id"`
	Stops    []*Stop   `keel:"many:move_id"`
	// FirstStop is the move's first stop.
	FirstStop *Stop `keel:"pick:stops,first=stops.sequence"`
}

// Stop is a pickup or delivery on a move.
type Stop struct {
	keel.Table `keel:"stops,report=Operations"`

	ID             ids.ID     `keel:"id,pk,autoid:stp_"`
	OrganizationID ids.ID     `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID     `keel:"business_unit_id,pk,tenant"`
	MoveID         ids.ID     `keel:"move_id"`
	LocationID     ids.ID     `keel:"location_id"`
	Sequence       int32      `keel:"sequence"`
	Kind           string     `keel:"kind"`
	ArrivedAt      *time.Time `keel:"arrived_at"`

	Location *Location `keel:"belongs:location_id"`
}

// Location is a place stops happen at.
type Location struct {
	keel.Table `keel:"locations,report=Operations"`

	ID             ids.ID `keel:"id,pk,autoid:loc_"`
	OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	Name           string `keel:"name"`
	City           string `keel:"city"`
}

// Tag labels shipments.
type Tag struct {
	keel.Table `keel:"tags"`

	ID             ids.ID `keel:"id,pk,autoid:tag_"`
	OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	Name           string `keel:"name"`
}

// Worker drives moves; it reports to another worker.
type Worker struct {
	keel.Table `keel:"workers"`

	ID             ids.ID    `keel:"id,pk,autoid:wrk_"`
	OrganizationID ids.ID    `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID    `keel:"business_unit_id,pk,tenant"`
	ManagerID      *ids.ID   `keel:"manager_id"`
	FirstName      string    `keel:"first_name"`
	LastName       string    `keel:"last_name"`
	WholeName      string    `keel:"whole_name,readonly"`
	HiredAt        time.Time `keel:"hired_at,autocreate"`
	Initials       string    `keel:"-"`

	Manager *Worker `keel:"belongs:manager_id"`
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

// Contact is a person at a customer. Its optional fields are nullzero: an empty value is stored as
// NULL and NULL reads back as the empty value.
type Contact struct {
	keel.Table `keel:"contacts"`

	ID             ids.ID         `keel:"id,pk,autoid:con_"`
	OrganizationID ids.ID         `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID         `keel:"business_unit_id,pk,tenant"`
	CustomerID     ids.ID         `keel:"customer_id,nullzero"`
	Name           string         `keel:"name"`
	Phone          string         `keel:"phone,nullzero"`
	Floor          int16          `keel:"floor,nullzero"`
	Visits         int32          `keel:"visits,nullzero"`
	Score          int64          `keel:"score,nullzero"`
	Rating         float64        `keel:"rating,nullzero"`
	Weight         float32        `keel:"weight,nullzero"`
	VIP            bool           `keel:"vip,nullzero"`
	Status         Status         `keel:"status,nullzero"`
	Priority       Priority       `keel:"priority,nullzero"`
	LastSeenAt     time.Time      `keel:"last_seen_at,nullzero"`
	Balance        Money          `keel:"balance,nullzero"`
	Notes          []string       `keel:"notes,nullzero"`
	Prefs          map[string]any `keel:"prefs,jsonb,nullzero"`
	Avatar         []byte         `keel:"avatar,nullzero"`
	Version        int64          `keel:"version,version"`
	// CustomerName is the name of the contact's customer, filled by queries that compute it.
	CustomerName string `keel:"customer_name,computed,nullzero"`
	// Loads counts the shipments of the contact's customer, filled by queries that compute it.
	Loads int64 `keel:"loads,computed"`

	Customer *Customer `keel:"belongs:customer_id"`
}

// Note is a free-text note about a customer, searched by its words.
type Note struct {
	keel.Table `keel:"notes"`

	ID             ids.ID `keel:"id,pk,autoid:not_"`
	OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	CustomerID     ids.ID `keel:"customer_id"`
	Body           string `keel:"body"`
	// SearchVector is the note's body stemmed in English.
	SearchVector string `keel:"search_vector,noselect,readonly,tsvector=english"`
	// Rank is how well the note matches a search, filled by queries that compute it.
	Rank float32 `keel:"rank,computed"`

	Customer *Customer `keel:"belongs:customer_id"`
}

// Leave is a worker's paid time off.
type Leave struct {
	keel.Table `keel:"leaves,report=Workforce"`

	ID             ids.ID `keel:"id,pk,autoid:lv_"`
	OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	WorkerID       ids.ID `keel:"worker_id"`
	Days           int32  `keel:"days"`

	// Entries are the ledger entries that draw on the leave, which belong to the same worker.
	Entries []*LeaveEntry `keel:"many:source_leave_id|worker_id"`
	// LastEntry is the leave's latest ledger entry.
	LastEntry *LeaveEntry `keel:"pick:entries,last=entries.posted_at"`
}

// LeaveEntry is a ledger entry of a worker's leave balance. It draws on a leave of the same
// worker: its relation to the leave joins on both columns.
type LeaveEntry struct {
	keel.Table `keel:"leave_entries,report=Workforce"`

	ID             ids.ID `keel:"id,pk,autoid:lve_"`
	OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	WorkerID       ids.ID `keel:"worker_id"`
	SourceLeaveID  ids.ID `keel:"source_leave_id,nullzero"`
	Hours          int32  `keel:"hours"`
	PostedAt       int64  `keel:"posted_at"`

	// Source is the leave the entry draws on.
	Source *Leave `keel:"belongs:source_leave_id|worker_id"`
}

// Plant is a table without tenant columns, shared by every tenant.
type Plant struct {
	keel.Table `keel:"plants"`

	Code string `keel:"code,pk"`
	Name string `keel:"name"`
}

// Depot is a business unit's depot, shared by every organization of the business unit: its tenant
// key holds only the business unit, which is not a prefix of the scope's organization-first key.
type Depot struct {
	keel.Table `keel:"depots"`

	ID             ids.ID `keel:"id,pk,autoid:dep_"`
	BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
	Name           string `keel:"name"`
}

// AuditEntry records a tool call. Its arguments and result keep every digit of a large integer:
// their JSON numbers decode as json.Number (jsonnumber).
type AuditEntry struct {
	keel.Table `keel:"audit_entries"`

	ID             ids.ID         `keel:"id,pk,autoid:aud_"`
	OrganizationID ids.ID         `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID         `keel:"business_unit_id,pk,tenant"`
	Tool           string         `keel:"tool"`
	Args           map[string]any `keel:"args,jsonb,jsonnumber"`
	Result         []any          `keel:"result,jsonb,jsonnumber,nullzero"`
	Raw            *any           `keel:"raw,jsonb,jsonnumber"`
	Meta           map[string]any `keel:"meta,jsonb"`
}

// Badge has columns whose database defaults fill an insert that leaves them unset.
type Badge struct {
	keel.Table `keel:"badges"`

	ID             ids.ID         `keel:"id,pk,autoid:bdg_"`
	OrganizationID ids.ID         `keel:"organization_id,pk,tenant"`
	BusinessUnitID ids.ID         `keel:"business_unit_id,pk,tenant"`
	Name           string         `keel:"name"`
	Level          *int32         `keel:"level,default"`
	Color          string         `keel:"color,nullzero,default"`
	Note           sql.NullString `keel:"note,default"`
}
