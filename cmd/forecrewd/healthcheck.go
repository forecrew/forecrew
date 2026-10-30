package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/forecrew/forecrew/internal/config"
)

const healthcheckTimeout = 5 * time.Second

// runHealthcheck implements "forecrewd healthcheck": it asks the local
// forecrewd named by the config for /healthz and returns 0 when it is
// healthy, 1 otherwise. Container health checks run it.
func runHealthcheck(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("forecrewd healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "forecrew.yaml", "path to forecrew.yaml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := config.Load(*path, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	target, serverName := healthTarget(cfg)
	client := &http.Client{
		Timeout:   healthcheckTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}},
	}
	if err := checkHealth(ctx, client, target); err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	return 0
}

// healthTarget returns the loopback /healthz URL of the configured listener
// and, in TLS mode, the domain its certificate is checked against.
func healthTarget(cfg *config.Config) (target, serverName string) {
	_, port, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		port = "80"
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", port), Path: "/healthz"}
	if cfg.Server.TLSDomain != "" {
		u.Scheme = "https"
		serverName = cfg.Server.TLSDomain
	}
	return u.String(), serverName
}

// checkHealth returns nil when GET target answers 200.
func checkHealth(ctx context.Context, client *http.Client, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", target, resp.Status)
	}
	return nil
}
