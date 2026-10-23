package keel_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// span is what a tracing observer keeps for one statement.
type span struct {
	sql    string
	parent *span
	ended  bool
}

type spanKey struct{}

// tracer is an observer that starts a span in BeforeQuery and ends the span it finds on the
// context in AfterQuery. With cancel set, the context it returns is already canceled.
type tracer struct {
	mu      sync.Mutex
	spans   []*span
	missing []string
	cancel  bool
}

func (tr *tracer) BeforeQuery(ctx context.Context, e *keel.QueryEvent) context.Context {
	parent, _ := ctx.Value(spanKey{}).(*span)
	sp := &span{sql: e.SQL, parent: parent}
	tr.mu.Lock()
	tr.spans = append(tr.spans, sp)
	tr.mu.Unlock()
	ctx = context.WithValue(ctx, spanKey{}, sp)
	if tr.cancel {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c
	}
	return ctx
}

func (tr *tracer) AfterQuery(ctx context.Context, e *keel.QueryEvent) {
	sp, _ := ctx.Value(spanKey{}).(*span)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if sp == nil || sp.sql != e.SQL || sp.ended {
		tr.missing = append(tr.missing, e.SQL)
		return
	}
	sp.ended = true
}

// check fails the test unless every span was ended, from its own statement's context, and no span
// was opened inside another statement's span.
func (tr *tracer) check(t *testing.T, statements int) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.missing) > 0 {
		t.Fatalf("AfterQuery did not get the statement's context for %q", tr.missing)
	}
	eq(t, len(tr.spans), statements)
	for _, sp := range tr.spans {
		if !sp.ended {
			t.Fatalf("span of %q never ended", sp.sql)
		}
		if sp.parent != nil {
			t.Fatalf("span of %q started inside the span of %q", sp.sql, sp.parent.sql)
		}
	}
}

func TestObserverContextReachesAfterQuery(t *testing.T) {
	tr := &tracer{}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{tr} })
	ctx := newTenant()
	c := fleetdb.Customers

	cust := &fleet.Customer{Name: "Traced", Code: "T1"}
	ok(t, c.Create(ctx, d, cust))
	must(c.Get(ctx, d, cust.ID))(t)

	var rows []*fleet.Customer
	var n int64
	ok(t, keel.Batch(ctx, d,
		keel.ListInto(c.Where(c.Code.Eq("T1")).Limit(5), &rows),
		keel.CountInto(c.Query(), &n),
	))
	eq(t, n, int64(1))

	copied := must(c.Copy(ctx, d, []*fleet.Customer{{Name: "Copied", Code: "T2"}}))(t)
	eq(t, copied, int64(1))

	ok(t, keel.Write(ctx, d, func(ctx context.Context) error {
		return c.Save(ctx, d, cust)
	}))

	tr.check(t, 6)
}

func TestObserverContextReachesExecution(t *testing.T) {
	tr := &tracer{cancel: true}
	d := openDB(t, func(cfg *keel.Config) { cfg.Observers = []keel.Observer{tr} })
	ctx := newTenant()
	c := fleetdb.Customers

	if err := c.Create(ctx, d, &fleet.Customer{Name: "Canceled", Code: "X1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("lone statement: got %v, want context.Canceled", err)
	}
	if _, err := c.Copy(ctx, d, []*fleet.Customer{{Name: "Canceled", Code: "X2"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy: got %v, want context.Canceled", err)
	}

	var n int64
	var exists bool
	ok(t, keel.Batch(ctx, d, keel.CountInto(c.Query(), &n), keel.ExistsInto(c.Query(), &exists)))
	eq(t, n, int64(0))
}
