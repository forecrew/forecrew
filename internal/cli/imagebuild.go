package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
)

// imageBuildCommands are the commands of the image pipeline that work on
// local files and need no server: the key, the manifest, the signature and
// the root filesystem.
func imageBuildCommands() []*cobra.Command {
	return []*cobra.Command{imageKeygenCommand(), imageManifestCommand(), imageSignCommand(), imageVerifyCommand(), imageRootfsCommand()}
}

func imageKeygenCommand() *cobra.Command {
	var out, public string
	cmd := &cobra.Command{
		Use:   "keygen --out <private-key-file>",
		Short: "Make a signing key for images; prints the public key fleet agents trust",
		Long: "Make an Ed25519 signing key. Keep the private key off the machines that run jobs: a person signs images with it. " +
			"The public key line goes in the local configuration of the fleet agents that may use your images.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pub, key, err := images.GenerateKey()
			if err != nil {
				return err
			}
			if err := images.WritePrivateKey(out, key); err != nil {
				return err
			}
			line := images.FormatPublicKey(pub)
			if public != "" {
				if err := os.WriteFile(public, []byte(line+"\n"), 0o644); err != nil { // #nosec G306 -- a public key
					return err
				}
			}
			cmd.Println(line)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "where to write the private key (required; never overwritten)")
	cmd.Flags().StringVar(&public, "public", "", "also write the public key line to this file")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func imageManifestCommand() *cobra.Command {
	var b images.Build
	var osName, arch, format, builtAt, out, key string
	var packages []string
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Write the manifest of an image you built: its digest, contents and build time",
		Long: "Write the manifest of an image: it computes the digest of --file (and of --kernel for a rootfs image). " +
			"With --key it signs the manifest too; otherwise a person signs it later with forecrew image sign.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b.OS, b.Arch, b.Format = domain.MachineOS(osName), domain.MachineArch(arch), domain.ImageFormat(format)
			b.BuiltAt = time.Now()
			if builtAt != "" {
				t, err := time.Parse(time.RFC3339, builtAt)
				if err != nil {
					return fmt.Errorf("--built-at must be RFC 3339: %w", err)
				}
				b.BuiltAt = t
			}
			for _, p := range packages {
				name, version, ok := strings.Cut(p, "=")
				if !ok {
					return fmt.Errorf("--package %q must be name=version", p)
				}
				b.Packages = append(b.Packages, domain.ImagePackage{Name: name, Version: version})
			}
			m, err := images.NewManifest(b)
			if err != nil {
				return err
			}
			if key != "" {
				k, err := images.ReadPrivateKey(key)
				if err != nil {
					return err
				}
				m = m.SignedWith(k)
			}
			return writeManifest(cmd, m, out)
		},
	}
	f := cmd.Flags()
	f.StringVar(&b.Name, "name", "", "image name (required)")
	f.StringVar(&b.Version, "version", "", "image version (required)")
	f.StringVar(&osName, "os", "linux", "linux, windows or macos")
	f.StringVar(&arch, "arch", "amd64", "amd64 or arm64")
	f.StringVar(&format, "format", "", "rootfs, qcow2, vhdx, ami or tart (required)")
	f.StringVar(&b.File, "file", "", "the image file, whose digest goes in the manifest")
	f.StringVar(&b.Digest, "digest", "", "sha256:<hex> of the build's manifest, for an image that is not one file")
	f.StringVar(&b.URI, "uri", "", "where the image will be fetched from, without credentials (required)")
	f.StringVar(&b.Kernel, "kernel", "", "the kernel file of a rootfs image")
	f.StringVar(&b.KernelURI, "kernel-uri", "", "where the kernel will be fetched from")
	f.StringSliceVar(&b.Labels, "label", nil, "a label the image offers, for example docker or runtime:codex (repeatable)")
	f.StringSliceVar(&packages, "package", nil, "a package in the image, name=version (repeatable)")
	f.StringVar(&builtAt, "built-at", "", "build time, RFC 3339; default now")
	f.StringVar(&out, "out", "", "write the manifest here; default standard output")
	f.StringVar(&key, "key", "", "sign with this private key")
	for _, required := range []string{"name", "version", "format", "uri"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}

func writeManifest(cmd *cobra.Command, m images.Manifest, out string) error {
	data, err := m.Encode()
	if err != nil {
		return err
	}
	if out == "" {
		_, err = cmd.OutOrStdout().Write(data)
		return err
	}
	return os.WriteFile(out, data, 0o644) // #nosec G306 -- a manifest holds no secret
}

func imageSignCommand() *cobra.Command {
	var key, out string
	cmd := &cobra.Command{
		Use:   "sign <manifest.json> --key <private-key-file>",
		Short: "Sign a manifest, replacing any signature it has",
		Long: "Sign a manifest after checking it. This is the step a person does for an image that was built on a host, " +
			"such as a Windows image from your own ISO: look at what it holds first.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := readManifest(args[0])
			if err != nil {
				return err
			}
			if err := m.Validate(); err != nil {
				return err
			}
			k, err := images.ReadPrivateKey(key)
			if err != nil {
				return err
			}
			if out == "" {
				out = args[0]
			}
			return writeManifest(cmd, m.SignedWith(k), out)
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "private key file (required)")
	cmd.Flags().StringVar(&out, "out", "", "write the signed manifest here; default replaces the input")
	_ = cmd.MarkFlagRequired("key")
	return cmd
}

func imageVerifyCommand() *cobra.Command {
	var trusted, file string
	cmd := &cobra.Command{
		Use:   "verify <manifest.json> --trusted <public-keys-file> [--file <image>]",
		Short: "Check a manifest's signature, and an image file against its digest, as a fleet agent does",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := readManifest(args[0])
			if err != nil {
				return err
			}
			text, err := os.ReadFile(trusted) // #nosec G304 -- the operator's file
			if err != nil {
				return err
			}
			keys, err := images.ParsePublicKeys(string(text))
			if err != nil {
				return err
			}
			img := m.Image(domain.NewID())
			if file != "" {
				err = images.VerifyFile(img, file, keys)
			} else {
				err = images.Verify(img, keys)
			}
			if err != nil {
				return err
			}
			cmd.Printf("%s@%s is signed by a trusted key\n", m.Name, m.Version)
			return nil
		},
	}
	cmd.Flags().StringVar(&trusted, "trusted", "", "file of trusted public keys, one ed25519:<base64> per line (required)")
	cmd.Flags().StringVar(&file, "file", "", "also check this image file against the signed digest")
	_ = cmd.MarkFlagRequired("trusted")
	return cmd
}

func imageRootfsCommand() *cobra.Command {
	var from, tarFile, worker, cache, name, mke2fs string
	var labels, envs []string
	var size int
	var noCLIs bool
	cmd := &cobra.Command{
		Use:   "rootfs (--from <docker-image> | --tar <file>) --worker <forecrew-worker>",
		Short: "Make the ext4 root filesystem a microVM boots from a Docker image",
		Long: "Convert a Docker image, such as an environment's forecrew-env:<hash> or a toolbox image, into an ext4 file system " +
			"with the worker and its start-up files added. The result is cached by the image's content, the added files and the size, " +
			"so the same environment is converted once. It needs mke2fs 1.47.1 or newer with libarchive (libarchive13t64 on Debian and Ubuntu), and the image must have sh, ip, git, CA certificates and the agent CLIs: the command says what is missing. " +
			"--worker is a static forecrew-worker binary for the image's architecture. Prints the file's path.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (from == "") == (tarFile == "") {
				return errors.New("give exactly one of --from and --tar")
			}
			if cache == "" {
				dir, err := os.UserCacheDir()
				if err != nil {
					return fmt.Errorf("no cache directory: pass --cache: %w", err)
				}
				cache = filepath.Join(dir, "forecrew-images")
			}
			files, err := images.Guest(worker, labels)
			if err != nil {
				return err
			}
			c := &images.Converter{CacheDir: cache, Docker: images.DockerCLI{}, Mke2fs: mke2fs}
			req := images.Request{Ref: from, Files: files, SizeMB: size, Env: envs, NoAgentCLIs: noCLIs}
			var res images.Result
			if from != "" {
				res, err = c.Convert(cmd.Context(), req)
			} else {
				res, err = convertTarFile(cmd, c, tarFile, name, req)
			}
			if err != nil {
				return err
			}
			cmd.Println(res.Path)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "a Docker image reference")
	f.StringVar(&tarFile, "tar", "", "the exported file system of an image, for example from docker export")
	f.StringVar(&name, "name", "", "with --tar: a name for the cached file; default the tar's file name")
	f.StringVar(&worker, "worker", "", "the forecrew-worker binary to put in the image (required)")
	f.StringSliceVar(&labels, "label", nil, "a label the image offers, for example docker or runtime:codex (repeatable)")
	f.StringVar(&cache, "cache", "", "where converted files are kept; default the user cache directory")
	f.StringVar(&mke2fs, "mke2fs", "", "the mke2fs command; default the one on PATH")
	f.StringSliceVar(&envs, "env", nil, "with --tar: a NAME=value the image's processes get (a Docker image's own environment is read from Docker)")
	f.BoolVar(&noCLIs, "no-agent-clis", false, "do not require the agent CLIs: the image is for command jobs only")
	f.IntVar(&size, "size", 0, "file system size in MB; default sized from the content")
	_ = cmd.MarkFlagRequired("worker")
	return cmd
}

func convertTarFile(cmd *cobra.Command, c *images.Converter, tarFile, name string, req images.Request) (images.Result, error) {
	id, err := images.FileDigest(tarFile)
	if err != nil {
		return images.Result{}, err
	}
	f, err := os.Open(tarFile) // #nosec G304 -- the operator's file
	if err != nil {
		return images.Result{}, err
	}
	defer func() { _ = f.Close() }()
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(tarFile), filepath.Ext(tarFile))
	}
	return c.ConvertTar(cmd.Context(), f, name, id, req)
}
