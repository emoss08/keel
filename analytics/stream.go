package analytics

import (
	"context"
	"io"

	"github.com/emoss08/keel/analytics/table"
)

// Stream is a report's rows read as they arrive: Run starts the statement, and each Next decodes
// the following batch on the run's connection, so an export of any size holds one batch in memory.
// It implements table.Reader; a batch is valid until the next call to Next. Close stops the
// statement and releases the connection and the tenant's slot; reading to io.EOF does it too.
type Stream struct {
	cols   []table.Column
	want   chan struct{}
	items  chan *table.Batch
	done   chan struct{}
	cancel context.CancelFunc
	cached table.Reader

	err       error
	truncated bool
	rollup    string
}

// Columns returns the output schema.
func (s *Stream) Columns() []table.Column { return s.cols }

// Next returns the next batch, or io.EOF after the last one.
func (s *Stream) Next(ctx context.Context) (*table.Batch, error) {
	if s.cached != nil {
		return s.cached.Next(ctx)
	}
	select {
	case s.want <- struct{}{}:
	case <-s.done:
		return nil, s.end()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case b, ok := <-s.items:
		if ok {
			return b, nil
		}
		<-s.done
		return nil, s.end()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// end returns the stream's final error, or io.EOF.
func (s *Stream) end() error {
	if s.err != nil {
		return s.err
	}
	return io.EOF
}

// Truncated reports, after io.EOF, that the result was cut at the row or byte cap.
func (s *Stream) Truncated() bool {
	if s.cached != nil {
		return s.cached.Truncated()
	}
	return s.truncated
}

// Rollup names the rollup table that answers the report, or "".
func (s *Stream) Rollup() string { return s.rollup }

// Close stops the statement if it is still running and waits for its transaction to end.
func (s *Stream) Close() error {
	if s.cached != nil {
		return nil
	}
	s.cancel()
	<-s.done
	return nil
}

// Run streams a report's rows, at most MaxRows (or the definition's limit if lower), in batches of
// StreamBatch rows. It returns once the statement has started (its guards passed and, for a
// dynamic pivot, its values discovered), so a refused or failed run is an error here; errors
// while streaming come from Next. A result of at most MaxCachedRows rows is cached as it streams.
func (r *Runner) Run(ctx context.Context, req Request) (*Stream, error) {
	comp, err := r.compile(ctx, req, CompileOptions{MaxRows: r.opts.MaxRows, Probe: true})
	if err != nil {
		return nil, err
	}
	key := r.cacheKeyOf(ctx, comp, "run", req.Format, "")
	if res := r.cachedResult(ctx, key); res != nil {
		return &Stream{cols: res.columns, cached: res.Reader(), rollup: res.rollup}, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Stream{want: make(chan struct{}), items: make(chan *table.Batch), done: make(chan struct{}),
		cancel: cancel, rollup: comp.Rollup}
	ready := make(chan *Compiled, 1)
	go r.produce(ctx, req, comp, key, s, ready)
	select {
	case comp, ok := <-ready:
		if !ok {
			<-s.done
			cancel()
			return nil, s.err
		}
		s.cols = comp.Columns
		return s, nil
	case <-s.done:
		cancel()
		return nil, s.err
	}
}

// produce runs a streamed report: it signals ready with the compiled report once the statement
// starts, hands each batch over when the consumer asks for it, and stops at the row and byte caps.
func (r *Runner) produce(ctx context.Context, req Request, comp *Compiled, key string, s *Stream,
	ready chan *Compiled,
) {
	defer close(s.done)
	var kept []*table.Batch
	var final *Compiled
	cacheable := key != ""
	err := r.execute(ctx, req, comp, r.opts.Timeouts.Run, func(ctx context.Context, comp *Compiled) error {
		final = comp
		ready <- comp
		rows, size := 0, int64(0)
		for res, err := range comp.Query.Stream(ctx, r.db, r.opts.StreamBatch) {
			if err != nil {
				return err
			}
			limit := 0
			if comp.Limit > 0 {
				if limit = comp.Limit - rows; limit == 0 {
					s.truncated = res.Rows > 0
					return nil
				}
			}
			b, truncated, err := comp.batchOf(res, limit)
			if err != nil {
				return err
			}
			if size += batchBytes(b); r.opts.MaxBytes > 0 && size > r.opts.MaxBytes {
				s.truncated = true
				return nil
			}
			if b.Len > 0 {
				select {
				case <-s.want:
				case <-ctx.Done():
					return ctx.Err()
				}
				s.items <- b
			}
			rows += b.Len
			cacheable = cacheable && rows <= r.opts.MaxCachedRows
			if cacheable {
				kept = append(kept, b)
			}
			if truncated {
				s.truncated = true
				return nil
			}
		}
		return nil
	})
	if final == nil {
		close(ready)
	}
	s.err = err
	close(s.items)
	if err == nil && cacheable {
		r.storeResult(context.WithoutCancel(ctx), key, &Result{columns: final.Columns, batches: kept,
			truncated: s.truncated, rollup: final.Rollup})
	}
}
