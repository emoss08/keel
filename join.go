package keel

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
)

// JoinTx makes keel run inside a transaction another data layer began on conn, for an application
// moving to keel while it still writes through another library over the same pgx connection
// (Bun on pgx's stdlib driver, plain pgx). Every keel operation on the returned context runs on
// conn, in that transaction: keel sends no BEGIN, COMMIT or Config.OnBegin statements (the owner
// began the transaction and set its scope), Read and Write join it, Savepoint nests in it, and
// replicas are not used. Scope checks, tenancy, interceptors, read filters, hooks and observers
// apply as in a transaction keel began. The transaction's scope is ctx's: keel work under another
// scope returns ErrScopeMismatch, and a Detached Read or Write runs on keel's own pool instead.
//
// The owner calls done once the transaction ends: done(true) after COMMIT runs the Committed
// hooks and AfterCommit callbacks and hands the tables the transaction changed to the data
// versions; done(false) after ROLLBACK drops them. Calls after the first do nothing, and keel
// refuses to run on the context afterwards.
//
//	tx, err := bunDB.BeginTx(ctx, nil) // on a pgx connection
//	ctx, done := keel.JoinTx(ctx, db, conn)
//	err = keel.Write(ctx, db, func(ctx context.Context) error { … })
//	… tx.Commit() …
//	done(err == nil)
//
// conn is used by one goroutine at a time, as any transaction is.
func JoinTx(ctx context.Context, db *DB, conn *pgx.Conn) (context.Context, func(committed bool)) {
	tx := &txState{db: db, conn: conn, joined: true, begun: true, scopeCtx: ctx}
	var once sync.Once
	done := func(committed bool) {
		once.Do(func() { tx.close(ctx, committed) })
	}
	return context.WithValue(ctx, txKey{}, tx), done
}

// close ends a joined transaction after its owner committed or rolled back it: the connection is
// let go, and on commit the data versions it changed are recorded and its after-commit work runs.
func (tx *txState) close(ctx context.Context, committed bool) {
	tx.conn = nil
	if !committed {
		tx.changes, tx.afterCommit = nil, nil
		return
	}
	tx.committed = true
	if tx.changes != nil {
		tx.db.versions.add(tx.changes)
		tx.changes = nil
	}
	tx.runAfterCommit(ctx)
}
