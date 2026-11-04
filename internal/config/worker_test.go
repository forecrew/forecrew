package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workerEnv(token string) func(string) string {
	return func(k string) string {
		if k == EnvWorkerToken {
			return token
		}
		return ""
	}
}

func TestParseWorker(t *testing.T) {
	y := `
server_url: https://forecrew.example.com
name: build-1
labels: [go, node]
work_dir: /srv/forecrew
max_jobs: 2
image_prune_days: 3
registry_auth: true
`
	w, err := ParseWorker([]byte(y), workerEnv("fcw_token"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "build-1" || w.WorkDir != "/srv/forecrew" || w.MaxJobs != 2 || w.ImagePruneDays != 3 || !w.RegistryAuth || w.Token != "fcw_token" || len(w.Labels) != 2 {
		t.Fatalf("worker = %+v", w)
	}
}

func TestParseWorkerDefaults(t *testing.T) {
	w, err := ParseWorker([]byte("server_url: http://localhost:8080\n"), workerEnv("t"))
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if w.MaxJobs != DefaultMaxJobs || w.ImagePruneDays != DefaultImagePruneDays || w.RegistryAuth || w.Name != host || !strings.HasSuffix(w.WorkDir, "forecrew-worker") {
		t.Fatalf("worker = %+v", w)
	}
}

func TestParseWorkerInvalid(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		token string
		want  string
	}{
		{name: "no token", yaml: "server_url: https://x.example.com\n", want: EnvWorkerToken},
		{name: "no server", yaml: "name: a\n", token: "t", want: "server_url"},
		{name: "bad max jobs", yaml: "server_url: https://x.example.com\nmax_jobs: -1\n", token: "t", want: "max_jobs"},
		{name: "bad prune days", yaml: "server_url: https://x.example.com\nimage_prune_days: -2\n", token: "t", want: "image_prune_days"},
		{name: "token in file", yaml: "server_url: https://x.example.com\ntoken: t\n", token: "t", want: "field token not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseWorker([]byte(tt.yaml), workerEnv(tt.token))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

func TestParseWorkerNoCacheDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LocalAppData", "")
	if _, err := ParseWorker([]byte("server_url: https://x.example.com\n"), workerEnv("t")); err == nil || !strings.Contains(err.Error(), "work_dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, []byte("server_url: https://x.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorker(path, workerEnv("t")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorker(path+".missing", workerEnv("t")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}
