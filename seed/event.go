package seed

import (
	"time"
)

// EventKind says what an Event reports.
type EventKind uint8

const (
	// Planned reports the plan of the run, in Event.Plan, once the run holds the lock.
	Planned EventKind = iota + 1
	// LockWait reports, about every second, that another process holds the seed lock. Duration is
	// the time waited so far and Holder the backend holding the lock.
	LockWait
	// LockAcquired reports that the run holds the seed lock.
	LockAcquired
	// SeedStart reports that a seed starts, with the Reason it runs.
	SeedStart
	// TenantStart reports that one tenant of a PerTenant seed starts.
	TenantStart
	// TenantEnd reports that one tenant of a PerTenant seed finished, with its rows, duration and
	// error.
	TenantEnd
	// SeedEnd reports that a seed finished, with its rows, duration and error.
	SeedEnd
	// RunEnd reports the end of the run, with its duration and error. It is always the last event.
	RunEnd
)

// String returns the kind's name in lower case.
func (k EventKind) String() string {
	switch k {
	case Planned:
		return "planned"
	case LockWait:
		return "lock wait"
	case LockAcquired:
		return "lock acquired"
	case SeedStart:
		return "seed start"
	case TenantStart:
		return "tenant start"
	case TenantEnd:
		return "tenant end"
	case SeedEnd:
		return "seed end"
	case RunEnd:
		return "run end"
	}
	return "unknown"
}

// Event reports the progress of an Apply to Options.Observer.
type Event struct {
	Kind EventKind
	// Seed is the seed the event is about; nil for Planned, lock and run events.
	Seed *Seed
	// Reason is why the seed runs (SeedStart, TenantStart, TenantEnd, SeedEnd).
	Reason Reason
	// Index is the seed's 0-based position among the seeds this run executes.
	Index int
	// Total is the number of seeds this run executes.
	Total int
	// Tenant is the tenant's key (TenantStart, TenantEnd).
	Tenant string
	// Tenants is the number of tenants a PerTenant seed runs for, excluding tenants already
	// applied with the current fingerprint.
	Tenants int
	// TenantsDone is the number of those tenants finished so far.
	TenantsDone int
	// Rows is the number of rows reported through Run.Count (SeedEnd, TenantEnd).
	Rows int64
	// Duration is the time taken (SeedEnd, TenantEnd, RunEnd) or waited so far (LockWait).
	Duration time.Duration
	// Err is the failure (SeedEnd, TenantEnd, RunEnd).
	Err error
	// Plan is the run's plan (Planned).
	Plan *Plan
	// Holder is the backend holding the seed lock (LockWait), when Postgres reports it.
	Holder *Backend
}

// Backend describes a Postgres backend, from pg_stat_activity.
type Backend struct {
	// PID is the backend's process ID.
	PID int32
	// Application is the backend's application_name.
	Application string
	// State is the backend's state: active, idle, idle in transaction.
	State string
	// Query is the backend's current or last statement.
	Query string
	// QueryStart is when that statement started, or when the backend connected.
	QueryStart time.Time
}
