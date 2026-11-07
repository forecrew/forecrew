package tart

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/forecrew/forecrew/internal/fleet"
)

// cli runs the tart command with an environment of its own: the agent's
// environment holds other secrets (its provider token), so none of it reaches
// Tart or, through it, a guest.
type cli struct {
	bin     string
	home    string
	getenv  func(string) string
	user    string // env:NAME references
	pass    string
	regHost string
}

// vmInfo is a line of `tart list --format json`.
type vmInfo struct {
	Source string `json:"Source"`
	Name   string `json:"Name"`
	// Disk is null while a running VM holds its disk open: unknown.
	Disk    *int   `json:"Disk"`
	Running bool   `json:"Running"`
	State   string `json:"State"`
}

// env is what a tart command sees; withRegistry adds the registry's
// credentials, for the commands that pull.
func (c cli) env(withRegistry bool) (env []string, secret string) {
	env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + c.home, "TART_HOME=" + filepath.Join(c.home, "tart-home")}
	if withRegistry && c.user != "" {
		u, p := c.getenv(refName(c.user)), c.getenv(refName(c.pass))
		if u != "" && p != "" {
			env = append(env, "TART_REGISTRY_USERNAME="+u, "TART_REGISTRY_PASSWORD="+p)
			if c.regHost != "" {
				env = append(env, "TART_REGISTRY_HOSTNAME="+c.regHost)
			}
			secret = p
		}
	}
	return env, secret
}

// snippet makes what a command said safe to put in an error: quoted, so it is
// inert text, bounded, and without the registry password or any known secret
// shape. Output of a guest is untrusted, and is never more than this.
func snippet(b []byte, secret string) string {
	s := string(b)
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	s = oneLine(fleet.CleanError(s))
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "") + "..."
	}
	return strconv.Quote(s)
}

func (c cli) do(ctx context.Context, withRegistry bool, stdin string, args ...string) (string, error) {
	env, secret := c.env(withRegistry)
	cmd := exec.CommandContext(ctx, c.bin, args...) // #nosec G204 -- the operator's command with arguments of this package
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, errb := &capBuffer{}, &capBuffer{}
	cmd.Stdout, cmd.Stderr = out, errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tart %s: %w: %s", args[0], err, snippet(append(errb.Bytes(), out.Bytes()...), secret))
	}
	return out.String(), nil
}

// maxOutput bounds what is kept of a command's output: a guest chooses what
// `tart exec` prints, and must not fill the agent's memory with it.
const maxOutput = 1 << 20

// capBuffer keeps the first maxOutput bytes written to it and drops the rest,
// still reading it all so the command is never blocked.
type capBuffer struct{ buf bytes.Buffer }

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := maxOutput - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capBuffer) Bytes() []byte  { return c.buf.Bytes() }
func (c *capBuffer) String() string { return c.buf.String() }

// output runs a command that has an answer to read.
func (c cli) output(ctx context.Context, args ...string) (string, error) {
	return c.do(ctx, false, "", args...)
}

func (c cli) version(ctx context.Context) (string, error) {
	out, err := c.do(ctx, false, "", "--version")
	if err != nil {
		return "", err
	}
	if v := regexp.MustCompile(`\d+\.\d+\.\d+`).FindString(out); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("tart --version said %s", snippet([]byte(out), ""))
}

func (c cli) list(ctx context.Context) ([]vmInfo, error) {
	out, err := c.do(ctx, false, "", "list", "--source", "local", "--format", "json")
	if err != nil {
		return nil, err
	}
	var vms []vmInfo
	if err := json.Unmarshal([]byte(out), &vms); err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	return vms, nil
}

func (c cli) info(ctx context.Context, name string) (vmInfo, error) {
	out, err := c.do(ctx, false, "", "get", name, "--format", "json")
	if err != nil {
		return vmInfo{}, err
	}
	var v vmInfo
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return vmInfo{}, fmt.Errorf("tart get: %w", err)
	}
	return v, nil
}

func (c cli) pull(ctx context.Context, ref string) error {
	_, err := c.do(ctx, true, "", "pull", ref)
	return err
}

func (c cli) clone(ctx context.Context, src, name string) error {
	_, err := c.do(ctx, true, "", "clone", src, name)
	return err
}

func (c cli) set(ctx context.Context, name string, args ...string) error {
	_, err := c.do(ctx, false, "", append([]string{"set", name}, args...)...)
	return err
}

func (c cli) stop(ctx context.Context, name string, seconds int) error {
	_, err := c.do(ctx, false, "", "stop", "--timeout", strconv.Itoa(seconds), name)
	return err
}

func (c cli) delete(ctx context.Context, name string) error {
	_, err := c.do(ctx, false, "", "delete", name)
	return err
}

func (c cli) prune(ctx context.Context, days int) error {
	_, err := c.do(ctx, false, "", "prune", "--entries", "caches", "--older-than", strconv.Itoa(days))
	return err
}

// exec runs a command in a running guest through its guest agent. stdin is
// how a secret goes in: never an argument, which the host's process list shows.
func (c cli) exec(ctx context.Context, stdin, name string, command ...string) error {
	args := []string{"exec"}
	if stdin != "" {
		args = append(args, "-i")
	}
	_, err := c.do(ctx, false, stdin, append(append(args, name), command...)...)
	return err
}

// start runs the VM in its own session, so that it outlives the agent, with its
// output in log. The returned channel receives how it ended.
func (c cli) start(name, log string, flags []string) (<-chan error, error) {
	f, err := os.OpenFile(log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) // #nosec G304 -- a path of this package
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	env, _ := c.env(false)
	cmd := exec.Command(c.bin, append(append([]string{"run"}, flags...), name)...) // #nosec G204 -- see do
	cmd.Env, cmd.Stdout, cmd.Stderr, cmd.Dir = env, f, f, "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tart run: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }() // reaps it, as the parent must
	return done, nil
}

// tailRaw is the last 300 bytes of a log.
func tailRaw(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- a path of this package
	if err != nil {
		return ""
	}
	if len(b) > 300 {
		b = b[len(b)-300:]
	}
	return string(b)
}

// tail is the end of a log, as a snippet.
func tail(path string) string { return snippet([]byte(tailRaw(path)), "") }
