package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/fake"
	"github.com/emoss08/keel/keelcli/internal/render"
)

var t0 = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

func sources(e *fake.Engine) Sources {
	return Sources{Facts: e.Facts(), Migrations: e.Migrations(), Now: func() time.Time { return t0 }}
}

func levels(r Report) map[string]Level {
	out := map[string]Level{}
	for _, c := range r.Checks {
		out[c.Name] = c.Level
	}
	return out
}

func TestRunAgainstTheFake(t *testing.T) {
	e := fake.New(fake.Options{Now: func() time.Time { return t0 }})
	r := Run(context.Background(), sources(e))
	l := levels(r)
	if l["connection"] != Pass || l["migrations"] != Warn || l["locks"] != Warn || l["templates"] != Info || r.Failed() {
		t.Fatalf("levels %v", l)
	}
	if r.Schema != "20261024150000" || len(r.Activity.LockHolders) != 2 {
		t.Fatalf("report %+v", r)
	}
	text := Text(r, render.Plain, 0, false)
	for _, want := range []string{"plan_cache_mode", "Lock holders", "Long-running queries", "report-runner"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestFailures(t *testing.T) {
	d := fake.New(fake.Options{Drift: true, Now: func() time.Time { return t0 }})
	src := sources(d)
	src.Extensions = []string{"postgis"}
	r := Run(context.Background(), src)
	l := levels(r)
	if l["migrations"] != Fail || l["extensions"] != Fail || !r.Failed() {
		t.Fatalf("levels %v", l)
	}
	src.Facts = broken{d.Facts()}
	r = Run(context.Background(), src)
	if len(r.Checks) != 1 || r.Checks[0].Level != Fail || !strings.Contains(r.Checks[0].Detail, "connection refused") {
		t.Fatalf("checks %+v", r.Checks)
	}
}

type broken struct{ factsport.Facts }

func (broken) Connection(context.Context) (factsport.Connection, error) {
	return factsport.Connection{}, errors.New("dial tcp 127.0.0.1:5432: connection refused")
}
