package knowledge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/schema"
)

// Entry is a repo knowledge file split into its frontmatter and body
// (docs/contracts/knowledge-entry.schema.json).
type Entry struct {
	Title string `json:"title"`
	// HasProvenance is false when the frontmatter lacks author_role,
	// source or created; the index then derives provenance from git.
	HasProvenance bool              `json:"has_provenance"`
	Provenance    domain.Provenance `json:"provenance"`
	Body          string            `json:"body"` // Markdown after the frontmatter
}

// frontmatter is the part of the YAML forecrew reads; other keys are
// ignored.
type frontmatter struct {
	Title        string `json:"title"`
	AuthorRole   string `json:"author_role"`
	Source       string `json:"source"`
	Created      string `json:"created"`
	LastVerified string `json:"last_verified"`
}

// ParseEntry splits a Markdown knowledge file at path into frontmatter and
// body and checks the frontmatter against its schema. A file may have no
// frontmatter. The title is the frontmatter's, else the first # heading,
// else the file name without its extension. Errors wrap ErrInvalid.
func ParseEntry(file string, data []byte) (Entry, error) {
	front, body, err := splitFrontmatter(data)
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", file, err)
	}
	var fm frontmatter
	if front != nil {
		raw, inst, err := schema.DecodeYAML(front)
		if err != nil {
			return Entry{}, fmt.Errorf("%s: frontmatter: %w", file, err)
		}
		if err := schema.CheckContract("knowledge-entry.schema.json", inst); err != nil {
			return Entry{}, fmt.Errorf("%s: frontmatter: %w", file, err)
		}
		if err := jsonDecode(raw, &fm); err != nil {
			return Entry{}, fmt.Errorf("%s: frontmatter: %w", file, err)
		}
	}
	e := Entry{Title: fm.Title, Body: string(body)}
	if e.Title == "" {
		e.Title = firstHeading(e.Body)
	}
	if e.Title == "" {
		e.Title = strings.TrimSuffix(path.Base(file), path.Ext(file))
	}
	e.Provenance = domain.Provenance{AuthorRole: fm.AuthorRole, Source: fm.Source}
	// The schema checked the time format, so parsing cannot fail.
	e.Provenance.Created = parseTime(fm.Created)
	e.Provenance.LastVerified = parseTime(fm.LastVerified)
	e.HasProvenance = fm.AuthorRole != "" && fm.Source != "" && fm.Created != ""
	return e, nil
}

// splitFrontmatter returns the YAML between a leading "---" line and the
// next "---" line, and the rest. Without a leading "---" line the whole
// file is the body and front is nil.
func splitFrontmatter(data []byte) (front, body []byte, err error) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(data, []byte("---\n"))
	if !ok {
		return nil, data, nil
	}
	if after, ok := bytes.CutPrefix(rest, []byte("---\n")); ok {
		return []byte{}, after, nil
	}
	front, body, ok = bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		if front, ok = bytes.CutSuffix(rest, []byte("\n---")); !ok {
			return nil, nil, fmt.Errorf("%w: frontmatter has no closing ---", ErrInvalid)
		}
	}
	return append(front, '\n'), body, nil
}

func firstHeading(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if t, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// parseTime parses a date or RFC 3339 time, as UTC; empty is zero.
func parseTime(s string) time.Time {
	for _, layout := range []string{time.DateOnly, time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func jsonDecode(raw json.RawMessage, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}
