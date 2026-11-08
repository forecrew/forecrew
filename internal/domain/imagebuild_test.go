package domain

import (
	"errors"
	"strings"
	"testing"
)

func validBuild() ImageBuild {
	return ImageBuild{State: ImageBuildRequested, Version: "2026.11.1", ISO: "/isos/server.iso", ISOChecksum: "sha256:" + strings.Repeat("a", 64),
		ChocolateySHA256: strings.Repeat("b", 64), LicenseConfirmedBy: "user:ana"}
}

func TestImageBuildValidate(t *testing.T) {
	if err := validBuild().Validate(); err != nil {
		t.Fatal(err)
	}
	win := validBuild()
	win.ISO = `D:\isos\server.iso`
	win.WindowsImageName = "Windows Server 2022 SERVERSTANDARD"
	if err := win.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(b *ImageBuild){
		"state":       func(b *ImageBuild) { b.State = "paused" },
		"version":     func(b *ImageBuild) { b.Version = "-1" },
		"relative":    func(b *ImageBuild) { b.ISO = "server.iso" },
		"traversal":   func(b *ImageBuild) { b.ISO = "/isos/../etc/passwd" },
		"control":     func(b *ImageBuild) { b.ISO = "/isos/a\nb.iso" },
		"empty":       func(b *ImageBuild) { b.ISO = "" },
		"checksum":    func(b *ImageBuild) { b.ISOChecksum = "md5:12" },
		"chocolatey":  func(b *ImageBuild) { b.ChocolateySHA256 = "abc" },
		"edition":     func(b *ImageBuild) { b.WindowsImageName = "a;b" },
		"licence":     func(b *ImageBuild) { b.LicenseConfirmedBy = "" },
		"percent":     func(b *ImageBuild) { b.Percent = 101 },
		"percent low": func(b *ImageBuild) { b.Percent = -1 },
	}
	for name, mutate := range tests {
		b := validBuild()
		mutate(&b)
		if err := b.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestImageBuildStates(t *testing.T) {
	for _, tt := range []struct {
		from, to ImageBuildState
		ok       bool
	}{
		{ImageBuildRequested, ImageBuildRunning, true},
		{ImageBuildRequested, ImageBuildFailed, true},
		{ImageBuildRequested, ImageBuildDone, false},
		{ImageBuildRunning, ImageBuildDone, true},
		{ImageBuildRunning, ImageBuildRequested, false},
		{ImageBuildDone, ImageBuildFailed, false},
		{ImageBuildFailed, ImageBuildRunning, false},
	} {
		if got := tt.from.CanTransition(tt.to); got != tt.ok {
			t.Errorf("%s to %s = %v", tt.from, tt.to, got)
		}
	}
	for _, s := range ImageBuildStates() {
		if s.Open() != (s == ImageBuildRequested || s == ImageBuildRunning) {
			t.Errorf("%s open = %v", s, s.Open())
		}
	}
}
