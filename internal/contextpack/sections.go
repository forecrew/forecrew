package contextpack

import (
	"fmt"
	"math"
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

// space is room in a pack: characters, which the role's budget counts, and
// bytes, which the prompt's budget counts.
type space struct{ chars, bytes int }

// unbounded is room without limits.
var unbounded = space{math.MaxInt / 2, math.MaxInt / 2}

func measure(s string) space { return space{utf8.RuneCountInString(s), len(s)} }

func (a space) minus(b space) space { return space{a.chars - b.chars, a.bytes - b.bytes} }

func (a space) plus(b space) space { return space{a.chars + b.chars, a.bytes + b.bytes} }

func (a space) fits(b space) bool { return b.chars <= a.chars && b.bytes <= a.bytes }

// render renders the item cut to fit limit; the second result says whether
// it was cut. It is empty when not even a cut item fits.
func (it Item) render(limit space) (string, bool) {
	if it.Body == "" {
		text := "- " + it.Label + "\n"
		if !limit.fits(measure(text)) {
			return "", true // a list entry is not cut
		}
		return text, false
	}
	head := "### " + it.Label + "\n\n"
	body := strings.TrimRight(it.Body, "\n")
	full := head + it.wrap(body)
	if limit.fits(measure(full)) {
		return full, false
	}
	// Cut the body before it is wrapped, so the closing marker survives.
	// The wrapped text may be longer than the cut one: shrink until it fits.
	// The body is cut to bytes, which is at least its characters.
	overhead := measure(head + it.wrap(""))
	room := min(limit.chars-overhead.chars, limit.bytes-overhead.bytes) - 1
	for room > 0 {
		text := head + it.wrap(cut(body, room, nil))
		m := measure(text)
		if limit.fits(m) {
			return text, true
		}
		room -= max(m.chars-limit.chars, m.bytes-limit.bytes, 1)
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
// until the budget is used. The budget is the role's, in characters, and
// maxBytes (0 for none): what the prompt has room for. An item that does not
// fit is cut when there is room for a useful part of it, the rest are
// dropped. The prompt says how many items a section left out, and at the end
// which sections had no room at all; the manifest names every item. Sizes
// count the characters of the text the prompt gets, headings included. The
// test plan is not a section of the block.
func fillSections(role roles.Role, candidates []Section, maxBytes int) packBlock {
	left := space{role.ContextBudget, unbounded.bytes}
	if left.chars <= 0 {
		left.chars = roles.DefaultContextBudget("")
	}
	budget := left.chars
	if maxBytes > 0 {
		left.bytes = maxBytes
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
	cands := make([]Section, len(names))
	for i, n := range names {
		cands[i] = Section{Name: n}
		for _, c := range candidates {
			if c.Name == n {
				cands[i] = c
			}
		}
	}
	intro := "\n" + contextIntro
	left = left.minus(measure(intro))
	m := &domain.ContextManifest{Budget: budget}
	// The last line of the block names the sections left out; keep room for
	// it from the start.
	reserve := measure(omittedLine(cands, true))
	if !left.fits(reserve) {
		for _, c := range cands {
			m.Sections = append(m.Sections, omitted(c))
		}
		return packBlock{manifest: m}
	}
	left = left.minus(reserve)
	var b strings.Builder
	var missing []Section
	untrusted := false
	for _, c := range cands {
		sec, text, trust := fillSection(c, left)
		left = left.minus(measure(text))
		untrusted = untrusted || trust
		b.WriteString(text)
		if text == "" && len(sec.Dropped) > 0 {
			missing = append(missing, c)
		}
		m.Sections = append(m.Sections, sec)
	}
	if b.Len() == 0 {
		return packBlock{manifest: m}
	}
	text := intro + b.String() + omittedLine(missing, false)
	m.Size = utf8.RuneCountInString(text)
	return packBlock{text: text, untrusted: untrusted, manifest: m}
}

// omittedLine says which of cands had no room. With worst it is the longest
// line possible for them, the room to keep for it.
func omittedLine(cands []Section, worst bool) string {
	var parts []string
	for _, c := range cands {
		switch {
		case len(c.Items) == 0:
		case worst:
			parts = append(parts, fmt.Sprintf("%s (%d items)", c.Name, max(len(c.Items), 1000)))
		default:
			parts = append(parts, fmt.Sprintf("%s (%s)", c.Name, count(len(c.Items))))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\nLeft out for the budget: " + strings.Join(parts, ", ") + ".\n"
}

// count says how many items, e.g. "1 item", "3 items".
func count(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}

// omitted is the manifest entry of a section with no room in the pack.
func omitted(c Section) domain.ContextSection {
	sec := domain.ContextSection{Name: c.Name, Items: []domain.ContextItem{}}
	for _, it := range c.Items {
		sec.Dropped = append(sec.Dropped, it.manifest(utf8.RuneCountInString(fullText(it))))
	}
	return sec
}

const sectionH = "\n## %s\n\n"

// leftOut says how many items of a section did not fit.
func leftOut(dropped, placed int) string {
	if placed == 0 {
		return count(dropped) + " left out for the budget.\n"
	}
	return fmt.Sprintf("%s more left out for the budget.\n", count(dropped))
}

// fillSection fills a section in at most room. It returns the manifest
// entry, the text and whether an item in it is untrusted. A section without
// items and note has no text; neither has one that has no room for its
// heading and a line.
func fillSection(cand Section, room space) (sec domain.ContextSection, text string, untrusted bool) {
	name := cand.Name
	sec = domain.ContextSection{Name: name, Items: []domain.ContextItem{}}
	if len(cand.Items) == 0 && cand.Note == "" {
		return sec, "", false
	}
	head := fmt.Sprintf(sectionH, sectionHeading(name))
	note := ""
	if cand.Note != "" {
		note = "\n" + cand.Note + "\n"
	}
	explain := measure("\n" + leftOut(len(cand.Items), 1))
	need := measure(head + note)
	if note == "" {
		need = measure(head).plus(explain)
	}
	if !room.fits(need) {
		return omitted(cand), "", false
	}
	room = room.minus(measure(head + note))
	var body strings.Builder
	prevEntry := false // the last item written is a list entry
	for i, it := range cand.Items {
		// A blank line sets an item off from the list entry before it.
		lead := ""
		if prevEntry && it.Body != "" {
			lead = "\n"
		}
		avail := room
		if i < len(cand.Items)-1 || len(sec.Dropped) > 0 {
			avail = room.minus(explain) // a later item may be left out
		}
		item, _ := it.render(unbounded)
		if !avail.fits(measure(lead + item + it.gap())) {
			item = ""
			if limit := avail.minus(measure(lead + it.gap())); limit.chars >= minTruncated && limit.bytes >= minTruncated {
				item, _ = it.render(limit)
			}
			if item == "" {
				sec.Dropped = append(sec.Dropped, it.manifest(utf8.RuneCountInString(fullText(it))))
				continue
			}
		}
		body.WriteString(lead + item + it.gap())
		sec.Items = append(sec.Items, it.manifest(utf8.RuneCountInString(item)))
		room = room.minus(measure(lead + item + it.gap()))
		prevEntry = it.Body == ""
		untrusted = untrusted || it.Untrusted
	}
	text = head + strings.TrimRight(body.String(), "\n")
	if body.Len() > 0 {
		text += "\n"
	}
	if len(sec.Dropped) > 0 {
		if body.Len() > 0 {
			text += "\n"
		}
		text += leftOut(len(sec.Dropped), len(sec.Items))
	}
	if note != "" {
		if len(sec.Items) == 0 && len(sec.Dropped) == 0 {
			note = note[1:] // no blank line after the heading
		}
		text += note
	}
	sec.Size = utf8.RuneCountInString(text)
	return sec, text, untrusted
}

func fullText(it Item) string {
	text, _ := it.render(unbounded)
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
