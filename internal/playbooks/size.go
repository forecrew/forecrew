package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

// triageDoc is the output of the triage role (packs/schemas/triage.json).
type triageDoc struct {
	Impact        domain.Level              `json:"impact"`
	Reversibility domain.Reversibility      `json:"reversibility"`
	Areas         []string                  `json:"areas"`
	Reason        string                    `json:"reason"`
	Assumptions   []domain.StatedAssumption `json:"assumptions"`
}

// components counts the distinct areas the goal touches.
func (t triageDoc) components() int {
	seen := map[string]bool{}
	for _, a := range t.Areas {
		if a = strings.TrimSpace(a); a != "" {
			seen[a] = true
		}
	}
	return len(seen)
}

// check reports why the triage cannot be used, if so.
func (t triageDoc) check() error {
	if err := (domain.GoalSize{Impact: t.Impact, Reversibility: t.Reversibility, Reason: t.Reason}).Validate(); err != nil {
		return err
	}
	if t.components() == 0 {
		return errors.New("the triage names no area")
	}
	return nil
}

// councils applies the sizing rule to the triage: the product council when
// the impact or the reversibility is one the rule names, the technical
// council when the impact is one it names or the goal touches enough
// components.
func (t triageDoc) councils(s config.Sizing) []domain.CouncilKind {
	councils := []domain.CouncilKind{}
	if slices.Contains(s.Product.Impact, t.Impact) || slices.Contains(s.Product.Reversibility, t.Reversibility) {
		councils = append(councils, domain.CouncilProduct)
	}
	if slices.Contains(s.Technical.Impact, t.Impact) || t.components() >= s.Technical.Components {
		councils = append(councils, domain.CouncilTechnical)
	}
	return councils
}

// sizeOutput is the output of goal.size. The if_ lists are for a foreach
// that skips the council steps: [goal_id] when the goal gets the council,
// [] otherwise.
type sizeOutput struct {
	Councils    []domain.CouncilKind `json:"councils"`
	IfProduct   []uuid.UUID          `json:"if_product"`
	IfTechnical []uuid.UUID          `json:"if_technical"`
}

// goalSize is the action goal.size. A goal without a size takes it from
// with.triage, the output of the triage job: the councils the sizing rule
// (config sizing) gives it, stored as goals.size. A goal that has a size
// keeps it and triage is not read; that is how a human override (create_goal
// with councils) skips the triage job. It outputs the goal's councils and
// the if_product and if_technical lists.
func (a *actions) goalSize(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID       `json:"goal_id"`
		Triage json.RawMessage `json:"triage"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	g, err := a.loadGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	if g.Size == nil {
		var doc triageDoc
		if err := parseOutput(with.Triage, &doc); err != nil {
			return nil, fmt.Errorf("with: triage: %w", err)
		}
		if err := doc.check(); err != nil {
			return nil, fmt.Errorf("with: triage: %w", err)
		}
		size := domain.GoalSize{Impact: doc.Impact, Reversibility: doc.Reversibility, Councils: doc.councils(a.Sizing), Reason: doc.Reason}
		if err := a.Store.Goals().SetSize(ctx, g.AccountID, g.ID, size); err != nil {
			return nil, fmt.Errorf("store the size of %s: %w", g.Ref(), err)
		}
		// Read the goal back: a size stored by a call that ran first is the one that counts.
		if g, err = a.Store.Goals().Get(ctx, g.AccountID, g.ID); err != nil {
			return nil, fmt.Errorf("get %s: %w", g.Ref(), err)
		}
	}
	out := sizeOutput{Councils: g.Size.Councils, IfProduct: []uuid.UUID{}, IfTechnical: []uuid.UUID{}}
	if out.Councils == nil {
		out.Councils = []domain.CouncilKind{}
	}
	if slices.Contains(out.Councils, domain.CouncilProduct) {
		out.IfProduct = []uuid.UUID{g.ID}
	}
	if slices.Contains(out.Councils, domain.CouncilTechnical) {
		out.IfTechnical = []uuid.UUID{g.ID}
	}
	return out, nil
}
