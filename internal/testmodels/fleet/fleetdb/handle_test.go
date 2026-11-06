package fleetdb

import (
	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
)

// Generated tables implement keel.Handle. keel gen writes no such check into the generated
// package, where it would compile Handle's method wrappers for every model.
var (
	_ keel.Handle[*fleet.AuditEntry] = (*AuditEntriesTable)(nil)
	_ keel.Handle[*fleet.Badge]      = (*BadgesTable)(nil)
	_ keel.Handle[*fleet.Contact]    = (*ContactsTable)(nil)
	_ keel.Handle[*fleet.Customer]   = (*CustomersTable)(nil)
	_ keel.Handle[*fleet.Depot]      = (*DepotsTable)(nil)
	_ keel.Handle[*fleet.Leave]      = (*LeavesTable)(nil)
	_ keel.Handle[*fleet.LeaveEntry] = (*LeaveEntriesTable)(nil)
	_ keel.Handle[*fleet.Location]   = (*LocationsTable)(nil)
	_ keel.Handle[*fleet.Move]       = (*MovesTable)(nil)
	_ keel.Handle[*fleet.Note]       = (*NotesTable)(nil)
	_ keel.Handle[*fleet.Plant]      = (*PlantsTable)(nil)
	_ keel.Handle[*fleet.Shipment]   = (*ShipmentsTable)(nil)
	_ keel.Handle[*fleet.Stop]       = (*StopsTable)(nil)
	_ keel.Handle[*fleet.Tag]        = (*TagsTable)(nil)
	_ keel.Handle[*fleet.Worker]     = (*WorkersTable)(nil)
)
