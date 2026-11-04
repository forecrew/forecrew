package contextpack

import (
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
)

// WrapKnowledge marks every text of an untrusted search result as data: its
// title, path, link, snippet, reverify reason, tags and, unless it is a note,
// the author and source of its provenance, each between its own delimiters. The label names only
// the kind and provider, never text the author controls. A trusted result is
// returned as it is.
func WrapKnowledge(r knowledge.Result) knowledge.Result {
	if !r.Untrusted {
		return r
	}
	label := "knowledge " + string(r.Kind) + " " + r.Provider
	wrap := func(s string) string {
		if s == "" {
			return ""
		}
		return WrapUntrusted(label, s)
	}
	r.Title, r.Path, r.SourceURL, r.Snippet = wrap(r.Title), wrap(r.Path), wrap(r.SourceURL), wrap(r.Snippet)
	r.ReverifyReason = wrap(r.ReverifyReason)
	if r.Provider != domain.ProviderInternal { // forecrewd writes the provenance of a note itself
		r.Provenance.AuthorRole, r.Provenance.Source = wrap(r.Provenance.AuthorRole), wrap(r.Provenance.Source)
	}
	if len(r.Tags) > 0 {
		tags := make([]string, len(r.Tags))
		for i, t := range r.Tags {
			tags[i] = wrap(t)
		}
		r.Tags = tags
	}
	return r
}
