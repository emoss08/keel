package seed_test

import (
	"context"
	"errors"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/seed"
)

func TestTransactRunsSeedsInTheApplicationsTransaction(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	errOther := errors.New("the other layer failed")
	failOther := map[string]bool{}
	transactions := 0
	o := opts(nil)
	o.Transact = func(ctx context.Context, fn func(ctx context.Context) error) error {
		transactions++
		pooled, err := d.Pool().Acquire(ctx)
		if err != nil {
			return err
		}
		defer pooled.Release()
		conn := pooled.Conn()
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		jctx, done := keel.JoinTx(ctx, d, conn)
		err = fn(jctx)
		var name string
		if err == nil {
			err = tx.QueryRow(ctx, "SELECT coalesce(max(name), '') FROM keel_seeds").Scan(&name)
		}
		if err == nil && failOther[name] {
			err = errOther
		}
		if err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO plants (code, name) VALUES ($1, 'other layer')", "OTHER_"+name)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			done(false)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			done(false)
			return err
		}
		done(true)
		return nil
	}
	plant := func(code string) func(context.Context, *seed.Run) error {
		return func(ctx context.Context, r *seed.Run) error {
			if !keel.InTx(ctx, r.DB) {
				return errors.New("the seed runs outside the joined transaction")
			}
			return fleetdb.Plants.Create(ctx, r.DB, &fleet.Plant{Code: code, Name: "seeded"})
		}
	}
	g := seed.NewRegistry()
	g.Add(seed.Seed{Name: "a_kept", Kind: seed.Reference, Run: plant("KEPT")})
	g.Add(seed.Seed{Name: "b_lost", Kind: seed.Reference, Run: plant("LOST")})
	failOther["b_lost"] = true

	_, err := g.Apply(ctx, d, o)
	if !errors.Is(err, errOther) {
		t.Fatalf("got %v", err)
	}
	eq(t, transactions, 2)
	eq(t, scalar[string](t, d, "SELECT string_agg(code, ',' ORDER BY code) FROM plants"), "KEPT,OTHER_a_kept")
	eq(t, scalar[string](t, d, "SELECT string_agg(name, ',' ORDER BY name) FROM keel_seeds"), "a_kept")

	delete(failOther, "b_lost")
	rep, err := g.Apply(ctx, d, o)
	ok(t, err)
	eq(t, result(t, rep, "b_lost").Ran, true)
	eq(t, scalar[string](t, d, "SELECT string_agg(code, ',' ORDER BY code) FROM plants"), "KEPT,LOST,OTHER_a_kept,OTHER_b_lost")
	eq(t, scalar[string](t, d, "SELECT string_agg(name, ',' ORDER BY name) FROM keel_seeds"), "a_kept,b_lost")
}
