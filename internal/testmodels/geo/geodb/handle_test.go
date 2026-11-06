package geodb

import (
	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/geo"
)

// Generated tables implement keel.Handle. keel gen writes no such check into the generated
// package, where it would compile Handle's method wrappers for every model.
var (
	_ keel.Handle[*geo.Place] = (*PlacesTable)(nil)
	_ keel.Handle[*geo.Zone]  = (*ZonesTable)(nil)
)
