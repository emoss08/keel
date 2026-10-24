package analytics_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
	"github.com/emoss08/keel/schema"
)

// taggedTables returns copies of the shipments and tags tables in which the shipment's tags are a
// reportable many-to-many edge and its customer the only other one.
func taggedTables() []*schema.Table {
	tag := *fleetdb.TagsSchema()
	tag.Report = &schema.TableReport{Category: "Operations"}
	tag.Columns = append([]schema.Column(nil), tag.Columns...)
	for i := range tag.Columns {
		info := schema.ReportInfo{Included: true, Kind: schema.Dimension, Type: schema.ValueString,
			Aggs: schema.AggsOrdered}
		tag.Columns[i].Report = &info
	}
	tables := shipmentsWithCustomers(fleetdb.CustomersSchema(), func(s *schema.Table) {
		r := &s.Relations[s.Relation("Tags")]
		r.NoReport = false
		r.Target = &tag
	})
	return append(tables, &tag)
}

// TestFanoutJoinTableStaysInTenant counts distinct tags across a many-to-many edge in two tenants
// whose rows share IDs. Tenant b links its shipment to one of its two tags; tenant a links the
// shipment with the same ID to both tag IDs. A join table read across tenants and joined on its
// keys alone would give b's shipment the tag a linked and b did not.
func TestFanoutJoinTableStaysInTenant(t *testing.T) {
	d := db(t)
	raw := make([]byte, 5)
	_, _ = rand.Read(raw)
	run := hex.EncodeToString(raw)
	id := func(s string) ids.ID { return ids.ID(s + "_" + run) }
	a, b := newTenant(), newTenant()
	for _, side := range []struct {
		ctx    context.Context
		name   string
		linked []string
	}{{a, "A", []string{"tag1", "tag2"}}, {b, "B", []string{"tag1"}}} {
		ctx := side.ctx
		ok(t, fleetdb.Customers.Create(ctx, d, &fleet.Customer{ID: id("cus"), Name: "Customer " + side.name,
			Code: "C" + side.name}))
		ok(t, fleetdb.Shipments.Create(ctx, d, &fleet.Shipment{ID: id("shp"), CustomerID: id("cus"),
			ProNumber: "PRO-" + side.name, Status: fleet.StatusNew}))
		ok(t, fleetdb.Tags.CreateMany(ctx, d, []*fleet.Tag{{ID: id("tag1"), Name: "First " + side.name},
			{ID: id("tag2"), Name: "Second " + side.name}}))
		tn := tenantOf(ctx)
		for _, tag := range side.linked {
			_, err := keel.Exec(ctx, d, `INSERT INTO shipment_tags (shipment_id, tag_id, organization_id,
				business_unit_id) VALUES ($1, $2, $3, $4)`, id("shp"), id(tag), tn.org, tn.bu)
			ok(t, err)
		}
	}
	cat := must[*analytics.Catalog](t)(analytics.Build(taggedTables(), analytics.Options{}))
	r := must[*analytics.Runner](t)(analytics.NewRunner(d, cat, analytics.RunnerOptions{PreviewRows: 100}))
	report := def(t, `{"entity": "shipment", "columns": [
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "tags", "measure": {"field": "tags.name", "agg": "count_distinct"}},
		{"id": "links", "measure": {"field": "tags.id", "agg": "count"}}],
		"filters": {"field": "id", "op": "eq", "value": "`+string(id("shp"))+`"},
		"sort": [{"column": "customer"}]}`)
	for _, c := range []struct {
		ctx  context.Context
		want string
	}{{a, "Customer A | 2 | 2"}, {b, "Customer B | 1 | 1"}} {
		res, err := r.Preview(c.ctx, analytics.Request{Definition: report})
		ok(t, err)
		want(t, rows(t, res), c.want)
	}
	comp := must[*analytics.Compiled](t)(analytics.Compile(b, cat, nil, report, nil, analytics.CompileOptions{}))
	sql, _, err := comp.ToSQL(b, d)
	ok(t, err)
	for _, frag := range []*regexp.Regexp{
		regexp.MustCompile(`FROM "shipment_tags" (st\d*) WHERE st\d*\."organization_id" = \$\d+ AND ` +
			`st\d*\."business_unit_id" = \$\d+\)`),
		regexp.MustCompile(`JOIN "keel_cte_\d+" kc\d+ ON \(kc\d+\."c1" = s\d*\."id"\) AND ` +
			`\(kc\d+\."c3" = s\d*\."organization_id"\) AND \(kc\d+\."c4" = s\d*\."business_unit_id"\)`),
	} {
		if !frag.MatchString(sql) {
			t.Fatalf("the join table is not read in the tenant (%s):\n%s", frag, sql)
		}
	}
}
