package testkit

import (
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// SignalBus is the signal bus of internal/workflow, re-exported so tests
// build it next to the other fakes.
type SignalBus = workflow.Bus

// NewSignalBus returns a bus storing its signals in st.
func NewSignalBus(st store.Store) *SignalBus { return workflow.NewBus(st.Signals()) }
