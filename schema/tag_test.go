package schema

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseFieldTag(t *testing.T) {
	cases := []struct {
		tag  string
		ok   bool
		want FieldTag
	}{
		{tag: "", ok: false},
		{tag: "-", ok: false},
		{tag: "id,pk,autoid:shp_", ok: true, want: FieldTag{Name: "id", Flags: FlagPK | FlagAutoID, IDPrefix: "shp_"}},
		{tag: "organization_id,pk,tenant", ok: true, want: FieldTag{Name: "organization_id", Flags: FlagPK | FlagTenant}},
		{tag: "search_vector,noselect,readonly,tsvector", ok: true,
			want: FieldTag{Name: "search_vector", Flags: FlagNoSelect | FlagReadOnly, TSVector: true}},
		{tag: "v, version ,autocreate,autoupdate", ok: true,
			want: FieldTag{Name: "v", Flags: FlagVersion | FlagAutoCreate | FlagAutoUpdate}},
		{tag: "attrs,jsonb", ok: true, want: FieldTag{Name: "attrs", Flags: FlagJSONB}},
		{tag: "args,jsonb,jsonnumber", ok: true, want: FieldTag{Name: "args", Flags: FlagJSONB | FlagJSONNumber}},
		{tag: "phone,nullzero", ok: true, want: FieldTag{Name: "phone", Flags: FlagNullZero}},
		{tag: "rank,computed", ok: true, want: FieldTag{Name: "rank", Flags: FlagComputed}},
		{tag: "sv,tsvector=english", ok: true, want: FieldTag{Name: "sv", TSVector: true, TSConfig: "english"}},
		{tag: "sv,tsvector=public.my_cfg", ok: true, want: FieldTag{Name: "sv", TSVector: true, TSConfig: "public.my_cfg"}},
		{tag: "geom,readonly,nullzero", ok: true, want: FieldTag{Name: "geom", Flags: FlagReadOnly | FlagNullZero}},
		{tag: "pro_number,label=PRO Number", ok: true, want: FieldTag{Name: "pro_number", Label: "PRO Number"}},
		{tag: "x,noreport", ok: true, want: FieldTag{Name: "x", Report: ReportTag{Exclude: true}}},
		{tag: "x,report,dimension", ok: true, want: FieldTag{Name: "x", Report: ReportTag{Include: true, Kind: Dimension}}},
		{tag: "x,measure", ok: true, want: FieldTag{Name: "x", Report: ReportTag{Kind: Measure}}},
		{tag: "x,format=weight", ok: true, want: FieldTag{Name: "x", Report: ReportTag{Format: "weight"}}},
		{tag: "x,format=date:short", ok: true, want: FieldTag{Name: "x", Report: ReportTag{Format: "date:short"}}},
		{tag: "x,type=epoch", ok: true, want: FieldTag{Name: "x", Report: ReportTag{Type: ValueEpoch}}},
		{tag: "x,type=date", ok: true, want: FieldTag{Name: "x", Report: ReportTag{Type: ValueDate}}},
		{tag: "x,type=decimal", ok: true, want: FieldTag{Name: "x", Report: ReportTag{Type: ValueDecimal}}},
		{tag: "x,agg=sum|avg| count_distinct", ok: true,
			want: FieldTag{Name: "x", Report: ReportTag{Aggs: AggSum | AggAvg | AggCountDistinct}}},
		{tag: "belongs:customer_id", ok: true, want: FieldTag{Rel: BelongsTo, RelKeys: []RelKey{{FK: "customer_id"}}}},
		{tag: "belongs:source_pto_id|worker_id", ok: true, want: FieldTag{Rel: BelongsTo,
			RelKeys: []RelKey{{FK: "source_pto_id"}, {FK: "worker_id"}}}},
		{tag: "many:shipment_id|driver_id=worker_id", ok: true, want: FieldTag{Rel: HasMany,
			RelKeys: []RelKey{{FK: "shipment_id"}, {FK: "driver_id", Key: "worker_id"}}}},
		{tag: "belongs:bill_to_id,label=Bill To,noreport", ok: true,
			want: FieldTag{Rel: BelongsTo, RelKeys: []RelKey{{FK: "bill_to_id"}}, Label: "Bill To", NoReport: true}},
		{tag: "one:worker_id", ok: true, want: FieldTag{Rel: HasOne, RelKeys: []RelKey{{FK: "worker_id"}}}},
		{tag: "many:shipment_id", ok: true, want: FieldTag{Rel: HasMany, RelKeys: []RelKey{{FK: "shipment_id"}}}},
		{tag: "m2m:shipment_tags.shipment_id.tag_id", ok: true,
			want: FieldTag{Rel: ManyToMany, JoinTable: "shipment_tags", JoinLocal: "shipment_id", JoinRemote: "tag_id"}},
		{tag: "pick:moves.stops,first=moves.sequence|stops.sequence", ok: true, want: FieldTag{Rel: Pick,
			PickPath:  []string{"moves", "stops"},
			PickOrder: []PickRef{{"moves", "sequence"}, {"stops", "sequence"}}}},
		{tag: "pick:stops,last=stops.sequence,noreport,label=Last", ok: true, want: FieldTag{Rel: Pick,
			PickPath: []string{"stops"}, PickOrder: []PickRef{{"stops", "sequence"}}, PickLast: true, NoReport: true,
			Label: "Last"}},
		{tag: "pick:moves.stops,first=moves.sequence|stops.sequence,where=stops.type:Pickup;SplitPickup|moves.status:Open",
			ok: true, want: FieldTag{Rel: Pick,
				PickPath:  []string{"moves", "stops"},
				PickOrder: []PickRef{{"moves", "sequence"}, {"stops", "sequence"}},
				PickWhere: []PickCond{
					{Segment: "stops", Column: "type", Values: []string{"Pickup", "SplitPickup"}},
					{Segment: "moves", Column: "status", Values: []string{"Open"}},
				}}},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			ft, ok, err := ParseFieldTag(c.tag)
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.ok {
				t.Fatalf("ok = %v", ok)
			}
			if !reflect.DeepEqual(ft, c.want) {
				t.Fatalf("got %+v\nwant %+v", ft, c.want)
			}
		})
	}
}

func TestParseFieldTagErrors(t *testing.T) {
	for tag, want := range map[string]string{
		"Name":                            `identifier "Name" must match`,
		"1abc":                            "must not start with a digit",
		"a-b":                             `identifier "a-b" must match`,
		"name,unknown":                    `unknown option "unknown"`,
		"name,":                           "empty option",
		"name,pk,pk":                      "option pk is repeated",
		"name,pk=yes":                     "option pk takes no value",
		"name,tsvector=1":                 "tsvector=1: identifier \"1\" must not start with a digit",
		"id,autoid:":                      "option autoid needs a value (autoid:…)",
		"id,autoid=x":                     "write autoid:<prefix>",
		"id,autoid:a/b":                   `autoid prefix "a/b" must match`,
		"x,readonly,pk":                   "readonly excludes write options",
		"x,nullzero,version":              "nullzero excludes pk, tenant, version",
		"x,default,pk":                    "default excludes pk, tenant, version",
		"x,computed,noselect":             "computed excludes pk, tenant, version, autoid, autocreate, autoupdate, readonly and noselect",
		"x,tsvector=English":              `tsvector=English: identifier "English" must match`,
		"x,tsvector=":                     "option tsvector needs a value",
		"belongs:a|a":                     "relation key a is repeated",
		"belongs:a|":                      "empty identifier",
		"many:a=B":                        `identifier "B" must match`,
		"x,nullzero,autoid:x_":            "nullzero excludes pk, tenant, version, autoid",
		"x,jsonb,tsvector":                "jsonb and tsvector exclude each other",
		"x,jsonnumber":                    "jsonnumber needs the jsonb option",
		"x,jsonb,jsonnumber=1":            "option jsonnumber takes no value",
		"x,label":                         "option label needs a value (label=…)",
		"x,label= ":                       "option label needs a value",
		"x,report,noreport":               "report and noreport exclude each other",
		"x,report=Ops":                    "option report takes no value",
		"x,dimension,measure":             "dimension and measure exclude each other",
		"x,measure=1":                     "option measure takes no value",
		"x,format":                        "option format needs a value",
		"x,format=a b":                    `format "a b" must match`,
		"x,type=money":                    "type=money: want epoch, date or decimal",
		"x,type":                          "option type needs a value",
		"x,agg=":                          "option agg needs a value",
		"x,agg=sum|median":                `unknown aggregation "median"`,
		"x,agg=sum|sum":                   "agg=sum|sum: sum is repeated",
		"x,first=a.b":                     `unknown option "first"`,
		"belongs:customer_id,pk":          `relation option "pk"`,
		"belongs:Customer":                `identifier "Customer" must match`,
		"belongs:c,first=c.id":            "option first is only for pick relations",
		"belongs:c,label=":                "option label needs a value",
		"belongs:c,noreport=1":            "option noreport takes no value",
		"m2m:shipment_tags.shipment_id":   "m2m needs m2m:<join_table>.<local_fk>.<remote_fk>",
		"m2m:a.B.c":                       `identifier "B" must match`,
		"pick:moves.stops":                "pick relations need first= or last=",
		"pick:,first=x.y":                 "pick path: empty identifier",
		"pick:moves.Stops,first=moves.id": `pick path: identifier "Stops" must match`,
		"pick:moves,first=moves.id,last=moves.id":     "first= and last= exclude each other",
		"pick:moves,first=":                           "option first needs a value",
		"pick:moves,first=moves":                      `first=moves: "moves" must be <segment>.<column>`,
		"pick:moves,first=stops.id":                   "first=stops.id: stops is not a segment of the path moves",
		"pick:moves,first=moves.ID":                   `first=moves.ID: identifier "ID" must match`,
		"pick:moves,first=moves.id,pk":                `relation option "pk"`,
		"belongs:move_id,where=moves.id:x":            "option where is only for pick relations",
		"pick:moves,first=moves.id,where=moves.id":    `where=moves.id: "moves.id" must be <segment>.<column>:<value>`,
		"pick:moves,first=moves.id,where=moves:x":     `where=moves:x: "moves" must be <segment>.<column>`,
		"pick:moves,first=moves.id,where=stops.id:x":  "where=stops.id:x: stops is not a segment of the path moves",
		"pick:moves,first=moves.id,where=moves.id:a;": "where=moves.id:a;: moves.id has an empty value",
	} {
		t.Run(tag, func(t *testing.T) {
			_, _, err := ParseFieldTag(tag)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want an error containing %q", err, want)
			}
			if !strings.HasPrefix(err.Error(), `keel tag "`+tag+`": `) {
				t.Fatalf("error %q does not name the tag", err)
			}
		})
	}
}

func TestParseTableTag(t *testing.T) {
	for tag, want := range map[string]TableTag{
		"shipments":                         {Name: "shipments"},
		"shipments,report":                  {Name: "shipments", Report: true},
		"shipments,report=Operations":       {Name: "shipments", Report: true, Category: "Operations"},
		"people,label=Person,plural=People": {Name: "people", Label: "Person", Plural: "People"},
	} {
		got, err := ParseTableTag(tag)
		if err != nil || got != want {
			t.Errorf("ParseTableTag(%q) = %+v, %v; want %+v", tag, got, err, want)
		}
	}
	for tag, want := range map[string]string{
		"":                        "table name: empty identifier",
		"Shipments":               `identifier "Shipments" must match`,
		"shipments,alias:sp":      `unknown table option "alias:sp"`,
		"shipments,report=":       "option report needs a value",
		"shipments,label=":        "option label needs a value",
		"shipments,plural":        "option plural needs a value",
		"shipments,report,report": "option report is repeated",
		"shipments,pk":            `unknown table option "pk"`,
	} {
		if _, err := ParseTableTag(tag); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseTableTag(%q) = %v, want an error containing %q", tag, err, want)
		}
	}
}

func TestAggs(t *testing.T) {
	if got := strings.Join(AggsNumeric.Names(), ","); got != "count,count_distinct,sum,avg,min,max" {
		t.Fatal(got)
	}
	if !AggsNumeric.Has(AggsOrdered) || AggsOrdered.Has(AggSum) || !AggsOrdered.Has(AggsCount) {
		t.Fatal("aggregation sets do not nest")
	}
	for _, n := range AggsNumeric.Names() {
		if a, ok := ParseAgg(n); !ok || !AggsNumeric.Has(a) {
			t.Fatalf("ParseAgg(%s) = %v, %v", n, a, ok)
		}
	}
	if _, ok := ParseAgg("median"); ok {
		t.Fatal("median parsed")
	}
}

func TestValueTypes(t *testing.T) {
	for _, v := range []ValueType{ValueInt, ValueFloat, ValueDecimal} {
		if !v.Numeric() || !v.Ordered() {
			t.Errorf("%d is not numeric and ordered", v)
		}
	}
	for _, v := range []ValueType{ValueString, ValueTime, ValueDate, ValueEpoch} {
		if v.Numeric() || !v.Ordered() {
			t.Errorf("%d is numeric or not ordered", v)
		}
	}
	for _, v := range []ValueType{ValueEnum, ValueBool, ValueJSON, ValueArray, ValueBytes, ValueTSVector, ValueOther} {
		if v.Numeric() || v.Ordered() {
			t.Errorf("%d is numeric or ordered", v)
		}
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
		{Name: "rank", Flags: FlagComputed},
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
	if tb.Columns[8].Stored() || !tb.Columns[2].Stored() {
		t.Fatal("Stored misreports a computed field")
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
