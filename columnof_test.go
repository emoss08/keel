package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestColumnOf(t *testing.T) {
	s := fleetdb.Shipments
	table, index, isCol := keel.ColumnOf(s.CreatedAt)
	if !isCol || table != fleetdb.ShipmentsSchema() || table.Columns[index].Name != "created_at" {
		t.Fatalf("ColumnOf(Shipments.CreatedAt) = %v, %d, %v", table, index, isCol)
	}
	table, index, isCol = keel.ColumnOf(s.As("x").Status)
	if !isCol || table != fleetdb.ShipmentsSchema() || table.Columns[index].Name != "status" {
		t.Fatal("a column of an alias is the table's own column")
	}
	for _, e := range []keel.Expr{s.Customer().Name, keel.Val(1), s.Total.Sum()} {
		if _, _, isCol := keel.ColumnOf(e); isCol {
			t.Fatalf("ColumnOf(%T) reported a column of the table", e)
		}
	}
}

func TestTenantColumns(t *testing.T) {
	d := db(t)
	cols := d.TenantColumns()
	eq(t, strings.Join(cols, ","), "organization_id,business_unit_id")
	cols[0] = "changed"
	eq(t, d.TenantColumns()[0], "organization_id")
}
