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
	Pool string `json:"pool"`
	// Ephemeral marks the worker of a machine (fleet.md): made when the
	// machine boots, gone with it. False on every worker an admin adds.
	Ephemeral  bool      `json:"ephemeral"`
	TokenHash  []byte    `json:"-"` // SHA-256 of the worker token
	Labels     []string  `json:"labels"`
	Version    string    `json:"version"`
	MaxJobs    int       `json:"max_jobs"`
	LastSeenAt time.Time `json:"last_seen_at,omitzero"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
	CreatedAt  time.Time `json:"created_at"`
	// Agents are the agent CLIs the worker reported at its last poll, with
	// where each came from and why an install failed; nil from a worker that
	// reports none.
	Agents []WorkerAgent `json:"agents,omitempty"`
}

// AgentInstallCapability is what a worker that installs a missing agent CLI
// when asked says when it polls (worker-api.md, poll). forecrewd then gives
// it AgentInstallLabel, and asks only such workers.
const AgentInstallCapability = "agent_install"

// AgentInstallLabel marks a worker that installs agent CLIs when asked; a
// worker never sets it itself.
const AgentInstallLabel = "agents:install"

// Where an agent CLI of a worker came from.
const (
	AgentSourceImage     = "image"     // the image of a fleet machine
	AgentSourceInstalled = "installed" // installed by the worker from its release's manifest
	AgentSourceSystem    = "system"    // found on the machine's PATH
)

// Why a worker could not install an agent CLI. The worker reports only the
// code; the sentence people read is forecrewd's.
const (
	AgentInstallUnsupported = "unsupported" // the manifest has no standalone binary for the machine's system
	AgentInstallDownload    = "download"    // the download failed
	AgentInstallChecksum    = "checksum"    // the download did not match its SHA-256 and was deleted
	AgentInstallArchive     = "archive"     // the archive did not hold the binary
	AgentInstallWrite       = "write"       // the worker could not write its own directory
)

// WorkerAgent is one agent CLI as a worker reports it: Source is empty while
// it is missing, and Error holds the code of a failed install.
type WorkerAgent struct {
	Adapter string `json:"adapter"`
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Agent returns what the worker reported of adapter.
func (w Worker) Agent(adapter string) (WorkerAgent, bool) {
	i := slices.IndexFunc(w.Agents, func(a WorkerAgent) bool { return a.Adapter == adapter })
	if i < 0 {
		return WorkerAgent{}, false
	}
	return w.Agents[i], true
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
