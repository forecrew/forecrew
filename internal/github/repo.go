package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	gh "github.com/google/go-github/v93/github"
)

// Tree implements Tracker.
func (c *Client) Tree(ctx context.Context, repo, ref string) ([]TreeEntry, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	op := fmt.Sprintf("list files of %s at %s", repo, ref)
	tree, _, err := g.Git.GetTree(ctx, owner, name, ref, true)
	if err != nil {
		return nil, wrap(op, err)
	}
	if tree.GetTruncated() {
		return nil, fmt.Errorf("%s: %w: GitHub truncated the tree", op, ErrTooLarge)
	}
	var out []TreeEntry
	for _, e := range tree.Entries {
		if e.GetType() == "blob" {
			out = append(out, TreeEntry{Path: e.GetPath(), SHA: e.GetSHA(), Size: e.GetSize()})
		}
	}
	slices.SortFunc(out, func(a, b TreeEntry) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// Compare implements Tracker.
func (c *Client) Compare(ctx context.Context, repo, base, head string) (Comparison, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return Comparison{}, err
	}
	op := fmt.Sprintf("compare %s...%s of %s", base, head, repo)
	var out Comparison
	opts := gh.ListOptions{PerPage: 100}
	for {
		cmp, resp, err := g.Repositories.CompareCommits(ctx, owner, name, base, head, &opts)
		if err != nil {
			return Comparison{}, wrap(op, err)
		}
		for _, f := range cmp.Files {
			out.Files = append(out.Files, FileChange{Path: f.GetFilename(), PreviousPath: f.GetPreviousFilename(), Status: f.GetStatus(), SHA: f.GetSHA(), Patch: f.GetPatch()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// LastCommit implements Tracker.
func (c *Client) LastCommit(ctx context.Context, repo, ref, path string) (Commit, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return Commit{}, err
	}
	op := fmt.Sprintf("last commit of %s at %s of %s", path, ref, repo)
	commits, _, err := g.Repositories.ListCommits(ctx, owner, name, &gh.CommitsListOptions{SHA: ref, Path: path, PerPage: 1})
	if err != nil {
		return Commit{}, wrap(op, err)
	}
	if len(commits) == 0 {
		return Commit{}, fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	rc := commits[0]
	out := Commit{SHA: rc.GetSHA(), Author: rc.GetAuthor().GetLogin(), Bot: rc.GetAuthor().GetType() == "Bot", Time: rc.GetCommit().GetAuthor().GetDate().UTC()}
	if out.Author == "" {
		out.Author = rc.GetCommit().GetAuthor().GetName()
	}
	return out, nil
}

// HeadSHA implements Tracker.
func (c *Client) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return "", err
	}
	ref, _, err := g.Git.GetRef(ctx, owner, name, "heads/"+branch)
	if statusOf(err) == http.StatusConflict {
		return "", fmt.Errorf("get branch %q of %s: %w: %w", branch, repo, ErrEmptyRepo, err)
	}
	if err != nil {
		return "", wrap(fmt.Sprintf("get branch %q of %s", branch, repo), err)
	}
	return ref.GetObject().GetSHA(), nil
}

// Archive implements Tracker. GitHub answers the archive request with a
// redirect to a short-lived download URL, which needs no token.
func (c *Client) Archive(ctx context.Context, repo, ref string) (io.ReadCloser, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	op := fmt.Sprintf("download %s at %s", repo, ref)
	link, _, err := g.Repositories.GetArchiveLink(ctx, owner, name, gh.Tarball, &gh.RepositoryContentGetOptions{Ref: ref}, 0)
	if err != nil {
		return nil, wrap(op, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	resp, err := c.http.Do(req) //nolint:gosec // the URL is the download link GitHub returned
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: %w: status %d", op, ErrNotFound, resp.StatusCode)
	}
	return resp.Body, nil
}
