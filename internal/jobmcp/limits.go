package jobmcp

import (
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// Rate of all calls of one job attempt (job-mcp.md, Limits).
const (
	CallsPerMinute = 60
	CallBurst      = 20
)

// MaxDeniedAudits is how many refused and rate-limited calls of one attempt
// are audited one by one. Those calls cost the caller nothing, so past it
// they are still answered but only one summary row is written.
const MaxDeniedAudits = 100

// idleAfter is how long the limits of an attempt are kept after its last
// call.
const idleAfter = time.Hour

// attempt identifies one lease of a job: the limits are per attempt.
type attempt struct {
	job      uuid.UUID
	attempts int
}

func attemptOf(j domain.Job) attempt { return attempt{job: j.ID, attempts: j.Attempts} }

// attemptState is the limit state of one attempt.
type attemptState struct {
	tokens float64
	last   time.Time
	calls  map[string]int
	asking bool // an ask_human call is waiting
	denied int  // refused and rate-limited calls
}

// limiter holds the call limits of every attempt in memory: a token bucket
// for all calls and a count per tool. A forecrewd restart resets them.
type limiter struct {
	mu        sync.Mutex
	now       func() time.Time
	attempts  map[attempt]*attemptState
	lastPrune time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, attempts: map[attempt]*attemptState{}}
}

// state returns the state of j's attempt, refilling its bucket; l.mu must
// be held.
func (l *limiter) state(j domain.Job) *attemptState {
	now := l.now()
	if now.Sub(l.lastPrune) > time.Minute {
		for k, s := range l.attempts {
			if now.Sub(s.last) > idleAfter && !s.asking {
				delete(l.attempts, k)
			}
		}
		l.lastPrune = now
	}
	s, ok := l.attempts[attemptOf(j)]
	if !ok {
		s = &attemptState{tokens: CallBurst, last: now, calls: map[string]int{}}
		l.attempts[attemptOf(j)] = s
	}
	s.tokens = min(CallBurst, s.tokens+now.Sub(s.last).Seconds()*CallsPerMinute/60)
	s.last = now
	return s
}

// allow takes one call of t for j's attempt, or reports false when the
// attempt is over the rate or over t's MaxCalls.
func (l *limiter) allow(j domain.Job, t Tool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state(j)
	if s.tokens < 1 || (t.MaxCalls > 0 && s.calls[t.Name] >= t.MaxCalls) {
		return false
	}
	s.tokens--
	s.calls[t.Name]++
	return true
}

// denied counts a refused or rate-limited call of j's attempt. audit says
// the call gets its own audit row, summary that this call passed
// MaxDeniedAudits and the summary row is due.
func (l *limiter) denied(j domain.Job) (audit, summary bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state(j)
	s.denied++
	return s.denied <= MaxDeniedAudits, s.denied == MaxDeniedAudits+1
}

// startAsking marks an ask_human call of j's attempt as waiting; false when
// one already is.
func (l *limiter) startAsking(j domain.Job) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state(j)
	if s.asking {
		return false
	}
	s.asking = true
	return true
}

func (l *limiter) doneAsking(j domain.Job) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state(j).asking = false
}

// Attachment is a file evidence_attach marked, as the heartbeat answer
// carries it (worker-api.md).
type Attachment struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// attachmentTTL is how long the marks of an attempt nobody picks up are
// kept: a worker that died never sends the heartbeat that would drop them.
const attachmentTTL = 24 * time.Hour

// marks are the unacknowledged attachments of one attempt.
type marks struct {
	files []Attachment
	last  time.Time
}

// Attachments holds the files marked by each job attempt until the worker
// acknowledges them, so a heartbeat answer lost on the way costs nothing. It
// is in memory: marks are lost on a forecrewd restart.
type Attachments struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[attempt]*marks
}

// NewAttachments returns an empty queue; now defaults to the real UTC time.
func NewAttachments(now func() time.Time) *Attachments {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Attachments{now: now, m: map[attempt]*marks{}}
}

// Add queues at for j's attempt: a later mark of the same artifact name
// replaces the earlier one. It drops what earlier attempts of j left and
// marks nobody picked up for attachmentTTL.
func (a *Attachments) Add(j domain.Job, at Attachment) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, m := range a.m {
		if (k.job == j.ID && k.attempts != j.Attempts) || now.Sub(m.last) > attachmentTTL {
			delete(a.m, k)
		}
	}
	m, ok := a.m[attemptOf(j)]
	if !ok {
		m = &marks{}
		a.m[attemptOf(j)] = m
	}
	m.last = now
	m.files = slices.DeleteFunc(m.files, func(f Attachment) bool { return f.Name == at.Name })
	m.files = append(m.files, at)
}

// Pending returns what j's attempt marked and the worker has not
// acknowledged; nil when nothing.
func (a *Attachments) Pending(j domain.Job) []Attachment {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if m, ok := a.m[attemptOf(j)]; ok {
		return slices.Clone(m.files)
	}
	return nil
}

// Ack forgets the attachments of j's attempt the worker reports it received.
func (a *Attachments) Ack(j domain.Job, received []Attachment) {
	if a == nil || len(received) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.m[attemptOf(j)]
	if !ok {
		return
	}
	m.files = slices.DeleteFunc(m.files, func(f Attachment) bool { return slices.Contains(received, f) })
	if len(m.files) == 0 {
		delete(a.m, attemptOf(j))
	}
}

// Drop forgets everything queued for job.
func (a *Attachments) Drop(job uuid.UUID) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.m {
		if k.job == job {
			delete(a.m, k)
		}
	}
}
