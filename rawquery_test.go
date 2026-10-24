package keel_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel"
)

func TestQueryRowsJoinsTheTransaction(t *testing.T) {
	rec := &recorder{}
	d := scopedDB(t, rec)
	ctx := context.Background()
	code := "QRY" + newTenantID()

	boom := errors.New("boom")
	err := keel.Write(ctx, d, func(ctx context.Context) error {
		if _, err := keel.Exec(ctx, d, "INSERT INTO plants (code, name) VALUES ($1, $2)", code, "inside"); err != nil {
			return err
		}
		name, err := keel.QueryValue[string](ctx, d, "SELECT name FROM plants WHERE code = $1", code)
		if err != nil {
			return err
		}
		eq(t, name, "inside")
		setting, err := keel.QueryValue[string](ctx, d, "SELECT current_setting('keel.org')")
		if err != nil {
			return err
		}
		eq(t, setting, "system")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, err := keel.QueryValue[string](ctx, d, "SELECT name FROM plants WHERE code = $1", code); !errors.Is(err, keel.ErrNotFound) {
		t.Fatalf("a rolled back row was read, or a missing row was not ErrNotFound: %v", err)
	}

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		n, err := keel.ExecBatch(ctx, d,
			keel.Statement{SQL: "INSERT INTO plants (code, name) VALUES ($1, $2)", Args: []any{code + "A", "a"}},
			keel.Statement{SQL: "INSERT INTO plants (code, name) VALUES ($1, $2)", Args: []any{code + "B", "b"}},
			keel.Statement{SQL: "UPDATE plants SET name = upper(name) WHERE code LIKE $1", Args: []any{code + "%"}})
		if err != nil {
			return err
		}
		eq(t, slices.Equal(n, []int64{1, 1, 2}), true)
		return nil
	}))

	rec.reset()
	var got []string
	ok(t, keel.QueryRows(ctx, d, func(row pgx.CollectableRow) error {
		var c, n string
		if err := row.Scan(&c, &n); err != nil {
			return err
		}
		got = append(got, strings.TrimPrefix(c, code)+"="+n)
		return nil
	}, "SELECT code, name FROM plants WHERE code LIKE $1 ORDER BY code", code+"%"))
	eq(t, strings.Join(got, ","), "A=A,B=B")
	eq(t, rec.trips(), 1)
	rec.mu.Lock()
	op := rec.events[len(rec.events)-1].Op
	rec.mu.Unlock()
	eq(t, op, "query")

	names := must(keel.QueryAll(ctx, d, pgx.RowTo[string], "SELECT name FROM plants WHERE code LIKE $1 ORDER BY code", code+"%"))(t)
	eq(t, strings.Join(names, ","), "A,B")

	stop := errors.New("stop")
	seen := 0
	err = keel.QueryRows(ctx, d, func(pgx.CollectableRow) error {
		seen++
		return stop
	}, "SELECT generate_series(1, 5)")
	if !errors.Is(err, stop) || seen != 1 {
		t.Fatalf("the callback's error did not stop the rows: %v after %d rows", err, seen)
	}
	if _, err := keel.ExecBatch(ctx, d, keel.Statement{SQL: "SELECT 1"}, keel.Statement{SQL: "SELECT * FROM no_such_table"}); keel.Code(err) != "42P01" {
		t.Fatalf("got %v, want undefined_table", err)
	}
}

func TestQueryRowsInsideRead(t *testing.T) {
	d := db(t)
	ctx := context.Background()
	err := keel.Read(ctx, d, func(ctx context.Context) error {
		ro, err := keel.QueryValue[string](ctx, d, "SELECT current_setting('transaction_read_only')")
		if err != nil {
			return err
		}
		eq(t, ro, "on")
		_, err = keel.QueryValue[string](ctx, d, "INSERT INTO plants (code, name) VALUES ($1, $2) RETURNING code", "RO"+newTenantID(), "x")
		return err
	})
	if keel.Code(err) != "25006" {
		t.Fatalf("got %v, want a read-only transaction error", err)
	}
}
