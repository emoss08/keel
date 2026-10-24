package analytics_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/schema"
)

func TestCatalogDerivesEntitiesFieldsAndEdges(t *testing.T) {
	cat := catalog(t)
	keys := strings.Join(cat.EntityKeys(), ",")
	if keys != "shipment,customer,move,stop,location" {
		t.Fatalf("entities %s", keys)
	}
	if got := strings.Join(cat.FieldKeys("shipment"), ","); got !=
		"id,proNumber,status,priority,weight,total,hazmat,labels,stageRank,canceledAt,createdAt,updatedAt" {
		t.Fatalf("shipment fields %s", got)
	}
	if got := strings.Join(cat.EdgeNames("shipment"), ","); got != "customer,billTo,moves,originStop,destinationStop" {
		t.Fatalf("shipment edges %s", got)
	}
	ship, _ := cat.Entity("shipment")
	status, _ := ship.Field("status")
	if status.Type != formula.Enum || len(status.Enum) != 5 || status.Enum[2].Label != "In Transit" ||
		status.LabelKey != "shipment.status" || !status.Groupable {
		t.Fatalf("status %+v", status)
	}
	total, _ := ship.Field("total")
	if total.Type != formula.Decimal || total.Format != "money" || total.Groupable || len(total.Aggs) != 6 {
		t.Fatalf("total %+v", total)
	}
	created, _ := ship.Field("createdAt")
	if created.Type != formula.Epoch {
		t.Fatalf("createdAt %s", created.Type)
	}
	info, ok := cat.Field("shipment", "labels")
	if !ok || info.Type != formula.JSON || info.Groupable {
		t.Fatalf("labels %+v", info)
	}
	moves, _ := cat.Edge("shipment", "moves")
	origin, _ := ship.Edge("originStop")
	if !moves.ToMany || moves.Target != "move" || origin.ToMany || !origin.Pick || origin.Target.Key != "stop" {
		t.Fatalf("edges %+v %+v", moves, origin)
	}
	via, f, ok := cat.Resolve("shipment", "originStop.location.city")
	if !ok || via.String() != "originStop.location" || f.Key != "city" {
		t.Fatalf("resolve %v %v", via, f)
	}
	if len(cat.Version()) != 32 || cat.Version() != catalog(t).Version() {
		t.Fatal("the version is not a stable hash")
	}
}

// shipmentsWithCustomers returns a copy of the shipments table whose only reported edges lead to
// customer, the table given, after edit changes the copy.
func shipmentsWithCustomers(cust *schema.Table, edit func(*schema.Table)) []*schema.Table {
	ship := *fleetdb.ShipmentsSchema()
	ship.Columns = append([]schema.Column(nil), ship.Columns...)
	ship.Relations = append([]schema.Relation(nil), ship.Relations...)
	for i := range ship.Relations {
		r := &ship.Relations[i]
		if r.Name == "Customer" {
			r.Target = cust
			continue
		}
		r.NoReport = true
	}
	if edit != nil {
		edit(&ship)
	}
	return []*schema.Table{&ship, cust}
}

func TestCatalogLeavesOutComputedFields(t *testing.T) {
	cust := fleetdb.CustomersSchema()
	cat := must[*analytics.Catalog](t)(analytics.Build(shipmentsWithCustomers(cust, func(s *schema.Table) {
		info := *s.Columns[5].Report
		info.Included = true
		s.Columns = append(s.Columns, schema.Column{Name: "rank", Field: "Rank", Kind: schema.KindFloat64,
			Flags: schema.FlagComputed, Report: &info})
	}), analytics.Options{}))
	if _, ok := cat.Field("shipment", "rank"); ok {
		t.Fatal("a computed field is a report field")
	}
}

func TestCatalogVersionTracksWhatDefinitionsDependOn(t *testing.T) {
	cust := fleetdb.CustomersSchema()
	base := must[*analytics.Catalog](t)(analytics.Build(shipmentsWithCustomers(cust, nil), analytics.Options{}))
	relabel := must[*analytics.Catalog](t)(analytics.Build(shipmentsWithCustomers(cust, func(s *schema.Table) {
		s.Columns[5].Label = "Pro"
	}), analytics.Options{}))
	if base.Version() != relabel.Version() {
		t.Fatal("a label changed the version")
	}
	dropped := must[*analytics.Catalog](t)(analytics.Build(shipmentsWithCustomers(cust, func(s *schema.Table) {
		info := *s.Columns[5].Report
		info.Included = false
		s.Columns[5].Report = &info
	}), analytics.Options{}))
	if base.Version() == dropped.Version() {
		t.Fatal("removing a field kept the version")
	}
}

func TestCatalogChecksTheGraph(t *testing.T) {
	tables := shipmentsWithCustomers(fleetdb.CustomersSchema(), func(s *schema.Table) {
		s.Relations[4].NoReport = false
	})
	_, err := analytics.Build(append(tables, fleetdb.TagsSchema()), analytics.Options{})
	if analytics.ErrorCode(err) != analytics.CodeCatalog || !strings.Contains(err.Error(), "not reportable") {
		t.Fatalf("edge to a non-reportable model: %v", err)
	}
	pick := *fleetdb.ShipmentsSchema()
	pick.Relations = append([]schema.Relation(nil), pick.Relations...)
	pick.Relations[5].Order = pick.Relations[5].Order[:2]
	_, err = analytics.Build([]*schema.Table{&pick, fleetdb.CustomersSchema(), fleetdb.MovesSchema(),
		fleetdb.StopsSchema(), fleetdb.LocationsSchema()}, analytics.Options{})
	if err == nil || !strings.Contains(err.Error(), "primary key") {
		t.Fatalf("pick without a total order: %v", err)
	}
	cust := *fleetdb.CustomersSchema()
	cust.Columns = append([]schema.Column(nil), cust.Columns...)
	cust.Columns[2].Flags &^= schema.FlagTenant
	cust.Tenant = cust.Tenant[:1]
	_, err = analytics.Build(shipmentsWithCustomers(&cust, nil), analytics.Options{})
	if err == nil || !strings.Contains(err.Error(), "tenant columns") {
		t.Fatalf("joined tables with different tenant columns: %v", err)
	}
}

// fleetPolicy denies hazmat to everyone, weight through the billTo edge, and starting from
// stops; it marks totals confidential, counts every question and hides customer G's rows when
// rows is set.
type fleetPolicy struct {
	asked map[string]int
	rows  bool
}

func (p *fleetPolicy) Entity(_ context.Context, e *analytics.Entity) analytics.Access {
	p.asked["entity "+e.Key]++
	if e.Key == "stop" {
		return analytics.Denied("needs_dispatch")
	}
	return analytics.Allowed()
}

func (p *fleetPolicy) Field(_ context.Context, f *analytics.Field, via analytics.Path) analytics.Access {
	p.asked["field "+f.LabelKey+"@"+via.String()]++
	switch {
	case f.Key == "hazmat":
		return analytics.Denied("needs_safety")
	case f.Key == "total":
		return analytics.Allowed().WithSensitivity(table.Confidential)
	case f.Key == "name" && via.String() == "billTo":
		return analytics.Denied("needs_billing")
	}
	return analytics.Allowed()
}

func (p *fleetPolicy) Edge(_ context.Context, e *analytics.Edge, via analytics.Path) analytics.Access {
	p.asked["edge "+e.LabelKey+"@"+via.String()]++
	return analytics.Allowed()
}

func (p *fleetPolicy) Rows(_ context.Context, e *analytics.Entity) []keel.Pred {
	p.asked["rows "+e.Key]++
	if p.rows && e.Key == "customer" {
		return []keel.Pred{keel.DynFrom(e.Table).Path("code").Ne("G")}
	}
	if p.rows && e.Key == "shipment" {
		return []keel.Pred{fleetdb.Shipments.ProNumber.Ne("P01")}
	}
	return nil
}

func TestViewAppliesThePolicy(t *testing.T) {
	cat := catalog(t)
	p := &fleetPolicy{asked: map[string]int{}}
	v := cat.For(context.Background(), p)
	stop, _ := v.Entity("stop")
	if stop.Access.Allowed || stop.Access.Reason != "needs_dispatch" {
		t.Fatalf("stop %+v", stop.Access)
	}
	ship, _ := v.Entity("shipment")
	for _, f := range ship.Fields {
		if f.Key == "hazmat" && (f.Access.Allowed || f.Access.Reason != "needs_safety") {
			t.Fatalf("hazmat %+v", f.Access)
		}
		if f.Key == "total" && f.Access.Sensitivity != table.Confidential {
			t.Fatalf("total %+v", f.Access)
		}
		if f.Key == "status" && len(f.Operators) == 0 {
			t.Fatal("no operators")
		}
	}
	name, ok := v.Field("shipment", "billTo.name")
	if !ok || name.Access.Allowed {
		t.Fatalf("billTo.name %+v", name)
	}
	if name, _ := v.Field("shipment", "customer.name"); !name.Access.Allowed {
		t.Fatal("customer.name denied")
	}
	for k, n := range p.asked {
		if n != 1 {
			t.Fatalf("%s asked %d times", k, n)
		}
	}
	desc := v.Describe()
	for _, wantLine := range []string{"entity shipment: Shipment [Operations]", "  status enum{New|Assigned|InTransit|Delivered|Canceled} agg=count,count_distinct: Status",
		"  edge moves -> move (many): Moves", "  total decimal measure agg=count,count_distinct,sum,avg,min,max nogroup: Total"} {
		if !strings.Contains(desc, wantLine) {
			t.Fatalf("Describe lacks %q:\n%s", wantLine, desc)
		}
	}
	if strings.Contains(desc, "hazmat") || strings.Contains(desc, "entity stop") {
		t.Fatalf("Describe shows denied things:\n%s", desc)
	}
}

func TestViewTranslates(t *testing.T) {
	cat, err := analytics.Build(fleetTables(), analytics.Options{Translate: func(_ context.Context, key, fallback string) string {
		if key == "shipment.proNumber" {
			return "Numéro PRO"
		}
		return fallback
	}})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := cat.For(context.Background(), nil).Field("shipment", "proNumber")
	if f.Label != "Numéro PRO" {
		t.Fatalf("label %q", f.Label)
	}
}

func TestRulesArePublishedAsData(t *testing.T) {
	rules := catalog(t).Rules()
	b, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"operators":{`, `"enum":["eq","ne","in","not_in","is_null","is_not_null"]`,
		`"buckets":["hour","day","week","month","quarter","year"]`, `"display":{"types":`, `"pivotOther":"__other__"`,
		`"name":"date_trunc"`} {
		if !strings.Contains(string(b), s) {
			t.Fatalf("rules lack %s", s)
		}
	}
	if len(rules.Transforms) != len(ir.TransformOps()) || len(rules.Windows) != len(ir.WindowFns()) {
		t.Fatal("rules lack transforms or windows")
	}
}

func TestCheckReportsDrift(t *testing.T) {
	cat := catalog(t)
	d := def(t, `{"entity": "shipment", "columns": [{"id": "n", "dimension": {"field": "customer.nam"}}]}`)
	diags := cat.Check(context.Background(), d, nil)
	if len(diags) != 1 || diags[0].Code != "unknown_field" || diags[0].Suggestions[0] != "customer.name" {
		t.Fatalf("diagnostics %+v", diags)
	}
	d = def(t, `{"entity": "shipment", "columns": [{"id": "h", "dimension": {"field": "hazmat"}},
		{"id": "b", "dimension": {"field": "billTo.name"}}]}`)
	diags = cat.Check(context.Background(), d, &fleetPolicy{asked: map[string]int{}})
	if len(diags) != 2 || diags[0].Reason != "needs_billing" || diags[0].Pointer != "/columns/1/dimension/field" ||
		diags[1].Reason != "needs_safety" {
		t.Fatalf("diagnostics %+v", diags)
	}
	if len(cat.Check(context.Background(), def(t, `{"entity": "shipment", "columns": [{"id": "s", "dimension": {"field": "status"}}]}`), nil)) != 0 {
		t.Fatal("a valid definition has diagnostics")
	}
}
