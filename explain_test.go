package keel_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestExplain(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	s := fleetdb.Shipments
	typed := s.Where(s.Status.Eq(fleet.StatusNew)).Load(s.Customer()).Order(s.CreatedAt.Desc()).Limit(10)
	q := registry(t).Query("shipments")
	q.Select(q.Path("customer", "code"), keel.DynCount().As("n")).Group(q.Path("customer", "code"))
	explainers := map[string]interface {
		Explain(context.Context, *keel.DB, keel.ExplainOptions) (*keel.Plan, error)
	}{"typed": typed, "dynamic": q}
	for name, e := range explainers {
		p := must(e.Explain(w.ctx, d, keel.ExplainOptions{}))(t)
		if p.TotalCost <= 0 || p.Rows <= 0 || len(p.JSON) == 0 {
			t.Fatalf("%s: plan %+v", name, p)
		}
		if p.ActualTime != 0 || p.ExecutionTime != 0 {
			t.Fatalf("%s: measured without Analyze: %+v", name, p)
		}
		if strings.Contains(string(p.JSON), "Actual Rows") {
			t.Fatalf("%s: analyzed without Analyze", name)
		}

		p = must(e.Explain(w.ctx, d, keel.ExplainOptions{Analyze: true}))(t)
		if p.TotalCost <= 0 || p.ActualRows <= 0 || p.ExecutionTime <= 0 ||
			!strings.Contains(string(p.JSON), "Shared Hit Blocks") {
			t.Fatalf("%s: analyzed plan %+v", name, p)
		}
	}
	p := must(typed.Explain(w.ctx, d, keel.ExplainOptions{Analyze: true}))(t)
	eq(t, p.ActualRows, 5.0)

	ok(t, keel.Write(w.ctx, d, func(ctx context.Context) error {
		c := &fleet.Customer{Name: "Inside", Code: "IN"}
		if err := fleetdb.Customers.Create(ctx, d, c); err != nil {
			return err
		}
		plan, err := fleetdb.Customers.Where(fleetdb.Customers.Code.Eq("IN")).
			Explain(ctx, d, keel.ExplainOptions{Analyze: true})
		if err != nil {
			return err
		}
		if plan.ActualRows != 1 {
			t.Errorf("the analyzed statement sees the transaction's rows: %v", plan.ActualRows)
		}
		_, err = registry(t).Query("customers").Where(registry(t).Query("customers").Path("nope").Eq(1)).
			Explain(ctx, d, keel.ExplainOptions{Analyze: true})
		if err == nil {
			t.Error("explained an invalid query")
		}
		_, err = fleetdb.Customers.Where(keel.Raw("1 / (? - 1) = 1", int64(1))).
			Explain(ctx, d, keel.ExplainOptions{Analyze: true})
		if err == nil || !strings.Contains(err.Error(), "division by zero") {
			t.Errorf("a failing analyzed statement: %v", err)
		}
		n, err := fleetdb.Customers.Query().Count(ctx, d)
		if n != 4 {
			t.Errorf("the transaction lost a row: %d", n)
		}
		return err
	}))
	ok(t, keel.Read(w.ctx, d, func(ctx context.Context) error {
		_, err := typed.Explain(ctx, d, keel.ExplainOptions{Analyze: true})
		return err
	}))

	_, err := typed.Explain(context.Background(), d, keel.ExplainOptions{})
	if !errors.Is(err, keel.ErrNoScope) {
		t.Fatalf("explain without scope: %v", err)
	}

	hooked := openDB(t, func(cfg *keel.Config) {
		cfg.OnBegin = func(ctx context.Context, tx keel.TxInfo) ([]keel.Statement, error) {
			return []keel.Statement{{SQL: "SELECT set_config('keel.explain', 'on', true)"}}, nil
		}
	})
	p = must(typed.Explain(w.ctx, hooked, keel.ExplainOptions{Analyze: true}))(t)
	eq(t, p.ActualRows, 5.0)
}
