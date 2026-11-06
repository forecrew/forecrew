package fleet_test

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestNewJoinCode(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)
	seen := map[string]bool{}
	chars := map[rune]bool{}
	for range 2000 {
		code, hash := fleet.NewJoinCode()
		if !shape.MatchString(code) {
			t.Fatalf("code %q", code)
		}
		if !bytes.Equal(hash, fleet.HashJoinCode(code)) {
			t.Fatalf("hash of %q", code)
		}
		if seen[code] {
			t.Fatalf("%q came twice", code)
		}
		seen[code] = true
		for _, c := range strings.ReplaceAll(code, "-", "") {
			chars[c] = true
		}
	}
	if len(chars) != 32 { // every character of the alphabet, so all 5 bits vary
		t.Fatalf("%d characters in use", len(chars))
	}
}

func TestHashJoinCodeForgivesTyping(t *testing.T) {
	want := fleet.HashJoinCode("K7PQ-1M0X")
	for _, typed := range []string{"K7PQ1M0X", "k7pq-1m0x", " k7pq 1m0x", "K7PQ-IMOX", "K7PQ-LMOX"} {
		if !bytes.Equal(fleet.HashJoinCode(typed), want) {
			t.Errorf("%q hashes differently", typed)
		}
	}
	if bytes.Equal(fleet.HashJoinCode("K7PQ-1M0Y"), want) {
		t.Error("another code hashes the same")
	}
}

func TestJoin(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account, _ := e.org("a")
	now := e.clock.Now()
	code, hash := fleet.NewJoinCode()
	c := domain.ProviderJoinCode{AccountID: account, CodeHash: hash, ExpiresAt: now.Add(time.Hour)}
	must(t, e.st.ProviderJoinCodes().Create(ctx, &c))
	req := fleet.JoinRequest{Code: code, Claim: "mine", Kind: domain.ProviderMicroVM, Facts: domain.ProviderCapabilities{Host: "nas"}}

	if _, err := fleet.Join(ctx, e.st, now, fleet.JoinRequest{Code: "NOPE-NOPE", Claim: "mine"}); !errors.Is(err, fleet.ErrJoinRefused) {
		t.Errorf("unknown code: %v", err)
	}
	token, err := fleet.Join(ctx, e.st, now, req)
	must(t, err)
	if token != "" {
		t.Fatal("a token nobody confirmed")
	}
	stolen := req
	stolen.Claim = "theirs"
	if _, err := fleet.Join(ctx, e.st, now, stolen); !errors.Is(err, fleet.ErrJoinClaimed) {
		t.Errorf("second agent: %v", err)
	}
	req.Facts.Host = "nas-2"
	token, err = fleet.Join(ctx, e.st, now, req) // waiting updates the facts
	must(t, err)
	got, err := e.st.ProviderJoinCodes().Get(ctx, c.ID)
	must(t, err)
	if token != "" || got.Facts.Host != "nas-2" || got.Kind != domain.ProviderMicroVM {
		t.Fatalf("code = %+v", got)
	}

	p := domain.CapacityProvider{AccountID: account, Name: "home", Kind: domain.ProviderMicroVM, Mode: domain.ProviderModeAgent}
	must(t, e.st.CapacityProviders().Create(ctx, &p))
	must(t, e.st.ProviderJoinCodes().Confirm(ctx, c.ID, p.ID))

	if _, err := fleet.Join(ctx, e.st, now, stolen); !errors.Is(err, fleet.ErrJoinClaimed) {
		t.Errorf("second agent after the confirmation: %v", err)
	}
	if _, err := fleet.Join(ctx, e.st, now.Add(2*time.Hour), req); !errors.Is(err, fleet.ErrJoinRefused) {
		t.Errorf("expired: %v", err)
	}
	req.Facts.Host = "changed after the confirmation"
	token, err = fleet.Join(ctx, e.st, now, req)
	must(t, err)
	if !strings.HasPrefix(token, "fcp_") || len(token) != 47 {
		t.Fatalf("token of %d characters", len(token))
	}
	stored, err := e.st.CapacityProviders().GetByTokenHash(ctx, mcp.HashToken(token))
	must(t, err)
	if stored.ID != p.ID || stored.Status != domain.ProviderActive {
		t.Fatalf("provider = %+v", stored)
	}
	got, err = e.st.ProviderJoinCodes().Get(ctx, c.ID)
	must(t, err)
	if got.Facts.Host != "nas-2" {
		t.Errorf("facts changed after a person confirmed them: %q", got.Facts.Host)
	}
	if _, err := fleet.Join(ctx, e.st, now, req); !errors.Is(err, fleet.ErrJoinRefused) {
		t.Errorf("once only: %v", err)
	}
}

// A provider revoked after the confirmation gets no token.
func TestJoinOfARevokedProvider(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account, _ := e.org("a")
	now := e.clock.Now()
	code, hash := fleet.NewJoinCode()
	c := domain.ProviderJoinCode{AccountID: account, CodeHash: hash, ExpiresAt: now.Add(time.Hour)}
	must(t, e.st.ProviderJoinCodes().Create(ctx, &c))
	req := fleet.JoinRequest{Code: code, Claim: "mine", Kind: domain.ProviderAWS}
	_, err := fleet.Join(ctx, e.st, now, req)
	must(t, err)
	p := domain.CapacityProvider{AccountID: account, Name: "home", Kind: domain.ProviderAWS, Mode: domain.ProviderModeAgent}
	must(t, e.st.CapacityProviders().Create(ctx, &p))
	must(t, e.st.ProviderJoinCodes().Confirm(ctx, c.ID, p.ID))
	must(t, e.st.CapacityProviders().SetStatus(ctx, p.ID, domain.ProviderRevoked))
	if _, err := fleet.Join(ctx, e.st, now, req); !errors.Is(err, fleet.ErrJoinRefused) {
		t.Fatalf("join: %v", err)
	}
	if got, err := e.st.CapacityProviders().Get(ctx, p.ID); err != nil || got.Status != domain.ProviderRevoked {
		t.Fatalf("provider = %+v, %v", got, err)
	}
}
