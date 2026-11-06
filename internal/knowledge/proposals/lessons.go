package proposals

import (
	"context"
	"fmt"
	"strings"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
)

// LessonSection is the section the feedback of gates is saved to.
const LessonSection = "lessons"

var _ gates.LessonSaver = (*Service)(nil)

// SaveLesson implements gates.LessonSaver: the feedback a human gave when
// resolving g becomes a proposal to the lessons section, sent as the human's
// own (a PR agent/kb-KB-n, no job and no gate), with the gate as its source.
// The entry is lessons/gate-<n>.md of the section's folder.
func (s *Service) SaveLesson(ctx context.Context, by domain.Actor, g domain.Gate, feedback string) error {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return fmt.Errorf("%w: a lesson needs the feedback as its text", ErrInvalid)
	}
	project, err := s.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return fmt.Errorf("project of %s: %w", g.Ref(), err)
	}
	policy, err := s.Policy(ctx, project)
	if err != nil {
		return err
	}
	file, ok := policy.EntryPath(LessonSection, strings.ToLower(g.Ref())+".md")
	if !ok {
		return fmt.Errorf("%w: the project has no lessons section in its repo to save to", ErrInvalid)
	}
	body := fmt.Sprintf("# Lesson from %s\n\n%s\n", g.Ref(), contextpack.OneLine(g.Title))
	body += "\n" + quoteLines(feedback) + "\n"
	_, err = s.Human(ctx, by, project, Input{
		Section: LessonSection, Path: file, Action: domain.KnowledgeAdd, Body: body, Gate: g.ID,
		Reason: fmt.Sprintf("Feedback given by %s when resolving %s (%s).", by.String(), g.Ref(), g.Status),
	}, func(protected bool) error {
		// Approving a protected section's change is a permission of its own,
		// which resolving a gate does not check.
		if protected {
			return fmt.Errorf("%w: the lessons section is protected: propose the lesson with propose_knowledge", ErrInvalid)
		}
		return nil
	})
	return err
}

// quoteLines writes text as a block quote: it is someone's words, and none
// of its lines becomes a heading or a list item of the entry.
func quoteLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}
