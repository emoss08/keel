package keel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// singleConnDB opens a DB on the test database whose pool holds one connection, so every
// transaction waits for the previous one's connection and a destroyed connection shows as a new
// one.
func singleConnDB(t *testing.T, edit func(cfg *keel.Config)) *keel.DB {
	t.Helper()
	return openDB(t, func(cfg *keel.Config) {
		pc, err := pgxpool.ParseConfig(testDSN)
		if err != nil {
			t.Fatal(err)
		}
		pc.MaxConns = 1
		cfg.Pool = pc
		if edit != nil {
			edit(cfg)
		}
	})
}

// idle waits until d's pool has no connection checked out.
func idle(t *testing.T, d *keel.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for d.Pool().Stat().AcquiredConns() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a connection is still checked out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestReadEndsWithoutWaitingAndKeepsItsConnection(t *testing.T) {
	d := singleConnDB(t, nil)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Reader", Code: "RD"}))
	for i := range 20 {
		rctx, cancel := context.WithCancel(ctx)
		ran := false
		err := keel.Read(rctx, d, func(ctx context.Context) error {
			n, err := c.Query().Count(ctx, d)
			if err != nil {
				return err
			}
			eq(t, n, int64(1))
			keel.AfterCommit(ctx, func(context.Context) { ran = true })
			return nil
		})
		cancel()
		ok(t, err)
		eq(t, ran, true)
		if i%2 == 1 {
			err = keel.Read(ctx, d, func(ctx context.Context) error {
				if _, err := c.Query().Count(ctx, d); err != nil {
					return err
				}
				return errors.New("rolled back")
			})
			if err == nil || err.Error() != "rolled back" {
				t.Fatalf("got %v", err)
			}
		}
	}
	idle(t, d)
	eq(t, d.ReadCommitFailures(), int64(0))
	eq(t, d.Pool().Stat().NewConnsCount(), int64(1))
	eq(t, must(c.Query().Count(ctx, d))(t), int64(1))
}

func TestAwaitReadCommitAndSerializableReadsWait(t *testing.T) {
	ctx := newTenant()
	for _, tc := range []struct {
		name  string
		await bool
		opts  []keel.TxOption
	}{
		{"AwaitReadCommit", true, nil},
		{"Serializable", false, []keel.TxOption{keel.Serializable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := singleConnDB(t, func(cfg *keel.Config) { cfg.AwaitReadCommit = tc.await })
			ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
				_, err := fleetdb.Customers.Query().Count(ctx, d)
				return err
			}, tc.opts...))
			eq(t, d.Pool().Stat().AcquiredConns(), int32(0))
		})
	}
}

func TestCloseWaitsForReadCommits(t *testing.T) {
	db(t)
	d, err := keel.Open(context.Background(), testConfig(testDSN))
	if err != nil {
		t.Fatal(err)
	}
	ctx := newTenant()
	for range 5 {
		ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
			_, err := fleetdb.Customers.Query().Count(ctx, d)
			return err
		}))
	}
	d.Close()
	eq(t, d.ReadCommitFailures(), int64(0))
}

// isolation returns the transaction_isolation of the transaction on ctx.
func isolation(t *testing.T, ctx context.Context, d *keel.DB) string {
	t.Helper()
	return must(keel.QueryValue[string](ctx, d, "SELECT current_setting('transaction_isolation')"))(t)
}

func TestReadIsolation(t *testing.T) {
	d := openDB(t, func(cfg *keel.Config) { cfg.ReadIsolation = keel.RepeatableRead })
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, c.Create(ctx, d, &fleet.Customer{Name: "First", Code: "F1"}))

	ok(t, keel.Read(ctx, d, func(rctx context.Context) error {
		eq(t, isolation(t, rctx, d), "repeatable read")
		before := must(c.Query().Count(rctx, d))(t)
		ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Second", Code: "F2"}))
		eq(t, must(c.Query().Count(rctx, d))(t), before)
		return nil
	}))
	ok(t, keel.Read(ctx, d, func(rctx context.Context) error {
		eq(t, isolation(t, rctx, d), "read committed")
		before := must(c.Query().Count(rctx, d))(t)
		ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Third", Code: "F3"}))
		eq(t, must(c.Query().Count(rctx, d))(t), before+1)
		return nil
	}, keel.ReadCommitted))
	ok(t, keel.Read(ctx, d, func(rctx context.Context) error {
		eq(t, isolation(t, rctx, d), "serializable")
		return nil
	}, keel.Serializable))
	ok(t, keel.Write(ctx, d, func(wctx context.Context) error {
		eq(t, isolation(t, wctx, d), "read committed")
		return nil
	}))

	err := keel.Read(ctx, d, func(context.Context) error { return nil }, keel.Isolation(9))
	if err == nil {
		t.Fatal("an unknown isolation level was accepted")
	}
	cfg := testConfig(testDSN)
	cfg.ReadIsolation = keel.Isolation(9)
	if _, err := keel.Open(context.Background(), cfg); err == nil {
		t.Fatal("Open accepted an unknown ReadIsolation")
	}
	eq(t, keel.RepeatableRead.String(), "REPEATABLE READ")
}

// statementTimeout returns the statement_timeout of ctx's statement, in milliseconds.
func statementTimeout(t *testing.T, ctx context.Context, d *keel.DB) int64 {
	t.Helper()
	return must(keel.QueryValue[int64](ctx, d,
		"SELECT (extract(epoch FROM current_setting('statement_timeout')::interval) * 1000)::int8"))(t)
}

func TestDeadlineTimeouts(t *testing.T) {
	d := openDB(t, func(cfg *keel.Config) {
		cfg.DeadlineTimeouts = true
		cfg.OnBegin = func(context.Context, keel.TxInfo) ([]keel.Statement, error) { return nil, nil }
	})
	ctx := newTenant()

	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ok(t, keel.Read(dctx, d, func(ctx context.Context) error {
		ms := statementTimeout(t, ctx, d)
		if ms <= 0 || ms > 5000 {
			t.Fatalf("statement_timeout %dms, want (0, 5000]", ms)
		}
		return nil
	}))

	ms := statementTimeout(t, dctx, d)
	if ms <= 0 || ms > 5000 {
		t.Fatalf("implicit transaction: statement_timeout %dms, want (0, 5000]", ms)
	}

	s := fleetdb.Customers
	ok(t, s.Create(ctx, d, &fleet.Customer{Name: "Timed", Code: "T1"}))
	n := must(s.Where(keel.Raw("current_setting('statement_timeout') <> ?", "0")).
		Timeout(3*time.Second).Count(ctx, d))(t)
	eq(t, n, int64(1))

	eq(t, statementTimeout(t, ctx, d), int64(0))
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		eq(t, statementTimeout(t, ctx, d), int64(0))
		return nil
	}))
}
