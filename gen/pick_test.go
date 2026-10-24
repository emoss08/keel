package gen

import (
	"strings"
	"testing"
)

// pickModels are a shipment with moves and stops, and tags through a join table.
const pickModels = `type Shipment struct {
	keel.Table ~keel:"shipments"~

	ID    string  ~keel:"id,pk"~
	Moves []*Move ~keel:"many:shipment_id"~
	Tags  []*Tag  ~keel:"m2m:shipment_tags.shipment_id.tag_id"~
%s
}

type Move struct {
	keel.Table ~keel:"moves"~

	ID         string    ~keel:"id,pk"~
	ShipmentID string    ~keel:"shipment_id"~
	Seq        int32     ~keel:"seq"~
	Shipment   *Shipment ~keel:"belongs:shipment_id"~
	Stops      []*Stop   ~keel:"many:move_id"~
}

type Stop struct {
	keel.Table ~keel:"stops"~

	ID     string ~keel:"id,pk"~
	MoveID string ~keel:"move_id"~
	Seq    int32  ~keel:"seq"~
	Move   *Move  ~keel:"belongs:move_id"~
}

type Tag struct {
	keel.Table ~keel:"tags"~

	ID   string ~keel:"id,pk"~
	Rank int32  ~keel:"rank"~
}
`

// pickField is the line of the pick field in pickModels.
const pickField = 7

// withPick returns pickModels with fieldSrc as the shipment's last field.
func withPick(fieldSrc string) string {
	return strings.Replace(pickModels, "%s", "\t"+fieldSrc, 1)
}

func TestGeneratePicks(t *testing.T) {
	files, err := generate(t, withPick(`Origin *Stop ~keel:"pick:moves.stops,first=moves.seq|stops.seq"~
	Last   *Stop ~keel:"pick:moves.stops,last=moves.seq|stops.id"~
	TopTag *Tag  ~keel:"pick:tags,first=tags.rank,label=Top"~
	Back   *Shipment ~keel:"pick:moves.shipment,first=moves.seq"~`))
	if err != nil {
		t.Fatal(err)
	}
	src := string(files[0].Content)
	has(t, line(t, src, "shipmentsTable", `{Name: "Origin"`), `Alias: "o", Kind: schema.Pick, Local: []int32{0}`,
		"Path: []int32{0, 1}, Order: []schema.PickKey{{Hop: 0, Column: 2}, {Hop: 1, Column: 2}, {Hop: 1, Column: 0}}")
	lacks(t, line(t, src, "shipmentsTable", `{Name: "Origin"`), "Last:")
	has(t, line(t, src, "shipmentsTable", `{Name: "Last"`),
		"Path: []int32{0, 1}, Order: []schema.PickKey{{Hop: 0, Column: 2}, {Hop: 1, Column: 0}}, Last: true")
	has(t, line(t, src, "shipmentsTable", `{Name: "TopTag"`), "Label: `Top`",
		"Path: []int32{1}, Order: []schema.PickKey{{Hop: 0, Column: 1}, {Hop: 0, Column: 0}}")
	has(t, line(t, src, "shipmentsTable", `{Name: "Back"`),
		"Path: []int32{0, 0}, Order: []schema.PickKey{{Hop: 0, Column: 2}, {Hop: 1, Column: 0}}")
	has(t, src, "func (t *ShipmentsTable) Origin() *StopsTable { return keel.Path(t.Ref, 2, BuildStops) }",
		"// Origin is the pick relation.", "shipmentsTable.Relations[2].Target = StopsSchema()",
		"case 2:\n\t\tr.Origin = (*models.Stop)(child)")
}

func TestGeneratePickErrors(t *testing.T) {
	at := field(pickField)
	runErrors(t, []errorCase{
		{"pick needs a pointer", withPick(`O []*Stop ~keel:"pick:moves.stops,first=stops.seq"~`),
			at + ": Shipment.O: pick relations need a pointer field"},
		{"unknown segment", withPick(`O *Stop ~keel:"pick:moves.halts,first=moves.seq"~`),
			at + ": Shipment.O: pick path moves.halts: Move has no relation halts"},
		{"through a pick", withPick(`O *Stop ~keel:"pick:moves.stops,first=stops.seq"~
	P *Stop ~keel:"pick:o.move.stops,first=o.seq"~`),
			field(pickField+1) + ": Shipment.P: pick path o.move.stops: Shipment.O is a pick relation"},
		{"no to-many", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n\tBID string ~keel:\"b_id\"~\n\tB *A ~keel:\"belongs:b_id\"~\n\tP *A ~keel:\"pick:b,first=b.id\"~\n}\n",
			field(6) + ": A.P: pick path b crosses no to-many relation; declare belongs or one instead"},
		{"wrong end", withPick(`O *Move ~keel:"pick:moves.stops,first=stops.seq"~`),
			at + ": Shipment.O: pick path moves.stops ends at Stop, not Move"},
		{"target not a model", withPick(`O *Ghost ~keel:"pick:moves,first=moves.seq"~`) + "type Ghost struct{}\n",
			at + ": Shipment.O: example.com/app/models.Ghost is not a model in the generated packages"},
		{"missing order column", withPick(`O *Stop ~keel:"pick:moves.stops,first=stops.position"~`),
			at + ": Shipment.O: order key stops.position: Stop has no column position"},
		{"ambiguous segment", withPick(`O *Move ~keel:"pick:moves.shipment.moves,first=moves.seq"~`),
			at + ": Shipment.O: order key moves.seq: moves appears twice in the path"},
		{"keys after the target key", withPick(`O *Stop ~keel:"pick:moves.stops,first=stops.id|stops.seq"~`),
			at + ": Shipment.O: order keys after stops.id never apply: it is Stop's key"},
		{"parent key type", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID float64 ~keel:\"id,pk\"~\n\tBs []*B ~keel:\"many:a_id\"~\n\tP *B ~keel:\"pick:bs,first=bs.id\"~\n}\n" +
			"type B struct {\n\tkeel.Table ~keel:\"b\"~\n\tID string ~keel:\"id,pk\"~\n\tAID float64 ~keel:\"a_id\"~\n}\n",
			field(5) + ": A.P: pick path bs starts from A.ID, which is not a string or integer column"},
		{"target without a single key", "type A struct {\n\tkeel.Table ~keel:\"a\"~\n\tID string ~keel:\"id,pk\"~\n\tBs []*B ~keel:\"many:a_id\"~\n\tP *B ~keel:\"pick:bs,first=bs.n\"~\n}\n" +
			"type B struct {\n\tkeel.Table ~keel:\"b\"~\n\tAID string ~keel:\"a_id\"~\n\tN int32 ~keel:\"n\"~\n}\n",
			field(5) + ": A.P: B needs exactly one primary key column besides its tenant columns"},
	})
}
