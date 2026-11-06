// Package retry holds the waiting that daemons dialling out to forecrewd
// share.
package retry

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff returns the delay before retry n (1, 2, ...): lo doubling up to hi,
// with the upper half jittered.
func Backoff(n int, lo, hi time.Duration) time.Duration {
	d := lo
	for range n - 1 {
		if d >= hi {
			break
		}
		d *= 2
	}
	d = min(d, hi)
	return d/2 + rand.N(d/2+1) //nolint:gosec // jitter, not security
}

// Sleep waits for d or until ctx ends, whose error it returns.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
