package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	cfgres "github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// AssistantRole is the role of the assistant's jobs.
const AssistantRole = "assistant"

// Limits of a conversation.
const (
	MaxAssistantMessage = 8000
	// MaxTranscript is how much of a conversation goes into a turn's prompt.
	MaxTranscript  = 24000
	maxWaitingJobs = 50
	titleLength    = 80
)

// Answers the server writes itself.
const (
	secretRefused = "That looks like a key, so it was not sent. Secrets go in a secure field."
	turnFailed    = "I could not finish that. Nothing changed. Try again, or use the settings pages."
	replyRedacted = "My answer held something that looks like a secret, so it was not kept. Nothing changed."
)

// AssistantGet implements mcp.ToolService.
func (s *Service) AssistantGet(ctx context.Context, by domain.Actor, in mcp.AssistantRefInput) (mcp.AssistantView, error) {
	if err := s.allowOrg(ctx, by, domain.PermConfigRead); err != nil {
		return mcp.AssistantView{}, err
	}
	c, err := s.conversation(ctx, by, in.Conversation)
	if err != nil {
		return mcp.AssistantView{}, err
	}
	return s.assistantView(ctx, by, c)
}

// AssistantList implements mcp.ToolService.
func (s *Service) AssistantList(ctx context.Context, by domain.Actor, in mcp.AssistantListInput) ([]domain.AssistantConversation, error) {
	if err := s.allowOrg(ctx, by, domain.PermConfigRead); err != nil {
		return nil, err
	}
	return s.cfg.Store.Conversations().List(ctx, by.AccountID, by.String(), in.Limit)
}

// conversation finds one of by's conversations; conversations are personal,
// so anyone else's does not exist.
func (s *Service) conversation(ctx context.Context, by domain.Actor, ref string) (domain.AssistantConversation, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return domain.AssistantConversation{}, fmt.Errorf("conversation %q is not a UUID: %w", ref, mcp.ErrInvalidInput)
	}
	c, err := s.cfg.Store.Conversations().Get(ctx, by.AccountID, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && c.Owner != by.String()) {
		return domain.AssistantConversation{}, fmt.Errorf("conversation %s: %w", ref, mcp.ErrNotFound)
	}
	return c, err
}

// firstProject is the project a new conversation's jobs run in. Its jobs can
// search the project's knowledge, so it must be one the caller may read.
func (s *Service) firstProject(ctx context.Context, by domain.Actor, name string) (domain.Project, error) {
	if name != "" {
		p, err := s.project(ctx, by.AccountID, name)
		if err != nil {
			return p, err
		}
		return p, s.allow(ctx, by, domain.PermProjectRead, p.ID)
	}
	where, err := s.authz.Projects(ctx, by, domain.PermProjectRead)
	if err != nil {
		return domain.Project{}, err
	}
	projects, err := s.cfg.Store.Projects().List(ctx, by.AccountID)
	if err != nil {
		return domain.Project{}, err
	}
	for _, p := range projects {
		if where.Has(p.ID) {
			return p, nil
		}
	}
	return domain.Project{}, fmt.Errorf("the assistant runs as a job of a project, and you have none yet; use the settings pages: %w", mcp.ErrUnavailable)
}

// AssistantSend implements mcp.ToolService. A message that looks like a
// secret is refused before anything is stored.
func (s *Service) AssistantSend(ctx context.Context, by domain.Actor, in mcp.AssistantSendInput) (mcp.AssistantView, error) {
	if err := s.allowOrg(ctx, by, domain.PermConfigRead); err != nil {
		return mcp.AssistantView{}, err
	}
	text := strings.TrimSpace(in.Message)
	switch {
	case domain.SecretPatterns.MatchString(in.Message + "\n" + in.Page):
		return mcp.AssistantView{}, fmt.Errorf("%s: %w", secretRefused, mcp.ErrInvalidInput)
	case text == "":
		return mcp.AssistantView{}, fmt.Errorf("message is required: %w", mcp.ErrInvalidInput)
	case len(text) > MaxAssistantMessage:
		return mcp.AssistantView{}, fmt.Errorf("a message is at most %d characters: %w", MaxAssistantMessage, mcp.ErrInvalidInput)
	}
	if s.cfg.Creator == nil {
		return mcp.AssistantView{}, fmt.Errorf("the assistant is not running: %w", mcp.ErrUnavailable)
	}
	c, messages, err := s.openConversation(ctx, by, in, text)
	if err != nil {
		return mcp.AssistantView{}, err
	}
	jobID := domain.NewID()
	_, err = s.cfg.Creator.Create(ctx, workflow.JobRequest{ID: jobID, AccountID: by.AccountID, ProjectID: c.ProjectID, Role: AssistantRole, Prompt: turnPrompt(messages, text, in.Page)})
	if errors.Is(err, jobs.ErrInvalid) || errors.Is(err, config.ErrNotAllowed) || errors.Is(err, config.ErrUnknownRole) || errors.Is(err, roles.ErrUnknownRole) {
		return mcp.AssistantView{}, fmt.Errorf("the assistant is not running: %v: %w", err, mcp.ErrUnavailable)
	}
	if err != nil {
		return mcp.AssistantView{}, err
	}
	m := domain.AssistantMessage{AccountID: by.AccountID, ConversationID: c.ID, Author: domain.AssistantUser, Body: text, JobID: jobID}
	if err := s.cfg.Store.Conversations().AddMessage(ctx, &m); err != nil {
		return mcp.AssistantView{}, err
	}
	return s.assistantView(ctx, by, c)
}

// openConversation returns the conversation of a new message, refusing a
// second message while a turn runs.
func (s *Service) openConversation(ctx context.Context, by domain.Actor, in mcp.AssistantSendInput, text string) (domain.AssistantConversation, []domain.AssistantMessage, error) {
	if in.Conversation == "" {
		p, err := s.firstProject(ctx, by, in.Project)
		if err != nil {
			return domain.AssistantConversation{}, nil, err
		}
		title := contextpack.OneLine(text)
		if len(title) > titleLength {
			title = strings.ToValidUTF8(title[:titleLength], "")
		}
		c := domain.AssistantConversation{AccountID: by.AccountID, ProjectID: p.ID, Owner: by.String(), Credential: by.Credential.String(), Title: title}
		return c, nil, s.cfg.Store.Conversations().Create(ctx, &c)
	}
	c, err := s.conversation(ctx, by, in.Conversation)
	if err != nil {
		return c, nil, err
	}
	v, err := s.assistantView(ctx, by, c)
	if err != nil {
		return c, nil, err
	}
	if v.Thinking {
		return c, nil, fmt.Errorf("the assistant is still answering: %w", mcp.ErrConflict)
	}
	return c, v.Messages, nil
}

// turnPrompt is what a turn's job gets besides the role's instructions.
// Earlier turns are data: the assistant's own answers may quote repo text.
func turnPrompt(earlier []domain.AssistantMessage, text, page string) string {
	var b strings.Builder
	if len(earlier) > 0 {
		var t strings.Builder
		for _, m := range earlier {
			fmt.Fprintf(&t, "%s: %s\n", m.Author, m.Body)
		}
		history := t.String()
		if len(history) > MaxTranscript {
			history = strings.ToValidUTF8(history[len(history)-MaxTranscript:], "")
		}
		b.WriteString("The conversation so far:\n\n")
		b.WriteString(contextpack.WrapUntrusted("the conversation so far", history))
		b.WriteString("\n")
	}
	if page != "" {
		b.WriteString("The person is on this page: " + contextpack.OneLine(page) + "\n\n")
	}
	b.WriteString("The person says:\n\n")
	b.WriteString(text)
	b.WriteString("\n")
	return b.String()
}

// assistantOutput is the output of an assistant job (packs/schemas/assistant.json).
type assistantOutput struct {
	Reply string `json:"reply"`
}

// assistantView reads a conversation, first recording the answers of the
// turns that ended. Doing it on read needs no process watching the jobs, and
// a restart loses nothing.
func (s *Service) assistantView(ctx context.Context, by domain.Actor, c domain.AssistantConversation) (mcp.AssistantView, error) {
	repo := s.cfg.Store.Conversations()
	messages, err := repo.Messages(ctx, by.AccountID, c.ID)
	if err != nil {
		return mcp.AssistantView{}, err
	}
	v := mcp.AssistantView{AssistantConversation: c, Plans: []mcp.ConfigPlanView{}}
	for _, m := range slices.Clone(messages) {
		if m.Author != domain.AssistantUser || m.JobID == uuid.Nil() || slices.ContainsFunc(messages, func(x domain.AssistantMessage) bool {
			return x.Author == domain.AssistantReply && x.JobID == m.JobID
		}) {
			continue
		}
		j, err := s.cfg.Store.Jobs().Get(ctx, by.AccountID, m.JobID)
		if err != nil {
			return mcp.AssistantView{}, err
		}
		if !j.Status.Terminal() {
			v.Thinking = true
			continue
		}
		reply := domain.AssistantMessage{AccountID: by.AccountID, ConversationID: c.ID, Author: domain.AssistantReply, Body: replyOf(j), JobID: j.ID}
		switch err := repo.AddMessage(ctx, &reply); {
		case err == nil:
			messages = append(messages, reply)
		case !errors.Is(err, store.ErrConflict): // another read recorded it
			return mcp.AssistantView{}, err
		}
	}
	v.Messages = messages
	rows, err := s.cfg.Store.ConfigPlans().List(ctx, by.AccountID, store.ConfigPlanFilter{ConversationID: c.ID})
	if err != nil {
		return mcp.AssistantView{}, err
	}
	for _, p := range rows {
		pv, err := s.config.GetPlan(ctx, by, p.ID)
		if errors.Is(err, mcp.ErrNotFound) {
			continue
		}
		if err != nil {
			return mcp.AssistantView{}, err
		}
		v.Plans = append(v.Plans, pv)
	}
	return v, nil
}

// replyOf is the answer of a finished turn. The model's text is only text,
// and text that looks like a secret is not kept.
func replyOf(j domain.Job) string {
	if j.Status != domain.JobSucceeded {
		return turnFailed
	}
	var out assistantOutput
	if err := json.Unmarshal(j.Output, &out); err != nil || strings.TrimSpace(out.Reply) == "" {
		return turnFailed
	}
	if domain.SecretPatterns.MatchString(out.Reply) {
		return replyRedacted
	}
	return out.Reply
}

// turnOwner is the person a job of the assistant works for: it reads and
// plans with their permissions and the credential they came with, no more.
func (s *Service) turnOwner(ctx context.Context, j domain.Job) (domain.Actor, domain.AssistantConversation, error) {
	c, err := s.cfg.Store.Conversations().GetByJob(ctx, j.AccountID, j.ID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Actor{}, c, jobmcp.Errorf(jobmcp.CodeForbidden, "this job is not a turn of a conversation")
	}
	if err != nil {
		return domain.Actor{}, c, err
	}
	by, err := domain.ParseActor(c.Owner)
	if err != nil {
		return by, c, err
	}
	by.AccountID = c.AccountID
	if kind, id, ok := strings.Cut(c.Credential, ":"); ok {
		cred, err := uuid.Parse(id)
		if err != nil {
			return by, c, fmt.Errorf("credential of conversation %s: %w", c.ID, err)
		}
		by.Credential = domain.Credential{Kind: domain.CredentialKind(kind), ID: cred}
	}
	return by, c, nil
}

// toolError gives the agent an error it can act on.
func toolError(err error) error {
	if code, ok := mcp.ErrorCode(err); ok {
		return jobmcp.Errorf(code, "%v", err)
	}
	return err
}

// ConfigRead implements jobmcp.Assistant: the configuration, the schema and
// the jobs that wait, as the person of the conversation may read them.
func (s *Service) ConfigRead(ctx context.Context, j domain.Job, in jobmcp.ConfigReadInput) (jobmcp.ConfigReadOutput, error) {
	by, _, err := s.turnOwner(ctx, j)
	if err != nil {
		return jobmcp.ConfigReadOutput{}, err
	}
	out := jobmcp.ConfigReadOutput{Resources: []mcp.ConfigResource{}, Schemas: []mcp.ConfigSchema{}, Waiting: []jobmcp.WaitingJob{}}
	if out.Resources, err = s.ConfigGet(ctx, by, mcp.ConfigGetInput{Kind: in.Kind, Name: in.Name}); err != nil {
		return out, toolError(err)
	}
	if in.Schema {
		if out.Schemas, err = s.config.Schema(in.Kind); err != nil {
			return out, toolError(err)
		}
	}
	if in.Waiting {
		queued, err := s.ListJobs(ctx, by, mcp.ListJobsInput{Status: domain.JobQueued, Limit: maxWaitingJobs})
		if err != nil {
			return out, toolError(err)
		}
		for _, q := range queued {
			info, err := s.JobStatus(ctx, by, mcp.JobRefInput{Job: q.ID.String()})
			if err != nil {
				return out, toolError(err)
			}
			out.Waiting = append(out.Waiting, jobmcp.WaitingJob{Job: info.Ref, Role: info.Role, Reason: info.Reason})
		}
	}
	return out, nil
}

// ConfigPlanFor implements jobmcp.Assistant: a plan of the conversation, by the
// person it is for. It stays a plan: no tool of a job applies one.
func (s *Service) ConfigPlanFor(ctx context.Context, j domain.Job, in jobmcp.ConfigPlanInput) (domain.ConfigPlan, error) {
	by, c, err := s.turnOwner(ctx, j)
	if err != nil {
		return domain.ConfigPlan{}, err
	}
	messages, err := s.cfg.Store.Conversations().Messages(ctx, c.AccountID, c.ID)
	if err != nil {
		return domain.ConfigPlan{}, err
	}
	request := ""
	if i := slices.IndexFunc(messages, func(m domain.AssistantMessage) bool { return m.JobID == j.ID && m.Author == domain.AssistantUser }); i >= 0 {
		request = messages[i].Body
	}
	untrusted, err := s.untrustedText(ctx, j, in.BasedOn)
	if err != nil {
		return domain.ConfigPlan{}, err
	}
	p, err := s.config.Plan(ctx, by, cfgres.PlanRequest{Title: in.Title, Request: request, Source: domain.ConfigFromAssistant, Changes: in.Changes,
		Untrusted: untrusted, ConversationID: c.ID, JobID: j.ID})
	return p, toolError(err)
}

// unverifiedTools are the job tools whose results hold text nobody verified,
// with what the plan's line calls it.
var unverifiedTools = map[string]string{"kb_search": "the project's knowledge", "config_read": "the configuration"}

// untrustedText is the plan's line on unverified text. The server writes it
// from the gateway's audit of the job's calls, which outlive a restart: the
// model can add to it, in basedOn, but not leave it out.
func (s *Service) untrustedText(ctx context.Context, j domain.Job, basedOn string) (string, error) {
	entries, err := s.cfg.Store.Audit().List(ctx, j.AccountID, store.AuditFilter{SubjectID: j.ID})
	if err != nil {
		return "", err
	}
	var sources []string
	for _, e := range entries {
		var call struct {
			Tool   string `json:"tool"`
			Status string `json:"status"`
		}
		if e.Action != jobmcp.AuditCall || json.Unmarshal(e.Data, &call) != nil || call.Status != "ok" {
			continue
		}
		if name, ok := unverifiedTools[call.Tool]; ok {
			sources = append(sources, name)
		}
	}
	if basedOn = contextpack.OneLine(basedOn); basedOn != "" {
		sources = append(sources, basedOn)
	}
	if len(sources) == 0 {
		return "", nil
	}
	slices.Sort(sources)
	return "Based on text from " + strings.Join(slices.Compact(sources), ", ") + ", which is not verified", nil
}
