package keel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// backend returns the server process of the connection ctx's statements run on.
func backend(t *testing.T, ctx context.Context, d *keel.DB) int32 {
	t.Helper()
	return must(keel.QueryValue[int32](ctx, d, "SELECT pg_backend_pid()"))(t)
}

// customerCount counts the customers of ctx's tenant with code, from a transaction of its own.
func customerCount(t *testing.T, ctx context.Context, d *keel.DB, code string) int64 {
	t.Helper()
	c := fleetdb.Customers
	return must(keel.ReadValue(ctx, d, func(ctx context.Context) (int64, error) {
		return c.Where(c.Code.Eq(code)).Count(ctx, d)
	}, keel.Detached()))(t)
}

func TestDetachedCommitsOnItsOwn(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	boom := errors.New("boom")
	var outerPID, innerPID int32
	afterInner, afterOuter := false, false

	err := keel.Write(ctx, d, func(ctx context.Context) error {
		keel.AfterCommit(ctx, func(context.Context) { afterOuter = true })
		ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Outer", Code: "OUT"}))
		outerPID = backend(t, ctx, d)
		ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
			if !keel.InTx(ctx, d) {
				t.Fatal("no transaction inside Detached")
			}
			innerPID = backend(t, ctx, d)
			keel.AfterCommit(ctx, func(context.Context) { afterInner = true })
			eq(t, must(c.Where(c.Code.Eq("OUT")).Count(ctx, d))(t), int64(0))
			return keel.Savepoint(ctx, d, func(ctx context.Context) error {
				return c.Create(ctx, d, &fleet.Customer{Name: "Inner", Code: "IN"})
			})
		}, keel.Detached()))
		if !afterInner {
			t.Fatal("the detached transaction's AfterCommit did not run when it committed")
		}
		eq(t, customerCount(t, ctx, d, "IN"), int64(1))
		eq(t, must(c.Where(c.Code.Eq("OUT")).Count(ctx, d))(t), int64(1))
		eq(t, backend(t, ctx, d), outerPID)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	if innerPID == outerPID {
		t.Fatal("the detached transaction ran on the outer transaction's connection")
	}
	if afterOuter {
		t.Fatal("the rolled back transaction ran its AfterCommit")
	}
	eq(t, customerCount(t, ctx, d, "OUT"), int64(0))
	eq(t, customerCount(t, ctx, d, "IN"), int64(1))
}

func TestDetachedRollsBackOnItsOwn(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	boom := errors.New("boom")
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Outer", Code: "OUT"}))
		err := keel.Write(ctx, d, func(ctx context.Context) error {
			keel.AfterCommit(ctx, func(context.Context) { t.Error("a rolled back AfterCommit ran") })
			ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Inner", Code: "IN"}))
			return boom
		}, keel.Detached())
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want boom", err)
		}
		return nil
	}))
	eq(t, customerCount(t, ctx, d, "OUT"), int64(1))
	eq(t, customerCount(t, ctx, d, "IN"), int64(0))
}

func TestDetachedWriteInsideARead(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, keel.Read(ctx, d, func(ctx context.Context) error {
		if err := keel.Write(ctx, d, func(context.Context) error { return nil }); !errors.Is(err, keel.ErrReadOnly) {
			t.Fatalf("a joined Write inside a Read: got %v, want ErrReadOnly", err)
		}
		return keel.Write(ctx, d, func(ctx context.Context) error {
			return c.Create(ctx, d, &fleet.Customer{Name: "Sequence", Code: "SEQ"})
		}, keel.Detached())
	}))
	eq(t, customerCount(t, ctx, d, "SEQ"), int64(1))
}

func TestDetachedRunsOnBeginWithItsScope(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	ctx := newTenant()
	org := string(ctx.Value(tenantKey{}).(tenant).org)
	setting := func(ctx context.Context) string {
		t.Helper()
		return must(keel.QueryValue[string](ctx, d, "SELECT current_setting('keel.org')"))(t)
	}
	var outer, inner, after string
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		outer = setting(ctx)
		sys := context.WithValue(context.WithValue(ctx, tenantKey{}, nil), systemKey{}, "nightly sweep")
		err := keel.Read(sys, d, func(ctx context.Context) error {
			inner = setting(ctx)
			s := fleetdb.Shipments
			_, err := s.AllTenants().Limit(1).List(ctx, d)
			return err
		}, keel.Detached())
		after = setting(ctx)
		return err
	}))
	eq(t, outer, org)
	eq(t, inner, "system")
	eq(t, after, org)
}

func TestScopeMismatchInsideATransaction(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	tn := ctx.Value(tenantKey{}).(tenant)
	c := fleetdb.Customers
	mismatch := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, keel.ErrScopeMismatch) {
			t.Fatalf("%s: got %v, want ErrScopeMismatch", what, err)
		}
	}
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		sys := context.WithValue(ctx, systemKey{}, "audit")
		other := context.WithValue(ctx, tenantKey{}, tenant{org: "org_other", bu: "bu_other"})
		for name, mctx := range map[string]context.Context{"system": sys, "other tenant": other} {
			mismatch(name+" Read", keel.Read(mctx, d, func(context.Context) error { return nil }))
			mismatch(name+" Write", keel.Write(mctx, d, func(context.Context) error { return nil }))
			mismatch(name+" Savepoint", keel.Savepoint(mctx, d, func(context.Context) error { return nil }))
			mismatch(name+" LockKey", keel.LockKey(mctx, d, "k"))
			_, err := keel.TryLockKey(mctx, d, "k")
			mismatch(name+" TryLockKey", err)
			_, err = keel.QueryValue[int](mctx, d, "SELECT 1")
			mismatch(name+" QueryValue", err)
			_, err = c.AllTenants().Limit(1).List(mctx, d)
			mismatch(name+" List", err)
		}
		mismatch("Create", c.Create(other, d, &fleet.Customer{Name: "Other", Code: "OTH"}))

		return keel.Write(context.WithValue(ctx, tenantKey{}, tenant{org: tn.org, bu: tn.bu}), d,
			func(ctx context.Context) error {
				return c.Create(ctx, d, &fleet.Customer{Name: "Same tenant", Code: "SAME"})
			})
	}))
	eq(t, customerCount(t, ctx, d, "SAME"), int64(1))
	eq(t, customerCount(t, ctx, d, "OTH"), int64(0))

	sys := systemCtx("first")
	ok(t, keel.Read(sys, d, func(ctx context.Context) error {
		return keel.Read(context.WithValue(ctx, systemKey{}, "second"), d, func(context.Context) error { return nil })
	}))
}

func TestDetachedInsideAJoinedTransaction(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	pooled, err := d.Pool().Acquire(context.Background())
	ok(t, err)
	defer pooled.Release()
	conn := pooled.Conn()
	tx, err := conn.Begin(context.Background())
	ok(t, err)
	jctx, done := keel.JoinTx(ctx, d, conn)
	after := 0
	ok(t, keel.Write(jctx, d, func(ctx context.Context) error {
		ok(t, c.Create(ctx, d, &fleet.Customer{Name: "Joined", Code: "JOIN"}))
		keel.AfterCommit(ctx, func(context.Context) { after += 10 })
		ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
			keel.AfterCommit(ctx, func(context.Context) { after++ })
			return c.Create(ctx, d, &fleet.Customer{Name: "Detached", Code: "DET"})
		}, keel.Detached()))
		eq(t, after, 1)
		mismatch := keel.Read(context.WithValue(ctx, systemKey{}, "x"), d, func(context.Context) error { return nil })
		if !errors.Is(mismatch, keel.ErrScopeMismatch) {
			t.Fatalf("a system Read in a joined tenant transaction: got %v, want ErrScopeMismatch", mismatch)
		}
		return nil
	}))
	ok(t, tx.Rollback(context.Background()))
	done(false)
	eq(t, after, 1)
	eq(t, customerCount(t, ctx, d, "JOIN"), int64(0))
	eq(t, customerCount(t, ctx, d, "DET"), int64(1))
}

func TestDetachedOutsideATransaction(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Customers
	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		return c.Create(ctx, d, &fleet.Customer{Name: "Plain", Code: "PLAIN"})
	}, keel.Detached(), keel.Retry(1)))
	eq(t, customerCount(t, ctx, d, "PLAIN"), int64(1))
}
