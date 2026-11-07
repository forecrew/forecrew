package models

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// PublicRepo reports whether a repo ("owner/name") is public; GitHub answers.
type PublicRepo func(ctx context.Context, repo string) (bool, error)

// publicFor is how long a repo that GitHub said is public counts as public.
// Nothing else is kept: a private repo, or one GitHub did not answer for, is
// asked again.
const publicFor = 5 * time.Minute

// visibility remembers which repos are public, for publicFor.
type visibility struct {
	ask PublicRepo
	now func() time.Time

	mu     sync.Mutex
	public map[string]time.Time // repo -> when GitHub said it is public
}

// WithPublicRepo makes s ask ask whether a repo is public: the private code
// policy does not apply to a public repo. Without it every repo counts as
// private.
func (s *Service) WithPublicRepo(ask PublicRepo, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	s.repos = &visibility{ask: ask, now: now, public: map[string]time.Time{}}
	return s
}

// isPublic reports whether repo is public. An error of GitHub counts as
// private: the policy fails closed.
func (v *visibility) isPublic(ctx context.Context, repo string, logger func(error)) bool {
	if v.ask == nil {
		return false
	}
	v.mu.Lock()
	at, ok := v.public[repo]
	v.mu.Unlock()
	if ok && v.now().Sub(at) < publicFor {
		return true
	}
	public, err := v.ask(ctx, repo)
	if err != nil {
		logger(err)
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if public {
		v.public[repo] = v.now()
	} else {
		delete(v.public, repo)
	}
	return public
}

// authorized reports whether c may receive the code of project.
func (s State) authorized(project uuid.UUID, c domain.ModelConnection) bool {
	return slices.ContainsFunc(s.Authorizations, func(a domain.ModelAuthorization) bool { return a.Covers(project, c) })
}

// private applies the private code policy (config-resources.md, Private
// code) to a decision for a job of project: an organization's connection
// receives a private repo's code only where it is authorized for the
// project, unless its endpoint is on your own network. The server file's
// runtimes are the existing setup and need nothing. A decision that fails
// waits, so its job stays queued: nothing falls back to another connection.
func (s *Service) private(ctx context.Context, accountID uuid.UUID, st State, project uuid.UUID, d Decision) (Decision, error) {
	if d.Source != SourceOrganization || d.Waiting != "" {
		return d, nil
	}
	c := d.connection
	if LocalEndpoint(c.BaseURL) || st.authorized(project, c) {
		return d, nil
	}
	p, err := s.st.Projects().Get(ctx, accountID, project)
	switch {
	case errors.Is(err, store.ErrNotFound):
		p = domain.Project{Name: "?", Repo: "?"}
	case err != nil:
		return d, fmt.Errorf("project of the job: %w", err)
	case s.repos.isPublic(ctx, p.Repo, func(err error) {
		s.logger.WarnContext(ctx, "repo visibility unknown, taken as private", "repo", p.Repo, "error", err)
	}):
		return d, nil
	}
	out := waiting("The code of %s is private, and connection %s is not authorized for project %s: confirm a plan with model_authorization %s %s",
		p.Repo, c.Name, p.Name, c.Name+"@"+p.Name, settingsHint)
	out.Code = ReasonPrivateCode
	return out, nil
}
