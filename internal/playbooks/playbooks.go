// Package playbooks holds the Go actions that the playbooks in
// packs/playbooks call, and registers them by name in the workflow action
// registry. Every action is idempotent for its ActionCall.Key: the engine
// calls an action again after a crash.
package playbooks

import (
	"context"
	"io/fs"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// KnowledgeSeeder indexes a project and says how its knowledge is mapped;
// *knowledge.Syncer implements it.
type KnowledgeSeeder interface {
	Sync(ctx context.Context, p domain.Project, before, head string) (knowledge.SyncResult, error)
	Policy(p domain.Project) knowledge.Policy
}

// Deps are the dependencies of the actions.
type Deps struct {
	Store   store.Store
	Tracker github.Tracker
	// Actions and Packs are what actions.install_workflow needs: GitHub's
	// file and Actions calls, and the packs holding the workflow template.
	// Without them the action is not registered.
	Actions github.Actions
	Packs   fs.FS
	Signals workflow.SignalBus
	// Sizing is the rule goal.size applies to a triage (forecrew.yaml sizing).
	Sizing config.Sizing
	// Roles resolves the roles whose write paths the actions check.
	Roles roles.Resolver
	// Knowledge indexes a project's repo for knowledge.seed; nil makes the
	// action do nothing.
	Knowledge KnowledgeSeeder
	Now       func() time.Time // defaults to the real UTC time
}

// Register adds every action of the package to registry, by name.
func Register(registry workflow.Actions, deps Deps) {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	d := &actions{Deps: deps, regs: registers.New(deps.Store, deps.Signals, deps.Now)}
	registry["project.register"] = d.registerProject
	registry["github.open_or_update_pr"] = d.openOrUpdatePR
	registry["github.merge_pr"] = d.mergePR
	registry["discover.github"] = d.discoverGitHub
	registry["matrix.plan"] = d.matrixPlan
	if d.Actions != nil && d.Packs != nil {
		registry["actions.install_workflow"] = d.installWorkflow
	}
	registry["profile.validate"] = d.validateProfile
	registry["profile.feedback"] = d.profileFeedback
	registry["onboarding.proposal"] = d.onboardingProposal
	registry["onboarding.settle"] = d.settleOnboarding
	registry["onboarding.complete"] = d.completeOnboarding
	registry["goal.intake"] = d.goalIntake
	registry["goal.size"] = d.goalSize
	registry["goal.spec"] = d.goalSpec
	registry["goal.plan"] = d.goalPlan
	registry["goal.task_move"] = d.taskMove
	registry["goal.round"] = d.taskRound
	registry["goal.pr_text"] = d.prText
	registry["goal.review_post"] = d.postReview
	registry["goal.hold"] = d.hold
	registry["goal.assess"] = d.assess
	registry["goal.if_approved"] = d.ifApproved
	registry["goal.task_done"] = d.taskDone
	registry["goal.close"] = d.closeGoal
	registry["goal.settle"] = d.settleGoal
	registry["tests.locked_paths"] = d.testsLockedPaths
	registry["tests.base_check_request"] = d.testsBaseCheckRequest
	registry["tests.base_check_result"] = d.testsBaseCheckResult
	registry["knowledge.seed"] = d.seedKnowledge
}

// actions implements the actions on top of Deps.
type actions struct {
	Deps
	regs *registers.Service
}
