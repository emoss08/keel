package guard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/keelcli/fake"
	"github.com/emoss08/keel/keelcli/seedport"
)

func TestCheck(t *testing.T) {
	prod := Target{Database: "trenova", Host: "db", Port: 5432, Environment: seedport.Production}
	dev := Target{Database: "trenova", Environment: "development"}
	prune := Action{Verb: "prune templates"}
	reset := Action{Verb: "reset the database", Never: true}
	cases := []struct {
		t       Target
		a       Action
		yesProd bool
		refused bool
	}{
		{dev, prune, false, false},
		{dev, reset, false, false},
		{prod, prune, false, true},
		{prod, prune, true, false},
		{prod, reset, true, true},
		{Target{}, prune, false, false},
	}
	for _, c := range cases {
		err := Check(c.t, c.a, c.yesProd)
		if (err != nil) != c.refused || err != nil && !errors.Is(err, ErrRefused) {
			t.Errorf("Check(%s, %s, %v) = %v", c.t.Mark(), c.a.Verb, c.yesProd, err)
		}
	}
	if err := Check(prod, prune, false); !strings.Contains(err.Error(), "trenova on db:5432 is marked production") {
		t.Errorf("message %q", err)
	}
}

func TestDescriptionAndRead(t *testing.T) {
	e := fake.New(fake.Options{Environment: seedport.Production, Now: func() time.Time { return time.Unix(0, 0) }})
	tg, err := Read(context.Background(), Sources{Facts: e.Facts()})
	if err != nil {
		t.Fatal(err)
	}
	if tg.Name() != "trenova on localhost:5432" || !tg.Production() || strings.Contains(tg.DSN, "s3cret") {
		t.Fatalf("target %+v", tg)
	}
	d := Description(tg, Action{Detail: []string{"Every row is lost."}})
	if d != "Database: trenova on localhost:5432\nEnvironment: production  (PRODUCTION)\nEvery row is lost." {
		t.Fatalf("description %q", d)
	}
	only, err := Read(context.Background(), Sources{Seeds: e.Seeds()})
	if err != nil || only.Name() != "the database" || !only.Production() {
		t.Fatalf("seeds-only target %+v %v", only, err)
	}
	if (Target{}).Mark() != "unmarked" {
		t.Fatal("empty mark")
	}
}
