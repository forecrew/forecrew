package hyperv

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// fakeExe is a shell script that stands in for powershell.exe: it records its
// arguments and standard input, and prints what the test says.
func fakeExe(t *testing.T, body string) (exe, record string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for powershell.exe is a shell script")
	}
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	exe = filepath.Join(dir, "powershell.exe")
	script := "#!/bin/sh\necho \"$@\" >" + record + "\ncat >>" + record + "\n" + body + "\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return exe, record
}

func TestPowerShellRun(t *testing.T) {
	exe, record := fakeExe(t, `echo noise; echo '{"id":"abc"}'`)
	ps := &PowerShell{Exe: exe, Dir: filepath.Join(t.TempDir(), "scripts"), Timeout: time.Minute}
	var out struct {
		ID string `json:"id"`
	}
	in := map[string]any{"name": "forecrew-1", "env": "FORECREW_BOOTSTRAP_TOKEN=fcb_topsecret"}
	if err := ps.Run(context.Background(), "create", in, &out); err != nil || out.ID != "abc" {
		t.Fatalf("%+v %v", out, err)
	}
	raw, _ := os.ReadFile(record)
	got := string(raw)
	args, stdin, _ := strings.Cut(got, "\n")
	// Parameters go in on standard input, never on the command line.
	if strings.Contains(args, "fcb_topsecret") || strings.Contains(args, "forecrew-1") {
		t.Errorf("a parameter is in the arguments: %s", args)
	}
	if !strings.Contains(stdin, `"name":"forecrew-1"`) || !strings.Contains(stdin, "fcb_topsecret") {
		t.Errorf("stdin = %q", stdin)
	}
	for _, want := range []string{"-NoProfile", "-NonInteractive", "-File", "create.ps1"} {
		if !strings.Contains(args, want) {
			t.Errorf("arguments %q lack %s", args, want)
		}
	}
	// What ran is the script with the prelude in front.
	text, err := os.ReadFile(filepath.Join(ps.Dir, "create.ps1"))
	if err != nil || !strings.HasPrefix(string(text), "# Start of every script") || !strings.Contains(string(text), "Assert-Under $p.dir") {
		t.Errorf("script = %.80q, %v", text, err)
	}
	if _, err := os.Stat(filepath.Join(ps.Dir, "prelude.ps1")); err == nil {
		t.Error("the prelude is a script of its own")
	}
	if st, _ := os.Stat(ps.Dir); st.Mode().Perm()&0o077 != 0 {
		t.Errorf("scripts directory mode %v", st.Mode())
	}
	if err := ps.Run(context.Background(), "create", in, nil); err != nil { // no output wanted
		t.Error(err)
	}
}

func TestPowerShellFailures(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"error text", `echo "Access is denied. fcb_AAAAbbbb1234-secret_token" >&2; exit 1`, "denied"},
		{"no output", `exit 0`, "unexpected output"},
		{"not json", `echo hello`, "unexpected output"},
		{"too much", `head -c 2000000 /dev/zero | tr '\0' 'x'`, "exit status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exe, _ := fakeExe(t, tt.body)
			ps := &PowerShell{Exe: exe, Dir: filepath.Join(t.TempDir(), "s"), Timeout: time.Minute}
			var out map[string]any
			err := ps.Run(context.Background(), "list", struct{}{}, &out)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "fcb_AAAAbbbb1234") || strings.Contains(err.Error(), "\n") {
				t.Errorf("err = %q", err)
			}
		})
	}
}

func TestPowerShellTimeout(t *testing.T) {
	exe, _ := fakeExe(t, `sleep 10`)
	ps := &PowerShell{Exe: exe, Dir: filepath.Join(t.TempDir(), "s"), Timeout: 100 * time.Millisecond}
	start := time.Now()
	err := ps.Run(context.Background(), "list", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func TestPowerShellSetupFailures(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	_ = os.WriteFile(file, nil, 0o600)
	ps := &PowerShell{Exe: "nothing", Dir: filepath.Join(file, "scripts"), Timeout: time.Second} // a directory under a file
	if err := ps.Run(context.Background(), "list", struct{}{}, nil); err == nil {
		t.Error("scripts written under a file")
	}
	ps = &PowerShell{Exe: "powershell-that-is-not-there", Dir: t.TempDir(), Timeout: time.Second}
	if err := ps.Run(context.Background(), "list", struct{}{}, nil); err == nil {
		t.Error("a missing command ran")
	}
	if err := ps.Run(context.Background(), "list", func() {}, nil); err == nil {
		t.Error("parameters that are not JSON were sent")
	}
	var l limited
	if _, err := l.Write(make([]byte, maxOutput+1)); err == nil {
		t.Error("too much output accepted")
	}
}

// Every script the provider runs exists, reads its parameters from $p, and
// stays away from what runs text as code or starts other programs.
func TestScripts(t *testing.T) {
	for _, name := range []string{"host", "list", "create", "update", "stop", "destroy", "secure"} {
		body, err := scripts.ReadFile("scripts/" + name + ".ps1")
		if err != nil {
			t.Fatalf("script %s: %v", name, err)
		}
		text := strings.ToLower(string(body))
		for _, banned := range []string{"invoke-expression", "iex ", "start-process", "invoke-command", "-encodedcommand", "invoke-webrequest", "invoke-restmethod", "new-object net.webclient"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s uses %s", name, banned)
			}
		}
	}
	prelude, _ := scripts.ReadFile("scripts/prelude.ps1")
	if !strings.Contains(string(prelude), "$ErrorActionPreference = 'Stop'") || !strings.Contains(string(prelude), "[Console]::In.ReadToEnd()") {
		t.Error("the prelude does not stop on errors or read its parameters")
	}
	// Scripts that remove anything check where.
	for _, name := range []string{"create", "destroy"} {
		body, _ := scripts.ReadFile("scripts/" + name + ".ps1")
		if !strings.Contains(string(body), "Assert-Under $p.dir $p.vm_dir") {
			t.Errorf("%s does not check its directory", name)
		}
	}
}

// secure runs from memory, as an encoded command, and writes nothing first.
func TestSecureRunsFromMemory(t *testing.T) {
	exe, record := fakeExe(t, `echo '{"ok":true}'`)
	dir := filepath.Join(t.TempDir(), "scripts")
	ps := &PowerShell{Exe: exe, Dir: dir, Timeout: time.Minute}
	if err := ps.Run(context.Background(), "secure", map[string]any{"dirs": []string{`C:\FC`}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(record)
	args, stdin, _ := strings.Cut(string(raw), "\n")
	if strings.Contains(args, "-File") || !strings.Contains(args, "-EncodedCommand") || !strings.Contains(stdin, `"dirs"`) {
		t.Fatalf("args %q, stdin %q", args, stdin)
	}
	encoded := args[strings.Index(args, "-EncodedCommand ")+len("-EncodedCommand "):]
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatal(err)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	text := string(utf16.Decode(u))
	for _, want := range []string{"function Assert-Closed", "S-1-5-18", "S-1-5-32-544", "SetAccessRuleProtection($true, $false)", "Assert-Closed $dir"} {
		if !strings.Contains(text, want) {
			t.Errorf("the encoded script lacks %q", want)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("scripts were written before the directories were closed")
	}
	// The scripts written later do not include it.
	if err := ps.Run(context.Background(), "list", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secure.ps1")); err == nil {
		t.Error("secure.ps1 was written to disk")
	}
	if len(args) > 30000 {
		t.Errorf("the command line is %d characters; Windows takes about 32000", len(args))
	}
}

// What the scripts promise about the network: IPv6 closed both ways at the lowest
// weight, answers to what is allowed let through, VMs found by object.
func TestNetworkScript(t *testing.T) {
	raw, _ := scripts.ReadFile("scripts/prelude.ps1")
	text := string(raw)
	for _, want := range []string{
		`foreach ($direction in 'Outbound', 'Inbound') {`, `-RemoteIPAddress '::/0' -Weight 1`,
		`if ($rule.proto) { $a.Protocol = $rule.proto.ToUpper(); $a.Stateful = $true }`,
		`if (-not $rule.proto -and -not $rule.allow) { & $add 'Inbound' $rule $weight }`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the prelude lacks %q", want)
		}
	}
	for _, name := range []string{"create", "update", "stop", "destroy", "prelude"} {
		body, _ := scripts.ReadFile("scripts/" + name + ".ps1")
		if strings.Contains(string(body), "-VMName") || strings.Contains(string(body), "Get-VM -Name") {
			t.Errorf("%s finds a VM by name", name)
		}
	}
	create, _ := scripts.ReadFile("scripts/create.ps1")
	if !strings.Contains(string(create), "Assert-Closed $cfgDisk -AllowVirtualMachines") {
		t.Error("create does not check who can read the config disk before the token is written")
	}
	if i, j := strings.Index(string(create), "Assert-Closed $cfgDisk"), strings.Index(string(create), "bootstrap.env"); i < 0 || j < i {
		t.Error("the token is written before the check")
	}
}

// An inbound rule's -RemotePort is the sender's own: every inbound Allow must name
// the machine's own port, and what the allowlist opens is outbound only.
func TestNoInboundAllowWithoutALocalPort(t *testing.T) {
	for _, name := range []string{"prelude", "create", "update", "stop", "destroy", "secure", "host", "list"} {
		raw, _ := scripts.ReadFile("scripts/" + name + ".ps1")
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "Inbound") || !strings.Contains(line, "Add-VMNetworkAdapterExtendedAcl") {
				continue
			}
			if strings.Contains(line, "-Action Allow") && !strings.Contains(line, "-LocalPort") {
				t.Errorf("%s: an inbound Allow names no local port: %s", name, strings.TrimSpace(line))
			}
		}
	}
	raw, _ := scripts.ReadFile("scripts/prelude.ps1")
	text := string(raw)
	// The generic rule is built from the rule's own Action, so it must never be
	// asked for inbound with an allow.
	if strings.Count(text, "& $add 'Inbound'") != 1 || !strings.Contains(text, "if (-not $rule.proto -and -not $rule.allow) { & $add 'Inbound' $rule $weight }") {
		t.Error("a rule is mirrored inbound that is not a denial")
	}
	if !strings.Contains(text, "-Direction Inbound -Action Allow -Protocol UDP -LocalPort 68 -RemotePort 67") {
		t.Error("the DHCP reply is not limited to the client port")
	}
	// An allowlist entry is stateful in both protocols, outbound.
	if !strings.Contains(text, "foreach ($protocol in 'TCP', 'UDP')") || !strings.Contains(text, "$a.Stateful = $true") {
		t.Error("an allowlist entry is not stateful TCP and UDP")
	}
}

// A script file, or a link, that is already there is removed, not written through.
func TestScriptsReplaceWhatIsThere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need rights on Windows")
	}
	dir := filepath.Join(t.TempDir(), "scripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "list.ps1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.ps1"), []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := writeScripts(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(elsewhere); string(got) != "mine" {
		t.Errorf("the link was written through: %q", got)
	}
	st, err := os.Lstat(filepath.Join(dir, "list.ps1"))
	if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o600 {
		t.Errorf("list.ps1: %v %v", st, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "host.ps1")); !strings.Contains(string(got), "Get-VMHost") {
		t.Errorf("host.ps1 = %.40q", got)
	}
	// What cannot be removed is an error.
	blocked := filepath.Join(t.TempDir(), "scripts")
	_ = os.MkdirAll(filepath.Join(blocked, "list.ps1", "inner"), 0o700)
	if err := writeScripts(blocked); err == nil {
		t.Error("a directory in the place of a script was overwritten")
	}
}
