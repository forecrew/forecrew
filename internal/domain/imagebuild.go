package domain

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"uuid"
)

// ImageBuildState is where the build of a Windows image on a location's host
// stands.
type ImageBuildState string

// Image build states. A build ends as done or failed; done means the host left an
// unsigned manifest, never a registered image (fleet.md, Windows images).
const (
	ImageBuildRequested ImageBuildState = "requested" // a person asked; the agent has not taken it yet
	ImageBuildRunning   ImageBuildState = "running"   // the agent is building it
	ImageBuildDone      ImageBuildState = "done"
	ImageBuildFailed    ImageBuildState = "failed"
)

// ImageBuildStates returns every image build state.
func ImageBuildStates() []ImageBuildState {
	return []ImageBuildState{ImageBuildRequested, ImageBuildRunning, ImageBuildDone, ImageBuildFailed}
}

var imageBuildTransitions = transitions[ImageBuildState]{
	ImageBuildRequested: {ImageBuildRunning, ImageBuildFailed},
	ImageBuildRunning:   {ImageBuildDone, ImageBuildFailed},
}

// CanTransition reports whether a build may move from s to to.
func (s ImageBuildState) CanTransition(to ImageBuildState) bool {
	return imageBuildTransitions.can(s, to)
}

// Open reports whether the build still needs its agent: one location builds one
// image at a time.
func (s ImageBuildState) Open() bool { return s == ImageBuildRequested || s == ImageBuildRunning }

// ImageBuild is a request to a location's fleet agent to build the Windows
// image of its host from the owner's ISO, with the progress the agent reports.
// It holds references only: the ISO is a path on the agent's host and its
// checksum, never the file, and no credential of the host. The result is an
// unsigned manifest on that host: a person registers and signs it through the
// image commands, and nothing here does.
type ImageBuild struct {
	ID         uuid.UUID       `json:"id"`
	AccountID  uuid.UUID       `json:"account_id,omitzero"` // the location's: zero for an instance-wide one
	ProviderID uuid.UUID       `json:"provider_id"`
	State      ImageBuildState `json:"state"`
	// Version is the version of the image to build, such as 2026.11.1.
	Version string `json:"version"`
	// ISO is an absolute path on the agent's host and ISOChecksum its
	// sha256:<hex>, from the owner's licence portal.
	ISO         string `json:"iso"`
	ISOChecksum string `json:"iso_checksum"`
	// ChocolateySHA256 is the hash of the Chocolatey install.ps1 the person reviewed.
	ChocolateySHA256 string `json:"chocolatey_sha256"`
	WindowsImageName string `json:"windows_image_name,omitempty"`
	// LicenseConfirmedBy is the person who stated that their licence covers
	// building and running the image (domain.Actor.String()).
	LicenseConfirmedBy string `json:"license_confirmed_by"`
	// Step, Percent and Error are what the agent reported from progress.json;
	// Error is its words, unchecked.
	Step    string `json:"step,omitempty"`
	Percent int    `json:"percent"`
	Error   string `json:"error,omitempty"`
	// Manifest is the path of the unsigned manifest on the agent's host, and
	// Digest the image's digest, once the build is done.
	Manifest   string    `json:"manifest,omitempty"`
	Digest     string    `json:"digest,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

var checksumRE = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
var sha256HexRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var windowsEditionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,63}$`)

// absolutePath reports a path an agent on Windows or on Linux takes as absolute,
// with no control character: forecrewd runs on neither and only checks the form.
func absolutePath(p string) bool {
	if p == "" || len(p) > 1024 || strings.IndexFunc(p, unicode.IsControl) >= 0 || strings.Contains(p, "..") {
		return false
	}
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || (len(p) > 2 && p[1] == ':' && (p[2] == '\\' || p[2] == '/'))
}

// Validate checks what a person gave, wrapping ErrInvalidRecord.
func (b ImageBuild) Validate() error {
	var p problems
	checkEnum(&p, "state", b.State, ImageBuildStates())
	if !imageVersionRE.MatchString(b.Version) {
		p.addf("version %q is not a version such as 2026.11.1", b.Version)
	}
	if !absolutePath(b.ISO) {
		p.addf("iso must be an absolute path on the host of the location, without ..")
	}
	if !checksumRE.MatchString(b.ISOChecksum) {
		p.addf("iso_checksum must be sha256:<64 hex digits>, from your licence portal")
	}
	if !sha256HexRE.MatchString(b.ChocolateySHA256) {
		p.addf("chocolatey_sha256 must be the 64 hex digits of the install.ps1 you reviewed")
	}
	if b.WindowsImageName != "" && !windowsEditionRE.MatchString(b.WindowsImageName) {
		p.addf("windows_image_name %q is not an edition name", b.WindowsImageName)
	}
	if b.LicenseConfirmedBy == "" {
		p.addf("the licence must be confirmed by a person")
	}
	if b.Percent < 0 || b.Percent > 100 {
		p.addf("percent must be 0 to 100")
	}
	return p.errOf(ErrInvalidRecord, "image build")
}
