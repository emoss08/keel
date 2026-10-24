package analytics_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics"
)

// servers records the server of every statement a DB sends.
type servers struct {
	mu   sync.Mutex
	seen []string
}

func (s *servers) BeforeQuery(ctx context.Context, e *keel.QueryEvent) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Op == "select" {
		s.seen = append(s.seen, e.Replica)
	}
	return ctx
}

func (s *servers) AfterQuery(context.Context, *keel.QueryEvent) {}

func TestRunsOnTheReplica(t *testing.T) {
	cfg := replicaConfig(t)
	w := seedWorld(t)
	rec := &servers{}
	cfg.Observers = []keel.Observer{rec}
	d := openHealthy(t, cfg)
	r := must[*analytics.Runner](t)(analytics.NewRunner(d, catalog(t), analytics.RunnerOptions{Slots: 2, MaxCost: 1e9}))
	report := def(t, lanes)
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := r.Preview(w.ctx, analytics.Request{Definition: report, Key: "replica"})
		if err != nil {
			t.Fatal(err)
		}
		if got := rows(t, res); strings.Join(got, "\n") == strings.Join(laneRows, "\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the replica never caught up")
		}
		time.Sleep(100 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.seen) == 0 || rec.seen[len(rec.seen)-1] == "" {
		t.Fatalf("the report ran on %q", rec.seen)
	}
	primary := must[*analytics.Runner](t)(analytics.NewRunner(d, catalog(t), analytics.RunnerOptions{Primary: true}))
	rec.seen = rec.seen[:0]
	rec.mu.Unlock()
	if _, err := primary.Preview(w.ctx, analytics.Request{Definition: report}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	if len(rec.seen) == 0 || rec.seen[len(rec.seen)-1] != "" {
		t.Fatalf("a primary runner ran on %q", rec.seen)
	}
}
