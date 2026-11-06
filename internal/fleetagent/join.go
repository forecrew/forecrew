package fleetagent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/retry"
)

// newClaim returns the 32 random bytes that prove on later calls that this is
// the agent that used the code first. It lives in memory: an agent that
// restarts starts over with a new code.
func newClaim() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

// Join reports what the agent found until a person confirms it, and returns the
// provider token. The token is a secret. wait is the time between calls when
// forecrewd names none; zero is a second.
func Join(ctx context.Context, c *Client, code string, kind domain.ProviderKind, facts domain.ProviderCapabilities, log *slog.Logger, wait time.Duration) (string, error) {
	if wait <= 0 {
		wait = time.Second
	}
	claim := newClaim()
	log.InfoContext(ctx, "join code in use: waiting for a person to confirm", "kind", kind, "host", facts.Host)
	for failures := 0; ; {
		answer, err := c.Join(ctx, code, claim, kind, facts)
		next := time.Duration(answer.RetryAfter) * time.Second
		switch {
		case errors.Is(err, ErrRejected):
			return "", fmt.Errorf("the join code is unknown, used or expired: %w", err)
		case err != nil && !retryable(err):
			return "", fmt.Errorf("join: %w", err)
		case err != nil:
			failures++
			next = retry.Backoff(failures, wait, backoffMax)
		case answer.Status == "confirmed" && answer.Token != "":
			return answer.Token, nil
		default:
			failures = 0
		}
		if next <= 0 {
			next = wait
		}
		if err := retry.Sleep(ctx, next); err != nil {
			return "", err
		}
	}
}

// LoadToken returns the token kept at path, or empty when there is none.
func LoadToken(path string) (string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's config
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// SaveToken keeps token at path, readable by the agent's user alone. It appears
// there whole or not at all.
func SaveToken(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".token-*") // 0600
	if err != nil {
		return fmt.Errorf("create token file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.WriteString(token + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	return nil
}

// credentials finds the provider token: the environment, the token file, or a
// join with the code, whose token goes to the token file.
func credentials(ctx context.Context, cfg *config.FleetAgent, c *Client, detect func(context.Context) domain.ProviderCapabilities, log *slog.Logger) (string, error) {
	if cfg.Token != "" {
		return cfg.Token, nil
	}
	token, err := LoadToken(cfg.TokenFile)
	if err != nil || token != "" {
		return token, err
	}
	if cfg.JoinCode == "" {
		return "", fmt.Errorf("no provider token: set %s or %s, or put one in %s", config.EnvProviderToken, config.EnvJoinCode, cfg.TokenFile)
	}
	token, err = Join(ctx, c, cfg.JoinCode, cfg.Kind, detect(ctx), log, 0)
	if err != nil {
		return "", err
	}
	if err := SaveToken(cfg.TokenFile, token); err != nil {
		return "", err
	}
	return token, nil
}

// Serve runs the agent of cfg until ctx ends. A revoked token ends it with
// ErrRejected.
func Serve(ctx context.Context, cfg *config.FleetAgent, version string, log *slog.Logger) error {
	p, err := NewProvider(cfg.Kind)
	if err != nil {
		return err
	}
	detect := func(ctx context.Context) domain.ProviderCapabilities { return Detect(ctx, p, version) }
	token, err := credentials(ctx, cfg, NewClient(cfg.ServerURL, "", nil), detect, log)
	if err != nil {
		return err
	}
	return New(Config{Client: NewClient(cfg.ServerURL, token, nil), Provider: p, Version: version, Logger: log, Detect: detect}).Run(ctx)
}
