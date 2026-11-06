package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	lo, hi := time.Second, 30*time.Second
	for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 5: 16 * time.Second, 6: hi, 50: hi} {
		for range 50 {
			if got := Backoff(n, lo, hi); got < want/2 || got > want {
				t.Fatalf("Backoff(%d) = %s, want %s to %s", n, got, want/2, want)
			}
		}
	}
}

func TestSleep(t *testing.T) {
	if err := Sleep(t.Context(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
