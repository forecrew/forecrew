package testkit

import (
	"time"

	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
)

// Gates is the gate service of internal/gates, re-exported so tests build it
// next to the other fakes.
type Gates = gates.Keeper

// NewGates returns a gate service. now defaults to the real UTC time.
func NewGates(st store.Store, bus gates.Publisher, now func() time.Time) *Gates {
	return gates.New(st, bus, now)
}
