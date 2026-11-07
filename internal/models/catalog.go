package models

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/schema"
)

// CatalogFile is the model catalog in the packs
// (docs/contracts/model-catalog.schema.json).
const CatalogFile = "packs/catalog/models.yaml"

// Where a provider's hosted API runs, besides a country code.
const (
	CountryRegion = "region" // the account picks the region
	CountryLocal  = "local"  // on your own machines
)

// How the key check sends a key.
const (
	AuthBearer  = "bearer"
	AuthXAPIKey = "x-api-key"
	AuthAPIKey  = "api-key"
	AuthNone    = "none"
)

// List formats of a models endpoint.
const (
	FormatAnthropic = "anthropic"
	FormatOpenAI    = "openai"
)

// Maker makes models.
type Maker struct {
	Name     string   `json:"name"`
	Title    string   `json:"title"`
	Prefixes []string `json:"prefixes"`
}

// ListEndpoint is how the key check lists a provider's models.
type ListEndpoint struct {
	Path   string `json:"path"`
	Auth   string `json:"auth"`
	Format string `json:"format"`
}

// Provider is a hosted API or a local server that serves models.
type Provider struct {
	Name         string        `json:"name"`
	Title        string        `json:"title"`
	Adapter      string        `json:"adapter"`
	BaseURL      string        `json:"base_url,omitempty"`
	KeyEnv       string        `json:"key_env"`
	ListEndpoint *ListEndpoint `json:"models_endpoint,omitempty"`
	Homepage     string        `json:"homepage"`
	Country      string        `json:"country"`
	Router       bool          `json:"router,omitempty"`
}

// Where says in words where the provider runs, for a plan.
func (p Provider) Where() string {
	switch p.Country {
	case CountryRegion:
		return "in the region of its endpoint"
	case CountryLocal:
		return "on your own machines"
	}
	return "in " + p.Country
}

// Price is in US dollars per million tokens.
type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// CatalogModel is a model the catalog knows.
type CatalogModel struct {
	ID      string `json:"id"`
	Maker   string `json:"maker"`
	Tier    string `json:"tier"`
	Context int    `json:"context"`
	Price   Price  `json:"price"`
}

// Catalog is the model catalog.
type Catalog struct {
	Version   int            `json:"version"`
	Makers    []Maker        `json:"makers"`
	Providers []Provider     `json:"providers"`
	Models    []CatalogModel `json:"models"`
}

// LoadCatalog reads CatalogFile from fsys, checks it against its schema and
// that its names are unique and refer to each other.
func LoadCatalog(fsys fs.FS) (*Catalog, error) {
	data, err := fs.ReadFile(fsys, CatalogFile)
	if err != nil {
		return nil, fmt.Errorf("read the model catalog: %w", err)
	}
	raw, inst, err := schema.DecodeYAML(data)
	if err == nil {
		err = schema.CheckContract("model-catalog.schema.json", inst)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", CatalogFile, err)
	}
	var c Catalog
	if err := schema.Strict(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", CatalogFile, err)
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", CatalogFile, err)
	}
	return &c, nil
}

func (c *Catalog) check() error {
	seen := map[string]bool{}
	for _, check := range []func(map[string]bool) error{c.checkMakers, c.checkProviders, c.checkModels} {
		if err := check(seen); err != nil {
			return err
		}
	}
	return nil
}

// once records name in seen, failing when it is there already.
func once(seen map[string]bool, name string) error {
	if seen[name] {
		return fmt.Errorf("%s is listed twice", name)
	}
	seen[name] = true
	return nil
}

func (c *Catalog) checkMakers(seen map[string]bool) error {
	prefixes := map[string]string{}
	for _, m := range c.Makers {
		if err := once(seen, "maker "+m.Name); err != nil {
			return err
		}
		for _, p := range m.Prefixes {
			if other, ok := prefixes[p]; ok {
				return fmt.Errorf("prefix %s is of %s and %s", p, other, m.Name)
			}
			prefixes[p] = m.Name
		}
	}
	return nil
}

func (c *Catalog) checkProviders(seen map[string]bool) error {
	for _, p := range c.Providers {
		if err := once(seen, "provider "+p.Name); err != nil {
			return err
		}
		if !config.ValidAdapter(p.Adapter) {
			return fmt.Errorf("provider %s: adapter %s is not one Forecrew runs", p.Name, p.Adapter)
		}
		if p.BaseURL != "" {
			if err := config.CheckBaseURL(p.BaseURL); err != nil {
				return fmt.Errorf("provider %s: base_url: %w", p.Name, err)
			}
		}
	}
	return nil
}

func (c *Catalog) checkModels(seen map[string]bool) error {
	for _, m := range c.Models {
		if err := once(seen, "model "+m.ID); err != nil {
			return err
		}
		if !seen["maker "+m.Maker] {
			return fmt.Errorf("model %s: maker %s is not listed", m.ID, m.Maker)
		}
		if Normalize(m.ID) != m.ID {
			return fmt.Errorf("model %s: write it as %s", m.ID, Normalize(m.ID))
		}
	}
	return nil
}

var (
	defaultOnce    sync.Once
	defaultCatalog *Catalog
	defaultErr     error
)

// DefaultCatalog is the catalog this binary ships with.
func DefaultCatalog() (*Catalog, error) {
	defaultOnce.Do(func() { defaultCatalog, defaultErr = LoadCatalog(forecrew.Files) })
	return defaultCatalog, defaultErr
}

// Provider returns the provider of a vendor.
func (c *Catalog) Provider(vendor string) (Provider, bool) {
	if c == nil {
		return Provider{}, false
	}
	i := slices.IndexFunc(c.Providers, func(p Provider) bool { return p.Name == strings.ToLower(vendor) })
	if i < 0 {
		return Provider{}, false
	}
	return c.Providers[i], true
}

// MakerTitle returns how people call a maker.
func (c *Catalog) MakerTitle(name string) string {
	if c != nil {
		if i := slices.IndexFunc(c.Makers, func(m Maker) bool { return m.Name == name }); i >= 0 {
			return c.Makers[i].Title
		}
	}
	return name
}

var (
	dateSuffix    = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2}|latest)$`)
	versionSuffix = regexp.MustCompile(`-v\d+$`)
	dotted        = regexp.MustCompile(`[._]`)
	word          = regexp.MustCompile(`^[a-z]+$`)
)

// Normalize returns a model's name as the catalog writes it: lowercase,
// without the provider part (anthropic/, us.anthropic.), a tag (:30b, :0), a
// date or version suffix, and with dashes for dots.
func Normalize(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, ":"); i >= 0 {
		m = m[:i]
	}
	// Bedrock names carry their region and maker: us.anthropic.claude-...
	parts := strings.Split(m, ".")
	for len(parts) > 1 && word.MatchString(parts[0]) {
		parts = parts[1:]
	}
	m = strings.Join(parts, ".")
	m = dotted.ReplaceAllString(m, "-")
	for {
		n := versionSuffix.ReplaceAllString(dateSuffix.ReplaceAllString(m, ""), "")
		if n == m {
			return m
		}
		m = n
	}
}

// Model returns what the catalog knows of a model, under any provider's
// name of it.
func (c *Catalog) Model(model string) (CatalogModel, bool) {
	if c == nil {
		return CatalogModel{}, false
	}
	id := Normalize(model)
	i := slices.IndexFunc(c.Models, func(m CatalogModel) bool { return m.ID == id })
	if i < 0 {
		return CatalogModel{}, false
	}
	return c.Models[i], true
}

// MakerOf returns the maker of a model: the catalog's, or the maker whose
// prefix starts its name; "" when nobody knows.
func (c *Catalog) MakerOf(model string) string {
	if m, ok := c.Model(model); ok {
		return m.Maker
	}
	if c == nil {
		return ""
	}
	id := Normalize(model)
	for _, mk := range c.Makers {
		for _, p := range mk.Prefixes {
			if strings.HasPrefix(id, p) {
				return mk.Name
			}
		}
	}
	return ""
}
