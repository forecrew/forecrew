package fleetapi

import (
	"sync"
	"time"
)

// The limits on join (fleet-agent-api.md, Joining). A waiting agent calls
// every 5 s, so one address has room for many agents behind one router.
const (
	// guessRate is how many calls with an unknown code all addresses
	// together get per second. At that rate about 3 million guesses fit in
	// a week-long code's life, against 2^40 codes.
	guessRate = 5
	// addressRate and addressBurst limit join calls of one address.
	addressRate  = 2
	addressBurst = 20
	// maxAddresses bounds the memory the limiter can be made to use.
	maxAddresses = 10000
)

// bucket is a token bucket; the caller holds the lock.
type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) fill(now time.Time, rate, burst float64) {
	b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
	b.at = now
}

func (b *bucket) take(now time.Time, rate, burst float64) bool {
	b.fill(now, rate, burst)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// limiter holds the state of both join limits. It lives in memory: forecrewd
// runs as one replica, and a restart costs a guesser nothing it could use.
type limiter struct {
	mu        sync.Mutex
	now       func() time.Time
	guesses   bucket
	addresses map[string]*bucket
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, guesses: bucket{tokens: guessRate, at: now()}, addresses: map[string]*bucket{}}
}

// allowAddress counts one join call of an address.
func (l *limiter) allowAddress(address string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.addresses[address]
	if !ok {
		if len(l.addresses) >= maxAddresses {
			l.forget(now)
		}
		b = &bucket{tokens: addressBurst, at: now}
		l.addresses[address] = b
	}
	return b.take(now, addressRate, addressBurst)
}

// forget drops the addresses that used nothing lately, and if all are in use
// every one of them: an attacker with that many addresses is held by the
// global limit.
func (l *limiter) forget(now time.Time) {
	for a, b := range l.addresses {
		if b.fill(now, addressRate, addressBurst); b.tokens >= addressBurst {
			delete(l.addresses, a)
		}
	}
	if len(l.addresses) >= maxAddresses {
		clear(l.addresses)
	}
}

// takeGuess reserves one call with a code that may be unknown, before any
// lookup, so that concurrent calls cannot overrun the limit.
func (l *limiter) takeGuess() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.guesses.take(l.now(), guessRate, guessRate)
}

// refundGuess gives back what takeGuess took, for a code that turned out to
// exist: only unknown codes count.
func (l *limiter) refundGuess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.guesses.tokens = min(guessRate, l.guesses.tokens+1)
}
