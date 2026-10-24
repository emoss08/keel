package keel_test

import (
	"context"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// noteFixture creates notes whose bodies the search tests look for.
func noteFixture(t *testing.T) (context.Context, *keel.DB) {
	t.Helper()
	d := db(t)
	ctx := newTenant()
	cust := newCustomer(t, ctx, "NOTE")
	var notes []*fleet.Note
	for _, body := range []string{
		"Dallas reefer loads every Monday",
		"Reefer to Dallas, then dry van back",
		"Chicago flatbed lanes",
		"Customer prefers reefers for produce",
	} {
		notes = append(notes, &fleet.Note{CustomerID: cust.ID, Body: body})
	}
	ok(t, fleetdb.Notes.CreateMany(ctx, d, notes))
	return ctx, d
}

// bodies returns the bodies of the notes p matches, in body order.
func bodies(t *testing.T, ctx context.Context, d *keel.DB, p keel.Pred) string {
	t.Helper()
	n := fleetdb.Notes
	rows := must(n.Where(p).Order(n.Body.Asc()).All(ctx, d))(t)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Fields(r.Body)[0]
	}
	return strings.Join(out, ",")
}

func TestSearchMatchModes(t *testing.T) {
	ctx, d := noteFixture(t)
	v := fleetdb.Notes.SearchVector
	for _, c := range []struct {
		name string
		p    keel.Pred
		want string
	}{
		{"websearch stems with the column's configuration", v.Match("reefers"), "Customer,Dallas,Reefer"},
		{"websearch operators", v.Match(`"dallas reefer" or chicago`), "Chicago,Dallas"},
		{"websearch excludes", v.Match("reefer -produce"), "Dallas,Reefer"},
		{"other configuration", v.Match("reefers", keel.SearchConfig("simple")), ""},
		{"configurations ORed", v.Match("reefers produce", keel.SearchConfig("simple", "english")), "Customer"},
		{"plain", v.Match("dallas & reefer", keel.SearchModes(keel.Plain)), "Dallas,Reefer"},
		{"phrase", v.Match("dallas reefer", keel.SearchModes(keel.Phrase)), "Dallas"},
		{"prefix", v.Match("dal ree", keel.SearchModes(keel.Prefix)), "Dallas,Reefer"},
		{"whole word only", v.Match("dal"), ""},
		{"any word", v.Match("chicago produce", keel.SearchModes(keel.AnyWord)), "Chicago,Customer"},
		{"any prefix", v.Match("chic prod", keel.SearchModes(keel.AnyPrefix)), "Chicago,Customer"},
		{"modes ORed", v.Match("flat", keel.SearchModes(keel.WebSearch, keel.Prefix)), "Chicago"},
		{"no words", v.Match("&&& ***", keel.SearchModes(keel.Prefix)), ""},
		{"quotes are data", v.Match(`o'brien\ ' | dallas`, keel.SearchModes(keel.AnyWord)), "Dallas,Reefer"},
	} {
		t.Run(c.name, func(t *testing.T) { eq(t, bodies(t, ctx, d, c.p), c.want) })
	}
}

func TestSearchRanks(t *testing.T) {
	ctx, d := noteFixture(t)
	n := fleetdb.Notes
	q := "dallas reefer"
	rows := must(n.Compute(n.Rank, n.SearchVector.Rank(q)).Where(n.SearchVector.Match(q)).
		Order(n.Rank.Desc(), n.Body.Asc()).All(ctx, d))(t)
	eq(t, len(rows), 2)
	if rows[0].Rank <= 0 || rows[0].Rank < rows[1].Rank {
		t.Fatalf("ranks %v %v", rows[0].Rank, rows[1].Rank)
	}

	cd := must(n.Where(n.SearchVector.Match(q)).Order(n.SearchVector.RankCD(q).Desc()).First(ctx, d))(t)
	eq(t, strings.HasPrefix(cd.Body, "Dallas reefer"), true)

	norm := must(n.Compute(n.Rank, n.SearchVector.Rank(q, keel.RankNormalization(32))).
		Where(n.SearchVector.Match(q)).Order(n.Rank.Desc()).First(ctx, d))(t)
	if norm.Rank <= 0 || norm.Rank >= 1 {
		t.Fatalf("normalized rank %v", norm.Rank)
	}

	sql, args := must2(n.Compute(n.Rank, n.SearchVector.Rank(q)).Where(n.SearchVector.Match(q)).ToSQL(ctx, d))(t)
	if strings.Count(sql, "websearch_to_tsquery('english'::regconfig, $3)") != 2 || len(args) != 3 {
		t.Fatalf("the search string is not bound once: %s %v", sql, args)
	}
	sql, _ = must2(n.Where(n.SearchVector.Match(q, keel.SearchConfig("simple", "english"),
		keel.SearchModes(keel.WebSearch, keel.Prefix))).ToSQL(ctx, d))(t)
	want := "(websearch_to_tsquery('simple'::regconfig, $3) || to_tsquery('simple'::regconfig, $4) || " +
		"websearch_to_tsquery('english'::regconfig, $3) || to_tsquery('english'::regconfig, $4))"
	if !strings.Contains(sql, want) {
		t.Fatalf("ORed query: %s", sql)
	}
}

func TestSearchErrors(t *testing.T) {
	ctx, d := noteFixture(t)
	n := fleetdb.Notes
	_, err := n.Where(n.SearchVector.Match("x", keel.SearchConfig("english'; drop"))).All(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("got %v", err)
	}
	_, err = n.Where(n.SearchVector.Match("x", keel.SearchConfig("klingon"))).All(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "klingon") {
		t.Fatalf("an unknown configuration ran: %v", err)
	}
	_, err = n.Where(n.SearchVector.Match("x", keel.SearchModes(keel.SearchMode(99)))).All(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "unknown search mode") {
		t.Fatalf("got %v", err)
	}
	_, err = n.Compute(n.Rank, n.SearchVector.Rank("x")).Compute(n.Rank, n.Body).All(ctx, d)
	if err == nil {
		t.Fatal("a text expression filled a real field")
	}
}

func TestSearchDynamic(t *testing.T) {
	ctx, d := noteFixture(t)
	q := keel.DynFrom(fleetdb.NotesSchema())
	vec := q.Path("search_vector")
	q.Compute("rank", vec.Rank("reefer")).
		Select(q.Path("body"), q.Path("rank"), vec.RankCD("reefer").As("cd")).
		Where(vec.Match("reefers"), vec.Match("dal", keel.SearchModes(keel.Prefix)).Not()).
		Order(q.Path("rank").Desc())
	res := must(q.Run(ctx, d))(t)
	eq(t, res.Rows, 1)
	eq(t, res.Columns[0].String[0], "Customer prefers reefers for produce")
	if res.Column("rank").Float64[0] <= 0 || res.Column("cd").Float64[0] <= 0 {
		t.Fatalf("ranks %+v", res.Columns)
	}
	bad := keel.DynFrom(fleetdb.NotesSchema())
	if err := bad.Path("search_vector").Match("x", keel.SearchConfig("Bad")).Err(); err == nil {
		t.Fatal("an invalid configuration was accepted")
	}
}
