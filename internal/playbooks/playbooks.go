// Package playbooks holds the Go actions that the playbooks in
// packs/playbooks call, and registers them by name in the workflow action
// registry. Every action is idempotent for its ActionCall.Key: the engine
// calls an action again after a crash.
package playbooks

import (
	"context"
	"io/fs"
	"log/slog"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/verification"
	"github.com/forecrew/forecrew/internal/workflow"
)

// KnowledgeSeeder indexes a project and says how its knowledge is mapped;
// *knowledge.Syncer implements it.
type KnowledgeSeeder interface {
	Sync(ctx context.Context, p domain.Project, before, head string) (knowledge.SyncResult, error)
	Policy(ctx context.Context, p domain.Project) (knowledge.Policy, error)
}

// Deps are the dependencies of the actions.
type Deps struct {
	Store   store.Store
	Tracker github.Tracker
	// Actions and Packs are what actions.install_workflow needs: GitHub's
	// file and Actions calls, and the packs holding the workflow template.
	// Without them the action fails, since the onboarding-tests playbook
	// names it.
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
	// Proposals runs the knowledge.* actions of proposals; they fail without
	// it.
	Proposals *proposals.Service
	Now       func() time.Time // defaults to the real UTC time

	// The rest is for the council actions.
	Config   *config.Config
	Councils *councils.Set
	Gates    gates.Service
	// Runs starts the council playbook; it is the engine, which is built
	// after the actions, so a call must only come once it exists.
	Runs councils.Starter
	// Evidence archives the URLs members cite; nil keeps no copy.
	Evidence evidence.Store
	// Fetcher fetches those URLs; it defaults to the zero councils.Fetcher.
	Fetcher *councils.Fetcher
	Logger  *slog.Logger // defaults to discarding
}

// Register adds every action of the package to registry, by name.
func Register(registry workflow.Actions, deps Deps) {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Fetcher == nil {
		deps.Fetcher = &councils.Fetcher{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	d := &actions{Deps: deps, regs: registers.New(deps.Store, deps.Signals, deps.Now), verifier: &verification.Publisher{Store: deps.Store, Evidence: deps.Evidence, Logger: deps.Logger}}
	if deps.Tracker != nil {
		d.verifier.Tracker = deps.Tracker
	}
	registry["project.register"] = d.registerProject
	registry["github.open_or_update_pr"] = d.openOrUpdatePR
	registry["github.merge_pr"] = d.mergePR
	registry["discover.github"] = d.discoverGitHub
	registry["matrix.plan"] = d.matrixPlan
	registry["actions.install_workflow"] = d.installWorkflow
	registry["profile.validate"] = d.validateProfile
	registry["profile.feedback"] = d.profileFeedback
	registry["onboarding.proposal"] = d.onboardingProposal
	registry["onboarding.settle"] = d.settleOnboarding
	registry["onboarding.complete"] = d.completeOnboarding
	registry["onboarding.finish"] = d.finishOnboarding
	registry["onboarding.start_tests"] = d.startTests
	registry["onboarding.tests_branch"] = d.testsBranchAction
	registry["tests.smoke_result"] = d.testsSmokeResult
	registry["onboarding.tests_feedback"] = d.testsFeedback
	registry["onboarding.tests_check"] = d.testsCheck
	registry["onboarding.tests_head"] = d.testsHead
	registry["onboarding.tests_proposal"] = d.testsProposal
	registry["onboarding.baseline_ref"] = d.baselineRef
	registry["tests.smoke_plan"] = d.testsSmokePlan
	registry["baseline.file_issues"] = d.baselineIssues
	registry["goal.intake"] = d.goalIntake
	registry["goal.size"] = d.goalSize
	registry["bug.spec"] = d.bugSpecAction
	registry["bug.plan"] = d.bugPlanAction
	registry["bug.reproduced"] = d.bugReproduced
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
	registry["tests.map"] = d.testsMap
	registry["curate.prepare"] = d.curatePrepare
	registry["curate.finish"] = d.curateFinish
	registry["curate.tools"] = d.curateTools
	registry["goal.retro_context"] = d.retroContext
	registry["goal.learn"] = d.learnGoal
	registry["goal.learn_decide"] = d.learnDecide
	registry["goal.learn_settle"] = d.learnSettle
	registry["tests.locked_paths"] = d.testsLockedPaths
	registry["tests.base_check_request"] = d.testsBaseCheckRequest
	registry["tests.base_check_result"] = d.testsBaseCheckResult
	registry["verification.build"] = d.verificationBuild
	registry["verification.rerun_cells"] = d.verificationRerunCells
	registry["verification.read"] = d.verificationRead
	registry["knowledge.seed"] = d.seedKnowledge
	registry["knowledge.propose"] = d.knowledgePropose
	registry["knowledge.assess"] = d.knowledgeAssess
	registry["knowledge.decide"] = d.knowledgeDecide
	registry["knowledge.settle"] = d.knowledgeSettle
	registry["knowledge.access_pr"] = d.knowledgeAccessPR
	registry["job.if_pushed"] = d.ifPushed
	registry["council.run"] = d.councilRun
	registry["council.start"] = d.councilStart
	registry["council.contribute"] = d.councilContribute
	registry["council.check_evidence"] = d.councilCheckEvidence
	registry["council.round_done"] = d.councilRoundDone
	registry["council.open_gate"] = d.councilOpenGate
	registry["council.decide"] = d.councilDecide
	registry["council.settle"] = d.councilSettle
}

// actions implements the actions on top of Deps.
type actions struct {
	Deps
	regs *registers.Service
	// verifier is one for all calls, which keeps its warning about a missing
	// Checks permission to once.
	verifier *verification.Publisher
}
