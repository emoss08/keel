package fake

import (
	"context"
	"time"

	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/seedport"
)

var settings = map[string]string{
	"plan_cache_mode":                     "auto",
	"file_copy_method":                    "clone",
	"lock_timeout":                        "0",
	"statement_timeout":                   "0",
	"idle_in_transaction_session_timeout": "0",
	"max_connections":                     "200",
	"shared_buffers":                      "4GB",
	"server_version":                      "18.1",
}

// Connection describes a local development database.
func (f Facts) Connection(context.Context) (factsport.Connection, error) {
	return factsport.Connection{
		DSN:           "postgres://trenova:s3cret@localhost:5432/trenova?sslmode=disable",
		Database:      "trenova",
		Host:          "localhost",
		Port:          5432,
		User:          "trenova",
		ServerVersion: "18.1",
		ServerNum:     180001,
		Latency:       420 * time.Microsecond,
	}, nil
}

// Settings returns the named settings, or every simulated one when none is named.
func (f Facts) Settings(_ context.Context, names ...string) ([]factsport.Setting, error) {
	if len(names) == 0 {
		for name := range settings {
			names = append(names, name)
		}
	}
	out := make([]factsport.Setting, 0, len(names))
	for _, name := range names {
		if v, ok := settings[name]; ok {
			out = append(out, factsport.Setting{Name: name, Value: v})
		}
	}
	return out, nil
}

// Extensions lists the installed extensions.
func (f Facts) Extensions(context.Context) ([]factsport.Extension, error) {
	return []factsport.Extension{
		{Name: "plpgsql", Version: "1.0"},
		{Name: "pg_trgm", Version: "1.6"},
		{Name: "btree_gin", Version: "1.3"},
		{Name: "pgcrypto", Version: "1.3"},
		{Name: "pg_stat_statements", Version: "1.11"},
	}, nil
}

// Activity reports a long report query holding a lock that a dashboard query waits on; ages grow
// with the fake's clock.
func (f Facts) Activity(_ context.Context, longer time.Duration) (factsport.Activity, error) {
	since := f.e.opts.Now().Sub(f.e.created)
	report := factsport.Backend{
		PID: 48121, Application: "report-runner", User: "reports", State: "active",
		Mode: "AccessShareLock", Relation: "shipments", Age: 4*time.Minute + 12*time.Second + since,
		Query: "SELECT s.pro_number, sum(m.distance) FROM shipments s JOIN moves m ON m.shipment_id = s.id WHERE s.organization_id = $1 GROUP BY 1 ORDER BY 2 DESC",
	}
	idle := factsport.Backend{
		PID: 48377, Application: "trenova-api", User: "trenova", State: "idle in transaction",
		Mode: "RowExclusiveLock", Relation: "shipments", Age: 38*time.Second + since,
		Query: "UPDATE shipments SET status = $1 WHERE id = $2",
	}
	waiter := factsport.Backend{
		PID: 49002, Application: "metabase", User: "bi", State: "active", Mode: "AccessExclusiveLock",
		Relation: "shipments", Age: 12*time.Second + since, Waiting: true, BlockedBy: []int32{48121, 48377},
		Query: "LOCK TABLE shipments IN ACCESS EXCLUSIVE MODE",
	}
	out := factsport.Activity{LockHolders: []factsport.Backend{report, idle}}
	for _, b := range []factsport.Backend{report, idle, waiter} {
		if b.Age >= longer {
			out.LongQueries = append(out.LongQueries, b)
		}
	}
	return out, nil
}

// Environment reads the database's environment mark.
func (f Facts) Environment(context.Context) (seedport.Environment, error) {
	f.e.mu.Lock()
	defer f.e.mu.Unlock()
	return f.e.env, nil
}
