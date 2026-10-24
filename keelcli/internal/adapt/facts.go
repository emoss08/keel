package adapt

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/emoss08/keel/keelcli/factsport"
)

// Facts implements factsport.Facts over the application's connection pool.
type Facts struct {
	Environments
}

// Facts implements every facts port.
var _ factsport.Facts = Facts{}

// Connection connects, timing one round trip, and reads the server version.
func (x Facts) Connection(ctx context.Context) (factsport.Connection, error) {
	db, err := x.DB.DB(ctx)
	if err != nil {
		return factsport.Connection{}, err
	}
	pool := db.Pool()
	cfg := pool.Config().ConnConfig
	out := factsport.Connection{
		DSN: pool.Config().ConnString(), Database: cfg.Database, Host: cfg.Host, Port: int(cfg.Port), User: cfg.User,
	}
	if err := pool.Ping(ctx); err != nil {
		return out, fmt.Errorf("keelcli: connect: %w", err)
	}
	start := time.Now()
	err = pool.QueryRow(ctx, "SELECT current_setting('server_version'), current_setting('server_version_num')::int").
		Scan(&out.ServerVersion, &out.ServerNum)
	out.Latency = time.Since(start)
	if err != nil {
		return out, fmt.Errorf("keelcli: read the server version: %w", err)
	}
	return out, nil
}

// Settings returns the named settings as the application's connections see them, or every
// setting when none is named.
func (x Facts) Settings(ctx context.Context, names ...string) ([]factsport.Setting, error) {
	db, err := x.DB.DB(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.Pool().Query(ctx, `SELECT name, setting FROM pg_settings
WHERE cardinality($1::text[]) = 0 OR name = ANY ($1::text[]) ORDER BY name`, append([]string{}, names...))
	if err != nil {
		return nil, fmt.Errorf("keelcli: read settings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (factsport.Setting, error) {
		var s factsport.Setting
		return s, row.Scan(&s.Name, &s.Value)
	})
	if err != nil {
		return nil, fmt.Errorf("keelcli: read settings: %w", err)
	}
	return out, nil
}

// Extensions lists the installed extensions.
func (x Facts) Extensions(ctx context.Context) ([]factsport.Extension, error) {
	db, err := x.DB.DB(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.Pool().Query(ctx, "SELECT extname, extversion FROM pg_extension ORDER BY extname")
	if err != nil {
		return nil, fmt.Errorf("keelcli: read extensions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (factsport.Extension, error) {
		var e factsport.Extension
		return e, row.Scan(&e.Name, &e.Version)
	})
	if err != nil {
		return nil, fmt.Errorf("keelcli: read extensions: %w", err)
	}
	return out, nil
}

// activitySQL lists the client backends of the current database other than this one, with the
// lock each holds that another waits for, or waits for itself, and the backends it waits on.
const activitySQL = `WITH backends AS (
	SELECT a.*, pg_blocking_pids(a.pid) AS blocked_by
	FROM pg_stat_activity a
	WHERE a.datname = current_database() AND a.pid <> pg_backend_pid() AND a.backend_type = 'client backend'
), holders AS (
	SELECT DISTINCT unnest(blocked_by) AS pid FROM backends
)
SELECT b.pid, coalesce(b.application_name, ''), coalesce(b.usename::text, ''), coalesce(b.state, ''),
	coalesce(extract(epoch FROM now() - coalesce(b.xact_start, b.query_start, b.backend_start)), 0)::float8,
	coalesce(b.query, ''), cardinality(b.blocked_by) > 0, b.blocked_by, b.pid IN (SELECT pid FROM holders),
	coalesce(l.mode, ''), coalesce(l.relation::regclass::text, l.locktype, '')
FROM backends b
LEFT JOIN LATERAL (
	SELECT h.mode, h.relation, h.locktype FROM pg_locks h
	WHERE h.pid = b.pid AND (
		(NOT h.granted) OR EXISTS (
			SELECT 1 FROM pg_locks w WHERE NOT w.granted AND w.pid <> b.pid AND w.locktype = h.locktype
				AND w.database IS NOT DISTINCT FROM h.database AND w.relation IS NOT DISTINCT FROM h.relation
				AND w.page IS NOT DISTINCT FROM h.page AND w.tuple IS NOT DISTINCT FROM h.tuple
				AND w.virtualxid IS NOT DISTINCT FROM h.virtualxid
				AND w.transactionid IS NOT DISTINCT FROM h.transactionid
				AND w.classid IS NOT DISTINCT FROM h.classid AND w.objid IS NOT DISTINCT FROM h.objid
				AND w.objsubid IS NOT DISTINCT FROM h.objsubid))
	ORDER BY h.granted
	LIMIT 1
) l ON true
ORDER BY b.pid`

// backendRow is one backend and whether others wait on a lock it holds.
type backendRow struct {
	backend factsport.Backend
	holder  bool
}

// Activity lists the backends holding locks others wait for, and the queries running longer than
// longer.
func (x Facts) Activity(ctx context.Context, longer time.Duration) (factsport.Activity, error) {
	db, err := x.DB.DB(ctx)
	if err != nil {
		return factsport.Activity{}, err
	}
	rows, err := db.Pool().Query(ctx, activitySQL)
	if err != nil {
		return factsport.Activity{}, fmt.Errorf("keelcli: read activity: %w", err)
	}
	all, err := pgx.CollectRows(rows, scanBackend)
	if err != nil {
		return factsport.Activity{}, fmt.Errorf("keelcli: read activity: %w", err)
	}
	var out factsport.Activity
	for _, r := range all {
		if r.holder {
			out.LockHolders = append(out.LockHolders, r.backend)
		}
		if r.backend.State != "idle" && r.backend.Age >= longer {
			out.LongQueries = append(out.LongQueries, r.backend)
		}
	}
	return out, nil
}

// scanBackend scans one row of activitySQL.
func scanBackend(row pgx.CollectableRow) (backendRow, error) {
	var r backendRow
	var age float64
	b := &r.backend
	err := row.Scan(&b.PID, &b.Application, &b.User, &b.State, &age, &b.Query, &b.Waiting, &b.BlockedBy, &r.holder,
		&b.Mode, &b.Relation)
	b.Age = time.Duration(age * float64(time.Second)).Round(time.Millisecond)
	return r, err
}
