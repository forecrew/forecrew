package worker

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// maxDevcontainerSize bounds the devcontainer.json the worker reads.
const maxDevcontainerSize = 1 << 20

// devcontainer is what the worker uses of a devcontainer.json: image,
// build.dockerfile, build.context, containerEnv and postCreateCommand. Every
// other key (mounts, runArgs, privileged, features, ...) is ignored, so a
// repo cannot widen the container through it.
type devcontainer struct {
	Image string `json:"image"`
	Build struct {
		Dockerfile string `json:"dockerfile"`
		Context    string `json:"context"`
	} `json:"build"`
	ContainerEnv      map[string]string `json:"containerEnv"`
	PostCreateCommand json.RawMessage   `json:"postCreateCommand"`
}

// envSource is what the image of a docker environment is made of, with the
// devcontainer resolved. Paths are relative to the repo root.
type envSource struct {
	Image      string // pulled when Dockerfile is empty
	Dockerfile string
	Context    string
	Setup      []string
	Env        map[string]string // containerEnv of a devcontainer
}

// resolveSource returns the source of e's image. A devcontainer is read from
// the work tree at root; its setup commands run before e's own.
func resolveSource(root string, e *domain.Environment) (envSource, error) {
	src := envSource{Image: e.Image, Dockerfile: e.Dockerfile, Context: e.Context, Setup: e.Setup}
	if e.Devcontainer != "" {
		dev, err := readDevcontainer(root, e.Devcontainer)
		if err != nil {
			return envSource{}, fmt.Errorf("devcontainer %s: %w", e.Devcontainer, err)
		}
		dir := path.Dir(e.Devcontainer)
		src = envSource{Image: dev.Image, Env: dev.ContainerEnv}
		if f := dev.Build.Dockerfile; f != "" {
			src.Image = ""
			src.Dockerfile = path.Join(dir, f)
			src.Context = path.Join(dir, cmp.Or(dev.Build.Context, "."))
		}
		if src.Image == "" && src.Dockerfile == "" {
			return envSource{}, errors.New("sets neither image nor build.dockerfile")
		}
		if src.Setup, err = postCreateCommands(dev.PostCreateCommand); err != nil {
			return envSource{}, fmt.Errorf("devcontainer %s: %w", e.Devcontainer, err)
		}
		src.Setup = append(src.Setup, e.Setup...)
	}
	if src.Dockerfile != "" && src.Context == "" {
		src.Context = "."
	}
	for _, p := range []string{src.Dockerfile, src.Context} {
		if p != "" && !domain.RelativePath(path.Clean(p)) {
			return envSource{}, fmt.Errorf("path %q must stay inside the repo", p)
		}
	}
	for k := range src.Env {
		if domain.SecretLikeName(k) {
			return envSource{}, fmt.Errorf("containerEnv %s looks like a secret; secrets never go in repo files", k)
		}
	}
	if strings.ContainsAny(src.Image, " \t\r\n") {
		return envSource{}, fmt.Errorf("image %q is not a valid image reference", src.Image)
	}
	return src, nil
}

// readDevcontainer reads and decodes the devcontainer.json at rel below root,
// reached without crossing a symlink.
func readDevcontainer(root, rel string) (devcontainer, error) {
	var dev devcontainer
	if err := noSymlinks(root, rel); err != nil {
		return dev, err
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() || info.Size() > maxDevcontainerSize {
		return dev, errors.New("is not a regular file of at most 1 MiB")
	}
	data, err := os.ReadFile(p) // #nosec G304 -- inside the job's workspace, symlinks refused
	if err != nil {
		return dev, err
	}
	if err := json.Unmarshal(jsoncToJSON(data), &dev); err != nil {
		return dev, fmt.Errorf("invalid JSON: %w", err)
	}
	return dev, nil
}

// postCreateCommands turns a postCreateCommand (a shell string, an argument
// list or an object of those) into shell commands.
func postCreateCommands(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var argv []string
	if json.Unmarshal(raw, &argv) == nil {
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = shellQuote(a)
		}
		return []string{strings.Join(quoted, " ")}, nil
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(raw, &named); err != nil {
		return nil, errors.New("postCreateCommand must be a string, a list or an object")
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(named)) {
		cmds, err := postCreateCommands(named[k])
		if err != nil {
			return nil, err
		}
		out = append(out, cmds...)
	}
	return out, nil
}

// shellQuote quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// jsoncToJSON removes the // and /* */ comments and the trailing commas that
// devcontainer.json allows and JSON does not, leaving strings alone.
func jsoncToJSON(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(in) && in[j] != '"' {
				if in[j] == '\\' {
					j++
				}
				j++
			}
			out = append(out, in[i:min(j+1, len(in))]...)
			i = j
		case c == '/' && i+1 < len(in) && in[i+1] == '/':
			for i < len(in) && in[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(in) && in[i+1] == '*':
			end := strings.Index(string(in[i+2:]), "*/")
			if end < 0 {
				return out
			}
			i += end + 3
		case c == ',' && nextIsClose(in[i+1:]):
		default:
			out = append(out, c)
		}
	}
	return out
}

// nextIsClose reports whether the next token of rest, after white space and
// comments, closes an object or array.
func nextIsClose(rest []byte) bool {
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
		case c == '/' && i+1 < len(rest) && rest[i+1] == '/':
			for i < len(rest) && rest[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(rest) && rest[i+1] == '*':
			end := strings.Index(string(rest[i+2:]), "*/")
			if end < 0 {
				return false
			}
			i += end + 3
		default:
			return c == '}' || c == ']'
		}
	}
	return false
}
