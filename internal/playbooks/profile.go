package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/workflow"
)

// validateOutput is the output of profile.validate.
type validateOutput struct {
	Valid  bool   `json:"valid"`
	Errors string `json:"errors"` // empty when valid
}

// validateProfile is the action profile.validate. It checks against the
// profile schema the project profile in with.profile, a JSON value (when the
// key is given), and the profile file of with.repo at with.ref (when with.repo
// is given). It outputs whether both are valid, with the validator's errors.
// When not, the action fails if with.attempt is at least with.attempts (the
// last chance of a retry loop), with the errors in the message; otherwise the
// playbook can retry.
func (a *actions) validateProfile(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Profile  json.RawMessage `json:"profile"`
		Repo     string          `json:"repo"`
		Ref      string          `json:"ref"`
		Attempt  int             `json:"attempt"`
		Attempts int             `json:"attempts"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var problems []string
	if _, ok := call.With["profile"]; ok {
		if err := analyzer.Validate(with.Profile); err != nil {
			if !errors.Is(err, analyzer.ErrInvalidProfile) {
				return nil, err
			}
			problems = append(problems, "the profile in your answer: "+err.Error())
		}
	}
	if with.Repo != "" {
		problem, err := a.profileFileProblem(ctx, with.Repo, with.Ref)
		if err != nil {
			return nil, err
		}
		if problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) == 0 {
		return validateOutput{Valid: true}, nil
	}
	msg := strings.Join(problems, "\n")
	switch {
	case with.Attempts > 0 && with.Attempt >= with.Attempts:
		return nil, fmt.Errorf("the profile is still invalid after %d attempts: %s", with.Attempt, msg)
	}
	return validateOutput{Errors: msg}, nil
}

// profileFeedback is the action profile.feedback. It builds the feedback of
// an analyst job from the reviewer's with.human feedback, the validator's
// with.errors and the GitHub discovery with.discovery (each may be empty) and
// outputs it as a string, empty when there is nothing to say.
func (a *actions) profileFeedback(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Human     string `json:"human"`
		Errors    string `json:"errors"`
		Discovery string `json:"discovery"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var parts []string
	if h := strings.TrimSpace(with.Human); h != "" {
		parts = append(parts, "A reviewer asked for changes to the profile:\n"+h)
	}
	if e := strings.TrimSpace(with.Errors); e != "" {
		parts = append(parts, "Your last profile does not match the profile schema. Fix .forecrew/profile.yaml and the profile in your answer:\n"+e)
	}
	if d := strings.TrimSpace(with.Discovery); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n"), nil
}
