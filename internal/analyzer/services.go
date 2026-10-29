package analyzer

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"
)

// composeFile is the part of a docker-compose file the analyzer reads.
type composeFile struct {
	Services map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
}

// composeServices lists the services of the compose files in files, sorted by
// file and name. A file that does not parse declares no services.
func composeServices(root string, files []string) ([]Service, error) {
	var out []Service
	for _, rel := range files {
		if !matchAny(composePatterns, path.Base(rel)) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- rel comes from a walk under root
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		var cf composeFile
		if err := yaml.Unmarshal(data, &cf); err != nil {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(cf.Services)) {
			out = append(out, Service{File: rel, Name: name, Image: cf.Services[name].Image})
		}
	}
	return nonNil(out), nil
}
