//go:build live

package github_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

// TestLiveSmoke runs against a sandbox repository where the App is installed:
//
//	FORECREW_LIVE_APP_ID=... FORECREW_LIVE_PRIVATE_KEY_FILE=key.pem \
//	FORECREW_LIVE_REPO=owner/sandbox go test -tags live -run Live ./internal/github
func TestLiveSmoke(t *testing.T) {
	id, _ := strconv.ParseInt(os.Getenv("FORECREW_LIVE_APP_ID"), 10, 64)
	repo := os.Getenv("FORECREW_LIVE_REPO")
	keyFile := os.Getenv("FORECREW_LIVE_PRIVATE_KEY_FILE")
	if id == 0 || repo == "" || keyFile == "" {
		t.Skip("FORECREW_LIVE_APP_ID, FORECREW_LIVE_PRIVATE_KEY_FILE and FORECREW_LIVE_REPO are not set")
	}
	key, err := os.ReadFile(keyFile)
	noErr(t, err)
	c, err := github.New(github.Config{AppID: id, PrivateKey: key})
	noErr(t, err)
	ctx := t.Context()

	ok, err := c.IsInstalled(ctx, repo)
	noErr(t, err)
	if !ok {
		t.Fatalf("app not installed on %s", repo)
	}
	noErr(t, c.EnsureLabel(ctx, repo, github.Label{Name: "forecrew-smoke", Color: "ededed"}))
	issue, err := c.CreateIssue(ctx, repo, github.IssueInput{Title: "forecrew smoke test", Labels: []string{"forecrew-smoke"}})
	noErr(t, err)
	closed := "closed"
	t.Cleanup(func() { _, _ = c.UpdateIssue(ctx, repo, issue.Number, github.IssueUpdate{State: &closed}) })
	_, err = c.CreateComment(ctx, repo, issue.Number, "smoke test")
	noErr(t, err)
	if _, err := c.InstallationToken(ctx, repo, github.AccessRead); err != nil {
		t.Fatal(err)
	}
	if st, err := c.CheckStatus(ctx, repo, "HEAD"); err != nil {
		t.Logf("check status of HEAD: %v", err)
	} else {
		t.Logf("HEAD checks: %+v", st)
	}
}
