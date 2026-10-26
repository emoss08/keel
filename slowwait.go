package keel

import (
	"context"
	"runtime"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SlowWaitConfig reports work that waits longer than After for a tenant slot
// (Config.TenantConcurrency), a pool connection, or another goroutine's read of the result cache
// entry it needs, so a starved tenant or an exhausted pool is visible while it happens rather than
// as requests that hang. Report is called once per wait that crosses After, from the waiting
// goroutine or a timer's; it must return quickly and must not run statements on the DB. Both
// fields must be set for keel to watch: it then records the call stack of every tenant slot it
// hands out and of every connection wait, about a microsecond each.
type SlowWaitConfig struct {
	After  time.Duration
	Report func(ctx context.Context, w *SlowWait)
}

// WaitKind is what a slow wait waited for.
type WaitKind uint8

// The kinds of wait SlowWaitConfig reports.
const (
	// WaitTenantSlot is a terminal or transaction waiting for one of its tenant's slots.
	WaitTenantSlot WaitKind = iota + 1
	// WaitConnection is a terminal, transaction or version read waiting for a pool connection.
	WaitConnection
	// WaitCacheFill is a terminal waiting for another goroutine's read of the same result cache
	// entry.
	WaitCacheFill
)

// String returns the kind's name: tenant_slot, connection or cache_fill.
func (k WaitKind) String() string {
	switch k {
	case WaitTenantSlot:
		return "tenant_slot"
	case WaitConnection:
		return "connection"
	case WaitCacheFill:
		return "cache_fill"
	}
	return "wait_" + strconv.Itoa(int(k))
}

// SlowWait describes a wait that crossed SlowWaitConfig.After. Stack is the waiting caller's, from
// its first frame outside keel, as "function file:line". For a tenant slot, Limit is the tenant's
// bound, Waiters counts the work waiting for its slots, this one included, and Holders lists the
// work holding them: how long each has held its slot and the stack that took it. For a connection,
// Pool is the pool's state when the report was made.
type SlowWait struct {
	Kind    WaitKind
	Tenant  string
	Waited  time.Duration
	Stack   []string
	Limit   int
	Waiters int
	Holders []SlotHolder
	Pool    PoolState
}

// SlotHolder is work holding a tenant slot: how long it has held it and the stack that took it.
type SlotHolder struct {
	Held  time.Duration
	Stack []string
}

// PoolState is a connection pool's state: connections checked out, idle, open and the most it
// opens.
type PoolState struct {
	Acquired int32
	Idle     int32
	Total    int32
	Max      int32
}

// slowWaits is the DB's SlowWaitConfig when both of its fields are set.
type slowWaits struct {
	after  time.Duration
	report func(ctx context.Context, w *SlowWait)
}

func newSlowWaits(cfg SlowWaitConfig) *slowWaits {
	if cfg.After <= 0 || cfg.Report == nil {
		return nil
	}
	return &slowWaits{after: cfg.After, report: cfg.Report}
}

// stackDepth bounds the frames a recorded stack keeps.
const stackDepth = 32

// callers records the stack of its caller's caller.
func callers() []uintptr {
	pcs := make([]uintptr, stackDepth)
	return pcs[:runtime.Callers(3, pcs)]
}

// frames formats pcs as "function file:line", from the first frame outside keel and generated
// packages (internalFrame).
func frames(pcs []uintptr) []string {
	if len(pcs) == 0 {
		return nil
	}
	it := runtime.CallersFrames(pcs)
	out := make([]string, 0, len(pcs))
	outside := false
	for {
		f, more := it.Next()
		if !outside && f.Function != "" && !internalFrame(f.Function) {
			outside = true
		}
		if outside && f.Function != "" {
			out = append(out, f.Function+" "+f.File+":"+strconv.Itoa(f.Line))
		}
		if !more {
			break
		}
	}
	return out
}

// acquire takes a connection from pool, reporting a wait that crosses SlowWaitConfig.After with
// the caller's stack, the tenant of sc and the pool's state.
func (db *DB) acquire(ctx context.Context, pool *pgxpool.Pool, sc *scopeState) (*pgxpool.Conn, error) {
	sw := db.slow
	if sw == nil {
		return pool.Acquire(ctx)
	}
	pcs := callers()
	start := time.Now()
	timer := time.AfterFunc(sw.after, func() {
		st := pool.Stat()
		sw.report(ctx, &SlowWait{
			Kind:   WaitConnection,
			Tenant: sc.tenantKey(),
			Waited: time.Since(start),
			Stack:  frames(pcs),
			Pool: PoolState{
				Acquired: st.AcquiredConns(),
				Idle:     st.IdleConns(),
				Total:    st.TotalConns(),
				Max:      st.MaxConns(),
			},
		})
	})
	conn, err := pool.Acquire(ctx)
	timer.Stop()
	return conn, err
}

// tenantKey returns the key of the scope's tenant, or "" for a system scope or none.
func (sc *scopeState) tenantKey() string {
	if sc == nil || sc.tenant == nil {
		return ""
	}
	return TenantKeyOf(sc.tenant...)
}

// waitFill waits for another goroutine's read of a result cache entry, or for ctx, reporting a
// wait that crosses SlowWaitConfig.After with the caller's stack.
func (db *DB) waitFill(ctx context.Context, wait <-chan struct{}, tenant string) {
	sw := db.slow
	if sw == nil {
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return
	}
	pcs := callers()
	start := time.Now()
	timer := time.NewTimer(sw.after)
	defer timer.Stop()
	for {
		select {
		case <-wait:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			sw.report(ctx, &SlowWait{
				Kind:   WaitCacheFill,
				Tenant: tenant,
				Waited: time.Since(start),
				Stack:  frames(pcs),
			})
		}
	}
}
