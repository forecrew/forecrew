package images

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubDocker writes a docker client that serves tarball and records the
// commands it gets in a log next to it.
func stubDocker(t *testing.T, tarball string, failExport bool) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "docker"), filepath.Join(dir, "log")
	export := "cat '" + tarball + "'"
	if failExport {
		export = "echo 'export broke' >&2; exit 3"
	}
	script := `#!/bin/sh
echo "$@" >>'` + log + `'
case $1 in
image) [ "$5" = missing:1 ] && { echo 'No such image' >&2; exit 1; }
	case $4 in *Config.Env*) echo '["PATH=/x:/usr/bin","LANG=C"]' ;; *) echo sha256:feedbeef ;; esac ;;
create) echo cont123 ;;
export) ` + export + ` ;;
rm) ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	return bin, log
}

func TestDockerCLI(t *testing.T) {
	tarPath := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(tarPath, baseTar(t), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, log := stubDocker(t, tarPath, false)
	d := DockerCLI{Bin: bin}

	id, err := d.ID(t.Context(), "forecrew-env:ab")
	if err != nil || id != "sha256:feedbeef" {
		t.Fatalf("ID = %q, %v", id, err)
	}
	if _, err := d.ID(t.Context(), "missing:1"); err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Fatalf("missing image: %v", err)
	}
	env, err := d.Env(t.Context(), "sha256:feedbeef")
	if err != nil || len(env) != 2 || env[0] != "PATH=/x:/usr/bin" {
		t.Fatalf("Env = %v, %v", env, err)
	}
	if _, err := d.Env(t.Context(), "-x"); err == nil {
		t.Error("Env accepted a bad reference")
	}
	// Through the converter: the tar is exported by ID, the container removed.
	c := &Converter{CacheDir: t.TempDir(), Docker: d, Mke2fs: stubMke2fs(t, "1.47.4", false)}
	if _, err := c.Convert(t.Context(), Request{Ref: "forecrew-env:ab", Files: testGuest(t)}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	for _, want := range []string{"create --network none sha256:feedbeef /nonexistent", "export cont123", "rm -f cont123"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("no %q in\n%s", want, data)
		}
	}
	for _, bad := range []string{"", "-v", "a b"} {
		if _, err := d.ID(t.Context(), bad); err == nil {
			t.Errorf("ID(%q) accepted", bad)
		}
		if _, err := d.Export(t.Context(), bad); err == nil {
			t.Errorf("Export(%q) accepted", bad)
		}
	}
	if _, err := (DockerCLI{Bin: filepath.Join(t.TempDir(), "none")}).Export(t.Context(), "x:1"); err == nil {
		t.Error("missing client accepted")
	}
}

func TestDockerCLIExportFails(t *testing.T) {
	bin, log := stubDocker(t, "", true)
	rc, err := DockerCLI{Bin: bin}.Export(t.Context(), "x:1")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	_, _ = rc.Read(buf)
	if err := rc.Close(); err == nil || !strings.Contains(err.Error(), "export broke") {
		t.Fatalf("Close = %v", err)
	}
	if data, _ := os.ReadFile(log); !strings.Contains(string(data), "rm -f") {
		t.Errorf("container not removed: %s", data)
	}
}
