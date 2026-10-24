package schema

import (
	"reflect"
	"testing"
)

// tenantTable returns a table with an id key, the given tenant columns and the given relations.
func tenantTable(name string, tenant []string, rels ...Relation) *Table {
	t := &Table{Name: name, Columns: []Column{{Name: "id", Kind: KindString, Flags: FlagPK}}, Relations: rels}
	for _, c := range tenant {
		t.Columns = append(t.Columns, Column{Name: c, Kind: KindString, Flags: FlagPK | FlagTenant})
	}
	Derive(t)
	return t
}

func TestSharedTenantAndJoinTableOf(t *testing.T) {
	both := []string{"organization_id", "business_unit_id"}
	tag := tenantTable("tags", both)
	org := tenantTable("orgs", []string{"organization_id"})
	global := tenantTable("plants", nil)
	m2m := func(target *Table) Relation {
		return Relation{Name: "Tags", Kind: ManyToMany, Target: target, Local: []int32{0}, Remote: []int32{0},
			JoinTable: "shipment_tags", JoinLocal: []string{"shipment_id"}, JoinRemote: []string{"tag_id"}}
	}
	ship := tenantTable("shipments", both, m2m(tag), m2m(org), m2m(global))
	local, remote := SharedTenant(ship, tag)
	if !reflect.DeepEqual(local, []int32{1, 2}) || !reflect.DeepEqual(remote, []int32{1, 2}) {
		t.Fatalf("shared tenant %v %v", local, remote)
	}
	if local, remote = SharedTenant(global, tag); local != nil || remote != nil {
		t.Fatalf("a table without tenant columns shares %v %v", local, remote)
	}
	names := func(j *Table) (cols, tenant []string) {
		for _, c := range j.Columns {
			cols = append(cols, c.Quoted)
		}
		for _, ci := range j.Tenant {
			tenant = append(tenant, j.Columns[ci].Name)
		}
		return cols, tenant
	}
	for i, want := range [][]string{
		{`"shipment_id"`, `"tag_id"`, `"organization_id"`, `"business_unit_id"`},
		{`"shipment_id"`, `"tag_id"`, `"organization_id"`},
		{`"shipment_id"`, `"tag_id"`},
	} {
		j := ship.JoinTableOf(&ship.Relations[i])
		cols, tenant := names(j)
		if !reflect.DeepEqual(cols, want) || len(tenant) != len(want)-2 || j.Quoted != `"shipment_tags"` ||
			j.Alias != "st" {
			t.Fatalf("join table of %s: %v, tenant %v, %s %s", ship.Relations[i].Target.Name, cols, tenant,
				j.Quoted, j.Alias)
		}
	}
	if ship.JoinTableOf(&Relation{Kind: HasMany, Target: tag}) != nil {
		t.Fatal("a has-many relation has a join table")
	}
}
