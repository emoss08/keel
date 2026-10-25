package keel_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestDefaultColumnsTakeTheDatabaseDefaultWhenUnset(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	b := fleetdb.Badges

	one := &fleet.Badge{Name: "one"}
	ok(t, b.Create(ctx, d, one))
	got := must(b.Where(b.ID.Eq(one.ID)).One(ctx, d))(t)
	eq(t, *got.Level, int32(3))
	eq(t, got.Color, "blue")
	eq(t, got.Note, sql.NullString{String: "none", Valid: true})

	five := int32(5)
	rows := []*fleet.Badge{
		{Name: "unset"},
		{Name: "set", Level: &five, Color: "red", Note: sql.NullString{String: "kept", Valid: true}},
	}
	ok(t, b.CreateMany(ctx, d, rows))
	unset := must(b.Where(b.Name.Eq("unset")).One(ctx, d))(t)
	eq(t, *unset.Level, int32(3))
	eq(t, unset.Color, "blue")
	eq(t, unset.Note.String, "none")
	set := must(b.Where(b.Name.Eq("set")).One(ctx, d))(t)
	eq(t, *set.Level, int32(5))
	eq(t, set.Color, "red")
	eq(t, set.Note.String, "kept")

	if _, err := b.Copy(ctx, d, []*fleet.Badge{{Name: "copied"}}); err == nil ||
		!strings.Contains(err.Error(), "database default") {
		t.Fatalf("got %v", err)
	}
	n := must(b.Copy(ctx, d, []*fleet.Badge{{Name: "copied", Level: &five, Color: "red",
		Note: sql.NullString{String: "x", Valid: true}}}))(t)
	eq(t, n, int64(1))
}
