//go:build e2e

// Package e2e runs forecrewd, real worker daemons and the real embedded
// playbooks together against Postgres, with a fake GitHub backed by local
// git repositories and a scripted runtime in place of the agents. Run it
// with `make test-e2e`.
package e2e
