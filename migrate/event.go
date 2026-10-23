package migrate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventKind is what an Event reports.
type EventKind uint8

const (
	// LockWait is sent about every second while another run holds the migration lock.
	LockWait EventKind = iota
	// LockAcquired is sent when the run holds the migration lock; Duration is the time waited.
	LockAcquired
	// MigrationStart is sent before a migration runs.
	MigrationStart
	// Statement is sent before each statement of a notx migration.
	Statement
	// Retry is sent before a migration or notx statement is retried after a lock timeout.
	Retry
	// MigrationEnd is sent after a migration committed or failed.
	MigrationEnd
	// BaselineMarked is sent when a baseline is recorded as applied without running.
	BaselineMarked
	// After is sent before each Options.After statement runs.
	After
	// RunEnd is sent when Up returns.
	RunEnd
)

// eventNames are the names of the event kinds.
var eventNames = [...]string{
	"lock_wait", "lock_acquired", "migration_start", "statement", "retry", "migration_end",
	"baseline_marked", "after", "run_end",
}

// String returns the kind's snake_case name, such as migration_start.
func (k EventKind) String() string {
	if int(k) < len(eventNames) {
		return eventNames[k]
	}
	return fmt.Sprintf("EventKind(%d)", k)
}

// MarshalText encodes the kind as its name.
func (k EventKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// Event is one step of a run, sent to Options.Observer.
type Event struct {
	Kind       EventKind
	Migration  *Migration
	Index      int
	Total      int
	Statement  int
	Statements int
	Attempt    int
	Blockers   []Blocker
	Duration   time.Duration
	Err        error
	// SQL is the statement a Statement or After event is about to run.
	SQL string
}

// Blocker is a backend holding a lock the run waits for.
type Blocker struct {
	PID         int32
	Application string
	State       string
	Mode        string
	Age         time.Duration
	Query       string
}

// String describes the blocker on one line.
func (b Blocker) String() string {
	var parts []string
	for _, p := range []string{b.Application, b.State, b.Mode} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	parts = append(parts, b.Age.Round(time.Millisecond).String())
	q := strings.Join(strings.Fields(b.Query), " ")
	if len(q) > 120 {
		q = q[:117] + "..."
	}
	s := fmt.Sprintf("pid %d (%s)", b.PID, strings.Join(parts, ", "))
	if q != "" {
		s += ": " + q
	}
	return s
}

// blockerColumns selects a Blocker's fields from pg_stat_activity a; $1 is the waiting backend
// for the lock mode.
const blockerColumns = `a.pid, coalesce(a.application_name, ''), coalesce(a.state, ''),
	coalesce(extract(epoch FROM now() - coalesce(a.xact_start, a.query_start, a.backend_start)), 0)::float8,
	coalesce(a.query, '')`

// advisoryBlockersSQL lists the backends holding the migration advisory lock.
const advisoryBlockersSQL = `SELECT ` + blockerColumns + `, l.mode
FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
WHERE l.locktype = 'advisory' AND l.granted AND l.pid <> pg_backend_pid()
	AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
	AND l.classid::int8 = $1 AND l.objid::int8 = $2 AND l.objsubid = 1
ORDER BY a.pid`

// relationBlockersSQL lists the backends blocking backend $1, with the modes they hold on the
// object it waits for.
const relationBlockersSQL = `SELECT ` + blockerColumns + `,
	coalesce((SELECT string_agg(DISTINCT h.mode, ',') FROM pg_locks h JOIN pg_locks w
		ON w.pid = $1 AND NOT w.granted AND h.locktype = w.locktype
		AND h.database IS NOT DISTINCT FROM w.database AND h.relation IS NOT DISTINCT FROM w.relation
		AND h.page IS NOT DISTINCT FROM w.page AND h.tuple IS NOT DISTINCT FROM w.tuple
		AND h.virtualxid IS NOT DISTINCT FROM w.virtualxid
		AND h.transactionid IS NOT DISTINCT FROM w.transactionid
		AND h.classid IS NOT DISTINCT FROM w.classid AND h.objid IS NOT DISTINCT FROM w.objid
		AND h.objsubid IS NOT DISTINCT FROM w.objsubid
		WHERE h.pid = a.pid AND h.granted), '')
FROM pg_stat_activity a
WHERE a.pid = ANY (pg_blocking_pids($1))
ORDER BY a.pid`

// queryBlockers runs a blocker query.
func queryBlockers(ctx context.Context, conn *pgx.Conn, sql string, args ...any) ([]Blocker, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Blocker
	for rows.Next() {
		var b Blocker
		var age float64
		if err := rows.Scan(&b.PID, &b.Application, &b.State, &age, &b.Query, &b.Mode); err != nil {
			return nil, err
		}
		b.Age = time.Duration(age * float64(time.Second)).Round(time.Millisecond)
		out = append(out, b)
	}
	return out, rows.Err()
}

// advisoryBlockers lists the backends holding the migration lock.
func advisoryBlockers(ctx context.Context, conn *pgx.Conn) ([]Blocker, error) {
	key := uint64(lockKey)
	return queryBlockers(ctx, conn, advisoryBlockersSQL, int64(key>>32), int64(key&0xffffffff))
}

// watcher finds the backends blocking the run's connection while one of its statements waits,
// from a second connection opened on first use.
type watcher struct {
	cfg  *pgx.ConnConfig
	pid  int32
	conn *pgx.Conn
}

// newWatcher returns a watcher for conn, the run's connection.
func newWatcher(conn *pgx.Conn) *watcher {
	cfg := conn.Config()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = "keel migrate monitor"
	return &watcher{cfg: cfg, pid: int32(conn.PgConn().PID())}
}

// watch probes for blockers every interval until the returned stop is called, which returns the
// last non-empty set found.
func (w *watcher) watch(ctx context.Context, interval time.Duration) func() []Blocker {
	done := make(chan struct{})
	var wg sync.WaitGroup
	var found []Blocker
	wg.Go(func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if bs := w.probe(ctx); len(bs) > 0 {
				found = bs
			}
		}
	})
	return func() []Blocker {
		close(done)
		wg.Wait()
		return found
	}
}

// probe queries the blockers once, reconnecting when needed; it ignores errors, since blockers
// only explain a wait.
func (w *watcher) probe(ctx context.Context) []Blocker {
	if w.conn == nil || w.conn.IsClosed() {
		c, err := pgx.ConnectConfig(ctx, w.cfg)
		if err != nil {
			return nil
		}
		w.conn = c
	}
	bs, _ := queryBlockers(ctx, w.conn, relationBlockersSQL, w.pid)
	return bs
}

// close closes the watcher's connection.
func (w *watcher) close() {
	if w.conn != nil {
		_ = w.conn.Close(context.Background())
	}
}

// probeInterval is how often a waiting statement is probed for blockers: twice per lock timeout,
// at most every 250ms.
func probeInterval(lockTimeout time.Duration) time.Duration {
	return max(min(lockTimeout/2, 250*time.Millisecond), 10*time.Millisecond)
}

// describeBlockers joins blockers for an error message.
func describeBlockers(bs []Blocker) string {
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = b.String()
	}
	return strings.Join(parts, "; ")
}
