package testkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // git blob IDs are SHA-1
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/github"
)

// refFiles returns the files at ref, by path: the tree of ref in the remote
// of a repo that has one (UseRemote), then the files set with SetFile.
func (r *fakeRepo) refFiles(ref string) map[string][]byte {
	out := r.remoteFiles(ref)
	prefix := ref + "\x00"
	for k, v := range r.files {
		if p, ok := strings.CutPrefix(k, prefix); ok {
			out[p] = v
		}
	}
	return out
}

// remoteFiles returns the files of commit ref in the remote, nothing when the
// repo has no remote or the remote has no such commit.
func (r *fakeRepo) remoteFiles(ref string) map[string][]byte {
	out := map[string][]byte{}
	if r.remote == "" {
		return out
	}
	listing, err := gitRaw(r.remote, "ls-tree", "-r", "-z", ref)
	if err != nil {
		return out
	}
	for entry := range bytes.SplitSeq(listing, []byte{0}) {
		meta, name, ok := bytes.Cut(entry, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		if content, err := gitRaw(r.remote, "cat-file", "blob", fields[2]); err == nil {
			out[string(name)] = content
		}
	}
	return out
}

// blobSHA is the git object ID of a file's content.
func blobSHA(content []byte) string {
	h := sha1.New() //nolint:gosec // git blob IDs are SHA-1
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// SetHeadOfBranch simulates a push to branch: HeadSHA now returns sha.
func (t *Tracker) SetHeadOfBranch(repo, branch, sha string) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.heads[branch] = sha
		return nil
	})
}

// SetLastCommit sets the commit LastCommit returns for path.
func (t *Tracker) SetLastCommit(repo, path string, c github.Commit) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.commits[path] = c
		return nil
	})
}

// Tree implements github.Tracker: the files set with SetFile at ref, with
// their real git blob IDs.
func (t *Tracker) Tree(_ context.Context, repo, ref string) ([]github.TreeEntry, error) {
	var out []github.TreeEntry
	err := t.with(repo, func(r *fakeRepo) error {
		files := r.refFiles(ref)
		if len(files) == 0 {
			return fmt.Errorf("tree of %s: %w", ref, github.ErrNotFound)
		}
		for p, b := range files {
			out = append(out, github.TreeEntry{Path: p, SHA: blobSHA(b), Size: len(b)})
		}
		slices.SortFunc(out, func(a, b github.TreeEntry) int { return strings.Compare(a.Path, b.Path) })
		return nil
	})
	return out, err
}

// Compare implements github.Tracker over the files set at the two refs. A
// removed and an added file with the same content are one rename. The patch
// lists the lines that left (-) and the lines that came (+), without
// context.
func (t *Tracker) Compare(_ context.Context, repo, base, head string) (github.Comparison, error) {
	var out github.Comparison
	err := t.with(repo, func(r *fakeRepo) error {
		from, to := r.refFiles(base), r.refFiles(head)
		if len(from) == 0 || len(to) == 0 {
			return fmt.Errorf("compare %s...%s: %w", base, head, github.ErrNotFound)
		}
		out.Files = compareFiles(from, to)
		return nil
	})
	return out, err
}

func compareFiles(from, to map[string][]byte) []github.FileChange {
	removed := map[string][]byte{}
	for p, b := range from {
		if _, ok := to[p]; !ok {
			removed[p] = b
		}
	}
	var out []github.FileChange
	for _, p := range slices.Sorted(maps.Keys(to)) {
		b, old := to[p], from[p]
		switch {
		case old != nil && bytes.Equal(b, old):
		case old != nil:
			out = append(out, github.FileChange{Path: p, Status: github.FileModified, SHA: blobSHA(b), Patch: patch(old, b)})
		default:
			if prev := renamedFrom(removed, b); prev != "" {
				delete(removed, prev)
				out = append(out, github.FileChange{Path: p, PreviousPath: prev, Status: github.FileRenamed, SHA: blobSHA(b)})
				continue
			}
			out = append(out, github.FileChange{Path: p, Status: github.FileAdded, SHA: blobSHA(b), Patch: patch(nil, b)})
		}
	}
	for _, p := range slices.Sorted(maps.Keys(removed)) {
		out = append(out, github.FileChange{Path: p, Status: github.FileRemoved, SHA: blobSHA(removed[p]), Patch: patch(removed[p], nil)})
	}
	return out
}

func renamedFrom(removed map[string][]byte, content []byte) string {
	for p, b := range removed {
		if bytes.Equal(b, content) {
			return p
		}
	}
	return ""
}

func patch(old, now []byte) string {
	var b strings.Builder
	oldLines, newLines := strings.Split(string(old), "\n"), strings.Split(string(now), "\n")
	for _, l := range oldLines {
		if l != "" && !slices.Contains(newLines, l) {
			b.WriteString("-" + l + "\n")
		}
	}
	for _, l := range newLines {
		if l != "" && !slices.Contains(oldLines, l) {
			b.WriteString("+" + l + "\n")
		}
	}
	return b.String()
}

// LastCommit implements github.Tracker: the commit set with SetLastCommit,
// else a commit by octocat, for a file that exists at ref.
func (t *Tracker) LastCommit(_ context.Context, repo, ref, path string) (github.Commit, error) {
	var out github.Commit
	err := t.with(repo, func(r *fakeRepo) error {
		if _, ok := r.files[fileKey(ref, path)]; !ok {
			return fmt.Errorf("last commit of %s at %s: %w", path, ref, github.ErrNotFound)
		}
		out = github.Commit{SHA: "c0ffee0", Author: "octocat", Time: t.now()}
		if c, ok := r.commits[path]; ok {
			out = c
		}
		return nil
	})
	return out, err
}

// HeadSHA implements github.Tracker: the commit set with SetHeadOfBranch.
func (t *Tracker) HeadSHA(_ context.Context, repo, branch string) (string, error) {
	var out string
	err := t.with(repo, func(r *fakeRepo) error {
		sha, ok := r.heads[branch]
		if !ok {
			sha, ok = r.branchSHA(branch) // a repo with a remote has its branches
		}
		if !ok {
			return fmt.Errorf("branch %s: %w", branch, github.ErrNotFound)
		}
		out = sha
		return nil
	})
	return out, err
}

// Archive implements github.Tracker: the files at ref in a tarball, under
// one top-level folder as GitHub makes it.
func (t *Tracker) Archive(_ context.Context, repo, ref string) (io.ReadCloser, error) {
	var buf bytes.Buffer
	err := t.with(repo, func(r *fakeRepo) error {
		files := r.refFiles(ref)
		if len(files) == 0 {
			return fmt.Errorf("archive of %s: %w", ref, github.ErrNotFound)
		}
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		top := strings.ReplaceAll(strings.ToLower(repo), "/", "-") + "-" + ref
		paths := make([]string, 0, len(files))
		for p := range files {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		for _, p := range paths {
			if err := tw.WriteHeader(&tar.Header{Name: top + "/" + p, Mode: 0o644, Size: int64(len(files[p])), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			if _, err := tw.Write(files[p]); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	})
	return io.NopCloser(&buf), err
}
