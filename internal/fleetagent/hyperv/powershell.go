package hyperv

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/forecrew/forecrew/internal/fleet"
)

//go:embed scripts/*.ps1
var scripts embed.FS

// maxOutput bounds what a script may print.
const maxOutput = 1 << 20

// Runner runs a script of the scripts directory by its name, with in as its JSON
// parameters, and decodes the JSON it printed into out (nil to ignore it).
// Tests replace it with testkit.FakePowerShell.
type Runner interface {
	Run(ctx context.Context, script string, in, out any) error
}

// PowerShell runs the scripts with powershell.exe. The text of a script is
// fixed; what it is given goes in on standard input, never on a command line.
type PowerShell struct {
	// Exe is the command, and Dir where the scripts are written for it to run
	// (a directory only the agent's user and administrators can write).
	Exe, Dir string
	Timeout  time.Duration

	once sync.Once
	err  error
}

// Run implements Runner.
func (ps *PowerShell) Run(ctx context.Context, script string, in, out any) error {
	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}
	if script == secureScript {
		// It runs from memory: the directory the other scripts are written to may
		// be open until this has closed it.
		text, err := inline(script)
		if err != nil {
			return err
		}
		args = append(args, "-EncodedCommand", text)
	} else {
		if err := ps.install(); err != nil {
			return err
		}
		args = append(args, "-File", filepath.Join(ps.Dir, script+".ps1"))
	}
	params, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, ps.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps.Exe, args...) // #nosec G204 -- the operator's command, fixed scripts
	cmd.Stdin = bytes.NewReader(append(params, '\n'))
	cmd.WaitDelay = 2 * time.Second // a child that holds the pipes open does not hold us
	var stdout, stderr limited
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		why := clean(stderr.String())
		if ctx.Err() != nil {
			why = "timed out: " + why
		}
		return fmt.Errorf("hyper-v %s: %s", script, strings.TrimSpace(why+" "+clean(err.Error())))
	}
	if out == nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), out); err != nil {
		return fmt.Errorf("hyper-v %s: unexpected output", script)
	}
	return nil
}

// secureScript is the one script that is run from memory, before any other.
const secureScript = "secure"

// inline returns a script, with the prelude in front, as PowerShell's
// -EncodedCommand takes it: base64 of UTF-16 little-endian text.
func inline(name string) (string, error) {
	text, err := withPrelude(name + ".ps1")
	if err != nil {
		return "", err
	}
	u := utf16.Encode([]rune(string(text)))
	raw := make([]byte, 0, 2*len(u))
	for _, c := range u {
		raw = binary.LittleEndian.AppendUint16(raw, c)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func withPrelude(file string) ([]byte, error) {
	prelude, err := scripts.ReadFile("scripts/prelude.ps1")
	if err != nil {
		return nil, err
	}
	body, err := scripts.ReadFile("scripts/" + file)
	if err != nil {
		return nil, err
	}
	return append(append(bytes.Clone(prelude), '\n'), body...), nil
}

// install writes the scripts, each with the prelude in front, on first use.
func (ps *PowerShell) install() error {
	ps.once.Do(func() { ps.err = writeScripts(ps.Dir) })
	return ps.err
}

func writeScripts(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := scripts.ReadDir("scripts")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "prelude.ps1" || name == secureScript+".ps1" {
			continue
		}
		text, err := withPrelude(name)
		if err != nil {
			return err
		}
		// Whatever is there goes first: a file or link put there earlier, with an
		// owner or access of its own, is not written through.
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(path, text, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// clean makes text of the host safe to show: no secret, one short line.
func clean(s string) string {
	s = strings.Join(strings.Fields(fleet.CleanError(s)), " ")
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "")
	}
	return s
}

// limited keeps what a script printed, up to maxOutput bytes.
type limited struct{ b bytes.Buffer }

func (l *limited) Write(p []byte) (int, error) {
	if l.b.Len()+len(p) > maxOutput {
		return 0, errors.New("a script printed too much")
	}
	return l.b.Write(p)
}

func (l *limited) String() string { return l.b.String() }

var _ io.Writer = (*limited)(nil)
