package seed_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/emoss08/keel/seed"
)

func noop(context.Context, *seed.Run) error { return nil }

func names(seeds []*seed.Seed) string {
	out := make([]string, len(seeds))
	for i, s := range seeds {
		out[i] = s.Name
	}
	return strings.Join(out, ",")
}

func TestSeedsInDependencyOrder(t *testing.T) {
	g := seed.NewRegistry()
	c := g.Add(seed.Seed{Name: "c", Kind: seed.Fixture, Run: noop})
	d := g.Add(seed.Seed{Name: "d", Kind: seed.Reference, Run: noop})
	b := g.Add(seed.Seed{Name: "b", Kind: seed.Reference, Run: noop})
	a := g.Add(seed.Seed{Name: "a", Kind: seed.Reference, Run: noop})
	c.After = []*seed.Seed{b, d}
	b.After = []*seed.Seed{a}
	ok(t, g.Validate())
	eq(t, names(g.Seeds()), "d,a,b,c")
	eq(t, names(c.After), "b,d")

	other := seed.NewRegistry()
	e := other.Add(seed.Seed{Name: "e", Kind: seed.Fixture, After: []*seed.Seed{c}, Run: noop})
	m := seed.Merge(g, other, g)
	ok(t, m.Validate())
	eq(t, names(m.Seeds()), "d,a,b,c,e")
	eq(t, e.After[0], c)
}

func TestValidationReportsEveryProblem(t *testing.T) {
	fsys := fstest.MapFS{"data/ok.yaml": {Data: []byte("[]")}}
	outside := seed.NewRegistry().Add(seed.Seed{Name: "outside", Kind: seed.Reference, Run: noop})
	g := seed.NewRegistry()
	g.Add(seed.Seed{Name: "dup", Kind: seed.Reference, Run: noop})
	g.Add(seed.Seed{Name: "dup", Kind: seed.Reference, Run: noop})
	g.Add(seed.Seed{Name: "orphan", Kind: seed.Reference, After: []*seed.Seed{outside}, Run: noop})
	x := g.Add(seed.Seed{Name: "x", Kind: seed.Reference, Run: noop})
	y := g.Add(seed.Seed{Name: "y", Kind: seed.Reference, After: []*seed.Seed{x}, Run: noop})
	z := g.Add(seed.Seed{Name: "z", Kind: seed.Reference, After: []*seed.Seed{y}, Run: noop})
	x.After = []*seed.Seed{z}
	fix := g.Add(seed.Seed{Name: "fix", Kind: seed.Fixture, Run: noop})
	g.Add(seed.Seed{Name: "ref", Kind: seed.Reference, After: []*seed.Seed{fix}, Run: noop})
	g.Add(seed.Seed{Name: "files", Kind: seed.Reference, Run: noop,
		Inputs: seed.Files(fsys, "data/ok.yaml", "data/missing.yaml", "data/*.csv")})
	g.Add(seed.Seed{Name: "kindless", Run: noop})
	g.Add(seed.Seed{Name: "runless", Kind: seed.Fixture})
	g.Add(seed.Seed{Kind: seed.Fixture, Run: noop})

	err := g.Validate()
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		`seed "dup" is registered more than once`,
		`seed "orphan" runs after seed "outside", which is not in the registry`,
		"seed: After cycle: x → y → z → x",
		`reference seed "ref" runs after fixture seed "fix"`,
		"data/missing.yaml",
		`input pattern "data/*.csv" matches no file`,
		`seed "kindless" has no Kind`,
		`seed "runless" has no Run function`,
		"a seed has no name",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	eq(t, strings.Count(err.Error(), "cycle"), 1)

	d := newDB(t)
	_, perr := g.Plan(context.Background(), d, opts(nil))
	eq(t, perr.Error(), err.Error())
	rec := &recorder{}
	_, aerr := g.Apply(context.Background(), d, opts(rec))
	eq(t, aerr.Error(), err.Error())
	events := rec.list()
	eq(t, len(events), 1)
	eq(t, events[0].Kind, seed.RunEnd)
	eq(t, scalar[bool](t, d, "SELECT to_regclass('keel_seeds') IS NULL"), true)
}
