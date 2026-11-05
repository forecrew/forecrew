package contextpack

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/roles"
)

// Names of the sections of a context pack that are filled from the knowledge
// base, the registers and the tasks. They are the names a role file's
// context: lists (roles.ContextSections), besides SectionTestPlan.
const (
	SectionKnowledge    = "knowledge"
	SectionDecisions    = "decisions"
	SectionAssumptions  = "assumptions"
	SectionSimilarTasks = "similar_tasks"
	SectionNotes        = "notes"
	SectionRelatedFiles = "related_files"
	// SectionTestPlan is the repo's test matrix, strategy and environments.
	// It is not cut to the pack's budget: it is a section of the prompt
	// like the spec, and is cut with the prompt (Render).
	SectionTestPlan = "test_plan"
)

// minTruncated is the least room worth cutting an item to; an item that does
// not fit in less is dropped.
const minTruncated = 200

// contextIntro opens the context block of a prompt.
const contextIntro = "Background the project already holds for this job, most important first. " +
	"It may be incomplete or out of date: the checkout and the task are authoritative.\n"

// Item is one candidate of a Section.
type Item struct {
	// ID is the ref or UUID of the item (A-4, DEC-2, TASK-9, an entry's ID)
	// and Path its repo or knowledge path, for the job's manifest.
	ID, Path string
	// Label is the item's heading. It is trusted text of forecrewd: refs,
	// dates and names it validated, never text an author controls.
	Label string
	// Body is the item's text, markdown.
	Body string
	// Untrusted marks Body as written by an agent, a repo or a person outside
	// the allowlist: it reaches the prompt between untrusted-data markers,
	// with Source naming where it comes from (WrapUntrusted).
	Untrusted bool
	Source    string
}

// Section holds the candidates of one section of a context pack, the most
// relevant first. Render fills the sections a role lists, in the role's
// order, as far as its budget allows.
type Section struct {
	Name  string
	Items []Item
	// Note is trusted text shown instead of items when the section cannot be
	// filled, e.g. because the knowledge policy cannot be read now.
	Note string
}

// packBlock is the rendered context block of a prompt.
type packBlock struct {
	text      string
	untrusted bool
	manifest  *domain.ContextManifest
}

// render renders the item cut to at most limit characters (0 for no limit);
// the second result says whether it was cut.
func (it Item) render(limit int) (string, bool) {
	if it.Body == "" {
		text := "- " + it.Label + "\n"
		if limit > 0 && utf8.RuneCountInString(text) > limit {
			return "", true // a list entry is not cut
		}
		return text, false
	}
	head := "### " + it.Label + "\n\n"
	body := strings.TrimRight(it.Body, "\n")
	full := head + it.wrap(body)
	if limit <= 0 || utf8.RuneCountInString(full) <= limit {
		return full, false
	}
	// Cut the body before it is wrapped, so the closing marker survives.
	// The wrapped text may be longer than the cut one: shrink until it fits.
	room := limit - utf8.RuneCountInString(head+it.wrap("")) - 1
	for room > 0 {
		text := head + it.wrap(cut(body, room, nil))
		if utf8.RuneCountInString(text) <= limit {
			return text, true
		}
		room -= utf8.RuneCountInString(text) - limit
	}
	return "", true
}

// gap is the text between the item and the next one: a blank line, except
// after an item that is a list entry.
func (it Item) gap() string {
	if it.Body == "" {
		return ""
	}
	return "\n"
}

func (it Item) wrap(body string) string {
	if it.Untrusted {
		return WrapUntrusted(it.Source, body)
	}
	return body + "\n"
}

func (it Item) manifest(size int) domain.ContextItem {
	return domain.ContextItem{ID: it.ID, Path: it.Path, Size: size}
}

// fillSections builds the context block of a role from the candidate
// sections: those the role lists, in its order, each filled item by item
// until the budget in characters is used. An item that does not fit is cut
// when there is room for a useful part of it, the rest are dropped; the
// manifest names them. Sizes count the characters of the text the prompt
// gets, headings included. The test plan is not a section of the block.
func fillSections(role roles.Role, candidates []Section) packBlock {
	budget := role.ContextBudget
	if budget <= 0 {
		budget = roles.DefaultContextBudget("")
	}
	var names []string
	for _, n := range role.Context {
		if n != SectionTestPlan {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return packBlock{}
	}
	intro := "\n" + contextIntro
	used := utf8.RuneCountInString(intro)
	var b strings.Builder
	m := &domain.ContextManifest{Budget: budget}
	untrusted := false
	for _, name := range names {
		var cand Section
		for _, c := range candidates {
			if c.Name == name {
				cand = c
			}
		}
		sec, text, trust := fillSection(cand, name, budget-used)
		used += sec.Size
		untrusted = untrusted || trust
		b.WriteString(text)
		m.Sections = append(m.Sections, sec)
	}
	if b.Len() == 0 {
		return packBlock{manifest: m}
	}
	text := intro + b.String()
	m.Size = utf8.RuneCountInString(text)
	return packBlock{text: text, untrusted: untrusted, manifest: m}
}

// Texts a section shows when it holds no item.
const (
	noRoom   = "Nothing fits the context budget.\n"
	sectionH = "\n## %s\n\n"
)

// fillSection fills section name from cand in at most room characters. It
// returns the manifest entry, the text and whether an item in it is
// untrusted. A section without items and note, or whose heading does not
// fit, has no text.
func fillSection(cand Section, name string, room int) (sec domain.ContextSection, text string, untrusted bool) {
	sec = domain.ContextSection{Name: name, Items: []domain.ContextItem{}}
	head := fmt.Sprintf(sectionH, sectionHeading(name))
	room -= utf8.RuneCountInString(head)
	if room < utf8.RuneCountInString(noRoom) {
		for _, it := range cand.Items {
			sec.Dropped = append(sec.Dropped, it.manifest(utf8.RuneCountInString(fullText(it))))
		}
		return sec, "", false
	}
	var body strings.Builder
	prevEntry := false // the last item written is a list entry
	for _, it := range cand.Items {
		// A blank line sets an item off from the list entry before it.
		lead := ""
		if prevEntry && it.Body != "" {
			lead = "\n"
		}
		item, _ := it.render(0)
		if utf8.RuneCountInString(lead+item)+len(it.gap()) > room {
			item = ""
			if room-len(lead)-len(it.gap()) >= minTruncated {
				item, _ = it.render(room - len(lead) - len(it.gap()))
			}
			if item == "" {
				sec.Dropped = append(sec.Dropped, it.manifest(utf8.RuneCountInString(fullText(it))))
				continue
			}
		}
		body.WriteString(lead + item + it.gap())
		sec.Items = append(sec.Items, it.manifest(utf8.RuneCountInString(item)))
		room -= utf8.RuneCountInString(lead+item) + len(it.gap())
		prevEntry = it.Body == ""
		untrusted = untrusted || it.Untrusted
	}
	switch {
	case body.Len() > 0:
	case cand.Note != "" && utf8.RuneCountInString(cand.Note)+1 <= room:
		body.WriteString(cand.Note + "\n")
	case len(sec.Dropped) > 0:
		body.WriteString(noRoom)
	default:
		return sec, "", false // nothing to say
	}
	text = head + strings.TrimRight(body.String(), "\n") + "\n"
	sec.Size = utf8.RuneCountInString(text)
	return sec, text, untrusted
}

func fullText(it Item) string {
	text, _ := it.render(0)
	return text
}

// sectionHeading is the heading of a section in the prompt.
func sectionHeading(name string) string {
	switch name {
	case SectionSimilarTasks:
		return "Similar tasks"
	case SectionRelatedFiles:
		return "Related files"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
