package schema_test

import (
	"testing"

	"github.com/emoss08/keel/schema"
)

func TestCatalogFindsTablesByNameAndUnambiguousModel(t *testing.T) {
	a := &schema.Table{Name: "customers", Model: "Customer"}
	b := &schema.Table{Name: "billing_customers", Model: "Customer"}
	c := &schema.Table{Name: "tags", Model: "Tag"}
	cat := schema.NewCatalog([]*schema.Table{a, b, c})
	if got, ok := cat.Table("customers"); !ok || got != a {
		t.Fatalf("Table(customers) = %v, %v", got, ok)
	}
	if got, ok := cat.Model("Tag"); !ok || got != c {
		t.Fatalf("Model(Tag) = %v, %v", got, ok)
	}
	if _, ok := cat.Model("Customer"); ok {
		t.Fatal("an ambiguous model resolved")
	}
	if _, ok := cat.Table("nope"); ok {
		t.Fatal("an unknown table resolved")
	}
	if len(cat.Tables()) != 3 {
		t.Fatalf("Tables() = %d", len(cat.Tables()))
	}
}
