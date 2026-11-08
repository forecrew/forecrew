package fleetagent

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent/imagebuild"
)

// Timings of reporting a build.
const (
	// buildLook is how often progress.json is read while a build runs.
	buildLook = 5 * time.Second
	// buildHeartbeat is the longest time between reports of a running build:
	// the step of an hour-long Packer run does not change, and forecrewd fails
	// a build it stops hearing about (fleet.BuildSilentFor).
	buildHeartbeat = 2 * time.Minute
)

// ImageBuilder does the Windows image build forecrewd asked for and says how it
// went. Progress is called with what progress.json says, at each change and
// as a heartbeat; the return value is the end: done with the unsigned manifest,
// or failed with the reason. It never registers or signs anything.
type ImageBuilder interface {
	Build(ctx context.Context, req fleet.ImageBuildRequest, progress func(fleet.ImageBuildReport)) fleet.ImageBuildReport
}

// ImageRunner builds with imagebuild.Builder, from the request and the
// settings of this host (agent.yaml, image_build). The request names the ISO
// and says whether a person confirmed the licence; the rest never leaves the host.
type ImageRunner struct {
	Settings config.FleetAgentImageBuild
	Builder  imagebuild.Builder
	// GOOS says which builder runs; empty is this program's.
	GOOS string
	// Look and Heartbeat default to the constants above.
	Look, Heartbeat time.Duration
	Now             func() time.Time
}

func failedBuild(step string, percent int, err error) fleet.ImageBuildReport {
	return fleet.ImageBuildReport{State: domain.ImageBuildFailed, Step: step, Percent: percent, Error: fleet.CleanError(err.Error())}
}

// options turns a request into the options of the build, refusing an ISO
// outside the directory the owner allowed.
func (r ImageRunner) options(req fleet.ImageBuildRequest) (imagebuild.Options, error) {
	iso, err := insideDir(r.Settings.ISODir, req.ISO)
	if err != nil {
		return imagebuild.Options{}, err
	}
	goos := r.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	s := r.Settings
	return imagebuild.Options{
		TemplatesDir: s.TemplatesDir, Packer: s.Packer, ISO: iso, ISOChecksum: req.ISOChecksum, LicenseConfirmed: req.LicenseConfirmed,
		WindowsImageName: req.WindowsImageName, ChocolateyInstallerSHA256: req.ChocolateySHA256, Worker: s.Worker,
		GuestTools: s.GuestTools, GuestToolsSHA256: s.GuestToolsSHA256, Version: req.Version, URIBase: s.URIBase,
		OutDir: filepath.Join(s.OutDir, req.ID.String()), CacheDir: s.CacheDir, Switch: s.Switch, GOOS: goos,
	}, nil
}

// insideDir returns path with its links resolved when it is a file under dir
// (itself resolved). A path that goes elsewhere, by .. or by a link, is refused
// before anything reads it.
func insideDir(dir, path string) (string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("the ISO directory of this agent (image_build.iso_dir): %w", err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("the ISO is not at the path you gave on this host: %w", err)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the ISO must be in %s, the directory this agent allows (image_build.iso_dir in agent.yaml)", dir)
	}
	return real, nil
}

// Build implements ImageBuilder.
func (r ImageRunner) Build(ctx context.Context, req fleet.ImageBuildRequest, progress func(fleet.ImageBuildReport)) fleet.ImageBuildReport {
	look, beat, now := r.Look, r.Heartbeat, r.Now
	if look <= 0 {
		look = buildLook
	}
	if beat <= 0 {
		beat = buildHeartbeat
	}
	if now == nil {
		now = time.Now
	}
	o, err := r.options(req)
	if err != nil {
		return failedBuild("failed", 0, err)
	}
	progress(fleet.ImageBuildReport{State: domain.ImageBuildRunning, Step: "starting"})
	type outcome struct {
		res imagebuild.Result
		err error
	}
	ended := make(chan outcome, 1)
	go func() {
		res, err := r.Builder.Run(ctx, o)
		ended <- outcome{res, err}
	}()
	var last imagebuild.Progress
	var lastSent time.Time
	tick := time.NewTicker(look)
	defer tick.Stop()
	for {
		select {
		case out := <-ended:
			p, _ := imagebuild.ReadProgress(o.OutDir)
			if out.err != nil {
				return failedBuild("failed", p.Percent, out.err)
			}
			return fleet.ImageBuildReport{State: domain.ImageBuildDone, Step: "done", Percent: 100, Manifest: out.res.Manifest, Digest: out.res.Digest}
		case <-tick.C:
			p, err := imagebuild.ReadProgress(o.OutDir)
			if err != nil || p.State != "running" {
				continue // not written yet; the end comes from the build itself
			}
			if p != last || now().Sub(lastSent) >= beat {
				progress(fleet.ImageBuildReport{State: domain.ImageBuildRunning, Step: p.Step, Percent: p.Percent})
				last, lastSent = p, now()
			}
		}
	}
}
