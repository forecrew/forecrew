package testkit

import (
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
)

var _ queue.Queue = (*Queue)(nil)

// Queue is the queue.Queue on top of a store's job repository, with leases
// of domain.LeaseDuration. It is the same code the Postgres queue runs.
type Queue struct {
	queue.Queue
}

// NewQueue returns a queue that keeps its jobs in st.
func NewQueue(st store.Store) *Queue {
	return &Queue{queue.New(st.Jobs())}
}
