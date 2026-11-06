package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/mcp"
)

// MaxRepoFile is the largest repo file a plan writes or reads.
const MaxRepoFile = 256 << 10

// repoDir is the only place a plan writes in a repo.
const repoDir = ".forecrew/"

// profileFile holds the integrations of a project.
const profileFile = repoDir + "profile.yaml"

// knownRepoFiles are the single files exported as repo_file resources.
var knownRepoFiles = []string{"matrix.yaml"}

// Repo reads a repo and opens pull requests; github.Tracker implements it.
type Repo interface {
	GetFile(ctx context.Context, repo, ref, path string) ([]byte, error)
	HeadSHA(ctx context.Context, repo, branch string) (string, error)
	BranchExists(ctx context.Context, repo, branch string) (bool, error)
	CreateBranch(ctx context.Context, repo, branch, sha string) error
	WriteFile(ctx context.Context, repo, branch, path string, content []byte, message string) (bool, error)
	DeleteFile(ctx context.Context, repo, branch, path, message string) (bool, error)
	FindPR(ctx context.Context, repo, branch string) (github.PullRequest, error)
	CreatePR(ctx context.Context, repo string, in github.PRInput) (github.PullRequest, error)
}

// openPR writes files on a branch of its own and opens the pull request.
// Every step finds what an earlier try did, so retrying a plan is safe.
func openPR(ctx context.Context, r Repo, p domain.Project, branch, title string, files []FileChange) (string, error) {
	exists, err := r.BranchExists(ctx, p.Repo, branch)
	if err != nil {
		return "", err
	}
	if !exists {
		sha, err := r.HeadSHA(ctx, p.Repo, p.DefaultBranch)
		if err != nil {
			return "", err
		}
		if err := r.CreateBranch(ctx, p.Repo, branch, sha); err != nil {
			return "", err
		}
	}
	for _, f := range files {
		if f.Content == nil {
			_, err = r.DeleteFile(ctx, p.Repo, branch, f.Path, title)
		} else {
			_, err = r.WriteFile(ctx, p.Repo, branch, f.Path, f.Content, title)
		}
		if err != nil {
			return "", err
		}
	}
	pr, err := r.FindPR(ctx, p.Repo, branch)
	if errors.Is(err, github.ErrNotFound) {
		pr, err = r.CreatePR(ctx, p.Repo, github.PRInput{Title: title, Head: branch, Base: p.DefaultBranch, Body: "Opened from a change plan of Forecrew."})
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", p.Repo, pr.Number), nil
}

// projectOf finds the project a repo resource is named after.
func projectOf(ctx context.Context, e *Env, name string) (domain.Project, error) {
	p, err := e.Store.Projects().GetByName(ctx, e.Actor.AccountID, name)
	if err != nil {
		return p, missing(err, "project "+name)
	}
	return p, nil
}

// readFile reads a file of the project's default branch; nil when it is not
// there. Without GitHub, repo files read as absent.
func readFile(ctx context.Context, e *Env, p domain.Project, path string) ([]byte, error) {
	if e.Repo == nil {
		return nil, nil
	}
	data, err := e.Repo.GetFile(ctx, p.Repo, p.DefaultBranch, path)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s of %s: %w", path, p.Repo, err)
	}
	if len(data) > MaxRepoFile {
		return nil, invalid("%s of %s is over %d bytes", path, p.Repo, MaxRepoFile)
	}
	return data, nil
}

// writes records the files of a repo change for its project: a plan opens
// one pull request, on one repo.
func writes(ctx context.Context, e *Env, projectName string, files ...FileChange) error {
	p, err := projectOf(ctx, e, projectName)
	if err != nil {
		return err
	}
	if e.project != nil && e.project.ID != p.ID {
		return invalid("a plan changes the files of one repo")
	}
	e.project = &p
	e.files = append(e.files, files...)
	return nil
}

// repoNeeds is what changing a project's files takes: project.manage on it.
func repoNeeds(ctx context.Context, e *Env, r *Resource, name func(string) string) ([]Need, error) {
	p, err := projectOf(ctx, e, name(r.Name))
	if err != nil {
		return nil, err
	}
	return []Need{{Permission: domain.PermProjectManage, ProjectID: p.ID}}, nil
}

func firstOf(old, want *Resource) *Resource {
	if want != nil {
		return want
	}
	return old
}

func same(s string) string { return s }

// repoArea fills in what the repo kinds have in common.
func repoArea(a Area, name func(string) string) Area {
	a.RepoFiles = true
	a.Affects = "Nothing changes until the pull request is merged."
	a.Needs = func(ctx context.Context, e *Env, old, want *Resource, _ []domain.ConfigFieldChange) ([]Need, error) {
		return repoNeeds(ctx, e, firstOf(old, want), name)
	}
	return a
}

// perProject reads one resource per project the caller may read.
func perProject(ctx context.Context, e *Env, fn func(p domain.Project) ([]Resource, error)) ([]Resource, error) {
	projects, err := e.Store.Projects().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	var out []Resource
	for _, p := range projects {
		ok, err := e.Authz.Can(ctx, e.Actor, domain.PermProjectRead, projectResource(e, p))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		rs, err := fn(p)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

type integrationSpec struct {
	Integrations []analyzer.Integration `json:"integrations"`
}

func integrationArea() Area {
	a := base[integrationSpec](KindIntegration, "Integrations of", ScopeProject, domain.PermOrgRead)
	a.Setting, a.Hidden = integrationSpec{}, nil
	a = repoArea(a, same)
	a.NoDelete = "integrations are removed by leaving them out of the list"
	a.Read = integrationRead
	a.Validate = integrationValidate
	a.Apply = integrationApply
	return a
}

func integrationRead(ctx context.Context, e *Env) ([]Resource, error) {
	return perProject(ctx, e, func(p domain.Project) ([]Resource, error) {
		if len(p.Profile) == 0 {
			return nil, nil
		}
		var profile struct {
			Integrations []analyzer.Integration `json:"integrations"`
		}
		if err := json.Unmarshal(p.Profile, &profile); err != nil {
			return nil, fmt.Errorf("profile of %s: %w", p.Name, err)
		}
		return []Resource{{Kind: KindIntegration, Name: p.Name, Spec: encode(integrationSpec{profile.Integrations})}}, nil
	})
}

func integrationValidate(ctx context.Context, e *Env, _ *Resource, want Resource) error {
	if _, err := decode[integrationSpec](want.Spec); err != nil {
		return err
	}
	if err := checkNoSecret(string(want.Spec)); err != nil {
		return err
	}
	_, err := projectOf(ctx, e, want.Name)
	return err
}

func integrationApply(ctx context.Context, e *Env, _, want *Resource) error {
	s, err := decode[integrationSpec](want.Spec)
	if err != nil {
		return err
	}
	p, err := projectOf(ctx, e, want.Name)
	if err != nil {
		return err
	}
	data, err := readFile(ctx, e, p, profileFile)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return invalid("%s does not parse: %v", profileFile, err)
	}
	if doc == nil {
		return invalid("%s has no profile yet", want.Name)
	}
	var list any
	raw, err := yaml.Marshal(s.Integrations)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(raw, &list); err != nil {
		return err
	}
	doc["integrations"] = list
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := analyzer.Validate(out); err != nil {
		return invalid("the profile would not be valid: %v", err)
	}
	return writes(ctx, e, want.Name, FileChange{Path: profileFile, Content: out})
}

// knowledgePolicySpec is .forecrew/knowledge-policy.yaml as text.
type knowledgePolicySpec struct {
	Content string `json:"content"`
}

func knowledgePolicyArea() Area {
	a := base[knowledgePolicySpec](KindKnowledgePolicy, "Knowledge policy of", ScopeProject, domain.PermOrgRead)
	a.Setting = knowledgePolicySpec{}
	a = repoArea(a, same)
	a.Read = knowledgePolicyRead
	a.Validate = knowledgePolicyValidate
	a.Apply = knowledgePolicyApply
	return a
}

func knowledgePolicyRead(ctx context.Context, e *Env) ([]Resource, error) {
	return perProject(ctx, e, func(p domain.Project) ([]Resource, error) {
		data, err := readFile(ctx, e, p, knowledge.PolicyFile)
		if err != nil || data == nil {
			return nil, err
		}
		return []Resource{{Kind: KindKnowledgePolicy, Name: p.Name, Spec: encode(knowledgePolicySpec{string(data)})}}, nil
	})
}

func knowledgePolicyValidate(ctx context.Context, e *Env, _ *Resource, want Resource) error {
	s, err := decode[knowledgePolicySpec](want.Spec)
	if err != nil {
		return err
	}
	if _, err := knowledge.ParsePolicy([]byte(s.Content)); err != nil {
		return invalid("%v", err)
	}
	if err := checkNoSecret(s.Content); err != nil {
		return err
	}
	_, err = projectOf(ctx, e, want.Name)
	return err
}

func knowledgePolicyApply(ctx context.Context, e *Env, old, want *Resource) error {
	if want == nil {
		return writes(ctx, e, old.Name, FileChange{Path: knowledge.PolicyFile})
	}
	s, err := decode[knowledgePolicySpec](want.Spec)
	if err != nil {
		return err
	}
	return writes(ctx, e, want.Name, FileChange{Path: knowledge.PolicyFile, Content: []byte(s.Content)})
}

// repoFileSpec is any file under .forecrew/. A repo file's name is
// <project>:<path>.
type repoFileSpec struct {
	Content string `json:"content"`
}

func splitRepoFile(name string) (project, path string, err error) {
	project, path, ok := strings.Cut(name, ":")
	if !ok || project == "" || !strings.HasPrefix(path, repoDir) || !domain.RelativePath(path) || path == repoDir {
		return "", "", invalid("a repo file is named <project>:.forecrew/<path>, not %q", name)
	}
	return project, path, nil
}

func repoFileProject(name string) string {
	project, _, _ := strings.Cut(name, ":")
	return project
}

func repoFileArea() Area {
	a := base[repoFileSpec](KindRepoFile, "Repo file", ScopeProject, domain.PermOrgRead)
	a.Setting = repoFileSpec{}
	a = repoArea(a, repoFileProject)
	a.Read = repoFileRead
	a.Validate = repoFileValidate
	a.Apply = repoFileApply
	return a
}

func repoFileRead(ctx context.Context, e *Env) ([]Resource, error) {
	return perProject(ctx, e, func(p domain.Project) ([]Resource, error) {
		var out []Resource
		for _, f := range append(slices.Clone(knownRepoFiles), "profile.yaml") {
			data, err := readFile(ctx, e, p, repoDir+f)
			if err != nil {
				return nil, err
			}
			if data != nil {
				out = append(out, Resource{Kind: KindRepoFile, Name: p.Name + ":" + repoDir + f, Spec: encode(repoFileSpec{string(data)})})
			}
		}
		return out, nil
	})
}

func repoFileValidate(ctx context.Context, e *Env, _ *Resource, want Resource) error {
	s, err := decode[repoFileSpec](want.Spec)
	if err != nil {
		return err
	}
	project, _, err := splitRepoFile(want.Name)
	if err != nil {
		return err
	}
	if len(s.Content) > MaxRepoFile {
		return invalid("%s is over %d bytes", want.Name, MaxRepoFile)
	}
	if err := checkNoSecret(s.Content); err != nil {
		return err
	}
	_, err = projectOf(ctx, e, project)
	return err
}

func repoFileApply(ctx context.Context, e *Env, old, want *Resource) error {
	r := firstOf(old, want)
	project, path, err := splitRepoFile(r.Name)
	if err != nil {
		return err
	}
	if want == nil {
		return writes(ctx, e, project, FileChange{Path: path})
	}
	s, err := decode[repoFileSpec](want.Spec)
	if err != nil {
		return err
	}
	return writes(ctx, e, project, FileChange{Path: path, Content: []byte(s.Content)})
}

// checkNoSecret refuses text that looks like a credential.
func checkNoSecret(text string) error {
	if domain.SecretPatterns.MatchString(text) {
		return fmt.Errorf("this looks like a secret, and secrets go in a secure field: %w", mcp.ErrInvalidInput)
	}
	return nil
}
