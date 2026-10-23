package schema

import "testing"

func TestParseFieldTag(t *testing.T) {
	cases := []struct {
		tag   string
		ok    bool
		err   bool
		check func(t *testing.T, ft FieldTag)
	}{
		{tag: "", ok: false},
		{tag: "-", ok: false},
		{tag: "id,pk,autoid:shp_", ok: true, check: func(t *testing.T, ft FieldTag) {
			if ft.Name != "id" || !ft.Flags.Has(FlagPK|FlagAutoID) || ft.IDPrefix != "shp_" {
				t.Fatalf("%+v", ft)
			}
		}},
		{tag: "organization_id,pk,tenant", ok: true, check: func(t *testing.T, ft FieldTag) {
			if !ft.Flags.Has(FlagPK | FlagTenant) {
				t.Fatalf("%+v", ft)
			}
		}},
		{tag: "search_vector,noselect,readonly,tsvector", ok: true, check: func(t *testing.T, ft FieldTag) {
			if !ft.Flags.Has(FlagNoSelect|FlagReadOnly) || !ft.TSVector {
				t.Fatalf("%+v", ft)
			}
		}},
		{tag: "belongs:customer_id", ok: true, check: func(t *testing.T, ft FieldTag) {
			if ft.Rel != BelongsTo || ft.RelKey != "customer_id" {
				t.Fatalf("%+v", ft)
			}
		}},
		{tag: "many:shipment_id", ok: true, check: func(t *testing.T, ft FieldTag) {
			if ft.Rel != HasMany || !ft.Rel.ToMany() {
				t.Fatalf("%+v", ft)
			}
		}},
		{tag: "m2m:shipment_tags.shipment_id.tag_id", ok: true, check: func(t *testing.T, ft FieldTag) {
			if ft.JoinTable != "shipment_tags" || ft.JoinLocal != "shipment_id" || ft.JoinRemote != "tag_id" {
				t.Fatalf("%+v", ft)
			}
		}},
		{tag: "m2m:shipment_tags.shipment_id", err: true},
		{tag: "belongs:customer_id,pk", err: true},
		{tag: "Name", err: true},
		{tag: "name,unknown", err: true},
		{tag: "id,autoid:", err: true},
		{tag: "x,readonly,pk", err: true},
		{tag: "1abc", err: true},
		{tag: "a-b", err: true},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			ft, ok, err := ParseFieldTag(c.tag)
			if c.err {
				if err == nil {
					t.Fatalf("no error for %q", c.tag)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.ok {
				t.Fatalf("ok = %v", ok)
			}
			if c.check != nil {
				c.check(t, ft)
			}
		})
	}
}

func TestParseTableTag(t *testing.T) {
	tt, err := ParseTableTag("shipments")
	if err != nil || tt.Name != "shipments" {
		t.Fatal(tt, err)
	}
	if _, err := ParseTableTag("shipments,alias:sp"); err == nil {
		t.Fatal("unknown option accepted")
	}
	if _, err := ParseTableTag(""); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestDerive(t *testing.T) {
	tb := &Table{Name: "t", Columns: []Column{
		{Name: "id", Flags: FlagPK | FlagAutoID},
		{Name: "org", Flags: FlagPK | FlagTenant},
		{Name: "name"},
		{Name: "gen", Flags: FlagReadOnly},
		{Name: "tsv", Flags: FlagReadOnly | FlagNoSelect},
		{Name: "version", Flags: FlagVersion},
		{Name: "created", Flags: FlagAutoCreate},
		{Name: "updated", Flags: FlagAutoUpdate},
	}}
	Derive(tb)
	eqInts(t, tb.PK, 0, 1)
	eqInts(t, tb.Tenant, 1)
	eqInts(t, tb.Select, 0, 1, 2, 3, 5, 6, 7)
	eqInts(t, tb.Insert, 0, 1, 2, 5, 6, 7)
	eqInts(t, tb.Update, 2, 7)
	eqInts(t, tb.Returning, 3)
	if tb.Version != 5 || tb.AutoID != 0 || tb.AutoCreate != 6 || tb.AutoUpdate != 7 {
		t.Fatalf("%+v", tb)
	}
	if tb.Quoted != `"t"` || tb.Columns[2].Quoted != `"name"` {
		t.Fatal("not quoted")
	}
}

func eqInts(t *testing.T, got []int32, want ...int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestQuote(t *testing.T) {
	if Quote(`a"b`) != `"a""b"` {
		t.Fatal(Quote(`a"b`))
	}
}

func TestAliasFor(t *testing.T) {
	for in, want := range map[string]string{
		"shipments": "s", "shipment_moves": "sm", "business_units": "bu", "do_items": "di",
		"delivery_orders": "do_", "a2b_links": "al", "": "t",
	} {
		if got := AliasFor(in); got != want {
			t.Errorf("AliasFor(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"BillTo": "bill_to", "HTTPServer": "http_server", "Moves": "moves", "ID": "id"} {
		if got := Snake(in); got != want {
			t.Errorf("Snake(%q) = %q, want %q", in, got, want)
		}
	}
}
