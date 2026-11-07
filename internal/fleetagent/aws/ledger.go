package aws

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ledger estimates the month's spend from the machines this agent started and
// the price its shapes give them. It is the customer's own brake, kept next to
// the agent: it does not ask forecrewd, and it is an estimate of what the shapes
// say a minute costs, not AWS's bill (docs/runbooks/byoc-aws.md). Losing the file
// resets the estimate, never the machine limit.
type ledger struct {
	path string
	now  func() time.Time

	mu sync.Mutex
	d  ledgerData
}

type ledgerData struct {
	Month  string              `json:"month"` // 2026-11, UTC
	Closed float64             `json:"closed"`
	Open   map[string]openCost `json:"open"`
}

// openCost is a machine still running: since when, at what price.
type openCost struct {
	Since     time.Time `json:"since"`
	PerMinute float64   `json:"per_minute"`
}

func openLedger(dir string, now func() time.Time) (*ledger, error) {
	l := &ledger{path: filepath.Join(dir, "spend.json"), now: now, d: ledgerData{Open: map[string]openCost{}}}
	raw, err := os.ReadFile(l.path) // #nosec G304 -- the agent's own state
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read spend ledger: %w", err)
	}
	if err := json.Unmarshal(raw, &l.d); err != nil {
		return nil, fmt.Errorf("read spend ledger %s: %w", l.path, err)
	}
	if l.d.Open == nil {
		l.d.Open = map[string]openCost{}
	}
	return l, nil
}

func monthOf(t time.Time) (key string, start time.Time) {
	t = t.UTC()
	start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start.Format("2006-01"), start
}

// roll starts a new month: what closed before is no longer counted, and what is
// still running counts from the first of the month. The caller holds mu.
func (l *ledger) roll() time.Time {
	now := l.now()
	key, start := monthOf(now)
	if l.d.Month != key {
		l.d.Month, l.d.Closed = key, 0
		for id, o := range l.d.Open {
			o.Since = start
			l.d.Open[id] = o
		}
	}
	return now
}

func (c openCost) minutes(now, start time.Time) float64 {
	since := c.Since
	if since.Before(start) {
		since = start
	}
	return max(now.Sub(since).Minutes(), 0)
}

// spent is the month's estimate so far.
func (l *ledger) spent() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.roll()
	_, start := monthOf(now)
	total := l.d.Closed
	for _, o := range l.d.Open {
		total += o.minutes(now, start) * o.PerMinute
	}
	return total
}

// start counts a machine from now. A second call for the same machine keeps the
// first start, so a retry is not charged twice.
func (l *ledger) start(id string, perMinute float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.roll()
	if _, ok := l.d.Open[id]; !ok {
		l.d.Open[id] = openCost{Since: now, PerMinute: perMinute}
	}
	return l.save()
}

// adopt counts a machine the ledger does not know from since, which is when it
// started, as long as that is this month.
func (l *ledger) adopt(id string, since time.Time, perMinute float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll()
	if _, ok := l.d.Open[id]; ok {
		return nil
	}
	l.d.Open[id] = openCost{Since: since, PerMinute: perMinute}
	return l.save()
}

// end stops counting a machine, and adds what it cost.
func (l *ledger) end(ids ...string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.roll()
	_, start := monthOf(now)
	changed := false
	for _, id := range ids {
		if o, ok := l.d.Open[id]; ok {
			l.d.Closed += o.minutes(now, start) * o.PerMinute
			delete(l.d.Open, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return l.save()
}

// onlyOpen ends every machine that is not in live: it is gone, however it went.
func (l *ledger) onlyOpen(live map[string]bool) error {
	l.mu.Lock()
	var gone []string
	for id := range l.d.Open {
		if !live[id] {
			gone = append(gone, id)
		}
	}
	l.mu.Unlock()
	return l.end(gone...)
}

// save writes the file whole or not at all. The caller holds mu.
func (l *ledger) save() error {
	raw, err := json.Marshal(l.d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("spend ledger: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(l.path), ".spend-*") // 0600
	if err != nil {
		return fmt.Errorf("spend ledger: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(raw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("spend ledger: %w", err)
	}
	if err := os.Rename(f.Name(), l.path); err != nil {
		return fmt.Errorf("spend ledger: %w", err)
	}
	return nil
}
