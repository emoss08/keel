package render

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

const queryWidth = 120

// Lines writes one human line per run event, for logs and pipes. Per-tenant progress prints at
// each tenth of the tenants, so a seed over thousands of tenants prints ten lines, not thousands.
// It is safe for concurrent use.
type Lines struct {
	mu      sync.Mutex
	w       io.Writer
	deciles map[string]int
}

// NewLines writes event lines to w.
func NewLines(w io.Writer) *Lines {
	return &Lines{w: w, deciles: map[string]int{}}
}

func (l *Lines) printf(prefix, format string, args ...any) {
	fmt.Fprintf(l.w, "%-8s%s\n", prefix, fmt.Sprintf(format, args...))
}

// Migration writes the lines of one migration event.
func (l *Lines) Migration(e migrateport.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch e.Kind {
	case migrateport.LockWait:
		if e.Duration > 0 {
			l.printf("lock", "waiting for the migration lock (attempt %d, %s)", e.Attempt, Duration(e.Duration))
		} else {
			l.printf("lock", "waiting for the migration lock (attempt %d)", e.Attempt)
		}
		l.blockers(e.Blockers)
	case migrateport.LockAcquired:
		l.printf("lock", "migration lock acquired after %s", Duration(e.Duration))
	case migrateport.MigrationStart:
		detail := e.Migration.Source
		if d := MigrationDetail(*e.Migration); d != "" {
			detail += " · " + d
		}
		l.printf("migrate", "%s %s  %s", step(e.Index, e.Total), e.Migration.ID(), detail)
	case migrateport.Statement:
		l.printf("migrate", "%s %s  statement %d/%d", step(e.Index, e.Total), e.Migration.ID(), e.Statement, e.Statements)
	case migrateport.Retry:
		l.printf("migrate", "%s %s  lock timeout, retry %d: %v", step(e.Index, e.Total), e.Migration.ID(), e.Attempt, e.Err)
		l.blockers(e.Blockers)
	case migrateport.MigrationEnd:
		if e.Err != nil {
			l.printf("migrate", "%s %s  %s after %s: %v", step(e.Index, e.Total), e.Migration.ID(), outcome(e.Err), Duration(e.Duration), e.Err)
			return
		}
		l.printf("migrate", "%s %s  applied in %s", step(e.Index, e.Total), e.Migration.ID(), Duration(e.Duration))
	case migrateport.BaselineMarked:
		l.printf("migrate", "baseline %s recorded as applied", e.Migration.ID())
	case migrateport.After:
		l.printf("migrate", "after statement %d/%d in %s", e.Statement, e.Statements, Duration(e.Duration))
	case migrateport.RunEnd:
		if e.Err != nil {
			l.printf("migrate", "stopped after %s, %s applied: %v", Duration(e.Duration), Plural(e.Total, "migration", "migrations"), e.Err)
			return
		}
		l.printf("migrate", "done: %s applied in %s", Plural(e.Total, "migration", "migrations"), Duration(e.Duration))
	}
}

func (l *Lines) blockers(bs []migrateport.Blocker) {
	for _, b := range bs {
		app := b.Application
		if app == "" {
			app = "(no application)"
		}
		l.printf("", "  blocked by pid %d %s, %s for %s, %s: %s", b.PID, app, b.State, Duration(b.Age), b.Mode,
			Fit(OneLine(b.Query), queryWidth))
	}
}

// Seed writes the lines of one seed event.
func (l *Lines) Seed(e seedport.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch e.Kind {
	case seedport.Planned:
		verb := "skips"
		if e.Reason.Runs() {
			verb = "runs"
		}
		l.printf("seed", "%s %s: %s", e.Seed.Name, verb, Reason(e.Reason))
	case seedport.LockWait:
		l.printf("lock", "waiting for the seed lock")
	case seedport.LockAcquired:
		l.printf("lock", "seed lock acquired after %s", Duration(e.Duration))
	case seedport.SeedStart:
		detail := e.Seed.Kind.String()
		if e.Tenants > 0 {
			detail += " · " + Plural(e.Tenants, "tenant", "tenants")
		}
		l.deciles[e.Seed.Name] = 0
		l.printf("seed", "%s %s  %s", step(e.Index, e.Total), e.Seed.Name, detail)
	case seedport.TenantEnd:
		l.tenant(e)
	case seedport.SeedEnd:
		if e.Err != nil {
			l.printf("seed", "%s %s  %s after %s: %v", step(e.Index, e.Total), e.Seed.Name, outcome(e.Err), Duration(e.Duration), e.Err)
			return
		}
		l.printf("seed", "%s %s  %s in %s", step(e.Index, e.Total), e.Seed.Name, Plural(int(e.Rows), "row", "rows"), Duration(e.Duration))
	case seedport.RunEnd:
		if e.Err != nil {
			l.printf("seed", "stopped after %s, %s applied: %v", Duration(e.Duration), Plural(e.Total, "seed", "seeds"), e.Err)
			return
		}
		l.printf("seed", "done: %s applied in %s", Plural(e.Total, "seed", "seeds"), Duration(e.Duration))
	}
}

func (l *Lines) tenant(e seedport.Event) {
	if e.Err != nil {
		if strings.Contains(e.Err.Error(), e.Tenant) {
			l.printf("seed", "%s %s  %v", step(e.Index, e.Total), e.Seed.Name, e.Err)
			return
		}
		l.printf("seed", "%s %s  tenant %s failed: %v", step(e.Index, e.Total), e.Seed.Name, e.Tenant, e.Err)
		return
	}
	if e.Tenants <= 0 {
		return
	}
	decile := e.TenantsDone * 10 / e.Tenants
	if decile <= l.deciles[e.Seed.Name] {
		return
	}
	l.deciles[e.Seed.Name] = decile
	l.printf("seed", "%s %s  tenants %s/%s", step(e.Index, e.Total), e.Seed.Name,
		Count(int64(e.TenantsDone)), Count(int64(e.Tenants)))
}

func outcome(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "failed"
}

func step(i, n int) string {
	if n == 0 {
		return ""
	}
	w := len(fmt.Sprint(n))
	return "[" + strings.Repeat(" ", w-len(fmt.Sprint(i))) + fmt.Sprintf("%d/%d]", i, n)
}
