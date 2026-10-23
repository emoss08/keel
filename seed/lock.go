package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	lockPoll       = 100 * time.Millisecond
	lockWaitReport = time.Second
)

const (
	tryLockSQL = `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`
	unlockSQL  = `SELECT pg_advisory_unlock(hashtextextended($1, 0))`
	holderSQL  = `SELECT a.pid, coalesce(a.application_name, ''), coalesce(a.state, ''), coalesce(a.query, ''),
       coalesce(a.query_start, a.backend_start)
  FROM pg_locks l
  JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
   AND l.classid::bigint = ((hashtextextended($1, 0) >> 32) & 4294967295)
   AND l.objid::bigint = (hashtextextended($1, 0) & 4294967295)
 LIMIT 1`
)

// sessionLock is the seed advisory lock, held on one pooled connection for a whole run.
type sessionLock struct {
	conn *pgxpool.Conn
}

// lock takes the seed lock, polling so that a waiting run holds no transaction open, and reports
// about every second that it waits.
func lock(ctx context.Context, pool *pgxpool.Pool, emit func(Event)) (*sessionLock, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: acquire a connection: %w", err)
	}
	start := time.Now()
	reported := -lockWaitReport
	for {
		var got bool
		if err := conn.QueryRow(ctx, tryLockSQL, lockKey).Scan(&got); err != nil {
			conn.Release()
			return nil, fmt.Errorf("seed: take the seed lock: %w", err)
		}
		if got {
			emit(Event{Kind: LockAcquired, Duration: time.Since(start)})
			return &sessionLock{conn: conn}, nil
		}
		if waited := time.Since(start); waited-reported >= lockWaitReport {
			reported = waited
			emit(Event{Kind: LockWait, Duration: waited, Holder: holder(ctx, conn)})
		}
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, fmt.Errorf("seed: wait for the seed lock: %w", ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// holder describes the backend holding the seed lock, or returns nil when it cannot be read.
func holder(ctx context.Context, conn *pgxpool.Conn) *Backend {
	b := &Backend{}
	err := conn.QueryRow(ctx, holderSQL, lockKey).Scan(&b.PID, &b.Application, &b.State, &b.Query, &b.QueryStart)
	if err != nil {
		return nil
	}
	return b
}

// unlock releases the lock and the connection. A connection whose unlock fails is closed, which
// releases the lock with the session.
func (l *sessionLock) unlock(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if _, err := l.conn.Exec(ctx, unlockSQL, lockKey); err != nil {
		_ = l.conn.Hijack().Close(ctx)
		return
	}
	l.conn.Release()
}
