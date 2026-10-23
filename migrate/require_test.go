package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRequire(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	err = Require(ctx, pool, "20260101000000")
	var behind *BehindError
	if !errors.Is(err, ErrBehind) || !errors.As(err, &behind) || behind.Have != "" || behind.Want != "20260101000000" {
		t.Fatalf("missing table: %v", err)
	}
	if err := Require(ctx, pool, ""); err != nil {
		t.Fatalf("empty version: %v", err)
	}

	src := files("20260101000000_a.sql", "CREATE TABLE a (id int);", "R_v.sql", "CREATE OR REPLACE VIEW v AS SELECT 1 AS one;")
	if _, err := open(t, dsn, one(src), Options{}).Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Require(ctx, pool, "20260101000000"); err != nil {
		t.Fatalf("applied: %v", err)
	}
	err = Require(ctx, connect(t, dsn), "20260102000000")
	if !errors.As(err, &behind) || behind.Have != "20260101000000" || !strings.Contains(err.Error(), "is at 20260101000000 and needs 20260102000000") {
		t.Fatalf("behind: %v", err)
	}

	src["20260102000000_b.sql"] = files("x", "CREATE TABLE b (id int);")["x"]
	if _, err := open(t, dsn, one(src), Options{}).Up(ctx, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Require(ctx, pool, "20260101000000"); err != nil {
		t.Fatalf("ahead: %v", err)
	}
}

func TestRequireWait(t *testing.T) {
	ctx := context.Background()
	dsn := database(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	err = RequireWait(short, pool, "20260101000000", 20*time.Millisecond)
	if !errors.Is(err, ErrBehind) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}

	applied := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		m, err := Open(ctx, dsn, one(files("20260101000000_a.sql", "CREATE TABLE a (id int);")), Options{})
		if err != nil {
			applied <- err
			return
		}
		defer m.Close()
		_, err = m.Up(ctx, UpOptions{})
		applied <- err
	}()
	start := time.Now()
	if err := RequireWait(ctx, pool, "20260101000000", 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("RequireWait returned before the migration")
	}
}
