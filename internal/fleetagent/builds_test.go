package fleetagent_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/store"
)

// fakeBuilder is an ImageBuilder the test steers: it reports a step, waits for
// release, then ends as the test says.
type fakeBuilder struct {
	mu      sync.Mutex
	seen    []fleet.ImageBuildRequest
	release chan struct{}
	end     fleet.ImageBuildReport
}

func newFakeBuilder(end fleet.ImageBuildReport) *fakeBuilder {
	return &fakeBuilder{release: make(chan struct{}), end: end}
}

func (f *fakeBuilder) Build(ctx context.Context, req fleet.ImageBuildRequest, progress func(fleet.ImageBuildReport)) fleet.ImageBuildReport {
	f.mu.Lock()
	f.seen = append(f.seen, req)
	f.mu.Unlock()
	progress(fleet.ImageBuildReport{State: domain.ImageBuildRunning, Step: "building the image: about an hour", Percent: 15})
	select {
	case <-f.release:
	case <-ctx.Done():
		return fleet.ImageBuildReport{State: domain.ImageBuildFailed, Percent: 15, Error: "stopped"}
	}
	return f.end
}

func (f *fakeBuilder) requests() []fleet.ImageBuildRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fleet.ImageBuildRequest{}, f.seen...)
}

func (w *world) build() domain.ImageBuild {
	w.t.Helper()
	b := domain.ImageBuild{AccountID: w.provider.AccountID, ProviderID: w.provider.ID, Version: "2026.11.1", ISO: "/isos/server.iso", ISOChecksum: "sha256:" + strings.Repeat("a", 64),
		ChocolateySHA256: strings.Repeat("b", 64), LicenseConfirmedBy: "user:ana"}
	must(w.t, w.st.ImageBuilds().Create(w.t.Context(), &b))
	return b
}

func (w *world) buildState(id uuid.UUID) domain.ImageBuild {
	w.t.Helper()
	all, err := w.st.ImageBuilds().List(context.Background(), w.provider.ID, 0)
	must(w.t, err)
	for _, b := range all {
		if b.ID == id {
			return b
		}
	}
	w.t.Fatalf("no build %s", id)
	return domain.ImageBuild{}
}

func TestAgentDoesTheBuildAndReportsIt(t *testing.T) {
	w := newWorld(t)
	digest := "sha256:" + strings.Repeat("c", 64)
	fb := newFakeBuilder(fleet.ImageBuildReport{State: domain.ImageBuildDone, Step: "done", Percent: 100, Manifest: "/out/m.json", Digest: digest})
	w.start(w.config(w.fake, func(c *fleetagent.Config) { c.Images = fb }))
	b := w.build()
	eventually(t, "the build to run", func() bool {
		got := w.buildState(b.ID)
		return got.State == domain.ImageBuildRunning && got.Percent == 15 && got.Step == "building the image: about an hour"
	})
	reqs := fb.requests()
	if len(reqs) != 1 || reqs[0].ID != b.ID || reqs[0].ISO != b.ISO || reqs[0].ISOChecksum != b.ISOChecksum || !reqs[0].LicenseConfirmed || reqs[0].ChocolateySHA256 != b.ChocolateySHA256 || reqs[0].Version != "2026.11.1" {
		t.Fatalf("the agent got %+v", reqs)
	}
	close(fb.release)
	eventually(t, "the build to end", func() bool { return w.buildState(b.ID).State == domain.ImageBuildDone })
	got := w.buildState(b.ID)
	if got.Manifest != "/out/m.json" || got.Digest != digest || got.Percent != 100 || got.Error != "" {
		t.Fatalf("done %+v", got)
	}
	// It is taken once, however many polls come after.
	time.Sleep(60 * time.Millisecond)
	if n := len(fb.requests()); n != 1 {
		t.Fatalf("built %d times", n)
	}
	// And it is only ever an unsigned manifest on the host: nothing was registered.
	images, err := w.st.Images().List(t.Context(), w.provider.AccountID, store.ImageFilter{})
	must(t, err)
	if len(images) != 0 {
		t.Fatalf("an image was registered: %+v", images)
	}
}

func TestAgentReportsAFailedBuild(t *testing.T) {
	w := newWorld(t)
	fb := newFakeBuilder(fleet.ImageBuildReport{State: domain.ImageBuildFailed, Step: "failed", Percent: 5, Error: "the ISO is sha256:x, not the one you gave"})
	close(fb.release)
	w.start(w.config(w.fake, func(c *fleetagent.Config) { c.Images = fb }))
	b := w.build()
	eventually(t, "the build to fail", func() bool { return w.buildState(b.ID).State == domain.ImageBuildFailed })
	if got := w.buildState(b.ID); got.Error != "the ISO is sha256:x, not the one you gave" || got.Percent != 5 {
		t.Fatalf("failed %+v", got)
	}
}

func TestAgentWithoutImageSettingsRefuses(t *testing.T) {
	w := newWorld(t)
	w.start(w.config(w.fake))
	b := w.build()
	eventually(t, "the refusal", func() bool { return w.buildState(b.ID).State == domain.ImageBuildFailed })
	if got := w.buildState(b.ID); !strings.Contains(got.Error, "not set up to build images") || !strings.Contains(got.Error, "image_build") {
		t.Fatalf("failed %+v", got)
	}
}

func TestAgentThatRestartedFailsARunningBuild(t *testing.T) {
	w := newWorld(t)
	b := w.build()
	_, err := fleet.ReportImageBuild(t.Context(), w.st, time.Now(), w.provider, b.ID, fleet.ImageBuildReport{State: domain.ImageBuildRunning, Step: "building", Percent: 15})
	must(t, err)
	fb := newFakeBuilder(fleet.ImageBuildReport{})
	w.start(w.config(w.fake, func(c *fleetagent.Config) { c.Images = fb }))
	eventually(t, "the failure", func() bool { return w.buildState(b.ID).State == domain.ImageBuildFailed })
	if got := w.buildState(b.ID); !strings.Contains(got.Error, "restarted while it built the image") {
		t.Fatalf("failed %+v", got)
	}
	if n := len(fb.requests()); n != 0 {
		t.Fatalf("started %d builds it did not hold", n)
	}
}

func TestAgentDeliversTheEndAfterAnOutage(t *testing.T) {
	w := newWorld(t)
	digest := "sha256:" + strings.Repeat("c", 64)
	fb := newFakeBuilder(fleet.ImageBuildReport{State: domain.ImageBuildDone, Step: "done", Percent: 100, Manifest: "/out/m.json", Digest: digest})
	w.start(w.config(w.fake, func(c *fleetagent.Config) { c.Images = fb }))
	b := w.build()
	eventually(t, "the build to run", func() bool { return w.buildState(b.ID).State == domain.ImageBuildRunning })
	w.down.Store(true)
	close(fb.release)
	time.Sleep(40 * time.Millisecond)
	if w.buildState(b.ID).State != domain.ImageBuildRunning {
		t.Fatal("it ended while forecrewd was away")
	}
	w.down.Store(false)
	eventually(t, "the end to arrive", func() bool { return w.buildState(b.ID).State == domain.ImageBuildDone })
}

func TestAgentStoppingFailsItsBuild(t *testing.T) {
	w := newWorld(t)
	fb := newFakeBuilder(fleet.ImageBuildReport{})
	stop, _ := w.start(w.config(w.fake, func(c *fleetagent.Config) { c.Images = fb }))
	b := w.build()
	eventually(t, "the build to run", func() bool { return w.buildState(b.ID).State == domain.ImageBuildRunning })
	stop()
	got := w.buildState(b.ID)
	if got.State != domain.ImageBuildFailed || !strings.Contains(got.Error, "agent stopped") {
		t.Fatalf("after stop %+v", got)
	}
}
