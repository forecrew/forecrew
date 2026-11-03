package domain

import (
	"slices"
	"strings"
	"time"
	"uuid"
)

// OnlineWindow is how recently a worker must have polled or heartbeated to
// count as online.
const OnlineWindow = 60 * time.Second

// Worker is a registered worker daemon.
type Worker struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	Name      string    `json:"name"` // unique per account
	// Pool is set by an admin when the worker is created and never taken
	// from what the worker sends; the worker leases only jobs of its pool.
	// Empty is stored as PoolBuild.
	Pool       string    `json:"pool"`
	TokenHash  []byte    `json:"-"` // SHA-256 of the worker token
	Labels     []string  `json:"labels"`
	Version    string    `json:"version"`
	MaxJobs    int       `json:"max_jobs"`
	LastSeenAt time.Time `json:"last_seen_at,omitzero"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
	CreatedAt  time.Time `json:"created_at"`
}

// Online reports whether the worker was seen within OnlineWindow of now.
func (w Worker) Online(now time.Time) bool {
	return !w.Revoked() && !w.LastSeenAt.IsZero() && now.Sub(w.LastSeenAt) <= OnlineWindow
}

// Revoked reports whether the worker's token was revoked.
func (w Worker) Revoked() bool { return !w.RevokedAt.IsZero() }

// RuntimeLabel returns the label a worker advertises for a runtime adapter
// whose CLI it found, e.g. runtime:claude-code.
func RuntimeLabel(runtime string) string { return "runtime:" + runtime }

// NormalizeLabels lowercases and trims labels, drops empty ones, sorts them
// and removes duplicates.
func NormalizeLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// MatchLabels reports whether every needed label is among have, compared
// case-insensitively.
func MatchLabels(needs, have []string) bool {
	h := NormalizeLabels(have)
	for _, n := range NormalizeLabels(needs) {
		if _, ok := slices.BinarySearch(h, n); !ok {
			return false
		}
	}
	return true
}
