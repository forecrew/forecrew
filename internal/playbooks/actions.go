package playbooks

import (
	"context"
	"errors"
	"fmt"

	"github.com/forecrew/forecrew/internal/actionsprovider"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/workflow"
)

// installOutput is the output of actions.install_workflow.
type installOutput struct {
	Path string `json:"path"`
	// Changed is false when the repo already had this exact workflow.
	Changed bool `json:"changed"`
}

// installWorkflow is the action actions.install_workflow. It writes the
// forecrew-matrix workflow (packs/workflows) to .github/workflows on
// with.branch of with.repo, which command jobs of pool actions dispatch. It
// leaves a file that already holds the same content alone, so a repeated
// call changes nothing.
func (a *actions) installWorkflow(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Repo == "" || with.Branch == "" {
		return nil, errors.New("with: repo and branch are required")
	}
	template, err := actionsprovider.Template(a.Packs)
	if err != nil {
		return nil, err
	}
	changed, err := a.Actions.WriteFile(ctx, with.Repo, with.Branch, actionsprovider.WorkflowPath, template, "Add the forecrew-matrix workflow")
	switch {
	case errors.Is(err, github.ErrForbidden):
		return nil, fmt.Errorf("the GitHub App may not write %s to %s: it needs the Contents and Workflows write permissions: %w", actionsprovider.WorkflowPath, with.Repo, err)
	case err != nil:
		return nil, fmt.Errorf("install %s on %s of %s: %w", actionsprovider.WorkflowFile, with.Branch, with.Repo, err)
	}
	return installOutput{Path: actionsprovider.WorkflowPath, Changed: changed}, nil
}
