package gen

import (
	"strings"
	"testing"
)

func TestLabels(t *testing.T) {
	def, err := newLabeler(nil)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := newLabeler([]string{"pro", "BOL", "SCAC", "DOT", "MC", "ID", "URL"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		l          *labeler
		name, want string
	}{
		{def, "ProNumber", "Pro Number"},
		{custom, "ProNumber", "PRO Number"},
		{def, "CustomerID", "Customer ID"},
		{def, "CustomerId", "Customer ID"},
		{def, "CustomerIDs", "Customer IDs"},
		{def, "ImageURL", "Image URL"},
		{def, "APIKey", "API Key"},
		{def, "RawJSON", "Raw JSON"},
		{def, "HTTPServer", "HTTP Server"},
		{custom, "BOLNumber", "BOL Number"},
		{custom, "ScacCode", "SCAC Code"},
		{custom, "DOTNumber", "DOT Number"},
		{custom, "McNumber", "MC Number"},
		{def, "Address2", "Address 2"},
		{def, "Line1Text", "Line 1 Text"},
		{def, "Phase10", "Phase 10"},
		{def, "first_name", "First Name"},
		{def, "ID", "ID"},
		{def, "X", "X"},
		{def, "InTransit", "In Transit"},
	} {
		if got := c.l.label(c.name); got != c.want {
			t.Errorf("label(%s) = %q, want %q", c.name, got, c.want)
		}
	}
	for in, want := range map[string]string{
		"Shipment": "Shipments", "Bill Of Lading": "Bill Of Ladings", "Category": "Categories", "PO": "POs",
		"Address": "Addresses",
	} {
		if got := pluralLabel(in); got != want {
			t.Errorf("pluralLabel(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestLabelerRefusesBadAcronyms(t *testing.T) {
	for _, bad := range [][]string{{""}, {"B-L"}, {"A B"}} {
		if _, err := newLabeler(bad); err == nil || !strings.Contains(err.Error(), "must be letters and digits") {
			t.Errorf("newLabeler(%q) = %v", bad, err)
		}
	}
}
