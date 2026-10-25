package keel_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/keeltables"
	"github.com/emoss08/keel/schema"
)

// TestVerifyGeneratedTables finds no drift between the test models and the test migrations; the
// PostGIS tests create the places and zones tables themselves.
func TestVerifyGeneratedTables(t *testing.T) {
	d := db(t)
	var tables []*schema.Table
	for _, tb := range keeltables.All() {
		if tb.Name != "places" && tb.Name != "zones" {
			tables = append(tables, tb)
		}
	}
	drifts, err := keel.Verify(context.Background(), d, tables)
	ok(t, err)
	for _, dr := range drifts {
		t.Errorf("drift: %s", dr)
	}
}

// verifyTable builds a hand-written table named name with the columns and relations given.
func verifyTable(name, model string, cols []schema.Column, rels ...schema.Relation) *schema.Table {
	t := &schema.Table{Name: name, Model: "app." + model, Columns: cols, Relations: rels}
	schema.Derive(t)
	return t
}

// TestVerifyReportsDrift checks tables built to differ from their models in every way Verify
// reports.
func TestVerifyReportsDrift(t *testing.T) {
	d := db(t)
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	p := "vt_" + hex.EncodeToString(raw) + "_"
	ctx := context.Background()
	_, err := d.Pool().Exec(ctx, fmt.Sprintf(`
CREATE DOMAIN %[1]sword AS text NOT NULL;
CREATE TABLE %[1]sx (
    id text NOT NULL,
    org text NOT NULL,
    small bigint NOT NULL,
    name integer NOT NULL,
    note text,
    label %[1]sword,
    total numeric GENERATED ALWAYS AS (1) STORED NOT NULL,
    doc text NOT NULL,
    words text NOT NULL,
    seq bigint GENERATED ALWAYS AS IDENTITY,
    tags text[] NOT NULL,
    PRIMARY KEY (id)
);
CREATE TABLE %[1]sy (id text NOT NULL, org text NOT NULL, PRIMARY KEY (id, org));
CREATE TABLE %[1]sxy (x_id text NOT NULL, y_id integer NOT NULL);
CREATE TABLE %[1]sz (id text PRIMARY KEY);
CREATE VIEW %[1]sv AS SELECT id, note FROM %[1]sx;`, p))
	ok(t, err)
	t.Cleanup(func() {
		_, _ = d.Pool().Exec(context.Background(), fmt.Sprintf(`DROP VIEW %[1]sv; DROP TABLE %[1]sx, %[1]sy, %[1]sxy, %[1]sz;
			DROP DOMAIN %[1]sword`, p))
	})
	y := verifyTable(p+"y", "Y", []schema.Column{
		{Name: "id", Field: "ID", Kind: schema.KindString, Flags: schema.FlagPK},
		{Name: "org", Field: "Org", Kind: schema.KindString, Flags: schema.FlagPK | schema.FlagTenant},
	})
	z := verifyTable(p+"z", "Z", []schema.Column{
		{Name: "id", Field: "ID", Kind: schema.KindString, Flags: schema.FlagPK},
		{Name: "x_id", Field: "XID", Kind: schema.KindString},
	})
	x := verifyTable(p+"x", "X", []schema.Column{
		{Name: "id", Field: "ID", Kind: schema.KindString, Flags: schema.FlagPK},
		{Name: "org", Field: "Org", Kind: schema.KindString, Flags: schema.FlagPK | schema.FlagTenant},
		{Name: "small", Field: "Small", Kind: schema.KindInt16},
		{Name: "name", Field: "Name", Kind: schema.KindString},
		{Name: "note", Field: "Note", Kind: schema.KindString},
		{Name: "label", Field: "Label", Kind: schema.KindString},
		{Name: "total", Field: "Total", Kind: schema.KindFloat64},
		{Name: "doc", Field: "Doc", Kind: schema.KindJSON, Flags: schema.FlagJSONB},
		{Name: "words", Field: "Words", Kind: schema.KindTSVector},
		{Name: "seq", Field: "Seq", Kind: schema.KindInt64},
		{Name: "tags", Field: "Tags", Kind: schema.KindStringArray},
		{Name: "gone", Field: "Gone", Kind: schema.KindInt32},
		{Name: "rank", Field: "Rank", Kind: schema.KindFloat64, Flags: schema.FlagComputed},
	},
		schema.Relation{Name: "Ys", Kind: schema.ManyToMany, Target: y, Local: []int32{0}, Remote: []int32{0},
			JoinTable: p + "xy", JoinLocal: []string{"x_id"}, JoinRemote: []string{"y_id"}},
		schema.Relation{Name: "Others", Kind: schema.ManyToMany, Target: y, Local: []int32{0}, Remote: []int32{0},
			JoinTable: p + "none", JoinLocal: []string{"x_id"}, JoinRemote: []string{"y_id"}},
		schema.Relation{Name: "Zs", Kind: schema.HasMany, Target: z, Local: []int32{0}, Remote: []int32{1}},
		schema.Relation{Name: "Lost", Kind: schema.BelongsTo, Local: []int32{0}, Remote: []int32{0}},
	)
	view := verifyTable(p+"v", "V", []schema.Column{
		{Name: "id", Field: "ID", Kind: schema.KindString, Flags: schema.FlagPK},
		{Name: "note", Field: "Note", Kind: schema.KindString},
	})
	w := verifyTable(p+"w", "W", []schema.Column{{Name: "id", Field: "ID", Kind: schema.KindString}})
	drifts, err := keel.Verify(ctx, d, []*schema.Table{x, y, view, w, x})
	ok(t, err)
	var got []string
	for _, dr := range drifts {
		got = append(got, fmt.Sprintf("%s %s.%s %s", dr.Kind, strings.TrimPrefix(dr.Table, p), dr.Column, dr.Field))
		if dr.Detail == "" || dr.Fix == "" || !strings.Contains(dr.String(), dr.Fix) {
			t.Errorf("drift without a detail or fix: %#v", dr)
		}
	}
	want := []string{
		"type x.small X.Small",
		"type x.name X.Name",
		"nullable x.note X.Note",
		"generated x.total X.Total",
		"json x.doc X.Doc",
		"type x.words X.Words",
		"generated x.seq X.Seq",
		"missing_column x.gone X.Gone",
		"primary_key x. X",
		"type xy.y_id X.Ys",
		"join_table xy.org X.Ys",
		"join_table none. X.Others",
		"relation_key z.x_id X.Zs",
		"relation_key x. X.Lost",
		"missing_table w. W",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, dr := range drifts {
		if dr.Kind == keel.DriftMissingColumn && !strings.Contains(dr.Fix, `ADD COLUMN "gone" integer NOT NULL`) {
			t.Fatalf("missing column fix: %s", dr.Fix)
		}
		if dr.Kind == keel.DriftJoinTable && dr.Column == "" &&
			!strings.Contains(dr.Fix, `("x_id" text NOT NULL, "y_id" text NOT NULL, "org" text NOT NULL)`) {
			t.Fatalf("join table fix: %s", dr.Fix)
		}
	}
}
