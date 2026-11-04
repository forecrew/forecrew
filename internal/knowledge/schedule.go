package knowledge

import (
	"context"
	"log/slog"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// ProjectSyncer is what the Scheduler runs; *Syncer implements it.
type ProjectSyncer interface {
	Sync(ctx context.Context, p domain.Project, before, head string) (SyncResult, error)
}

// Backoff after a failed sync: the first retry waits BackoffMin, each next
// one twice as long, up to BackoffMax.
const (
	BackoffMin = 30 * time.Second
	BackoffMax = 30 * time.Minute
)

// Scheduler runs knowledge syncs off the callers' goroutines: the webhook
// handler and the poller only say that a project needs a sync (Request),
// and one worker (Run) does it, a project at a time. Requests for a project
// that is waiting are coalesced into one sync from the oldest "before" to
// the newest head; a sync that fails is retried with exponential backoff,
// and a sync that stopped at its read limit goes on at once.
type Scheduler struct {
	Syncer ProjectSyncer
	Log    *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu      sync.Mutex
	pending map[uuid.UUID]*syncJob
	once    sync.Once
	wake    chan struct{}
}

type syncJob struct {
	project  domain.Project
	before   string // the oldest previous commit of the coalesced pushes
	head     string // the newest head; "" reads the branch head
	due      time.Time
	failures int
}

func (s *Scheduler) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now()
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

func (s *Scheduler) wakeCh() chan struct{} {
	s.once.Do(func() { s.wake = make(chan struct{}, 1) })
	return s.wake
}

// Request asks for a sync of p up to head ("" for the branch head) after a
// push whose previous commit was before ("" when unknown). It never blocks
// on GitHub. A project that already waits keeps its due time (and so its
// backoff), its oldest before and takes the newer head.
func (s *Scheduler) Request(p domain.Project, before, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = map[uuid.UUID]*syncJob{}
	}
	if j, ok := s.pending[p.ID]; ok {
		j.project = p
		if j.before == "" {
			j.before = before
		}
		if head != "" {
			j.head = head
		}
	} else {
		s.pending[p.ID] = &syncJob{project: p, before: before, head: head, due: s.now()}
	}
	select {
	case s.wakeCh() <- struct{}{}:
	default:
	}
}

// Pending returns how many projects wait for a sync.
func (s *Scheduler) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// firstDue returns when the first waiting job is due.
func (s *Scheduler) firstDue() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first time.Time
	for _, j := range s.pending {
		if first.IsZero() || j.due.Before(first) {
			first = j.due
		}
	}
	return first, !first.IsZero()
}

// next returns the job that is due, or else the time the first one is.
func (s *Scheduler) next() (*syncJob, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first *syncJob
	for _, j := range s.pending {
		if first == nil || j.due.Before(first.due) {
			first = j
		}
	}
	if first == nil {
		return nil, time.Time{}
	}
	if first.due.After(s.now()) {
		return nil, first.due
	}
	// The job leaves the queue while it runs, so a request that arrives
	// meanwhile starts a new one instead of being lost.
	job := *first
	delete(s.pending, first.project.ID)
	return &job, time.Time{}
}

// Drain runs every sync that is due now, one after another, and returns when
// none is. A job that continues (read limit) or fails is queued again.
func (s *Scheduler) Drain(ctx context.Context) {
	for ctx.Err() == nil {
		j, _ := s.next()
		if j == nil {
			return
		}
		s.run(ctx, j)
	}
}

func (s *Scheduler) run(ctx context.Context, j *syncJob) {
	res, err := s.Syncer.Sync(ctx, j.project, j.before, j.head)
	switch {
	case err != nil:
		j.failures++
		wait := min(BackoffMin<<min(j.failures-1, 16), BackoffMax)
		s.log().WarnContext(ctx, "knowledge sync failed, retrying later", "project", j.project.Name, "failures", j.failures, "retry_in", wait, "error", err)
		s.requeue(j, wait)
	case res.More:
		// The drift of this range was flagged by the run that stopped.
		j.before, j.head, j.failures = "", res.Head, 0
		s.requeue(j, 0)
	default:
		s.log().InfoContext(ctx, "knowledge synced", "account_id", j.project.AccountID, "project_id", j.project.ID, "head", res.Head,
			"indexed", res.Indexed, "removed", res.Removed, "skipped", len(res.Skipped), "flagged", res.Flagged, "generated", res.Generated)
	}
}

// requeue puts j back, unless a newer request for the project came in: that
// one is merged into j, which keeps its due time.
func (s *Scheduler) requeue(j *syncJob, wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.due = s.now().Add(wait)
	if s.pending == nil {
		s.pending = map[uuid.UUID]*syncJob{}
	}
	if n, ok := s.pending[j.project.ID]; ok {
		j.project = n.project
		if j.before == "" {
			j.before = n.before
		}
		if n.head != "" {
			j.head = n.head
		}
	}
	s.pending[j.project.ID] = j
}

// Run is the worker: it syncs what is due until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	wake := s.wakeCh()
	for {
		s.Drain(ctx)
		var timer <-chan time.Time
		var t *time.Timer
		if due, ok := s.firstDue(); ok {
			t = time.NewTimer(max(due.Sub(s.now()), time.Millisecond))
			timer = t.C
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return
		case <-wake:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
	}
}
