package advisor

import (
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/schema"
	"go.yaml.in/yaml/v3"
)

// CatalogDir holds the tool catalog in the packs.
const CatalogDir = "packs/catalog"

// Tool is an entry of the catalog (docs/contracts/catalog.schema.json).
type Tool struct {
	Name        string   `yaml:"name"`
	Kind        string   `yaml:"kind"`
	Purpose     string   `yaml:"purpose"`
	Homepage    string   `yaml:"homepage"`
	Install     string   `yaml:"install"`
	License     string   `yaml:"license"`
	Roles       []string `yaml:"roles"`
	Categories  []string `yaml:"categories"`
	Credentials []string `yaml:"credentials"`
}

// Catalog is the tools the advisor may suggest, by name.
type Catalog map[string]Tool

// modelsFile is the model catalog, which has its own schema
// (internal/models).
const modelsFile = "models.yaml"

// LoadCatalog reads every entry of CatalogDir in fsys but the model catalog
// and checks it against its schema; an entry is named after its file.
func LoadCatalog(fsys fs.FS) (Catalog, error) {
	files, err := fs.Glob(fsys, path.Join(CatalogDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("list the catalog: %w", err)
	}
	out := Catalog{}
	for _, file := range files {
		if path.Base(file) == modelsFile {
			continue
		}
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		_, inst, err := schema.DecodeYAML(data)
		if err == nil {
			err = schema.CheckContract("catalog.schema.json", inst)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		var t Tool
		if err := yaml.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if want := strings.TrimSuffix(path.Base(file), ".yaml"); t.Name != want {
			return nil, fmt.Errorf("%s: name %q is not the file's", file, t.Name)
		}
		out[t.Name] = t
	}
	return out, nil
}

// Has reports whether name is in the catalog.
func (c Catalog) Has(name string) bool {
	_, ok := c[name]
	return ok
}

// text lists the catalog for the advisor's prompt.
func (c Catalog) text() string {
	var b strings.Builder
	for _, name := range sortedNames(c) {
		t := c[name]
		fmt.Fprintf(&b, "- %s (%s, %s): %s Suits %s. Answers %s.\n", t.Name, t.Kind, t.License, t.Purpose, strings.Join(t.Roles, ", "), strings.Join(t.Categories, ", "))
	}
	return b.String()
}

func sortedNames(c Catalog) []string {
	names := make([]string, 0, len(c))
	for name := range c {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
