package gen

import (
	"os"
	"strings"
	"testing"
)

// metaModels exercise every derivation rule and every metadata option.
const metaModels = `// Status is a shipment status.
type Status string

const (
	// StatusInTransit is a shipment on the road.
	StatusInTransit Status = "in_transit"
	StatusNew       Status = "new" // StatusNew is a fresh shipment.
	Other           Status = "other"
)

// Level is a priority level.
type Level int32

const (
	LevelLow  Level = 1
	LevelHigh Level = 2
)

// Amount is a decimal amount.
type Amount struct{ C int64 }

// Scan reads an amount.
func (a *Amount) Scan(src any) error { return nil }

// KeelFormat displays amounts as money.
func (Amount) KeelFormat() string { return "money" }

// Weight is a weight in pounds.
type Weight int64

// KeelFormat displays weights.
func (*Weight) KeelFormat() string { return "weight" }

// Shipment is freight moving.
//
// It has a second
// paragraph.
type Shipment struct {
	keel.Table ~keel:"shipments,report=Operations"~

	ID   string ~keel:"id,pk"~
	Org  string ~keel:"org_id,pk,tenant"~
	Unit string ~keel:"unit_id,pk,tenant,report"~
	// ProNumber is the carrier's
	// progressive number.
	ProNumber  string        ~keel:"pro_number"~
	CustomerID string        ~keel:"customer_id"~
	BillToID   *string       ~keel:"bill_to_id"~
	Status     Status        ~keel:"status"~
	Level      *Level        ~keel:"level"~
	Mode       kinds.Mode    ~keel:"mode"~
	Total      Amount        ~keel:"total,type=decimal"~
	W          Weight        ~keel:"w"~
	Miles      float64       ~keel:"miles"~
	Ratio      float32       ~keel:"ratio,agg=avg|max,format=percent"~
	Count      int64         ~keel:"count,dimension"~
	Rank       int32         ~keel:"rank,measure"~
	ShippedAt  int64         ~keel:"shipped_at"~
	DueDate    int64         ~keel:"due_date"~
	PickTime   int64         ~keel:"pick_time"~
	Stamp      int64         ~keel:"stamp,type=epoch"~
	Day        time.Time     ~keel:"day,type=date"~
	When       time.Time     ~keel:"when_ts"~
	Raw        []byte        ~keel:"raw"~
	RawShown   []byte        ~keel:"raw_shown,report"~
	Hidden     string        ~keel:"hidden,noreport"~
	Doc        string        ~keel:"doc,noselect,readonly,tsvector"~
	Meta       map[string]any ~keel:"meta,jsonb"~
	Flag       bool          ~keel:"flag"~
	URLPath    string        ~keel:"url_path,label=Link"~
	Version    int64         ~keel:"version,version"~

	Customer *Customer ~keel:"belongs:customer_id"~
	// BillTo is who pays.
	BillTo *Customer ~keel:"belongs:bill_to_id,noreport,label=Payer"~
	Moves  []*Move   ~keel:"many:shipment_id"~
}

// Customer buys.
type Customer struct {
	keel.Table ~keel:"customers,report,label=Client,plural=Clientele"~

	ID string ~keel:"id,pk"~
}

type Move struct {
	keel.Table ~keel:"moves"~

	ID         string ~keel:"id,pk"~
	ShipmentID string ~keel:"shipment_id"~
	Seq        int32  ~keel:"seq"~
}

type Counter struct {
	keel.Table ~keel:"counters,report"~

	ID    int64   ~keel:"id,pk"~
	Hits  int64   ~keel:"hits"~
	Ticks []*Tick ~keel:"many:counter_id"~
}

type Tick struct {
	keel.Table ~keel:"ticks,report"~

	ID        int64 ~keel:"id,pk"~
	CounterID int64 ~keel:"counter_id"~
}

type BOLLine struct {
	keel.Table ~keel:"bol_lines"~

	ID        string ~keel:"id,pk"~
	ProNumber string ~keel:"pro_number"~
	SCACCode  string ~keel:"scac_code"~
}
`

// kindsPackage is a package the models import but keel gen does not load as a model package, so
// its enum and format are read from export data and parsed source.
const kindsPackage = `package kinds

// Mode is how freight travels.
type Mode string

const (
	// ModeAir is by air.
	ModeAir Mode = "air"
	// ModeRoad is by road.
	ModeRoad Mode = "road"
)

// KeelFormat displays modes.
func (Mode) KeelFormat() string { return "mode" }
`

// metaSource generates metaModels with the given acronyms and returns the models' file.
func metaSource(t *testing.T, acronyms []string) string {
	t.Helper()
	dir := moduleFiles(t, metaModels, map[string]string{"kinds/kinds.go": kindsPackage})
	path := dir + "/models/models.go"
	withImport(t, path)
	files, err := Generate(Options{Dir: dir, Patterns: []string{"./models"}, Acronyms: acronyms})
	if err != nil {
		t.Fatal(err)
	}
	return string(files[0].Content)
}

// withImport adds the kinds package to the imports of the models file at path.
func withImport(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src = []byte(strings.Replace(string(src), "\t\"github.com/emoss08/keel\"\n",
		"\t\"example.com/app/kinds\"\n\t\"github.com/emoss08/keel\"\n", 1))
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
}

// line returns the line of src that declares the column, relation or table field with the given
// prefix inside the literal of table.
func line(t *testing.T, src, table, prefix string) string {
	t.Helper()
	start := strings.Index(src, "var "+table+" = schema.Table{")
	if start < 0 {
		t.Fatalf("no table %s", table)
	}
	end := start + strings.Index(src[start:], "\n}\n")
	for _, l := range strings.Split(src[start:end], "\n") {
		if strings.Contains(l, prefix) {
			return l
		}
	}
	t.Fatalf("table %s has no line with %q", table, prefix)
	return ""
}

// has fails unless s contains every part.
func has(t *testing.T, s string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(s, p) {
			t.Errorf("%s\nlacks %s", s, p)
		}
	}
}

// lacks fails when s contains any part.
func lacks(t *testing.T, s string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(s, p) {
			t.Errorf("%s\nhas %s", s, p)
		}
	}
}

func TestGenerateMetadata(t *testing.T) {
	src := metaSource(t, nil)
	col := func(table, name string) string { return line(t, src, table, `{Name: "`+name+`"`) }

	t.Run("tables", func(t *testing.T) {
		has(t, src, "Label:  `Shipment`,", "Plural: `Shipments`,",
			`Doc:    "Shipment is freight moving.\n\nIt has a second paragraph.",`,
			"Report: &schema.TableReport{Category: `Operations`},",
			"Label:  `Client`,", "Plural: `Clientele`,", "Report: &schema.TableReport{},",
			"Label:  `BOL Line`,", "Plural: `BOL Lines`,")
		has(t, line(t, src, "movesTable", "Label:"), "`Move`")
		lacks(t, line(t, src, "movesTable", "Plural:"), "Report")
	})
	t.Run("labels and docs", func(t *testing.T) {
		has(t, col("shipmentsTable", "pro_number"), "Label: `Pro Number`",
			"Doc: `ProNumber is the carrier's progressive number.`")
		has(t, col("shipmentsTable", "customer_id"), "Label: `Customer ID`")
		has(t, col("shipmentsTable", "url_path"), "Label: `Link`")
		has(t, line(t, src, "shipmentsTable", `{Name: "BillTo"`), "Label: `Payer`", "Doc: `BillTo is who pays.`",
			"NoReport: true")
		has(t, line(t, src, "shipmentsTable", `{Name: "Customer"`), "Label: `Customer`")
		lacks(t, line(t, src, "shipmentsTable", `{Name: "Customer"`), "NoReport")
	})
	t.Run("enums", func(t *testing.T) {
		has(t, col("shipmentsTable", "status"), "Enum: enumStatus[:]", "Report: &reportDimEnum")
		has(t, col("shipmentsTable", "level"), "Enum: enumLevel[:]", "Report: &reportDimEnum")
		has(t, col("shipmentsTable", "mode"), "Enum: enumKindsMode[:]", "Report: &reportDimEnumMode")
		has(t, src, "var enumStatus = [...]schema.EnumValue{\n"+
			"\t{Value: `in_transit`, Label: `In Transit`, Doc: `StatusInTransit is a shipment on the road.`},\n"+
			"\t{Value: `new`, Label: `New`, Doc: `StatusNew is a fresh shipment.`},\n"+
			"\t{Value: `other`, Label: `Other`},\n}",
			"var enumLevel = [...]schema.EnumValue{\n\t{Value: `1`, Label: `Low`},\n\t{Value: `2`, Label: `High`},\n}",
			"var enumKindsMode = [...]schema.EnumValue{\n\t{Value: `air`, Label: `Air`, Doc: `ModeAir is by air.`},\n"+
				"\t{Value: `road`, Label: `Road`, Doc: `ModeRoad is by road.`},\n}")
		lacks(t, col("shipmentsTable", "pro_number"), "Enum:")
	})
	t.Run("included", func(t *testing.T) {
		has(t, col("shipmentsTable", "org_id"), "&reportDimStringHidden")
		has(t, col("shipmentsTable", "unit_id"), "&reportDimString}")
		has(t, col("shipmentsTable", "customer_id"), "&reportDimStringHidden")
		has(t, col("shipmentsTable", "bill_to_id"), "&reportDimString}")
		has(t, col("shipmentsTable", "raw"), "&reportDimByteaHidden")
		has(t, col("shipmentsTable", "raw_shown"), "&reportDimBytea}")
		has(t, col("shipmentsTable", "hidden"), "&reportDimStringHidden")
		has(t, col("shipmentsTable", "doc"), "&reportDimTsvectorHidden")
		has(t, col("shipmentsTable", "version"), "&reportMeasureIntegerHidden")
		has(t, col("shipmentsTable", "meta"), "&reportDimJsonb}")
		has(t, col("shipmentsTable", "id"), "&reportDimString}")
	})
	t.Run("value types and kinds", func(t *testing.T) {
		has(t, col("shipmentsTable", "total"), "&reportMeasureDecimalMoney")
		has(t, col("shipmentsTable", "w"), "&reportMeasureIntegerWeight")
		has(t, col("shipmentsTable", "miles"), "&reportMeasureFloat}")
		has(t, col("shipmentsTable", "ratio"), "&reportMeasureFloatPercentAggAvgMax")
		has(t, col("shipmentsTable", "count"), "&reportDimInteger}")
		has(t, col("shipmentsTable", "rank"), "&reportMeasureInteger}")
		for _, c := range []string{"shipped_at", "due_date", "pick_time", "stamp"} {
			has(t, col("shipmentsTable", c), "&reportDimEpoch}")
		}
		has(t, col("shipmentsTable", "day"), "&reportDimDate}")
		has(t, col("shipmentsTable", "when_ts"), "&reportDimTime}")
		has(t, col("shipmentsTable", "flag"), "&reportDimBool}")
		has(t, col("countersTable", "id"), "&reportDimInteger}")
		has(t, col("countersTable", "hits"), "&reportMeasureInteger}")
		has(t, col("ticksTable", "counter_id"), "&reportDimInteger}")
		lacks(t, line(t, src, "movesTable", `{Name: "seq"`), "Report:")
	})
	t.Run("shared values", func(t *testing.T) {
		has(t, src,
			"var reportDimEpoch = schema.ReportInfo{Included: true, Kind: schema.Dimension, Type: schema.ValueEpoch, Aggs: schema.AggsOrdered}",
			"var reportMeasureDecimalMoney = schema.ReportInfo{Included: true, Kind: schema.Measure, Type: schema.ValueDecimal, Aggs: schema.AggsNumeric, Format: `money`}",
			"var reportMeasureFloatPercentAggAvgMax = schema.ReportInfo{Included: true, Kind: schema.Measure, Type: schema.ValueFloat, Aggs: schema.AggAvg | schema.AggMax, Format: `percent`}",
			"var reportDimStringHidden = schema.ReportInfo{Kind: schema.Dimension, Type: schema.ValueString, Aggs: schema.AggsOrdered}",
			"var reportDimBool = schema.ReportInfo{Included: true, Kind: schema.Dimension, Type: schema.ValueBool, Aggs: schema.AggsCount}")
		if n := strings.Count(src, "var reportDimEpoch ="); n != 1 {
			t.Errorf("reportDimEpoch declared %d times", n)
		}
		if n := strings.Count(src, "&reportDimEpoch}"); n != 4 {
			t.Errorf("reportDimEpoch referenced %d times", n)
		}
	})
}

func TestGenerateAcronyms(t *testing.T) {
	src := metaSource(t, []string{"PRO", "BOL", "SCAC", "ID"})
	has(t, line(t, src, "shipmentsTable", `{Name: "pro_number"`), "Label: `PRO Number`")
	has(t, line(t, src, "bOLLinesTable", `{Name: "scac_code"`), "Label: `SCAC Code`")
	has(t, line(t, src, "shipmentsTable", `{Name: "url_path"`), "Label: `Link`")
	has(t, line(t, src, "shipmentsTable", `{Name: "raw_shown"`), "Label: `Raw Shown`")
	has(t, src, "Label:  `BOL Line`,")
}

func TestGenerateMetadataErrors(t *testing.T) {
	rep := func(fieldSrc string) string {
		return "type A struct {\n\tkeel.Table ~keel:\"a,report\"~\n\t" + fieldSrc + "\n}\n"
	}
	runErrors(t, []errorCase{
		{"report option off report", oneA(`N int64 ~keel:"n,format=x"~`), field(3) + ": A.N: report options need report on A's table tag"},
		{"noreport relation off report", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n\tAID *string ~keel:\"a_id\"~\n\tUp *A ~keel:\"belongs:a_id,noreport\"~\n}\n",
			field(5) + ": A.Up: noreport needs report on A's table tag"},
		{"measure on string", rep(`S string ~keel:"s,measure"~`), field(3) + ": A.S: measure needs a numeric column, not string"},
		{"measure on enum", "type E string\n\nconst EOne E = \"one\"\n\n" + rep(`S E ~keel:"s,measure"~`), field(7) + ": A.S: measure needs a numeric column, not enum"},
		{"sum on string", rep(`S string ~keel:"s,agg=count|sum"~`), field(3) + ": A.S: agg=sum does not apply to string values"},
		{"min on bool", rep(`B bool ~keel:"b,agg=min|max"~`), field(3) + ": A.B: agg=min|max does not apply to bool values"},
		{"avg on epoch", rep(`T int64 ~keel:"t_at,agg=avg"~`), field(3) + ": A.T: agg=avg does not apply to epoch values"},
		{"epoch on string", rep(`S string ~keel:"s,type=epoch"~`), field(3) + ": A.S: type=epoch needs an int64 column"},
		{"date on int64", rep(`S int64 ~keel:"s,type=date"~`), field(3) + ": A.S: type=date needs a time.Time column"},
		{"decimal on bool", rep(`S bool ~keel:"s,type=decimal"~`), field(3) + ": A.S: type=decimal needs a sql.Scanner or string column"},
		{"format not a literal", "type F string\n\nfunc (F) KeelFormat() string { f := \"x\"; return f }\n\n" + rep(`S F ~keel:"s"~`),
			at(3, 10) + ": KeelFormat must be a single return of a string literal"},
		{"format returns a name", "type F string\n\nconst name = \"x\"\n\nfunc (F) KeelFormat() string { return name }\n\n" + rep(`S F ~keel:"s"~`),
			at(5, 10) + ": KeelFormat must return a string literal"},
		{"format signature", "type F string\n\nfunc (F) KeelFormat() int { return 1 }\n\n" + rep(`S F ~keel:"s"~`),
			at(3, 10) + ": F.KeelFormat must be func() string"},
	})
}
