package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Timings of an image build that an agent does not keep to.
const (
	// BuildPickupWithin is how long a requested build waits for its agent. An
	// agent too old to know builds never takes it.
	BuildPickupWithin = 30 * time.Minute
	// BuildSilentFor is how long a running build may go without a report. An
	// agent reports at least every two minutes while it builds.
	BuildSilentFor = 15 * time.Minute
)

// Audit actions of image builds.
const (
	ActionImageBuildRequested = "image_build.requested"
	ActionImageBuildFinished  = "image_build.finished"
)

// maxBuildLine bounds the lines an agent reports about a build.
const maxBuildLine = 1024

var (
	// ErrBuildNotAvailable means the location cannot build a Windows image now.
	ErrBuildNotAvailable = errors.New("this location cannot build a Windows image")
	buildDigestRE        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ImageBuildRequest is a build as the fleet agent API hands it to the agent
// (fleet-agent-api.md, Image builds): references only. The agent holds the rest
// (the templates, the worker, the output directory) itself, and the ISO never
// leaves its host.
type ImageBuildRequest struct {
	ID               uuid.UUID              `json:"id"`
	State            domain.ImageBuildState `json:"state"`
	Version          string                 `json:"version"`
	ISO              string                 `json:"iso"`
	ISOChecksum      string                 `json:"iso_checksum"`
	LicenseConfirmed bool                   `json:"license_confirmed"`
	ChocolateySHA256 string                 `json:"chocolatey_sha256"`
	WindowsImageName string                 `json:"windows_image_name,omitempty"`
}

// ImageBuildReport is what the agent says about a build it holds, from its
// progress.json.
type ImageBuildReport struct {
	State    domain.ImageBuildState `json:"state"`
	Step     string                 `json:"step"`
	Percent  int                    `json:"percent"`
	Error    string                 `json:"error,omitempty"`
	Manifest string                 `json:"manifest,omitempty"`
	Digest   string                 `json:"digest,omitempty"`
}

// BuildableKinds are the kinds of location whose host can build a Windows image.
func BuildableKinds() []domain.ProviderKind {
	return []domain.ProviderKind{domain.ProviderHyperV, domain.ProviderProxmox}
}

// CheckBuildable says why p cannot build a Windows image now, or returns nil.
func CheckBuildable(p domain.CapacityProvider) error {
	switch {
	case p.Mode != domain.ProviderModeAgent || !slices.Contains(BuildableKinds(), p.Kind):
		return fmt.Errorf("%w: only a Hyper-V or Proxmox location served by a fleet agent builds one", ErrBuildNotAvailable)
	case p.Status != domain.ProviderActive:
		return fmt.Errorf("%w: it is %s", ErrBuildNotAvailable, p.Status)
	}
	return nil
}

func buildRequest(b domain.ImageBuild) ImageBuildRequest {
	return ImageBuildRequest{ID: b.ID, State: b.State, Version: b.Version, ISO: b.ISO, ISOChecksum: b.ISOChecksum,
		LicenseConfirmed: b.LicenseConfirmedBy != "", ChocolateySHA256: b.ChocolateySHA256, WindowsImageName: b.WindowsImageName}
}

// DesiredImageBuilds returns the open builds of p for its agent. A paused
// location gets none that are not running yet.
func DesiredImageBuilds(ctx context.Context, st store.Store, p domain.CapacityProvider) ([]ImageBuildRequest, error) {
	open, err := st.ImageBuilds().Open(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("list image builds: %w", err)
	}
	var out []ImageBuildRequest
	for _, b := range open {
		if b.State == domain.ImageBuildRequested && p.Status != domain.ProviderActive {
			continue
		}
		out = append(out, buildRequest(b))
	}
	return out, nil
}

// ExpireImageBuilds fails the open builds of provider that nobody works on: one
// its agent did not take, and one it stopped reporting. Nothing else watches
// them, so what reads builds calls this first.
func ExpireImageBuilds(ctx context.Context, st store.Store, now time.Time, providerID uuid.UUID) error {
	open, err := st.ImageBuilds().Open(ctx, providerID)
	if err != nil {
		return fmt.Errorf("list image builds: %w", err)
	}
	var errs []error
	for _, b := range open {
		var why string
		switch {
		case b.State == domain.ImageBuildRequested && now.Sub(b.CreatedAt) > BuildPickupWithin:
			why = "the agent did not take the build within 30 minutes: is the location online, and is its fleet agent up to date and set up to build images?"
		case b.State == domain.ImageBuildRunning && now.Sub(b.UpdatedAt) > BuildSilentFor:
			why = "the agent stopped reporting this build 15 minutes ago: it may have stopped or lost its connection; start the build again"
		default:
			continue
		}
		b.State, b.Error, b.FinishedAt = domain.ImageBuildFailed, why, now
		if err := saveBuild(ctx, st, &b); err != nil && !errors.Is(err, store.ErrConflict) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// plainLine reports a short line of text with no control character.
func plainLine(s string) bool {
	return len(s) <= maxBuildLine && strings.IndexFunc(s, unicode.IsControl) < 0
}

// ReportImageBuild records what the agent of p says about build id
// (fleet-agent-api.md, Image builds). ErrNotFound means the build is not p's,
// ErrBadReport a report an agent may not make, store.ErrConflict that the build
// ended otherwise already. A report that repeats the end is accepted, so the
// agent can retry. The words are cleaned and stored as text.
func ReportImageBuild(ctx context.Context, st store.Store, now time.Time, p domain.CapacityProvider, id uuid.UUID, r ImageBuildReport) (domain.ImageBuild, error) {
	b, err := st.ImageBuilds().Get(ctx, id)
	if err != nil {
		return b, err
	}
	if b.ProviderID != p.ID {
		return b, store.ErrNotFound
	}
	if err := checkBuildReport(r); err != nil {
		return b, err
	}
	if !b.State.Open() {
		if b.State == r.State {
			return b, nil
		}
		return b, fmt.Errorf("image build %s is %s: %w", id, b.State, store.ErrConflict)
	}
	if b.StartedAt.IsZero() {
		b.StartedAt = now
	}
	b.State, b.Step, b.Percent = r.State, r.Step, r.Percent
	switch r.State {
	case domain.ImageBuildDone:
		b.Percent, b.Error, b.Manifest, b.Digest, b.FinishedAt = 100, "", r.Manifest, r.Digest, now
	case domain.ImageBuildFailed:
		b.Error, b.FinishedAt = CleanError(r.Error), now
	}
	return b, saveBuild(ctx, st, &b)
}

// saveBuild stores b, and when it ends writes that to the organization's log
// in the same transaction: the person who asked finds it there. An instance-wide
// location has no organization to write to.
func saveBuild(ctx context.Context, st store.Store, b *domain.ImageBuild) error {
	if b.State.Open() || b.AccountID == uuid.Nil() {
		return st.ImageBuilds().Save(ctx, b)
	}
	return st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ImageBuilds().Save(ctx, b); err != nil {
			return err
		}
		raw, err := json.Marshal(map[string]any{"version": b.Version, "state": b.State, "digest": b.Digest, "requested_by": b.LicenseConfirmedBy})
		if err != nil {
			return fmt.Errorf("encode audit data: %w", err)
		}
		by := domain.SystemActor(b.AccountID, domain.ForecrewdName)
		return tx.Audit().Append(ctx, &domain.AuditEntry{AccountID: b.AccountID, Actor: by.String(), Action: ActionImageBuildFinished, SubjectType: "image_build", SubjectID: b.ID, Data: raw})
	})
}

func checkBuildReport(r ImageBuildReport) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrBadReport}, args...)...)
	}
	switch {
	case r.State != domain.ImageBuildRunning && r.State != domain.ImageBuildDone && r.State != domain.ImageBuildFailed:
		return bad("state %q: an agent reports running, done or failed", r.State)
	case r.Percent < 0 || r.Percent > 100:
		return bad("percent is 0 to 100")
	case !plainLine(r.Step) || !plainLine(r.Manifest):
		return bad("step and manifest are short lines of text")
	case len(r.Error) > maxErrorLength:
		return bad("error is at most %d characters", maxErrorLength)
	case r.State == domain.ImageBuildDone && (r.Manifest == "" || !buildDigestRE.MatchString(r.Digest)):
		return bad("a finished build names its manifest and the digest of its image")
	case r.State == domain.ImageBuildFailed && r.Error == "":
		return bad("a failed build says why")
	}
	return nil
}
