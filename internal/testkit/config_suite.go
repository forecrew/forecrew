package testkit

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ConfigStoreSuite checks the repositories of configuration resources, plans,
// secrets and conversations on st; jobID is a job of projectID.
func ConfigStoreSuite(t *testing.T, st store.Store, accountID, projectID, jobID uuid.UUID) {
	t.Helper()
	c := configConversations(t, st, accountID, projectID, jobID)
	configPlans(t, st, accountID, c, jobID)
	for _, scope := range []uuid.UUID{uuid.Nil(), accountID} {
		configSettings(t, st, scope)
		configSecrets(t, st, scope)
	}
	configAccounts(t, st, accountID)
	configModels(t, st, accountID, projectID, jobID)
}

func configModels(t *testing.T, st store.Store, accountID, projectID, jobID uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	c := domain.ModelConnection{AccountID: accountID, Name: "zai", Vendor: "zai", Adapter: "opencode", KeyEnv: "ZAI_KEY", SecretRef: "store:models/zai", Enabled: true, UpdatedBy: "user:ana"}
	must(st.ModelConnections().Put(ctx, &c))
	c.ID, c.Enabled, c.SecretRef = uuid.Nil(), false, "store:models/zai-2"
	must(st.ModelConnections().Put(ctx, &c))
	rows, err := st.ModelConnections().List(ctx, accountID)
	if err != nil || len(rows) != 1 || rows[0].Enabled || rows[0].SecretRef != "store:models/zai-2" || rows[0].KeyEnv != "ZAI_KEY" {
		t.Fatalf("connections = %+v, %v", rows, err)
	}
	configAuthorizations(t, st, accountID, projectID, rows[0])
	configChecks(t, st, accountID, rows[0], jobID)
	must(st.ModelConnections().Delete(ctx, accountID, "zai"))
	if err := st.ModelConnections().Delete(ctx, accountID, "zai"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete again = %v", err)
	}
	if auths, err := st.ModelAuthorizations().List(ctx, accountID); err != nil || len(auths) != 0 {
		t.Fatalf("authorizations outlive their connection: %+v, %v", auths, err)
	}
	if checks, err := st.ModelChecks().List(ctx, accountID); err != nil || len(checks) != 0 {
		t.Fatalf("checks outlive their connection: %+v, %v", checks, err)
	}

	off := false
	org := domain.ModelAssignment{AccountID: accountID, Tiers: map[string]map[string]string{"fast": {"zai": "zai/glm"}}, Roles: map[string]domain.ModelRole{"developer": {Connection: "zai", Tier: "fast"}}, ProjectOverrides: &off, UpdatedBy: "user:ana"}
	must(st.ModelAssignments().Put(ctx, &org))
	pr := domain.ModelAssignment{AccountID: accountID, ProjectID: projectID, UpdatedBy: "user:ana"}
	must(st.ModelAssignments().Put(ctx, &pr))
	pr.ID, pr.Roles = uuid.Nil(), map[string]domain.ModelRole{"reviewer": {Connection: "zai", Tier: "best"}}
	must(st.ModelAssignments().Put(ctx, &pr))
	list, err := st.ModelAssignments().List(ctx, accountID)
	if err != nil || len(list) != 2 || list[0].ProjectID != uuid.Nil() || list[0].OverridesAllowed() || list[0].Tiers["fast"]["zai"] != "zai/glm" ||
		list[1].ProjectID != projectID || list[1].Roles["reviewer"].Tier != "best" || !list[1].OverridesAllowed() {
		t.Fatalf("assignments = %+v, %v", list, err)
	}
	must(st.ModelAssignments().Delete(ctx, accountID, projectID))
	must(st.ModelAssignments().Delete(ctx, accountID, uuid.Nil()))
	if err := st.ModelAssignments().Delete(ctx, accountID, uuid.Nil()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete again = %v", err)
	}

	// Jobs are retargeted by class.
	classes, err := st.Jobs().QueuedClasses(ctx, accountID)
	must(err)
	for _, cl := range classes {
		moved := cl
		moved.Runtime, moved.Model, moved.Organization = domain.RuntimeNone, "m", !cl.Organization
		must(st.Jobs().Retarget(ctx, accountID, cl, moved))
		now, err := st.Jobs().QueuedClasses(ctx, accountID)
		must(err)
		if !slices.Contains(now, moved) || slices.Contains(now, cl) {
			t.Fatalf("classes = %+v, want %+v", now, moved)
		}
		must(st.Jobs().Retarget(ctx, accountID, moved, cl))
	}
	again, err := st.Jobs().QueuedClasses(ctx, accountID)
	must(err)
	if len(again) != len(classes) {
		t.Fatalf("classes = %+v, want %+v", again, classes)
	}
}

func configConversations(t *testing.T, st store.Store, accountID, projectID, jobID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	c := domain.AssistantConversation{AccountID: accountID, ProjectID: projectID, Owner: "user:me", Credential: "token:x", Title: "Add a machine"}
	must(st.Conversations().Create(ctx, &c))
	if c.ID == uuid.Nil() || c.CreatedAt.IsZero() {
		t.Fatalf("conversation = %+v", c)
	}
	m := domain.AssistantMessage{AccountID: accountID, ConversationID: c.ID, Author: domain.AssistantUser, Body: "hello", JobID: jobID}
	must(st.Conversations().AddMessage(ctx, &m))
	if err := st.Conversations().AddMessage(ctx, &domain.AssistantMessage{AccountID: accountID, ConversationID: c.ID, Author: domain.AssistantUser, Body: "again", JobID: jobID}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second message of a turn: %v", err)
	}
	must(st.Conversations().AddMessage(ctx, &domain.AssistantMessage{AccountID: accountID, ConversationID: c.ID, Author: domain.AssistantReply, Body: "hi", JobID: jobID}))
	got, err := st.Conversations().GetByJob(ctx, accountID, jobID)
	if err != nil || got.ID != c.ID || got.Credential != "token:x" {
		t.Fatalf("GetByJob = %+v, %v", got, err)
	}
	if _, err := st.Conversations().GetByJob(ctx, accountID, domain.NewID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetByJob of nothing: %v", err)
	}
	if got, err := st.Conversations().Get(ctx, accountID, c.ID); err != nil || got.Owner != "user:me" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if rows, err := st.Conversations().List(ctx, accountID, "user:me", 0); err != nil || len(rows) != 1 {
		t.Fatalf("List = %+v, %v", rows, err)
	}
	if rows, err := st.Conversations().List(ctx, accountID, "user:other", 0); err != nil || len(rows) != 0 {
		t.Fatalf("List of another = %+v, %v", rows, err)
	}
	messages, err := st.Conversations().Messages(ctx, accountID, c.ID)
	if err != nil || len(messages) != 2 || messages[0].Body != "hello" || messages[1].JobID != jobID {
		t.Fatalf("Messages = %+v, %v", messages, err)
	}
	return c.ID
}

func configPlans(t *testing.T, st store.Store, accountID, conversationID, jobID uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	p := domain.ConfigPlan{AccountID: accountID, Status: domain.ConfigPlanPending, Source: domain.ConfigFromAssistant, Title: "Add", Applies: "now",
		Changes: []domain.ConfigChange{{Op: domain.ConfigCreate, Kind: "team", Name: "core", New: json.RawMessage(`{"a":1}`), Fields: []domain.ConfigFieldChange{{Path: "a", New: "1"}}, Line: "+ Team core"}},
		Steps:   []domain.ConfigStep{{Kind: domain.ConfigStepSecret, Resource: "connection/okta", Title: "Secret", Secret: "conn-okta"}},
		Needs:   []domain.Permission{domain.PermTeamManage}, LowersProtection: []string{"why"}, ConfirmWord: "acme", Reversible: true, BaseDigest: "d", CreatedBy: "user:me",
		ConversationID: conversationID, JobID: jobID, ExpiresAt: time.Now().Add(time.Hour)}
	must(st.ConfigPlans().Create(ctx, &p))
	if rows, err := st.ConfigPlans().ListExpired(ctx, p.ExpiresAt.Add(-time.Minute), 10); err != nil || len(rows) != 0 {
		t.Fatalf("ListExpired before the expiry = %+v, %v", rows, err)
	}
	if rows, err := st.ConfigPlans().ListExpired(ctx, p.ExpiresAt.Add(time.Minute), 10); err != nil || len(rows) != 1 || rows[0].ID != p.ID {
		t.Fatalf("ListExpired = %+v, %v", rows, err)
	}
	row, err := st.ConfigPlans().Get(ctx, accountID, p.ID)
	if err != nil || len(row.Changes) != 1 || string(row.Changes[0].New) != `{"a": 1}` && string(row.Changes[0].New) != `{"a":1}` || row.Steps[0].Secret != "conn-okta" ||
		row.Needs[0] != domain.PermTeamManage || row.LowersProtection[0] != "why" || row.ConversationID != conversationID || row.JobID != jobID {
		t.Fatalf("Get = %+v, %v", row, err)
	}
	for name, f := range map[string]store.ConfigPlanFilter{
		"open": {Open: true}, "status": {Status: domain.ConfigPlanPending}, "author": {CreatedBy: "user:me"}, "conversation": {ConversationID: conversationID},
	} {
		if rows, err := st.ConfigPlans().List(ctx, accountID, f); err != nil || len(rows) != 1 {
			t.Errorf("List %s = %+v, %v", name, rows, err)
		}
	}
	configPlanMoves(t, st, row)
}

// configPlanMoves applies the pending plan row.
func configPlanMoves(t *testing.T, st store.Store, row domain.ConfigPlan) {
	t.Helper()
	ctx, accountID := t.Context(), row.AccountID
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	row.Status, row.AppliedBy, row.AppliedAt, row.PRURL = domain.ConfigPlanApplied, "user:me", time.Now(), "https://x"
	must(st.ConfigPlans().Move(ctx, &row, domain.ConfigPlanPending))
	if err := st.ConfigPlans().Move(ctx, &row, domain.ConfigPlanPending); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second Move: %v", err)
	}
	row.Status = domain.ConfigPlanPending
	if err := st.ConfigPlans().Move(ctx, &row, domain.ConfigPlanApplied); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("Move back: %v", err)
	}
	if rows, err := st.ConfigPlans().List(ctx, accountID, store.ConfigPlanFilter{Open: true}); err != nil || len(rows) != 0 {
		t.Fatalf("open plans = %+v, %v", rows, err)
	}
	if got, _ := st.ConfigPlans().Get(ctx, accountID, row.ID); got.AppliedBy != "user:me" || got.PRURL != "https://x" {
		t.Fatalf("after Move = %+v", got)
	}
	if _, err := st.ConfigPlans().Get(ctx, domain.NewID(), row.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a plan of another account: %v", err)
	}
}

func configSettings(t *testing.T, st store.Store, scope uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	s := domain.ConfigSetting{AccountID: scope, Kind: "pool", Name: "gpu", Spec: json.RawMessage(`{"a":1}`), UpdatedBy: "user:me"}
	must(st.ConfigSettings().Put(ctx, &s))
	s.Spec = json.RawMessage(`{"a":2}`)
	id := s.ID
	s.ID = uuid.Nil()
	must(st.ConfigSettings().Put(ctx, &s))
	if s.ID != id {
		t.Errorf("Put made a new row: %s, %s", id, s.ID)
	}
	rows, err := st.ConfigSettings().List(ctx, scope, "pool")
	if err != nil || len(rows) != 1 || rows[0].AccountID != scope || len(rows[0].Spec) == 0 {
		t.Fatalf("List = %+v, %v", rows, err)
	}
	all, err := st.ConfigSettings().ListAll(ctx, scope)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAll = %+v, %v", all, err)
	}
	must(st.ConfigSettings().Delete(ctx, scope, "pool", "gpu"))
	if err := st.ConfigSettings().Delete(ctx, scope, "pool", "gpu"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second Delete: %v", err)
	}
}

func configSecrets(t *testing.T, st store.Store, scope uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	v := domain.StoredSecret{AccountID: scope, Name: "conn-x", KeyID: "old", Nonce: []byte{1}, Ciphertext: []byte{2}, UpdatedBy: "user:me"}
	must(st.Secrets().Put(ctx, &v))
	if rows, err := st.Secrets().ListStale(ctx, "new", 10); err != nil || len(rows) != 1 || rows[0].KeyID != "old" || rows[0].AccountID != scope {
		t.Fatalf("ListStale = %+v, %v", rows, err)
	}
	if rows, err := st.Secrets().ListStale(ctx, "old", 10); err != nil || len(rows) != 0 {
		t.Fatalf("ListStale of the same key = %+v, %v", rows, err)
	}
	v.ID, v.Ciphertext = uuid.Nil(), []byte{3}
	must(st.Secrets().Put(ctx, &v))
	got, err := st.Secrets().Get(ctx, scope, "conn-x")
	if err != nil || got.Ciphertext[0] != 3 || got.AccountID != scope {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	must(st.Secrets().Delete(ctx, scope, "conn-x"))
	if _, err := st.Secrets().Get(ctx, scope, "conn-x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if err := st.Secrets().Delete(ctx, scope, "conn-x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second Delete: %v", err)
	}
}

func configAccounts(t *testing.T, st store.Store, accountID uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	a, err := st.Accounts().Get(ctx, accountID)
	must(err)
	must(st.Accounts().Rename(ctx, accountID, "Renamed", "renamed-org"))
	if got, _ := st.Accounts().Get(ctx, accountID); got.Name != "Renamed" || got.Slug != "renamed-org" {
		t.Fatalf("account = %+v", got)
	}
	must(st.Accounts().Rename(ctx, accountID, a.Name, a.Slug))
	other := domain.Account{Name: "Other", Slug: "other-org"}
	must(st.Accounts().Create(ctx, &other))
	if err := st.Accounts().Rename(ctx, other.ID, "x", a.Slug); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a slug that is taken: %v", err)
	}
}

// configAuthorizations checks model authorizations of connection c, and
// leaves one for the connection's delete to remove.
func configAuthorizations(t *testing.T, st store.Store, accountID, projectID uuid.UUID, c domain.ModelConnection) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	all := domain.ModelAuthorization{AccountID: accountID, ConnectionID: c.ID, Connection: c.Name, Adapter: c.Adapter, AuthorizedBy: "user:ana"}
	must(st.ModelAuthorizations().Put(ctx, &all))
	one := domain.ModelAuthorization{AccountID: accountID, ProjectID: projectID, ConnectionID: c.ID, Connection: c.Name, Adapter: c.Adapter, BaseURL: "https://x.example", AuthorizedBy: "user:ana"}
	must(st.ModelAuthorizations().Put(ctx, &one))
	one.ID, one.AuthorizedBy = uuid.Nil(), "user:bo"
	must(st.ModelAuthorizations().Put(ctx, &one))
	rows, err := st.ModelAuthorizations().List(ctx, accountID)
	if err != nil || len(rows) != 2 || rows[0].ProjectID != uuid.Nil() || rows[1].ProjectID != projectID || rows[1].AuthorizedBy != "user:bo" ||
		rows[1].BaseURL != "https://x.example" || rows[1].CreatedAt.IsZero() || rows[1].Covers(projectID, c) {
		t.Fatalf("authorizations = %+v, %v", rows, err)
	}
	if !rows[0].Covers(projectID, c) || !rows[0].Covers(domain.NewID(), c) {
		t.Fatalf("the authorization for every project does not cover: %+v", rows[0])
	}
	other := domain.ModelAuthorization{AccountID: domain.NewID(), ConnectionID: c.ID, Connection: c.Name, AuthorizedBy: "user:ana"}
	if err := st.ModelAuthorizations().Put(ctx, &other); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("put on another account's connection = %v", err)
	}
	must(st.ModelAuthorizations().Delete(ctx, accountID, c.ID, projectID))
	if err := st.ModelAuthorizations().Delete(ctx, accountID, c.ID, projectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete again = %v", err)
	}
	must(st.ModelAuthorizations().Put(ctx, &one))
	must(st.ModelAuthorizations().DeleteConnection(ctx, accountID, c.ID))
	if rows, _ := st.ModelAuthorizations().List(ctx, accountID); len(rows) != 0 {
		t.Fatalf("after DeleteConnection: %+v", rows)
	}
	must(st.ModelAuthorizations().Put(ctx, &all))
}

// configChecks checks the key check and the test of a model of connection c;
// jobID is a job of the account.
func configChecks(t *testing.T, st store.Store, accountID uuid.UUID, c domain.ModelConnection, jobID uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	key := domain.ModelCheck{AccountID: accountID, ConnectionID: c.ID, Connection: c.Name, JobID: jobID, Status: domain.ModelCheckQueued, CreatedBy: "user:ana"}
	must(st.ModelChecks().Put(ctx, &key))
	key.ID, key.Status = uuid.Nil(), domain.ModelCheckReachable
	key.Key = &domain.ModelCheckResult{Status: domain.ModelCheckReachable, Models: []string{"glm-4.6"}, AgentVersion: "1.0"}
	must(st.ModelChecks().Put(ctx, &key))
	test := domain.ModelCheck{AccountID: accountID, ConnectionID: c.ID, Connection: c.Name, Model: "glm-4.6", JobID: jobID, Status: domain.SuitabilityDone, CreatedBy: "user:ana", Stale: true,
		Test: &domain.SuitabilityResult{Abilities: map[string]bool{domain.AbilityRead: true}, Level: domain.LevelNone, Tokens: 9, DurationMS: 5}}
	must(st.ModelChecks().Put(ctx, &test))
	rows, err := st.ModelChecks().List(ctx, accountID)
	if err != nil || len(rows) != 2 || rows[0].Model != "" || rows[0].Key == nil || rows[0].Key.Models[0] != "glm-4.6" || rows[0].Status != domain.ModelCheckReachable ||
		rows[1].Test == nil || !rows[1].Test.Abilities[domain.AbilityRead] || !rows[1].Stale || rows[1].CreatedAt.IsZero() {
		t.Fatalf("checks = %+v, %v", rows, err)
	}
	got, err := st.ModelChecks().GetByJob(ctx, accountID, jobID)
	if err != nil || got.JobID != jobID {
		t.Fatalf("by job = %+v, %v", got, err)
	}
	if _, err := st.ModelChecks().GetByJob(ctx, accountID, domain.NewID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("by another job = %v", err)
	}
	other := domain.ModelCheck{AccountID: accountID, ConnectionID: domain.NewID(), Connection: "x", JobID: jobID, Status: "queued", CreatedBy: "user:ana"}
	if err := st.ModelChecks().Put(ctx, &other); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("put on a missing connection = %v", err)
	}
}
