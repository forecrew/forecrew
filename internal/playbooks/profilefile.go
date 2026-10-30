package playbooks

import (
	"context"
	"errors"
	"fmt"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/github"
)

// profilePath is where the analyst writes the profile in the repo.
const profilePath = ".forecrew/profile.yaml"

// profileFileProblem reads the profile file of repo at ref and validates it.
// It returns what is wrong with the file, or "" when it is valid. An error
// means the file could not be read, which a retry may fix.
func (a *actions) profileFileProblem(ctx context.Context, repo, ref string) (string, error) {
	file, err := a.Tracker.GetFile(ctx, repo, ref, profilePath)
	switch {
	case errors.Is(err, github.ErrNotFound):
		return fmt.Sprintf("%s does not exist at %s", profilePath, ref), nil
	case err != nil:
		return "", fmt.Errorf("read %s at %s: %w", profilePath, ref, err)
	}
	if err := analyzer.Validate(file); err != nil {
		if !errors.Is(err, analyzer.ErrInvalidProfile) {
			return "", err
		}
		return fmt.Sprintf("%s at %s: %s", profilePath, ref, err), nil
	}
	return "", nil
}
