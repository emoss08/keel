package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestInlineSQLWritesColumnsUnderTheAliasAndValuesAsLiterals(t *testing.T) {
	s := fleetdb.Shipments
	sql, cols, err := keel.InlineSQL(
		keel.And(s.Status.Eq(fleet.StatusDelivered), keel.Raw("? > ?", s.Weight, 5), s.ProNumber.Eq("O'Neil")),
		fleetdb.ShipmentsSchema(), "n",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`n."status"`, `'Delivered'`, `n."weight" > 5`, `n."pro_number"`, `'O''Neil'`} {
		if !strings.Contains(sql, want) {
			t.Errorf("InlineSQL = %s, want it to contain %s", sql, want)
		}
	}
	if strings.Contains(sql, "$") {
		t.Errorf("InlineSQL = %s, left a placeholder", sql)
	}
	if len(cols) != 3 {
		t.Errorf("columns read = %v, want status, weight and pro_number", cols)
	}
}

func TestInlineSQLWritesEveryValueOfASetIn(t *testing.T) {
	s := fleetdb.Shipments
	sql, _, err := keel.InlineSQL(s.Status.In(fleet.StatusNew, fleet.StatusAssigned),
		fleetdb.ShipmentsSchema(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `'{"New","Assigned"}'`) && !strings.Contains(sql, `'New'`) {
		t.Errorf("InlineSQL = %s, want the statuses written in", sql)
	}
	if strings.Contains(sql, "$") {
		t.Errorf("InlineSQL = %s, left a placeholder", sql)
	}
}

func TestInlineSQLLeavesQuotedTextAlone(t *testing.T) {
	s := fleetdb.Shipments
	sql, _, err := keel.InlineSQL(keel.Raw("? || '$1 stays'", s.ProNumber), fleetdb.ShipmentsSchema(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `'$1 stays'`) {
		t.Errorf("InlineSQL = %s, rewrote text inside a quoted literal", sql)
	}
}

func TestInlineSQLRefusesAColumnOfAnotherTable(t *testing.T) {
	_, _, err := keel.InlineSQL(fleetdb.Customers.Name.Eq("x"), fleetdb.ShipmentsSchema(), "s")
	if err == nil {
		t.Fatal("InlineSQL accepted a column of another table")
	}
}
