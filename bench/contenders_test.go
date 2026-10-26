package bench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// contender is one more library in the comparison. Each operation runs one iteration of a
// benchmark and checks its result; a nil operation is one the library has no counterpart for.
// save creates the row it then saves on each iteration.
type contender struct {
	name       string
	get        func(ctx context.Context, id string) error
	list       func(ctx context.Context, n int) error
	relations  func(ctx context.Context) error
	scoped     func(ctx context.Context, id string) error
	page       func(ctx context.Context) error
	create     func(ctx context.Context) error
	createMany func(ctx context.Context, n int) error
	save       func(ctx context.Context) (func(ctx context.Context) error, error)
	build      func(ctx context.Context) error
	close      func()
}

// contenders opens, in this order, the libraries benchmarked beside keel, Bun and pgx.
var contenders = []func(e *env) (*contender, error){
	openGORM, openEnt, openSqlc, openBob, openJet, openSqlx,
}

// sqlPoolConns is the pool size of the libraries on database/sql, the default pgxpool size on
// the bench VM's four CPUs, so every library has as many connections.
const sqlPoolConns = 4

// openSQL opens database/sql on pgx's stdlib driver, which caches each statement it prepares
// per connection, as pgx does.
func openSQL(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(sqlPoolConns)
	db.SetMaxIdleConns(sqlPoolConns)
	return db, db.Ping()
}

// pageCursor is the keyset position after the first page of 50 shipments by created_at
// descending, id descending: the page the Page benchmark reads is the second.
type pageCursor struct {
	createdAt int64
	id        string
}

// runContenders runs op for every contender that has it, as a sub-benchmark named after it.
func runContenders(b *testing.B, e *env, op func(c *contender) func(ctx context.Context) error) {
	b.Helper()
	runEach(b, e, false, op)
}

// runWriteContenders is runContenders for an operation that writes customers: each
// sub-benchmark removes what it wrote when it ends (resetWrites).
func runWriteContenders(b *testing.B, e *env, op func(c *contender) func(ctx context.Context) error) {
	b.Helper()
	runEach(b, e, true, op)
}

func runEach(b *testing.B, e *env, writes bool, op func(c *contender) func(ctx context.Context) error) {
	b.Helper()
	for _, c := range e.others {
		f := op(c)
		if f == nil {
			continue
		}
		b.Run(c.name, func(b *testing.B) {
			if writes {
				resetWrites(b, e)
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := f(e.ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// checkLen reports a result of the wrong size.
func checkLen(what string, got, want int) error {
	if got != want {
		return fmt.Errorf("%s: got %d rows, want %d", what, got, want)
	}
	return nil
}

// checkRelations reports a relations read that did not load 50 shipments, each with its
// customer and the first one with its three moves.
func checkRelations(n, moves int, customer bool) error {
	if n != 50 || moves != 3 || !customer {
		return fmt.Errorf("relations: %d shipments, %d moves on the first, customer loaded %v", n, moves, customer)
	}
	return nil
}

var errConflict = errors.New("save: version conflict")

// resetWrites removes, when the sub-benchmark ends, the customers the write benchmarks create
// (named bench or save, unlike the seeded ones) and refreshes the table's statistics, so neither
// the next library's writes nor the later reads run against the rows this one left.
func resetWrites(b *testing.B, e *env) {
	b.Helper()
	b.Cleanup(func() {
		if _, err := e.pgx.Exec(context.Background(),
			"DELETE FROM customers WHERE name IN ('bench', 'save')"); err != nil {
			b.Errorf("reset writes: %v", err)
		}
		if _, err := e.pgx.Exec(context.Background(), "VACUUM ANALYZE customers"); err != nil {
			b.Errorf("reset writes: %v", err)
		}
	})
}
