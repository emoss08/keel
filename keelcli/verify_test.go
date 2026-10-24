package keelcli

import (
	"strings"
	"testing"

	"github.com/emoss08/keel/schema"
)

// sampleItems is the sample's items table as a model would describe it, its id read as idKind and
// its state with flags stateFlags.
func sampleItems(idKind schema.Kind, stateFlags schema.ColFlag) *schema.Table {
	t := &schema.Table{Name: "items", Model: "sample.Item", Columns: []schema.Column{
		{Name: "id", Field: "ID", Kind: idKind, Flags: schema.FlagPK},
		{Name: "tenant_id", Field: "TenantID", Kind: schema.KindString},
		{Name: "name", Field: "Name", Kind: schema.KindString},
		{Name: "state", Field: "State", Kind: schema.KindString, Flags: stateFlags},
	}}
	schema.Derive(t)
	return t
}

func TestVerifyCommand(t *testing.T) {
	s := newSample(t)
	s.run(t, "migrate", "up", "-plain").expect(t, ExitOK)
	help := s.run(t, "help")
	if strings.Contains(help.stdout, "verify") {
		t.Fatalf("verify listed without Tables:\n%s", help.stdout)
	}

	tables := []*schema.Table{sampleItems(schema.KindInt32, schema.FlagNullable)}
	s.app.Tables = func() []*schema.Table { return tables }
	s.run(t, "help").expect(t, ExitOK, "verify", "compare the models with the database's tables")
	s.run(t, "verify", "-plain").expect(t, ExitOK, "No drift: 1 table checked.")

	tables = []*schema.Table{sampleItems(schema.KindInt16, 0)}
	s.run(t, "verify", "-plain").expect(t, ExitProblems,
		"type items.id (Item.ID)\n  a int16 field cannot hold the column's type int4\n  fix: change the column to smallint",
		"nullable items.state (Item.State)\n  the column is nullable",
		"2 drifts in 1 table.",
		"keelcli: verify found 2 drifts")

	var doc struct {
		Tables int
		Drifts []struct{ Kind, Table, Column, Field, Detail, Fix string }
	}
	out := s.run(t, "verify", "-json")
	if out.code != ExitProblems {
		t.Fatalf("exit %d:\n%s%s", out.code, out.stdout, out.stderr)
	}
	jsonDoc(t, out.stdout, &doc)
	if doc.Tables != 1 || len(doc.Drifts) != 2 || doc.Drifts[0].Kind != "type" || doc.Drifts[1].Column != "state" ||
		doc.Drifts[1].Fix == "" {
		t.Fatalf("json %+v", doc)
	}
}
