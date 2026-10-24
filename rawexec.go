package keel

import (
	"context"
	"encoding/json"
)

// Exec runs one SQL statement that keel does not render, such as DDL or a write to a bookkeeping
// table without a model, and returns the number of rows it affected. It joins the transaction on
// the context, so the statement commits or rolls back with the rest of it; outside a transaction it
// runs as any terminal does, in an implicit transaction when Config.OnBegin is set. Inside a Read,
// Postgres refuses a statement that writes.
//
// Observers see the statement with the Op "exec". keel cannot see what the statement touches, so it
// bypasses scope checks, interceptors and hooks, always runs on the primary, records no commit
// position for OnReplica outside a Write, and changes no data version: call MarkChanged with the
// tables it writes. Bind values as $1, $2, … arguments; never format them into sql.
func Exec(ctx context.Context, db *DB, sql string, args ...any) (int64, error) {
	mode := modeExec
	if tx := txFrom(ctx, db); tx != nil && tx.readOnly {
		mode = modePrimaryRead
	}
	run, err := db.runnerFor(ctx, mode)
	if err != nil {
		return 0, err
	}
	s := &stmt{sql: sql, args: args, op: "exec"}
	if err := run.close(ctx, run.one(ctx, s, true)); err != nil {
		return 0, err
	}
	return s.tag.RowsAffected(), nil
}

// JSON returns the JSON implementation db encodes and decodes json and jsonb values with:
// Config.JSON, or encoding/json when it is unset. Packages built on keel use it to read JSON the
// way the application configured.
func (db *DB) JSON() JSONCodec {
	if c := db.cfg.JSON; c != nil && c.Marshal != nil && c.Unmarshal != nil {
		return *c
	}
	return JSONCodec{Marshal: json.Marshal, Unmarshal: json.Unmarshal}
}
