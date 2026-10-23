package keel_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestExecJoinsTheTransaction(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	ctx := context.Background()
	code := "EXEC" + newTenantID()

	boom := errors.New("boom")
	err := keel.Write(ctx, d, func(ctx context.Context) error {
		n, err := keel.Exec(ctx, d, "INSERT INTO plants (code, name) VALUES ($1, $2)", code, "rolled back")
		if err != nil {
			return err
		}
		eq(t, n, int64(1))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	_, err = fleetdb.Plants.Get(ctx, d, code)
	if !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("rolled back insert is visible: %v", err)
	}

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		_, err := keel.Exec(ctx, d, "INSERT INTO plants (code, name) VALUES ($1, $2)", code, "kept")
		return err
	}))
	eq(t, must(fleetdb.Plants.Get(ctx, d, code))(t).Name, "kept")

	rec.reset()
	n := must(keel.Exec(ctx, d, "UPDATE plants SET name = $2 WHERE code = $1", code, "outside"))(t)
	eq(t, n, int64(1))
	eq(t, rec.trips(), 1)
	rec.mu.Lock()
	op := rec.events[len(rec.events)-1].Op
	rec.mu.Unlock()
	eq(t, op, "exec")
	eq(t, must(fleetdb.Plants.Get(ctx, d, code))(t).Name, "outside")
}

func TestExecInsideReadCannotWrite(t *testing.T) {
	d := db(t)
	ctx := context.Background()
	err := keel.Read(ctx, d, func(ctx context.Context) error {
		n, err := keel.Exec(ctx, d, "SELECT 1")
		if err != nil {
			return err
		}
		eq(t, n, int64(1))
		_, err = keel.Exec(ctx, d, "INSERT INTO plants (code, name) VALUES ($1, $2)", "RO", "read only")
		return err
	})
	if keel.Code(err) != "25006" {
		t.Fatalf("got %v, want a read-only transaction error", err)
	}
}

func TestDBJSON(t *testing.T) {
	db(t)
	d := must(keel.Open(context.Background(), testConfig(testDSN)))(t)
	defer d.Close()
	var out map[string]int
	ok(t, d.JSON().Unmarshal([]byte(`{"a":1}`), &out))
	eq(t, out["a"], 1)

	calls := 0
	cfg := testConfig(testDSN)
	cfg.JSON = &keel.JSONCodec{
		Marshal: json.Marshal,
		Unmarshal: func(b []byte, v any) error {
			calls++
			return json.Unmarshal(b, v)
		},
	}
	custom := must(keel.Open(context.Background(), cfg))(t)
	defer custom.Close()
	ok(t, custom.JSON().Unmarshal([]byte(`{"a":2}`), &out))
	eq(t, out["a"], 2)
	eq(t, calls, 1)
}

func newTenantID() string {
	return string(newTenant().Value(tenantKey{}).(tenant).org)
}
