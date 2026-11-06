package images

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// DockerCLI exports images with the docker command line client, which is
// what the worker's host already has and configures.
type DockerCLI struct {
	// Bin is the client, "docker" when empty.
	Bin string
}

func (d DockerCLI) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

func checkRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n") {
		return fmt.Errorf("image reference %q is not valid", ref)
	}
	return nil
}

func (d DockerCLI) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.bin(), args...) // #nosec G204 -- fixed subcommands; references are checked by checkRef
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ID implements Docker.
func (d DockerCLI) ID(ctx context.Context, ref string) (string, error) {
	if err := checkRef(ref); err != nil {
		return "", err
	}
	return d.output(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
}

// Env implements Docker: the image's Config.Env.
func (d DockerCLI) Env(ctx context.Context, ref string) ([]string, error) {
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	out, err := d.output(ctx, "image", "inspect", "--format", "{{json .Config.Env}}", ref)
	if err != nil {
		return nil, err
	}
	var env []string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return nil, fmt.Errorf("docker image inspect: environment of %s: %w", ref, err)
	}
	return env, nil
}

// Export implements Docker: it creates a container that never runs, streams
// its file system and removes it when the stream is closed.
func (d DockerCLI) Export(ctx context.Context, ref string) (io.ReadCloser, error) {
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	// A command is given because an image without one cannot be created.
	id, err := d.output(ctx, "create", "--network", "none", ref, "/nonexistent")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, d.bin(), "export", id) // #nosec G204 -- id is the answer of docker create
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		d.remove(id)
		return nil, fmt.Errorf("docker export: %w", err)
	}
	return &export{ReadCloser: out, cmd: cmd, stderr: &stderr, remove: func() { d.remove(id) }}, nil
}

func (d DockerCLI) remove(id string) {
	// Removing is cleanup; the caller's context may be the one that ended.
	_, _ = d.output(context.WithoutCancel(context.Background()), "rm", "-f", id)
}

// export is the stream of docker export. Close ends the client, reports
// why it failed if it did, and removes the container.
type export struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	remove func()
}

func (e *export) Close() error {
	_ = e.ReadCloser.Close()
	err := e.cmd.Wait()
	e.remove()
	if err != nil {
		return fmt.Errorf("docker export: %w: %s", err, strings.TrimSpace(e.stderr.String()))
	}
	return nil
}
