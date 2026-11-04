package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// imageHashVersion changes the hash of every image when the way images are
// built changes.
const imageHashVersion = "1"

// maxContextBytes bounds the build context the worker reads to hash it.
const maxContextBytes = 1 << 30

// imageHash identifies the image built for src: it covers the image
// reference, the setup commands, the Dockerfile's path and contents and
// every file of the build context (their paths, types and contents; the .git
// directory and everything the Dockerfile does not copy are included too,
// there is no .dockerignore handling). Two environments with the same
// inputs share an image. root is the work tree.
func imageHash(ctx context.Context, root string, src envSource) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "forecrew-env %s\nimage %q\ndockerfile %q\ncontext %q\n", imageHashVersion, src.Image, src.Dockerfile, src.Context)
	for _, s := range src.Setup {
		fmt.Fprintf(h, "setup %q\n", s)
	}
	if src.Dockerfile != "" {
		if err := hashRegular(h, root, src.Dockerfile); err != nil {
			return "", fmt.Errorf("dockerfile %s: %w", src.Dockerfile, err)
		}
		if err := hashContext(ctx, h, root, src.Context); err != nil {
			return "", fmt.Errorf("build context %s: %w", src.Context, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashRegular writes the contents of the regular file rel, reached without
// a symlink, to h.
func hashRegular(h io.Writer, root, rel string) error {
	if err := noSymlinks(root, rel); err != nil {
		return err
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
		return errors.New("is not a regular file")
	}
	f, err := os.Open(p) // #nosec G304 -- inside the job's workspace, symlinks refused
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(h, f)
	return err
}

// contextHash walks a build context for hashContext.
type contextHash struct {
	ctx     context.Context
	h       io.Writer
	root    string // the work tree
	rel     string // the context, relative to root
	base    string
	entries int
	total   int64
}

// hashContext writes the entries of the context directory to h in path
// order. Symlinks are hashed as their target text and never followed.
func hashContext(ctx context.Context, h io.Writer, root, rel string) error {
	if rel != "." {
		if err := noSymlinks(root, rel); err != nil {
			return err
		}
	}
	c := &contextHash{ctx: ctx, h: h, root: root, rel: rel, base: filepath.Join(root, filepath.FromSlash(rel))}
	return filepath.WalkDir(c.base, c.visit)
}

func (c *contextHash) visit(p string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if c.entries++; c.entries > maxWalkEntries {
		return errWalkLimit
	}
	if d.Name() == ".git" { // a directory, or the file of a worktree naming a path of this job
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	info, err := d.Info()
	if err != nil {
		return err
	}
	sub, _ := filepath.Rel(c.base, p)
	sub = filepath.ToSlash(sub)
	switch mode := info.Mode(); {
	case mode.IsDir():
		fmt.Fprintf(c.h, "d %q\n", sub)
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.h, "l %q %q\n", sub, target)
	case mode.IsRegular():
		if c.total += info.Size(); c.total > maxContextBytes {
			return fmt.Errorf("larger than %d MiB; set context to a smaller directory", maxContextBytes>>20)
		}
		fmt.Fprintf(c.h, "f %q %d %o\n", sub, info.Size(), mode.Perm())
		return hashRegular(c.h, c.root, path.Join(c.rel, sub))
	default:
		fmt.Fprintf(c.h, "x %q\n", sub)
	}
	return nil
}

// derivedDockerfile returns the Dockerfile that runs the setup commands on
// top of base. Each command is a JSON array so that nothing in it, newlines
// and quotes included, is read as Dockerfile syntax.
func derivedDockerfile(base string, setup []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", base)
	for _, s := range setup {
		cmd, _ := json.Marshal([]string{"sh", "-c", s})
		fmt.Fprintf(&b, "RUN %s\n", cmd)
	}
	return b.String()
}

// errBuild marks an image that could not be pulled or built: the job fails.
var errBuild = errors.New("environment image")

// image makes the image of src available and returns its tag. An image built
// before for the same hash is reused; otherwise the image is pulled or built,
// and the setup commands run in the build. Every image it pulls or builds is
// recorded for pruning.
func (dj *dockerJob) image(ctx context.Context, src envSource) (string, error) {
	hash, err := imageHash(ctx, dj.r.dir, src)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errBuild, err)
	}
	tag := envImageRepo + ":" + hash[:32]
	markers, now := dj.r.w.imageMarkers(), dj.r.w.cfg.Now()
	markers.touch(tag, now)
	if _, err := dj.d.output(ctx, "image", "inspect", "--format", "{{.Id}}", "--", tag); err == nil {
		dj.r.ev.text(domain.EventStatus, "environment image cached")
		return tag, nil
	}
	dj.r.ev.text(domain.EventStatus, "building environment")
	base := src.Image
	if src.Dockerfile != "" {
		base = tag
		if len(src.Setup) > 0 {
			base += "-base"
			markers.touch(base, now)
		}
		err = dj.build(ctx, src, base)
	} else {
		err = dj.pull(ctx, src.Image)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", errBuild, err)
	}
	if len(src.Setup) == 0 {
		if base != tag {
			err = dj.docker(ctx, "tag", "--", base, tag)
		}
		return tag, wrapBuild(err)
	}
	dir, err := os.MkdirTemp("", "forecrew-build-")
	if err != nil {
		return "", fmt.Errorf("%w: %w", errBuild, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(file, []byte(derivedDockerfile(base, src.Setup)), 0o600); err != nil {
		return "", fmt.Errorf("%w: %w", errBuild, err)
	}
	return tag, wrapBuild(dj.docker(ctx, "build", "--tag", tag, "--file", file, dir))
}

// build builds the Dockerfile of src as the image tag. The images its FROM
// lines pull are recorded for pruning unless they were on the daemon before
// and not the worker's.
func (dj *dockerJob) build(ctx context.Context, src envSource, tag string) error {
	file := filepath.Join(dj.r.dir, filepath.FromSlash(src.Dockerfile))
	text, _ := os.ReadFile(file) // #nosec G304 -- hashed above, symlinks refused
	var track []string
	for _, ref := range fromRefs(string(text)) {
		if dj.imageIsOurs(ctx, ref) {
			track = append(track, ref)
		}
	}
	err := dj.docker(ctx, "build", "--tag", tag, "--file", file, filepath.Join(dj.r.dir, filepath.FromSlash(src.Context)))
	if err != nil {
		return err // a name that does not exist must not be recorded: a later prune would remove an image of that name
	}
	for _, ref := range track {
		if _, err := dj.d.output(ctx, "image", "inspect", "--format", "{{.Id}}", "--", ref); err == nil {
			dj.r.w.imageMarkers().touch(ref, dj.r.w.cfg.Now())
		}
	}
	return nil
}

// imageIsOurs reports whether ref is an image the worker may remove when it
// is not used: it is not on the daemon yet (the job will pull it), or it is
// already recorded. An image the operator pulled is left alone.
func (dj *dockerJob) imageIsOurs(ctx context.Context, ref string) bool {
	if dj.r.w.imageMarkers().tracked(ref) {
		return true
	}
	_, err := dj.d.output(ctx, "image", "inspect", "--format", "{{.Id}}", "--", ref)
	return err != nil
}

// pull makes ref available: it pulls it, with the output in the job log,
// unless the daemon has it. The worker records an image it pulled (or already
// recorded) for pruning.
func (dj *dockerJob) pull(ctx context.Context, ref string) error {
	markers := dj.r.w.imageMarkers()
	tracked := markers.tracked(ref)
	_, err := dj.d.output(ctx, "image", "inspect", "--format", "{{.Id}}", "--", ref)
	exists := err == nil
	if !exists {
		if err := dj.docker(ctx, "pull", "--", ref); err != nil {
			return err
		}
	}
	if !exists || tracked {
		markers.touch(ref, dj.r.w.cfg.Now())
	}
	return nil
}

// fromRefs returns the images the FROM lines of a Dockerfile name: not
// scratch, not an earlier stage and not built from an ARG (those stay out of
// the records).
func fromRefs(dockerfile string) []string {
	var refs []string
	stages := map[string]bool{"scratch": true}
	for line := range strings.SplitSeq(dockerfile, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[0], "FROM") {
			continue
		}
		args := f[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			args = args[1:]
		}
		if len(args) == 0 {
			continue
		}
		if ref := args[0]; !stages[strings.ToLower(ref)] && !strings.Contains(ref, "$") && !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
		if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
			stages[strings.ToLower(args[2])] = true
		}
	}
	return refs
}

func wrapBuild(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errBuild, err)
}

// docker runs the docker client with the job's output going to the job log
// and its timeout budget.
func (dj *dockerJob) docker(ctx context.Context, args ...string) error {
	exit, err := dj.r.exec(ctx, dj.d.process(args...))
	if err != nil {
		return fmt.Errorf("docker %s: %w (exit code %d)", args[0], err, exit.Code)
	}
	return nil
}

// imageDigest returns the digest of the image tag when it came from a
// registry, otherwise its ID.
func (d *dockerCLI) imageDigest(ctx context.Context, tag string) (string, error) {
	out, err := d.output(ctx, "image", "inspect", "--format", `{{.Id}} {{join .RepoDigests " "}}`, "--", tag)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	switch len(fields) {
	case 0:
		return "", errors.New("no image id")
	case 1:
		return fields[0], nil
	}
	return fields[1], nil
}
