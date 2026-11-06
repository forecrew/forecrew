package fleetapi

import (
	"fmt"
	"testing"
	"time"
)

func TestGuessLimit(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(func() time.Time { return now })
	for i := range guessRate {
		if !l.takeGuess() {
			t.Fatalf("guess %d refused", i)
		}
	}
	if l.takeGuess() {
		t.Fatal("a sixth guess in one second")
	}
	l.refundGuess() // that code existed
	if !l.takeGuess() {
		t.Fatal("no place after a refund")
	}
	now = now.Add(time.Second)
	allowed := 0
	for range 100 {
		if l.takeGuess() {
			allowed++
		}
	}
	if allowed != guessRate {
		t.Fatalf("%d guesses in the next second", allowed)
	}
	// However long it sat idle, the bucket holds one second's worth.
	now = now.Add(time.Hour)
	for range guessRate {
		l.refundGuess()
	}
	allowed = 0
	for range 100 {
		if l.takeGuess() {
			allowed++
		}
	}
	if allowed != guessRate {
		t.Fatalf("%d guesses after an hour", allowed)
	}
}

func TestAddressLimit(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(func() time.Time { return now })
	for range addressBurst {
		if !l.allowAddress("10.0.0.1") {
			t.Fatal("refused inside the burst")
		}
	}
	if l.allowAddress("10.0.0.1") {
		t.Fatal("over the burst")
	}
	if !l.allowAddress("10.0.0.2") {
		t.Fatal("another address is limited too")
	}
	now = now.Add(time.Second)
	if !l.allowAddress("10.0.0.1") {
		t.Fatal("no refill")
	}
}

func TestAddressMemoryIsBounded(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(func() time.Time { return now })
	for i := range 3 * maxAddresses {
		l.allowAddress(fmt.Sprint("a", i))
	}
	if len(l.addresses) > maxAddresses {
		t.Fatalf("%d addresses held", len(l.addresses))
	}
	// Idle ones go first, and one still in use stays.
	clear(l.addresses)
	for i := range maxAddresses - 1 {
		l.allowAddress(fmt.Sprint("a", i))
	}
	now = now.Add(time.Hour)
	for range addressBurst {
		l.allowAddress("busy")
	}
	l.allowAddress("new")
	if _, ok := l.addresses["busy"]; !ok || len(l.addresses) != 2 {
		t.Fatalf("%d addresses, busy kept: %v", len(l.addresses), ok)
	}
}
