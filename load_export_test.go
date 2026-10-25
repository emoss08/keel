package keel

import "context"

// PreparedStatements renders the statements a Batch of item sends in its first round trip on ctx:
// the read's statement, its count, and its pipelined relation loads, in order. It only renders,
// so it works on a DB that never connects.
func PreparedStatements(ctx context.Context, db *DB, item BatchItem) ([]Statement, error) {
	run, err := db.newRunner(ctx, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = run.close(ctx, nil) }()
	rd, err := item.prepare(ctx, run)
	if err != nil {
		return nil, err
	}
	out := make([]Statement, len(rd.stmts))
	for i, s := range rd.stmts {
		out[i] = Statement{SQL: s.sql, Args: s.args}
	}
	return out, nil
}
