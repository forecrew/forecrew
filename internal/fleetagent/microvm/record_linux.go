package microvm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// record is what the provider knows of a machine. It is written before anything
// is made and removed after everything is gone, and it holds no secret.
type record struct {
	ID       string            `json:"id"`
	VMM      string            `json:"vmm"`
	Slot     int               `json:"slot"`
	PID      int               `json:"pid,omitempty"`
	MemoryMB int               `json:"memory_mb"`
	DiskGB   int               `json:"disk_gb"`
	GPUs     []string          `json:"gpus,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
	Allow    []string          `json:"allow,omitempty"`
	Server   []string          `json:"server,omitempty"` // addresses of forecrewd
	Port     int               `json:"port,omitempty"`
	Deadline time.Time         `json:"deadline,omitzero"`
	// OrgCache is the organization's cache it has, <org>/<pool>.
	OrgCache string `json:"org_cache,omitempty"`
	// Abuse says why the abuse checks stopped it; its cache is not kept.
	Abuse     string    `json:"abuse,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

var validID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

func (p *Provider) recordPath(id string) string {
	return filepath.Join(p.cfg.StateDir, "machines", id+".json")
}

// save writes r whole or not at all.
func (p *Provider) save(r *record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	path := p.recordPath(r.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	return nil
}

// load reads the record of id; a machine with none gives nil.
func (p *Provider) load(id string) (*record, error) {
	if !validID.MatchString(id) {
		return nil, nil // nothing of ours has that name
	}
	raw, err := os.ReadFile(p.recordPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read record: %w", err)
	}
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("read record of %s: %w", id, err)
	}
	return &r, nil
}

// records reads them all, oldest first.
func (p *Provider) records() ([]*record, error) {
	entries, err := os.ReadDir(filepath.Join(p.cfg.StateDir, "machines"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read records: %w", err)
	}
	var out []*record
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		r, err := p.load(id)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *record) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// running reports that r's VMM is alive: its process, not a zombie, and not
// another process that got its PID.
func running(r *record) bool {
	if r.PID <= 0 {
		return false
	}
	proc := filepath.Join("/proc", strconv.Itoa(r.PID))
	line, err := os.ReadFile(filepath.Join(proc, "cmdline")) // #nosec G304 -- a path of /proc
	if err != nil || !strings.Contains(string(line), r.ID) {
		return false
	}
	stat, err := os.ReadFile(filepath.Join(proc, "stat")) // #nosec G304 -- a path of /proc
	if err != nil {
		return false
	}
	// The state follows the name, which ends at the last parenthesis.
	s := string(stat)
	rest := s[strings.LastIndexByte(s, ')')+1:]
	return !strings.HasPrefix(strings.TrimSpace(rest), "Z")
}
