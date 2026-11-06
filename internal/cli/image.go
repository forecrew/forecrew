package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/mcp"
)

// imageCommand is `forecrew image`: list, register and remove images through
// /api/v1, and the commands that build them on a machine of the build:
// keygen, manifest, sign, verify and rootfs (imagebuild.go). Registering is
// for a person: it needs a person's token with worker.manage.
func imageCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "Machine images: list, register, sign, build"}

	var name, osName, arch, format string
	list := &cobra.Command{
		Use:   "list",
		Short: "List the images of the organization and the instance",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := listImages(cmd, r, name, osName, arch, format)
			if err != nil {
				return err
			}
			printImages(cmd.OutOrStdout(), items)
			return nil
		},
	}
	list.Flags().StringVar(&name, "name", "", "only this image name")
	list.Flags().StringVar(&osName, "os", "", "linux, windows or macos")
	list.Flags().StringVar(&arch, "arch", "", "amd64 or arm64")
	list.Flags().StringVar(&format, "format", "", "rootfs, qcow2, vhdx, ami or tart")

	var instance bool
	add := &cobra.Command{
		Use:   "add <manifest.json>",
		Short: "Register a signed image for the organization (--instance: for every organization, instance admins only)",
		Long: "Register an image from its signed manifest (forecrew image manifest, forecrew image sign). The server checks the form; " +
			"the fleet agent checks the signature against the keys it trusts before it uses the image.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := readManifest(args[0])
			if err != nil {
				return err
			}
			if !m.Signed() {
				return errors.New("the manifest is not signed: run forecrew image sign first")
			}
			var out mcp.ImageInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/images", images.AddInput{Manifest: m, Instance: instance}, &out); err != nil {
				return err
			}
			cmd.Printf("Registered %s@%s (%s, %s, %s) as %s\n", out.Name, out.Version, out.OS, out.Arch, out.Format, out.ID)
			return nil
		},
	}
	add.Flags().BoolVar(&instance, "instance", false, "register it for every organization of the instance")

	remove := &cobra.Command{
		Use:   "delete <image-id>",
		Short: "Remove an image; refused while a live machine uses it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out mcp.ImageInfo
			if err := r.call(cmd.Context(), http.MethodDelete, "/images/"+url.PathEscape(args[0]), nil, &out); err != nil {
				return err
			}
			cmd.Printf("Removed %s@%s (%s)\n", out.Name, out.Version, out.ID)
			return nil
		},
	}

	var keep int
	var dry, withInstance bool
	prune := &cobra.Command{
		Use:   "prune",
		Short: "Remove old builds: keep the newest few of each image (--keep, default 3)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := listImages(cmd, r, "", "", "", "")
			if err != nil {
				return err
			}
			var candidates []mcp.ImageInfo
			for _, i := range items {
				if withInstance || i.Scope == "organization" {
					candidates = append(candidates, i)
				}
			}
			return pruneImages(cmd, r, candidates, images.Retention{Keep: keep}, dry)
		},
	}
	prune.Flags().IntVar(&keep, "keep", images.DefaultKeep, "builds of each image to keep")
	prune.Flags().BoolVar(&dry, "dry-run", false, "only say what would be removed")
	prune.Flags().BoolVar(&withInstance, "instance", false, "include the instance's images (instance admins only)")

	cmd.AddCommand(list, add, remove, prune)
	cmd.AddCommand(imageBuildCommands()...)
	return cmd
}

func listImages(cmd *cobra.Command, r *remote, name, osName, arch, format string) ([]mcp.ImageInfo, error) {
	q := url.Values{}
	for k, v := range map[string]string{"name": name, "os": osName, "arch": arch, "format": format} {
		if v != "" {
			q.Set(k, v)
		}
	}
	q.Set("limit", strconv.Itoa(500))
	var items []mcp.ImageInfo
	err := r.call(cmd.Context(), http.MethodGet, "/images?"+q.Encode(), nil, &items)
	return items, err
}

// pruneImages removes what the rule expires from items. An image a live
// machine uses answers 409 and is kept.
func pruneImages(cmd *cobra.Command, r *remote, items []mcp.ImageInfo, rule images.Retention, dry bool) error {
	plain := make([]domain.Image, 0, len(items))
	for _, i := range items {
		plain = append(plain, i.Image)
	}
	expired := rule.Expired(plain)
	if len(expired) == 0 {
		cmd.Println("Nothing to remove.")
		return nil
	}
	var failed int
	for _, e := range expired {
		label := fmt.Sprintf("%s@%s (%s, %s)", e.Name, e.Version, e.Arch, e.Format)
		if dry {
			cmd.Printf("Would remove %s\n", label)
			continue
		}
		var out mcp.ImageInfo
		err := r.call(cmd.Context(), http.MethodDelete, "/images/"+url.PathEscape(e.ID.String()), nil, &out)
		switch ae, ok := errors.AsType[*apiError](err); {
		case err == nil:
			cmd.Printf("Removed %s\n", label)
		case ok && ae.Status == http.StatusConflict:
			cmd.Printf("Kept %s: a live machine uses it\n", label)
		default:
			failed++
			cmd.PrintErrf("Could not remove %s: %v\n", label, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d images could not be removed", failed)
	}
	return nil
}

func printImages(w io.Writer, items []mcp.ImageInfo) {
	rows := [][]string{{"NAME", "VERSION", "OS", "ARCH", "FORMAT", "SCOPE", "BUILT", "ID", "LABELS"}}
	for _, i := range items {
		rows = append(rows, []string{safeText(i.Name), safeText(i.Version), string(i.OS), string(i.Arch), string(i.Format), i.Scope,
			i.BuiltAt.Format("2006-01-02"), i.ID.String(), dash(safeText(strings.Join(i.Contents.Labels, ",")))})
	}
	table(w, rows)
}

func readManifest(path string) (images.Manifest, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator's file
	if err != nil {
		return images.Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, images.MaxManifestBytes+1))
	if err != nil {
		return images.Manifest{}, err
	}
	if len(data) > images.MaxManifestBytes {
		return images.Manifest{}, fmt.Errorf("%s is larger than %d bytes", path, images.MaxManifestBytes)
	}
	return images.ParseManifest(data)
}
