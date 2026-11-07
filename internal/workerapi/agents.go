package workerapi

import (
	"context"
	"log/slog"
	"regexp"
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Limits of what a worker reports of its agents.
const maxAgents = 16

var (
	agentAdapter = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	agentVersion = regexp.MustCompile(`^[0-9A-Za-z.+-]{0,40}$`)
	agentSources = []string{"", domain.AgentSourceImage, domain.AgentSourceInstalled, domain.AgentSourceSystem}
	agentErrors  = []string{"", domain.AgentInstallUnsupported, domain.AgentInstallDownload, domain.AgentInstallChecksum, domain.AgentInstallArchive, domain.AgentInstallWrite}
)

// cleanAgents keeps what a worker reported of its agents that is well
// formed: it is shown to people, so nothing else of it is stored.
func cleanAgents(in []domain.WorkerAgent) []domain.WorkerAgent {
	out := []domain.WorkerAgent{}
	for _, a := range in {
		if len(out) == maxAgents {
			break
		}
		if agentAdapter.MatchString(a.Adapter) && agentVersion.MatchString(a.Version) && slices.Contains(agentSources, a.Source) && slices.Contains(agentErrors, a.Error) {
			out = append(out, a)
		}
	}
	return out
}

// recordAgents stores the agents of a poll when they changed, and returns
// them. A failure only leaves the old report: the poll goes on.
func (s *Server) recordAgents(ctx context.Context, w domain.Worker, agents []domain.WorkerAgent, logger *slog.Logger) []domain.WorkerAgent {
	agents = cleanAgents(agents)
	if slices.Equal(agents, w.Agents) || (len(agents) == 0 && w.Agents == nil) {
		return agents
	}
	if err := s.cfg.Store.Workers().SetAgents(ctx, w.AccountID, w.ID, agents); err != nil && !clientGone(ctx, err) {
		logger.ErrorContext(ctx, "record worker agents", "error", err)
	}
	return agents
}

// installRequest returns the adapter w should install, or "": the runtime
// of the oldest queued agent job of w's pool that no online worker of the
// pool can run, when w has everything else the job needs, says it installs
// agents, and did not fail to install it. Workers of other pools are never
// asked.
func (s *Server) installRequest(ctx context.Context, w domain.Worker, labels []string, logger *slog.Logger) string {
	if w.Ephemeral || !slices.Contains(labels, domain.AgentInstallLabel) {
		return ""
	}
	queued, err := s.cfg.Store.Jobs().List(ctx, w.AccountID, store.JobFilter{Status: domain.JobQueued, Limit: 200})
	if err != nil {
		if !clientGone(ctx, err) {
			logger.ErrorContext(ctx, "list queued jobs for an agent install", "error", err)
		}
		return ""
	}
	var workers []domain.Worker
	now := s.cfg.Now()
	for _, j := range slices.Backward(queued) { // oldest first
		if j.Kind.OrDefault() != domain.JobKindAgent || j.Runtime == "" || j.Runtime == domain.RuntimeNone || domain.OrDefaultPool(j.Pool) != domain.OrDefaultPool(w.Pool) {
			continue
		}
		need := j.RequiredLabels()
		runtime := domain.RuntimeLabel(j.Runtime)
		if domain.MatchLabels(need, labels) || !domain.MatchLabels(slices.DeleteFunc(slices.Clone(need), func(l string) bool { return l == runtime }), labels) {
			continue
		}
		if a, ok := w.Agent(j.Runtime); ok && a.Error != "" {
			continue // it says why on the machine and on the job
		}
		if workers == nil {
			if workers, err = s.cfg.Store.Workers().List(ctx, w.AccountID); err != nil {
				return ""
			}
		}
		if slices.ContainsFunc(workers, func(o domain.Worker) bool {
			return o.Online(now) && domain.OrDefaultPool(o.Pool) == domain.OrDefaultPool(w.Pool) && domain.MatchLabels(need, o.Labels)
		}) {
			continue
		}
		return j.Runtime
	}
	return ""
}
