package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

func TestJSONCToJSON(t *testing.T) {
	in := `{
  // a comment with "quotes" and a , comma
  "image": "golang:1.27", /* block
  comment */
  "url": "http://x/y", "esc": "a\"//b",
  "list": [1, 2, /* last */ ],
  "obj": {"a": 1,},
}`
	var got map[string]any
	if err := json.Unmarshal(jsoncToJSON([]byte(in)), &got); err != nil {
		t.Fatalf("%v\n%s", err, jsoncToJSON([]byte(in)))
	}
	if got["image"] != "golang:1.27" || got["url"] != "http://x/y" || got["esc"] != `a"//b` || len(got["list"].([]any)) != 2 {
		t.Errorf("got = %v", got)
	}
	// An unterminated block comment or string must not panic.
	for _, s := range []string{`{"a": 1 /* open`, `{"a": "open`, `{"a": 1, // end`, `{"a": 1, /* open`, `{"a"\`} {
		_ = jsoncToJSON([]byte(s))
	}
}

func TestPostCreateCommands(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
		err  bool
	}{
		{"", nil, false},
		{"null", nil, false},
		{`"make deps"`, []string{"make deps"}, false},
		{`["npm","i","it's"]`, []string{`'npm' 'i' 'it'\''s'`}, false},
		{`{"b":"two","a":["one","x"],"c":{"d":"deep"}}`, []string{"'one' 'x'", "two", "deep"}, false},
		{`7`, nil, true},
		{`{"a":7}`, nil, true},
	}
	for _, tt := range tests {
		got, err := postCreateCommands([]byte(tt.raw))
		if (err != nil) != tt.err || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("postCreateCommands(%s) = %q, %v; want %q (error %v)", tt.raw, got, err, tt.want, tt.err)
		}
	}
}

func TestResolveSource(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".devcontainer/image.json", `{"image": "golang:1.27", "containerEnv": {"GOFLAGS": "-mod=mod"}, "postCreateCommand": "go mod download", "mounts": ["x"], "privileged": true}`)
	write(".devcontainer/build.json", `{"build": {"dockerfile": "Dockerfile", "context": ".."}}`)
	write(".devcontainer/default.json", `{"build": {"dockerfile": "Dockerfile"}}`)
	write(".devcontainer/escape.json", `{"build": {"dockerfile": "../../x/Dockerfile"}}`)
	write(".devcontainer/empty.json", `{}`)
	write(".devcontainer/secret.json", `{"image": "x", "containerEnv": {"API_KEY": "v"}}`)
	write(".devcontainer/badimage.json", `{"image": "x y"}`)
	write(".devcontainer/badcmd.json", `{"image": "x", "postCreateCommand": 3}`)
	write(".devcontainer/broken.json", `{`)
	write("ci/Dockerfile", "FROM x")
	if err := os.Symlink("/etc/hostname", filepath.Join(root, ".devcontainer", "link.json")); err != nil {
		t.Fatal(err)
	}
	dev := func(name string, setup ...string) *domain.Environment {
		return &domain.Environment{Name: "e", Provider: domain.ProviderDocker, Devcontainer: ".devcontainer/" + name + ".json", Setup: setup}
	}
	tests := []struct {
		name string
		env  *domain.Environment
		want envSource
		err  string
	}{
		{"image", &domain.Environment{Image: "golang:1.27", Setup: []string{"a"}}, envSource{Image: "golang:1.27", Setup: []string{"a"}}, ""},
		{"dockerfile", &domain.Environment{Dockerfile: "ci/Dockerfile"}, envSource{Dockerfile: "ci/Dockerfile", Context: "."}, ""},
		{"dockerfile with context", &domain.Environment{Dockerfile: "ci/Dockerfile", Context: "ci"}, envSource{Dockerfile: "ci/Dockerfile", Context: "ci"}, ""},
		{"devcontainer image", dev("image", "b"), envSource{Image: "golang:1.27", Setup: []string{"go mod download", "b"}, Env: map[string]string{"GOFLAGS": "-mod=mod"}}, ""},
		{"devcontainer build", dev("build"), envSource{Dockerfile: ".devcontainer/Dockerfile", Context: "."}, ""},
		{"devcontainer default context", dev("default"), envSource{Dockerfile: ".devcontainer/Dockerfile", Context: ".devcontainer"}, ""},
		{"devcontainer escapes", dev("escape"), envSource{}, "inside the repo"},
		{"devcontainer empty", dev("empty"), envSource{}, "neither image"},
		{"devcontainer secret", dev("secret"), envSource{}, "looks like a secret"},
		{"devcontainer bad image", dev("badimage"), envSource{}, "not a valid image"},
		{"devcontainer bad command", dev("badcmd"), envSource{}, "postCreateCommand"},
		{"devcontainer broken", dev("broken"), envSource{}, "invalid JSON"},
		{"devcontainer missing", dev("missing"), envSource{}, "no such file"},
		{"devcontainer symlink", dev("link"), envSource{}, "symlink"},
		{"dockerfile outside", &domain.Environment{Dockerfile: "../x"}, envSource{}, "inside the repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSource(root, tt.env)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("source = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}
