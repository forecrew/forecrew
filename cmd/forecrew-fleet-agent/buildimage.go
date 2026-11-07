package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"runtime"

	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetagent/imagebuild"
)

// buildImage builds the Windows image of this location from an ISO its owner
// supplies (docs/runbooks/add-provider.md). It takes a long time: progress.json
// in --out says where it is.
func buildImage(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build-image", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o imagebuild.Options
	fs.StringVar(&o.TemplatesDir, "templates", "", "deploy/images of the release this agent is from (absolute path)")
	fs.StringVar(&o.Packer, "packer", "packer", "the packer command")
	fs.StringVar(&o.ISO, "iso", "", "a Windows Server ISO you are licensed to use (absolute path)")
	fs.StringVar(&o.ISOChecksum, "iso-checksum", "", "sha256:<hex> of the ISO, from your licence portal")
	fs.BoolVar(&o.LicenseConfirmed, "license-confirmed", false, "you hold a Windows Server licence that covers building and running this image")
	fs.StringVar(&o.WindowsImageName, "windows-image-name", "", "the edition inside the ISO; default Windows Server 2022 SERVERSTANDARD")
	fs.StringVar(&o.ChocolateyInstallerSHA256, "chocolatey-sha256", "", "SHA-256 of the Chocolatey install.ps1 you reviewed")
	fs.StringVar(&o.Worker, "worker", "", "forecrew-worker.exe for windows/amd64 (absolute path)")
	fs.StringVar(&o.GuestTools, "guest-tools", "", "virtio-win-guest-tools.exe, for an image a Proxmox location starts")
	fs.StringVar(&o.GuestToolsSHA256, "guest-tools-sha256", "", "SHA-256 of that installer")
	fs.StringVar(&o.Version, "version", "", "image version, for example 2026.11.1")
	fs.StringVar(&o.URIBase, "uri-base", "", "where other hosts will fetch the image from")
	fs.StringVar(&o.OutDir, "out", "", "where the build goes (absolute path)")
	fs.StringVar(&o.CacheDir, "cache-dir", "", "a Hyper-V location's image_dir: the image is put there under its digest")
	fs.StringVar(&o.Switch, "switch", "", "Hyper-V: the virtual switch of the build VM, which needs the internet")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	o.GOOS = runtime.GOOS
	res, err := imagebuild.Builder{DiskFreeGB: fleetagent.DiskFreeGB}.Run(ctx, o)
	if err != nil {
		fmt.Fprintf(stderr, "forecrew-fleet-agent: build-image: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "built %s\nunsigned manifest %s (digest %s)\n\n"+
		"Next, on a computer that holds your image signing key, not this host:\n"+
		"  check the manifest, then: forecrew image sign %s --key image.key\n"+
		"  forecrew image add %s\n"+
		"and put the public key in this host's image-keys.pub.\n", res.Image, res.Manifest, res.Digest, res.Manifest, res.Manifest)
	return 0
}
