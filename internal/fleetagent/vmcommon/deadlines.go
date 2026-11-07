package vmcommon

import (
	"sync"
	"time"
)

// Deadlines keeps one wall-time kill per machine. It lives in the agent, so a
// restart arms the ones its machines still have again, from their notes.
type Deadlines struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

// Set arms id's kill to call stop at at, replacing an earlier one. A zero time
// only removes it. A time already past calls stop at once.
func (d *Deadlines) Set(id string, at time.Time, stop func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.timers[id]; t != nil {
		t.Stop()
		delete(d.timers, id)
	}
	if at.IsZero() {
		return
	}
	if d.timers == nil {
		d.timers = map[string]*time.Timer{}
	}
	d.timers[id] = time.AfterFunc(time.Until(at), stop)
}

// Stop removes id's kill.
func (d *Deadlines) Stop(id string) { d.Set(id, time.Time{}, nil) }

// Armed says how many kills are waiting.
func (d *Deadlines) Armed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.timers)
}
