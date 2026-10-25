package keel_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/keeltest"
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

func TestExplainGenericPlanShowsTheIndexAParameterCannotReach(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	b := fleetdb.Badges
	byParam := b.Pick(b.ID).Where(b.Color.Eq("gold"))
	byConst := b.Pick(b.ID).Where(keel.DynOf(b.Color).Eq(keel.DynConst("gold")))

	ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
		custom := must(byParam.Explain(ctx, d, keel.ExplainOptions{}))(t)
		generic := must(byParam.Explain(ctx, d, keel.ExplainOptions{GenericPlan: true}))(t)
		constant := must(byConst.Explain(ctx, d, keel.ExplainOptions{GenericPlan: true}))(t)
		if !slices.Contains(custom.Indexes(), "uq_badges_one_gold") {
			t.Errorf("the bound value reaches the partial index in a custom plan: %s", custom.JSON)
		}
		if slices.Contains(generic.Indexes(), "uq_badges_one_gold") {
			t.Errorf("the generic plan of a parameter reaches the partial index: %s", generic.JSON)
		}
		if !slices.Contains(constant.Indexes(), "uq_badges_one_gold") || len(constant.SeqScans()) > 0 {
			t.Errorf("the constant reaches the partial index: %s", constant.JSON)
		}
		return nil
	}, keel.Setting("enable_seqscan", "off")))

	keeltest.UsesIndex(t, ctx, d, byConst, "uq_badges_one_gold")
	keeltest.UsesIndex(t, ctx, d, fleetdb.Shipments.Where(fleetdb.Shipments.Status.Eq(fleet.StatusNew)), "idx_shipments_status")

	if _, err := byParam.Explain(ctx, d, keel.ExplainOptions{GenericPlan: true, Analyze: true}); err == nil {
		t.Fatal("analyzed a generic plan")
	}
	p := must(byParam.Explain(ctx, d, keel.ExplainOptions{GenericPlan: true}))(t)
	if len(p.SeqScans()) == 0 && len(p.Indexes()) == 0 {
		t.Fatalf("a plan outside a transaction: %s", p.JSON)
	}
}
