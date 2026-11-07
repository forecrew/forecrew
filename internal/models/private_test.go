package models_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/testkit"
)

// privateWorld has a cloud connection for the developer, not authorized.
func privateWorld(t *testing.T) (*world, domain.ModelConnection) {
	t.Helper()
	w := newWorld(t)
	c := conn("cloud", config.AdapterClaudeCode, "acme")
	c.AccountID, c.UpdatedBy = w.account, "user:ana"
	if err := w.st.ModelConnections().Put(t.Context(), &c); err != nil {
		t.Fatal(err)
	}
	if err := w.vault.Put(t.Context(), domain.Actor{}, w.account, "models/cloud", []byte("k")); err != nil {
		t.Fatal(err)
	}
	a := org("developer", "cloud")
	a.AccountID, a.UpdatedBy = w.account, "user:ana"
	if err := w.st.ModelAssignments().Put(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func TestPrivateCodeWaitsForAnAuthorization(t *testing.T) {
	w, c := privateWorld(t)
	ctx := t.Context()
	s := models.New(w.st, hosted(fileConfig()), w.vault, nil)
	j := w.job(t, "developer", config.AdapterClaudeCode)
	code, sentence, ok, err := s.WaitReason(ctx, j)
	if err != nil || !ok || code != models.ReasonPrivateCode || !strings.Contains(sentence, "acme/app is private") || !strings.Contains(sentence, "cloud@app") {
		t.Fatalf("wait = %q %q %v %v", code, sentence, ok, err)
	}
	if err := s.Settle(ctx, w.account); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.st.Jobs().Get(ctx, w.account, j.ID); got.Runtime != domain.RuntimeNone {
		t.Fatalf("settled = %+v", got)
	}
	w.authorize(t, c, w.project.ID)
	if err := s.Settle(ctx, w.account); err != nil {
		t.Fatal(err)
	}
	got, _ := w.st.Jobs().Get(ctx, w.account, j.ID)
	if got.Runtime != config.AdapterClaudeCode {
		t.Fatalf("authorized = %+v", got)
	}
	if g, err := s.Grant(ctx, got); err != nil || g.Waiting != "" || g.Connection != "cloud" {
		t.Fatalf("grant = %+v, %v", g, err)
	}
}

func TestAJobOfAMissingProjectCountsAsPrivate(t *testing.T) {
	w, _ := privateWorld(t)
	s := models.New(w.st, hosted(fileConfig()), w.vault, nil)
	d, err := s.Decide(t.Context(), w.account, models.Request{ProjectID: domain.NewID(), Role: "developer"})
	if err != nil || d.Code != models.ReasonPrivateCode {
		t.Fatalf("decide = %+v, %v", d, err)
	}
}

func TestRepoVisibilityIsAskedAndKeptBriefly(t *testing.T) {
	w, _ := privateWorld(t)
	ctx := t.Context()
	clock := testkit.NewClock(time.Now())
	asked := 0
	public, fail := true, error(nil)
	s := models.New(w.st, hosted(fileConfig()), w.vault, nil).WithPublicRepo(func(_ context.Context, repo string) (bool, error) {
		asked++
		return public && repo == "acme/app", fail
	}, clock.Now)
	req := models.Request{ProjectID: w.project.ID, Role: "developer"}
	decide := func() models.Decision {
		t.Helper()
		d, err := s.Decide(ctx, w.account, req)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := decide(); d.Waiting != "" {
		t.Fatalf("public repo waits: %+v", d)
	}
	decide()
	if asked != 1 {
		t.Fatalf("asked %d times", asked)
	}
	// After a while GitHub is asked again; it went private.
	public = false
	clock.Advance(10 * time.Minute)
	if d := decide(); d.Code != models.ReasonPrivateCode {
		t.Fatalf("private repo: %+v", d)
	}
	decide()
	if asked != 3 {
		t.Fatalf("a private answer is kept: asked %d times", asked)
	}
	// An error counts as private.
	public, fail = true, errors.New("down")
	clock.Advance(10 * time.Minute)
	if d := decide(); d.Code != models.ReasonPrivateCode {
		t.Fatalf("on error: %+v", d)
	}
	if s := models.New(w.st, fileConfig(), w.vault, nil).WithPublicRepo(nil, nil); s == nil {
		t.Fatal("nil")
	}
}

func TestLocalEndpoint(t *testing.T) {
	for url, want := range map[string]bool{
		"": false, "http://localhost:11434/v1": true, "http://127.0.0.1:1234": true, "http://10.0.0.2/v1": true, "http://[::1]:8080": true,
		"https://api.example.com": false, "http://169.254.169.254": false, "::bad": false, "https://192.168.1.10/v1": true,
	} {
		if got := models.LocalEndpoint(url); got != want {
			t.Errorf("LocalEndpoint(%q) = %v", url, got)
		}
	}
}

func TestAuthorizationOfEveryProjectCovers(t *testing.T) {
	w, c := privateWorld(t)
	w.authorize(t, c, uuid.Nil())
	s := models.New(w.st, hosted(fileConfig()), w.vault, nil)
	if d, err := s.Decide(t.Context(), w.account, models.Request{ProjectID: w.project.ID, Role: "developer"}); err != nil || d.Waiting != "" {
		t.Fatalf("decide = %+v, %v", d, err)
	}
}
