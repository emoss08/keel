package keel_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestJoinTxRunsInsideAForeignTransaction(t *testing.T) {
	rec := &recorder{}
	var began atomic.Int32
	d := openDB(t, func(cfg *keel.Config) {
		cfg.Observers = []keel.Observer{rec}
		cfg.DataVersions = keel.DataVersionsConfig{Enabled: true}
		cfg.OnBegin = func(context.Context, keel.TxInfo) ([]keel.Statement, error) {
			began.Add(1)
			return nil, nil
		}
	})
	committed := 0
	keel.OnCreate(d, fleetdb.Customers, keel.Committed, func(context.Context, *keel.Mutation[*fleet.Customer]) error {
		committed++
		return nil
	})
	ctx := newTenant()
	tn := ctx.Value(tenantKey{}).(tenant)
	pooled, err := d.Pool().Acquire(context.Background())
	ok(t, err)
	defer pooled.Release()
	conn := pooled.Conn()
	count := func(code string) int64 {
		t.Helper()
		return must(keel.QueryValue[int64](context.Background(), testDB,
			"SELECT (SELECT count(*) FROM customers WHERE code = $1) + (SELECT count(*) FROM tags WHERE name = $1)", code))(t)
	}
	version := func() int64 {
		t.Helper()
		ok(t, d.FlushDataVersions(context.Background()))
		return must(d.DataVersions(ctx, d.TenantKey(ctx), "customers"))(t)["customers"]
	}

	run := func(code string, commit bool) {
		t.Helper()
		tx, err := conn.Begin(context.Background())
		ok(t, err)
		jctx, done := keel.JoinTx(ctx, d, conn)
		beganBefore := began.Load()
		after := 0
		ok(t, keel.Write(jctx, d, func(ctx context.Context) error {
			keel.AfterCommit(ctx, func(context.Context) { after++ })
			if err := fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "Joined", Code: code}); err != nil {
				return err
			}
			lost := keel.Savepoint(ctx, d, func(ctx context.Context) error {
				if err := fleetdb.Customers.Create(ctx, d, &fleet.Customer{Name: "Savepoint", Code: code + "S"}); err != nil {
					return err
				}
				return errors.New("undo")
			})
			if lost == nil {
				t.Error("the savepoint's error was lost")
			}
			return nil
		}))
		_, err = tx.Exec(context.Background(), "INSERT INTO tags (id, organization_id, business_unit_id, name) VALUES ($1, $2, $3, $4)",
			"tag_"+code, string(tn.org), string(tn.bu), code)
		ok(t, err)
		n := must(fleetdb.Customers.Where(fleetdb.Customers.Code.Eq(code)).Count(jctx, d))(t)
		eq(t, n, int64(1))
		if began.Load() != beganBefore {
			t.Error("OnBegin ran for a joined transaction")
		}
		before := version()
		if commit {
			ok(t, tx.Commit(context.Background()))
		} else {
			ok(t, tx.Rollback(context.Background()))
		}
		done(commit)
		done(!commit)
		want := 0
		if commit {
			want = 1
		}
		eq(t, after, want)
		if moved := version() != before; moved != commit {
			t.Errorf("commit %v moved the data version: %v", commit, moved)
		}
		if _, err := fleetdb.Customers.Where(fleetdb.Customers.Code.Eq(code)).Count(jctx, d); err == nil {
			t.Error("keel ran on a joined transaction after it ended")
		}
	}

	rolled := "JR" + newTenantID()
	run(rolled, false)
	eq(t, count(rolled), int64(0))
	eq(t, count(rolled+"S"), int64(0))
	eq(t, committed, 0)

	kept := "JC" + newTenantID()
	run(kept, true)
	eq(t, count(kept), int64(2))
	eq(t, count(kept+"S"), int64(0))
	eq(t, committed, 1)
	for _, e := range rec.events {
		if e.Replica != "" {
			t.Fatalf("a joined statement ran on a replica: %s", e.SQL)
		}
	}
}
