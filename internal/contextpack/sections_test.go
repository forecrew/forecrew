package contextpack

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/forecrew/forecrew/internal/roles"
)

// allSections has an item or two for every section of a context pack.
func allSections() []Section {
	return []Section{
		{Name: SectionKnowledge, Items: []Item{
			{ID: "5b1f8f3e-0000-4000-8000-000000000001", Path: "docs/adr/0003-tokens.md", Label: "entry in architecture",
				Body: "ADR 3: Hash tokens\ndocs/adr/0003-tokens.md\n\nStore only the SHA-256 of a reset token.", Untrusted: true, Source: "knowledge entry repo"},
			{ID: "5b1f8f3e-0000-4000-8000-000000000002", Path: "knowledge/vision/why.md", Label: "entry in vision",
				Body: "Why Forecrew\nknowledge/vision/why.md\n\nThe queue is the product."},
		}},
		{Name: SectionDecisions, Items: []Item{
			{ID: "DEC-2", Label: "DEC-2", Body: "Question: Where do reset tokens live?\nChoice: In Postgres\nReasons: One store to back up.\n",
				Untrusted: true, Source: "decision DEC-2"},
			{ID: "5b1f8f3e-0000-4000-8000-000000000001", Path: "docs/adr/0003-tokens.md", Label: "entry in architecture",
				Body: "ADR 3: Hash tokens\ndocs/adr/0003-tokens.md", Untrusted: true, Source: "knowledge entry repo"},
		}},
		{Name: SectionAssumptions, Items: []Item{
			{ID: "A-4", Label: "A-4 (impact high, confidence low, verify by test)", Body: "Reset links are used once.",
				Untrusted: true, Source: "assumption A-4"},
		}},
		{Name: SectionSimilarTasks, Items: []Item{
			{ID: "TASK-9", Label: "TASK-9, merged, PR https://github.com/acme/app/pull/17, reviewed 2 time(s), last verdict approve",
				Body: "Add session store\n\nStore sessions in Postgres.", Untrusted: true, Source: "task TASK-9"},
		}},
		{Name: SectionNotes, Items: []Item{
			{ID: "5b1f8f3e-0000-4000-8000-000000000003", Path: "notes/abc", Label: "Note by developer (JOB-12), 2026-01-02",
				Body: "Token store\n\nThe store package wraps every error.", Untrusted: true, Source: "agent note by developer"},
		}},
		{Name: SectionRelatedFiles, Items: []Item{
			{Path: "internal/store/tokens.go", Label: "`internal/store/tokens.go`"},
			{Path: "web/src/reset.tsx", Label: "`web/src/reset.tsx`"},
			{ID: "GOAL-3", Label: "Areas the goal touches, from triage", Body: "- store\n- web\n", Untrusted: true, Source: "triage of GOAL-3"},
		}},
	}
}

func TestRenderContextGolden(t *testing.T) {
	c := catalog(t)
	untrustedGoal := goal
	for _, name := range []string{"developer", "test-engineer"} {
		t.Run(name, func(t *testing.T) {
			in := Input{Repo: "acme/app", BaseRef: "main", Branch: "agent/task-12", Profile: profile, Goal: untrustedGoal, Spec: spec, Task: task,
				Sections: allSections()}
			if name == "test-engineer" {
				in.TestPlan = testPlanText("main")
			}
			pack, err := Render(resolve(t, c, name), in, 0)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			checkGolden(t, filepath.Join("testdata", "golden", "context-"+name+".md"), pack.Prompt)
			m := pack.Manifest
			if m == nil || m.Budget != 16_000 || m.Size == 0 || m.Size > m.Budget || len(m.Sections) != len(resolve(t, c, name).Context)-testPlanCount(name) {
				t.Fatalf("manifest = %+v", m)
			}
			for _, s := range m.Sections {
				if len(s.Dropped) != 0 || len(s.Items) == 0 && s.Size != 0 {
					t.Errorf("section %s = %+v", s.Name, s)
				}
			}
		})
	}
}

func testPlanCount(role string) int {
	if role == "test-engineer" {
		return 1
	}
	return 0
}

// The sections come in the order of the role's list, which is their priority.
func TestFillSectionsOrder(t *testing.T) {
	role := roles.Role{Name: "x", Context: []string{SectionNotes, SectionAssumptions}}
	b := fillSections(role, allSections())
	if i, j := strings.Index(b.text, "## Notes"), strings.Index(b.text, "## Assumptions"); i < 0 || j < i {
		t.Errorf("notes should come before assumptions:\n%s", b.text)
	}
	for _, absent := range []string{"## Knowledge", "## Decisions", "## Similar tasks"} {
		if strings.Contains(b.text, absent) {
			t.Errorf("block has %q", absent)
		}
	}
	if !b.untrusted {
		t.Error("block with untrusted items is not marked untrusted")
	}
}

func bulky(name string, n, size int) Section {
	s := Section{Name: name}
	for i := range n {
		s.Items = append(s.Items, Item{ID: fmt.Sprintf("%s-%d", name, i+1), Label: fmt.Sprintf("%s %d", name, i+1),
			Body: strings.Repeat("word ", size/5), Untrusted: true, Source: "test"})
	}
	return s
}

// Sections are filled by priority: what does not fit is cut or dropped, the
// manifest names it, and the pack never exceeds the budget.
func TestFillSectionsBudget(t *testing.T) {
	role := roles.Role{Name: "x", ContextBudget: 3300, Context: []string{SectionDecisions, SectionAssumptions, SectionNotes}}
	cands := []Section{bulky(SectionDecisions, 2, 1000), bulky(SectionAssumptions, 3, 1000), bulky(SectionNotes, 2, 1000)}
	b := fillSections(role, cands)
	m := b.manifest
	if m.Budget != 3300 || m.Size != utf8.RuneCountInString(b.text) || m.Size > m.Budget {
		t.Fatalf("manifest budget %d, size %d, text %d", m.Budget, m.Size, utf8.RuneCountInString(b.text))
	}
	dec, ass, notes := m.Sections[0], m.Sections[1], m.Sections[2]
	if len(dec.Items) != 2 || len(dec.Dropped) != 0 {
		t.Errorf("decisions have the highest priority and fit: %+v", dec)
	}
	if len(ass.Items) != 1 || len(ass.Dropped) != 2 || ass.Items[0].ID != "assumptions-1" || ass.Dropped[1].ID != "assumptions-3" || ass.Items[0].Size >= ass.Dropped[0].Size {
		t.Errorf("assumptions should keep a cut first item and drop the others: %+v", ass)
	}
	if len(notes.Items) != 0 || len(notes.Dropped) != 2 || notes.Dropped[0].Size < 1000 {
		t.Errorf("notes have no room left: %+v", notes)
	}
	if !strings.Contains(b.text, truncated) {
		t.Errorf("a cut item should end with the truncation note:\n%s", b.text)
	}
	if strings.Contains(b.text, "assumptions 3") || strings.Contains(b.text, "## Notes") {
		t.Errorf("dropped items must not appear, nor the heading of a section without room:\n%s", b.text)
	}
	// A cut untrusted item keeps both of its markers.
	if strings.Count(b.text, "<untrusted-data ") != strings.Count(b.text, "</untrusted-data ") {
		t.Errorf("unbalanced untrusted markers:\n%s", b.text)
	}
	sum := 0
	for _, s := range m.Sections {
		sum += s.Size
	}
	if sum+utf8.RuneCountInString("\n"+contextIntro) != m.Size {
		t.Errorf("section sizes %d plus the intro should make %d", sum, m.Size)
	}
}

func TestFillSectionsEdges(t *testing.T) {
	t.Run("no sections listed", func(t *testing.T) {
		if b := fillSections(roles.Role{Name: "x", Context: []string{SectionTestPlan}}, allSections()); b.manifest != nil || b.text != "" {
			t.Errorf("block = %+v", b)
		}
		if b := fillSections(roles.Role{Name: "x"}, allSections()); b.manifest != nil {
			t.Errorf("block = %+v", b)
		}
	})
	t.Run("nothing to show", func(t *testing.T) {
		b := fillSections(roles.Role{Name: "x", Context: []string{SectionKnowledge, SectionNotes}}, nil)
		if b.text != "" || b.manifest == nil || len(b.manifest.Sections) != 2 || b.manifest.Size != 0 || b.manifest.Budget != 16_000 {
			t.Errorf("block = %+v", b)
		}
	})
	t.Run("note instead of items", func(t *testing.T) {
		b := fillSections(roles.Role{Name: "x", Context: []string{SectionKnowledge}}, []Section{{Name: SectionKnowledge, Note: "Not available now."}})
		if !strings.Contains(b.text, "## Knowledge\n\nNot available now.\n") || b.untrusted {
			t.Errorf("block = %q", b.text)
		}
	})
	t.Run("a section whose items were all dropped says so", func(t *testing.T) {
		role := roles.Role{Name: "x", ContextBudget: 340, Context: []string{SectionNotes}}
		b := fillSections(role, []Section{bulky(SectionNotes, 2, 2000)})
		if !strings.Contains(b.text, "## Notes\n\n"+noRoom) || len(b.manifest.Sections[0].Dropped) != 2 {
			t.Errorf("block = %q, %+v", b.text, b.manifest)
		}
	})
	t.Run("a budget too small for a heading", func(t *testing.T) {
		role := roles.Role{Name: "x", ContextBudget: 150, Context: []string{SectionNotes}}
		b := fillSections(role, []Section{bulky(SectionNotes, 1, 100)})
		s := b.manifest.Sections[0]
		if b.text != "" || len(s.Items) != 0 || len(s.Dropped) != 1 || b.manifest.Size != 0 {
			t.Errorf("block = %+v, section %+v", b, s)
		}
	})
	t.Run("list entries are never cut", func(t *testing.T) {
		long := Item{Path: strings.Repeat("a/", 100), Label: "`" + strings.Repeat("a/", 100) + "`"}
		if text, cutDown := long.render(50); text != "" || !cutDown {
			t.Errorf("render = %q, %v", text, cutDown)
		}
		short := Item{Label: "`a`"}
		if text, cutDown := short.render(50); text != "- `a`\n" || cutDown || short.gap() != "" {
			t.Errorf("render = %q, %v", text, cutDown)
		}
	})
	t.Run("a trusted item is cut as it is", func(t *testing.T) {
		it := Item{Label: "L", Body: strings.Repeat("abc ", 300)}
		text, cutDown := it.render(300)
		if !cutDown || utf8.RuneCountInString(text) > 300 || !strings.Contains(text, truncated) {
			t.Errorf("render = %q, %v", text, cutDown)
		}
	})
	t.Run("an item that cannot be cut to the limit", func(t *testing.T) {
		it := Item{Label: "L", Body: "abc", Untrusted: true, Source: "s"}
		if text, _ := it.render(10); text != "" {
			t.Errorf("render = %q", text)
		}
	})
}

func TestSectionHeading(t *testing.T) {
	for name, want := range map[string]string{SectionSimilarTasks: "Similar tasks", SectionRelatedFiles: "Related files", SectionKnowledge: "Knowledge"} {
		if got := sectionHeading(name); got != want {
			t.Errorf("sectionHeading(%q) = %q, want %q", name, got, want)
		}
	}
}
