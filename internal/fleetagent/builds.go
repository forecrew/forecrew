package fleetagent

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/retry"
)

// endTimeout bounds the last report of a build the agent stops while it runs.
const endTimeout = 10 * time.Second

// applyBuilds acts on the image builds forecrewd lists (fleet-agent-api.md,
// Image builds). A build it does not hold is started when it is `requested`;
// one that is already `running` is one the agent lost in a restart, which it
// reports as failed so a person starts it again. It never starts a build that
// forecrewd does not list, whatever it finds on disk.
func (a *Agent) applyBuilds(ctx context.Context, desired []fleet.ImageBuildRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	listed := map[uuid.UUID]bool{}
	for _, b := range desired {
		listed[b.ID] = true
		if _, held := a.builds[b.ID]; held {
			continue
		}
		a.builds[b.ID] = struct{}{}
		switch {
		case b.State != domain.ImageBuildRequested:
			a.wg.Go(func() {
				a.endBuild(ctx, b.ID, fleet.ImageBuildReport{State: domain.ImageBuildFailed, Step: "failed",
					Error: "the fleet agent restarted while it built the image: start the build again"})
			})
		case a.cfg.Images == nil:
			a.wg.Go(func() {
				a.endBuild(ctx, b.ID, fleet.ImageBuildReport{State: domain.ImageBuildFailed, Step: "failed",
					Error: "this fleet agent is not set up to build images: add image_build to its agent.yaml (docs/fleet.md, Windows images on a host)"})
			})
		default:
			a.wg.Go(func() { a.runBuild(ctx, b) })
		}
	}
	for id := range a.builds {
		if !listed[id] {
			delete(a.builds, id)
		}
	}
}

// runBuild does one build and reports how it went.
func (a *Agent) runBuild(ctx context.Context, b fleet.ImageBuildRequest) {
	a.cfg.Logger.InfoContext(ctx, "image build started", "build", b.ID, "version", b.Version)
	final := a.cfg.Images.Build(ctx, b, func(r fleet.ImageBuildReport) { a.progressBuild(ctx, b.ID, r) })
	if ctx.Err() != nil {
		final = fleet.ImageBuildReport{State: domain.ImageBuildFailed, Step: "failed", Percent: final.Percent, Error: "the fleet agent stopped while it built the image: start the build again"}
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
		defer stop()
	}
	a.cfg.Logger.InfoContext(ctx, "image build ended", "build", b.ID, "state", final.State)
	a.endBuild(ctx, b.ID, final)
}

// progressBuild sends a report of a running build once: the next one replaces
// it, so a lost one is not worth waiting for.
func (a *Agent) progressBuild(ctx context.Context, id uuid.UUID, r fleet.ImageBuildReport) {
	if err := a.cfg.Client.BuildProgress(ctx, id, r); err != nil {
		a.cfg.Logger.DebugContext(ctx, "progress of an image build not delivered", "build", id, "error", clean(err))
	}
}

// endBuild delivers the end of a build until forecrewd has it, or refuses it:
// a person is waiting for it.
func (a *Agent) endBuild(ctx context.Context, id uuid.UUID, r fleet.ImageBuildReport) {
	for failures := 1; ; failures++ {
		err := a.cfg.Client.BuildProgress(ctx, id, r)
		if err == nil {
			return
		}
		if !retryable(err) {
			a.cfg.Logger.WarnContext(ctx, "forecrewd refused the end of an image build", "build", id, "error", clean(err))
			return
		}
		if retry.Sleep(ctx, retry.Backoff(failures, a.cfg.BackoffMin, a.cfg.BackoffMax)) != nil {
			if !errors.Is(ctx.Err(), context.Canceled) {
				a.cfg.Logger.WarnContext(ctx, "the end of an image build was not delivered", "build", id)
			}
			return
		}
	}
}
