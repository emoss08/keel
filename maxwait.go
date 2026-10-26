package keel

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errWaitLimit is the cause of a context that Config.MaxWait ended.
var errWaitLimit = errors.New("keel: Config.MaxWait reached")

// boundWait returns ctx ended after limit as well, or ctx itself when limit is zero.
func boundWait(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if limit <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, limit, errWaitLimit)
}

// waitErr returns the error of a wait for what that wctx ended: ErrWaitLimit when Config.MaxWait
// ended it, else the caller's context error.
func waitErr(wctx context.Context, what string, limit time.Duration) error {
	if context.Cause(wctx) == errWaitLimit {
		return fmt.Errorf("%w: %s, after %v", ErrWaitLimit, what, limit)
	}
	return fmt.Errorf("keel: wait for %s: %w", what, wctx.Err())
}
