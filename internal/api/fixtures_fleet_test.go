package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
)

// fleetPlan is the plan the Add machines page makes for a Linux server that
// joined: a location, two machine types and a limit.
func fleetPlan(id int, status domain.ConfigPlanStatus, source domain.ConfigPlanSource, mod func(*mcp.ConfigPlanView)) mcp.ConfigPlanView {
	v := mcp.ConfigPlanView{
		ID: fid(id), AccountID: fid(1), Status: status, Source: source, Title: "Add machine types to Home server", Request: "Add my Linux server",
		Applies: "now", Needs: []domain.Permission{domain.PermWorkerManage}, Affects: "New jobs in all projects can run here. Running jobs are not touched.",
		Cost: "Free. It is your machine.", Undo: "Can be reverted from Change history.", Reversible: true, LowersProtection: []string{},
		BaseDigest: "d1",
		Changes: []domain.ConfigChange{
			{Op: domain.ConfigCreate, Kind: "shape", Name: "linux-small", New: json.RawMessage(`{"provider":"home-server","os":"linux","arch":"amd64","cpu":2,"memory_mb":4096,"disk_gb":20,"max":3,"enabled":true}`),
				Fields: []domain.ConfigFieldChange{{Path: "cpu", New: "2"}, {Path: "memory_mb", New: "4096"}}, Line: "+ Machine type linux-small (2 CPU, 4 GB)"},
			{Op: domain.ConfigCreate, Kind: "shape", Name: "linux-medium", New: json.RawMessage(`{"provider":"home-server","os":"linux","arch":"amd64","cpu":4,"memory_mb":8192,"disk_gb":20,"max":3,"enabled":true}`),
				Fields: []domain.ConfigFieldChange{{Path: "cpu", New: "4"}, {Path: "memory_mb", New: "8192"}}, Line: "+ Machine type linux-medium (4 CPU, 8 GB)"},
			{Op: domain.ConfigUpdate, Kind: "provider", Name: "home-server", Old: json.RawMessage(`{"kind":"microvm","limits":{}}`), New: json.RawMessage(`{"kind":"microvm","limits":{"max_machines":3}}`),
				Fields: []domain.ConfigFieldChange{{Path: "limits.max_machines", Old: "0", New: "3"}}, Line: "~ Location home-server: limits.max_machines 0 -> 3"},
		},
		Steps:     []domain.ConfigStep{},
		CreatedBy: "user:" + fid(300).String(), ExpiresAt: at(24 * 60), CreatedAt: at(0), UpdatedAt: at(0), CanConfirm: true}
	if mod != nil {
		mod(&v)
	}
	return v
}

func orFleet(v mcp.ConfigPlanView) func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) {
	return func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigPlanView, error) { return v, nil }
}

func fixtureProviders() []mcp.ProviderInfo {
	checks := []domain.ProviderCheck{{Name: "kvm", Status: "ok", Detail: "Available"}, {Name: "disk", Status: "ok", Detail: "412 GB free"}, {Name: "software", Status: "ok", Detail: "Up to date"}}
	return []mcp.ProviderInfo{
		{ID: fid(700), AccountID: fid(1), Name: "home-server", Kind: domain.ProviderMicroVM, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, LastSeenAt: at(-1),
			Capabilities: domain.ProviderCapabilities{OS: []domain.MachineOS{domain.OSLinux}, Arch: []domain.MachineArch{domain.ArchAMD64}, MaxCPU: 16, MaxMemoryMB: 65536, MaxDiskGB: 412, MaxMachines: 3, Checks: checks, Host: "home-nas <b>1</b>", AgentVersion: "0.9.2"},
			Limits:       domain.ProviderLimits{MaxMachines: 3}, NetworkAllow: []string{}, Tags: map[string]string{}, CreatedAt: at(-30000), UpdatedAt: at(-30000), Online: true, Ownership: domain.OwnershipOwn},
		{ID: fid(701), AccountID: fid(1), Name: "mac-mini", Kind: domain.ProviderTart, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, LastSeenAt: at(-240),
			Capabilities: domain.ProviderCapabilities{OS: []domain.MachineOS{domain.OSMacOS}, Arch: []domain.MachineArch{domain.ArchARM64}, MaxMachines: 2, Host: "mini"},
			Limits:       domain.ProviderLimits{MaxMachines: 2}, NetworkAllow: []string{}, Tags: map[string]string{}, CreatedAt: at(-20000), UpdatedAt: at(-20000), Online: false, Ownership: domain.OwnershipOwn},
		{ID: fid(702), AccountID: fid(1), Name: "aws-frankfurt", Kind: domain.ProviderAWS, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, LastSeenAt: at(-1),
			Capabilities: domain.ProviderCapabilities{OS: []domain.MachineOS{domain.OSLinux}, Arch: []domain.MachineArch{domain.ArchAMD64}, CloudAccount: "123456789012", Region: "eu-central-1"},
			Limits:       domain.ProviderLimits{MaxMachines: 20, MonthlyBudget: new(200.0)}, NetworkAllow: []string{"registry.acme.test"}, Tags: map[string]string{"team": "platform"}, CreatedAt: at(-10000), UpdatedAt: at(-10000), Online: true, Ownership: domain.OwnershipOwn},
	}
}

func fixtureShapes() []mcp.ShapeInfo {
	shape := func(id int, name, provider string, cpu, mem int, price float64, os domain.MachineOS) mcp.ShapeInfo {
		return mcp.ShapeInfo{ID: fid(id), AccountID: fid(1), ProviderID: fid(700), Name: name, OS: os, Arch: domain.ArchAMD64, CPU: cpu, MemoryMB: mem, DiskGB: 20, Pool: domain.PoolBuild,
			Labels: []string{}, ProviderConfig: map[string]any{}, Image: "ubuntu-24-04", PricePerMinute: price, Max: 3, IdleSeconds: 60, BootTimeoutSeconds: 300, MaxJobs: 1, Enabled: true, CreatedAt: at(-30000), UpdatedAt: at(-30000),
			Provider: provider, Ownership: domain.OwnershipOwn}
	}
	large := shape(712, "linux-large", "aws-frankfurt", 8, 16384, 0.04, domain.OSLinux)
	large.MonthlyBudget, large.Note, large.WarmMin = new(100.0), "Slower to start than a machine at home.", 1
	reasoning := shape(713, "linux-reasoning", "home-server", 2, 4096, 0, domain.OSLinux)
	reasoning.Pool = domain.PoolControl
	return []mcp.ShapeInfo{shape(710, "linux-small", "home-server", 2, 4096, 0, domain.OSLinux), shape(711, "linux-medium", "home-server", 4, 8192, 0, domain.OSLinux), reasoning, large}
}

func fixtureOverview() fleet.Overview {
	return fleet.Overview{
		Headline: "2 jobs waiting: Only Mac Mini can run this, and it is offline.", Working: 2, Waiting: 2,
		Spend: &fleet.Spend{Amount: 41, Limit: 200, Currency: "CHF"}, Routing: domain.RoutingOwnOnly,
		Locations: []fleet.Location{
			{ID: fid(700), Kind: fleet.LocationProvider, Name: "Home Server", Runs: "Linux", Status: fleet.StatusOnline, InUse: 2, Capacity: 3, Cost: "Free"},
			{ID: fid(701), Kind: fleet.LocationProvider, Name: "Mac Mini", Runs: "macOS", Status: fleet.StatusOffline, InUse: 0, Capacity: 2, Cost: "Free"},
			{ID: fid(702), Kind: fleet.LocationProvider, Name: "Aws Frankfurt", Runs: "Linux", Status: fleet.StatusOnline, InUse: 0, Capacity: 20, Cost: "About CHF 0.04 per minute"},
			{ID: fid(60), Kind: fleet.LocationComputer, Name: "build-box-1", Runs: "Linux, directly", Status: fleet.StatusOnline, InUse: 0, Cost: "Free"},
		},
		Queue: []fleet.WaitingJob{
			{Ref: "JOB-42", Role: "reviewer", Code: fleet.ReasonOffline, Sentence: "Only Mac Mini can run this, and it is offline"},
			{Ref: "JOB-46", Role: "developer", Code: fleet.ReasonBudget, Sentence: "This month's limit of CHF 200 is used up"},
		},
		Running: []fleet.RunningJob{
			{Ref: "JOB-41", Role: "developer", Machine: "linux-small-8f2", Location: "Home Server", Minutes: 4},
			{Ref: "JOB-44", Role: "reviewer", Machine: "linux-medium-1ac", Location: "Home Server", Minutes: 1},
		},
	}
}

func fleetFixtureCases() []fixtureCase {
	claimed := domain.ProviderJoinCode{ID: fid(720), AccountID: fid(1), Kind: domain.ProviderMicroVM, ExpiresAt: at(60), CreatedAt: at(-5),
		Facts: domain.ProviderCapabilities{OS: []domain.MachineOS{domain.OSLinux}, Arch: []domain.MachineArch{domain.ArchAMD64}, MaxCPU: 16, MaxMemoryMB: 65536, MaxDiskGB: 412, MaxMachines: 3, Host: "home-nas",
			Checks: []domain.ProviderCheck{{Name: "kvm", Status: "ok", Detail: "Available"}, {Name: "disk", Status: "ok", Detail: "412 GB free"}, {Name: "sleep", Status: "warn", Detail: "This <b>Mac</b> goes to sleep"}}}}
	waiting := domain.ProviderJoinCode{ID: fid(721), AccountID: fid(1), ExpiresAt: at(55), CreatedAt: at(-1)}
	joinCodes := func(e *env) {
		e.svc.ListJoinCodesFunc = func(context.Context, domain.Actor, mcp.ListJoinCodesInput) ([]mcp.JoinCodeInfo, error) {
			return []mcp.JoinCodeInfo{
				{ProviderJoinCode: claimed, Status: mcp.JoinClaimed, FactsHash: "f3a9c1"},
				{ProviderJoinCode: waiting, Status: mcp.JoinWaiting},
			}, nil
		}
	}
	plans := func(e *env) {
		applied := fleetPlan(730, domain.ConfigPlanApplied, domain.ConfigFromAssistant, func(v *mcp.ConfigPlanView) { v.AppliedBy, v.AppliedAt = "user:"+fid(300).String(), at(-60) })
		reverted := fleetPlan(731, domain.ConfigPlanReverted, domain.ConfigFromForm, func(v *mcp.ConfigPlanView) {
			v.Title, v.Request = "Raise budget to 200", ""
			v.AppliedBy, v.AppliedAt, v.Changes, v.CreatedAt = "user:"+fid(301).String(), at(-600), v.Changes[2:], at(-610)
		})
		pending := fleetPlan(732, domain.ConfigPlanAwaiting, domain.ConfigFromMCP, func(v *mcp.ConfigPlanView) {
			v.Title, v.Client, v.CreatedAt = "Add Windows test machine type", "Claude Desktop", at(-30)
		})
		e.svc.ConfigPlansFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfigPlansInput) ([]mcp.ConfigPlanView, error) {
			if in.Inbox {
				return []mcp.ConfigPlanView{pending}, nil
			}
			return []mcp.ConfigPlanView{pending, applied, reverted}, nil
		}
	}
	lowers := fleetPlan(733, domain.ConfigPlanPending, domain.ConfigFromForm, func(v *mcp.ConfigPlanView) {
		v.Title, v.Applies = "Let jobs use Forecrew Cloud as a fallback", "now"
		v.Changes = []domain.ConfigChange{{Op: domain.ConfigUpdate, Kind: "fleet_policy", Name: "fleet", Old: json.RawMessage(`{"routing":"own_only"}`), New: json.RawMessage(`{"routing":"own_first"}`),
			Fields: []domain.ConfigFieldChange{{Path: "routing", Old: "own_only", New: "own_first"}}, Line: "~ Fleet policy: routing own_only -> own_first"}}
		v.LowersProtection, v.ConfirmWord, v.Untrusted = []string{"Adds Forecrew Cloud as a fallback while your code was staying on your machines"}, "acme", "Based on text from README.md, which is not verified"
		v.Cost = "Billed by the minute on Forecrew Cloud."
	})
	secret := fleetPlan(734, domain.ConfigPlanPending, domain.ConfigFromAssistant, func(v *mcp.ConfigPlanView) {
		v.Title, v.Changes = "Add Okta sign-in", []domain.ConfigChange{}
		v.Steps = []domain.ConfigStep{{Kind: domain.ConfigStepSecret, Resource: "connection/okta", Title: "Okta client secret", Secret: "conn-okta"}, {Kind: domain.ConfigStepConnect, Resource: "provider/mac", Title: "Connect mac", Detail: "Run the line shown on the machine."}}
		v.ConversationID = fid(740)
	})
	conversation := mcp.AssistantView{
		ID: fid(740), AccountID: fid(1), ProjectID: fid(10), Owner: "user:" + fid(300).String(), Title: "Add my Linux server", CreatedAt: at(-3), UpdatedAt: at(-1),
		Messages: []domain.AssistantMessage{
			{ID: fid(741), AccountID: fid(1), ConversationID: fid(740), Author: domain.AssistantUser, Body: "Add my Linux server", CreatedAt: at(-3)},
			{ID: fid(742), AccountID: fid(1), ConversationID: fid(740), Author: domain.AssistantReply, Body: "I suggest **two sizes** and at most 3 machines at once.\n\n<script>alert(1)</script>", JobID: fid(150), CreatedAt: at(-1)},
		},
		Plans: []mcp.ConfigPlanView{secret},
	}
	waitingJob := func(e *env) {
		e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
			j := fixtureJob(42, domain.JobQueued, "reviewer", 0, time.Time{})
			return mcp.JobInfo{Job: j, Ref: "JOB-42", Reason: "Only Mac Mini can run this, and it is offline", ReasonCode: string(fleet.ReasonOffline)}, nil
		}
	}
	empty := func(e *env) {
		e.svc.FleetOverviewFunc = func(context.Context, domain.Actor, any) (fleet.Overview, error) {
			return fleet.Overview{Headline: "No machines yet. Agents need somewhere to work.", Routing: domain.RoutingOwnOnly, Locations: []fleet.Location{}, Queue: []fleet.WaitingJob{}, Running: []fleet.RunningJob{}}, nil
		}
	}
	return []fixtureCase{
		{"fleet", "GET", "/api/v1/fleet", "", 200, "bearer", func(e *env) {
			e.svc.FleetOverviewFunc = func(context.Context, domain.Actor, any) (fleet.Overview, error) { return fixtureOverview(), nil }
		}},
		{"fleet-empty", "GET", "/api/v1/fleet", "", 200, "bearer", empty},
		{"providers", "GET", "/api/v1/providers", "", 200, "bearer", func(e *env) {
			e.svc.ListProvidersFunc = func(context.Context, domain.Actor, mcp.ListProvidersInput) ([]mcp.ProviderInfo, error) {
				return fixtureProviders(), nil
			}
		}},
		{"shapes", "GET", "/api/v1/shapes", "", 200, "bearer", func(e *env) {
			e.svc.ListShapesFunc = func(context.Context, domain.Actor, any) ([]mcp.ShapeInfo, error) { return fixtureShapes(), nil }
		}},
		{"machines", "GET", "/api/v1/machines", "", 200, "bearer", func(e *env) {
			e.svc.ListMachinesFunc = func(context.Context, domain.Actor, mcp.ListMachinesInput) ([]mcp.MachineInfo, error) {
				return []mcp.MachineInfo{
					{ID: fid(750), AccountID: fid(1), ProviderID: fid(700), ShapeID: fid(710), State: domain.MachineBusy, JobsTaken: 1, CreatedAt: at(-5), UpdatedAt: at(-4), Name: "linux-small-8f2", Shape: "linux-small", Provider: "home-server", JobRef: "JOB-41"},
					{ID: fid(751), AccountID: fid(1), ProviderID: fid(700), ShapeID: fid(711), State: domain.MachineBooting, CreatedAt: at(-1), UpdatedAt: at(-1), Name: "linux-medium-1ac", Shape: "linux-medium", Provider: "home-server"},
				}, nil
			}
		}},
		{"join-codes", "GET", "/api/v1/join-codes", "", 200, "bearer", joinCodes},
		{"join-code-create", "POST", "/api/v1/join-codes", `{}`, 201, "bearer", func(e *env) {
			e.svc.CreateJoinCodeFunc = func(context.Context, domain.Actor, mcp.CreateJoinCodeInput) (mcp.CreateJoinCodeOutput, error) {
				return mcp.CreateJoinCodeOutput{JoinCode: mcp.JoinCodeInfo{ProviderJoinCode: waiting, Status: mcp.JoinWaiting}, Code: "K7PQ-2M4X"}, nil
			}
		}},
		{"join-confirm", "POST", "/api/v1/join-codes/" + fid(720).String() + "/confirm", `{"name":"home-server","facts_hash":"f3a9c1"}`, 200, "bearer", func(e *env) {
			e.svc.ConfirmJoinFunc = func(context.Context, domain.Actor, mcp.ConfirmJoinInput) (mcp.ProviderInfo, error) {
				p := fixtureProviders()[0]
				p.Status, p.Online = domain.ProviderPending, false
				return p, nil
			}
		}},
		{"image-register", "POST", "/api/v1/images", `{"name":"windows-11","version":"1","os":"windows","arch":"amd64","format":"vhdx","uri":"https://images.acme.test/windows-11.vhdx","digest":"sha256:` + fmt.Sprintf("%064d", 1) + `","signature":"ed25519:c2ln","built_at":"2026-03-01T10:00:00Z","contents":{}}`, 201, "bearer", nil},
		{"config-resources-provider", "GET", "/api/v1/config/resources?kind=provider", "", 200, "bearer", func(e *env) {
			e.svc.ConfigGetFunc = func(context.Context, domain.Actor, mcp.ConfigGetInput) ([]mcp.ConfigResource, error) {
				return []mcp.ConfigResource{
					{Kind: "provider", Name: "home-server", Scope: "organization", Spec: json.RawMessage(`{"kind":"microvm","limits":{"max_machines":3},"mode":"agent"}`)},
					{Kind: "provider", Name: "aws-frankfurt", Scope: "organization", Spec: json.RawMessage(`{"kind":"aws","limits":{"max_machines":20,"monthly_budget":200},"mode":"agent","network_allow":["registry.acme.test"],"tags":{"team":"platform"}}`)},
				}, nil
			}
		}},
		{"config-resources-fleet-policy", "GET", "/api/v1/config/resources?kind=fleet_policy", "", 200, "bearer", func(e *env) {
			e.svc.ConfigGetFunc = func(context.Context, domain.Actor, mcp.ConfigGetInput) ([]mcp.ConfigResource, error) {
				return []mcp.ConfigResource{{Kind: "fleet_policy", Name: "fleet", Scope: "organization", Spec: json.RawMessage(`{"egress":["registry.acme.test"],"plan":"free","quotas":{"concurrent_machines":20,"monthly_budget":200},"routing":"own_only"}`)}}, nil
			}
		}},
		{"config-export", "GET", "/api/v1/config/export", "", 200, "bearer", func(e *env) {
			e.svc.ConfigExportFunc = func(context.Context, domain.Actor, any) (mcp.ConfigExport, error) {
				return mcp.ConfigExport{YAML: "kind: provider\nname: home-server\nspec:\n  kind: microvm\n  limits:\n    max_machines: 3\n"}, nil
			}
		}},
		{"config-plans", "GET", "/api/v1/config/plans", "", 200, "bearer", plans},
		{"config-plans-inbox", "GET", "/api/v1/config/plans?inbox=true", "", 200, "bearer", plans},
		{"config-plans-empty", "GET", "/api/v1/config/plans?inbox=true", "", 200, "bearer", func(e *env) {
			e.svc.ConfigPlansFunc = func(context.Context, domain.Actor, mcp.ConfigPlansInput) ([]mcp.ConfigPlanView, error) {
				return []mcp.ConfigPlanView{}, nil
			}
		}},
		{"config-plan", "POST", "/api/v1/config/plans", `{"title":"Add machine types to Home server","source":"form","changes":[{"kind":"shape","name":"linux-small","spec":{"provider":"home-server","os":"linux","arch":"amd64","cpu":2,"memory_mb":4096,"disk_gb":20,"max":3,"enabled":true}}]}`, 201, "bearer", func(e *env) {
			e.svc.ConfigPlanFunc = func(context.Context, domain.Actor, mcp.ConfigPlanInput) (mcp.ConfigPlanView, error) {
				return fleetPlan(730, domain.ConfigPlanPending, domain.ConfigFromForm, nil), nil
			}
		}},
		{"config-plan-lowers", "GET", "/api/v1/config/plans/" + fid(733).String(), "", 200, "bearer", func(e *env) { e.svc.ConfigGetPlanFunc = orFleet(lowers) }},
		{"config-plan-needs-secret", "GET", "/api/v1/config/plans/" + fid(734).String(), "", 200, "bearer", func(e *env) { e.svc.ConfigGetPlanFunc = orFleet(secret) }},
		{"config-plan-stale", "GET", "/api/v1/config/plans/" + fid(730).String(), "", 200, "bearer", func(e *env) {
			e.svc.ConfigGetPlanFunc = orFleet(fleetPlan(730, domain.ConfigPlanPending, domain.ConfigFromForm, func(v *mcp.ConfigPlanView) { v.Stale = true }))
		}},
		{"config-plan-cannot-confirm", "GET", "/api/v1/config/plans/" + fid(730).String(), "", 200, "bearer", func(e *env) {
			e.svc.ConfigGetPlanFunc = orFleet(fleetPlan(730, domain.ConfigPlanPending, domain.ConfigFromForm, func(v *mcp.ConfigPlanView) { v.CanConfirm = false }))
		}},
		{"config-plan-applied", "POST", "/api/v1/config/plans/" + fid(730).String() + "/apply", `{}`, 200, "bearer", func(e *env) {
			e.svc.ConfigApplyFunc = func(context.Context, domain.Actor, mcp.ConfigApplyInput) (mcp.ConfigApplied, error) {
				return mcp.ConfigApplied{Plan: fleetPlan(730, domain.ConfigPlanApplied, domain.ConfigFromForm, nil), ShowOnce: []mcp.ConfigShowOnce{{Label: "SCIM token", Value: "scim_Zm9yZWNyZXc"}}}, nil
			}
		}},
		{"config-plan-approval", "POST", "/api/v1/config/plans/" + fid(730).String() + "/request-approval", ``, 200, "bearer", func(e *env) {
			e.svc.ConfigRequestApprovalFunc = func(context.Context, domain.Actor, mcp.ConfigPlanRefInput) (mcp.ConfigConfirmation, error) {
				return mcp.ConfigConfirmation{Status: "confirmation_required", Plan: fleetPlan(730, domain.ConfigPlanAwaiting, domain.ConfigFromForm, func(v *mcp.ConfigPlanView) { v.CanConfirm = false }),
					ConfirmURL: "https://forecrew.example.com/inbox?plan=" + fid(730).String(), Message: "A person with Manage machines confirms it in the Inbox."}, nil
			}
		}},
		{"config-plan-revert", "POST", "/api/v1/config/plans/" + fid(730).String() + "/revert", ``, 201, "bearer", func(e *env) {
			e.svc.ConfigRevertFunc = orFleet(fleetPlan(735, domain.ConfigPlanPending, domain.ConfigFromForm, func(v *mcp.ConfigPlanView) {
				v.Title, v.Reverts, v.Undo = "Revert: Add machine types to Home server", fid(730), "A machine that was destroyed does not come back."
				v.Reversible = false
			}))
		}},
		{"assistant-conversations", "GET", "/api/v1/assistant/conversations", "", 200, "bearer", func(e *env) {
			e.svc.AssistantListFunc = func(context.Context, domain.Actor, mcp.AssistantListInput) ([]domain.AssistantConversation, error) {
				return []domain.AssistantConversation{conversation.AssistantConversation}, nil
			}
		}},
		{"assistant-conversation", "GET", "/api/v1/assistant/conversations/" + fid(740).String(), "", 200, "bearer", func(e *env) {
			e.svc.AssistantGetFunc = func(context.Context, domain.Actor, mcp.AssistantRefInput) (mcp.AssistantView, error) {
				return conversation, nil
			}
		}},
		{"assistant-thinking", "GET", "/api/v1/assistant/conversations/" + fid(740).String(), "", 200, "bearer", func(e *env) {
			e.svc.AssistantGetFunc = func(context.Context, domain.Actor, mcp.AssistantRefInput) (mcp.AssistantView, error) {
				v := conversation
				v.Messages, v.Plans, v.Thinking = v.Messages[:1], []mcp.ConfigPlanView{}, true
				return v, nil
			}
		}},
		{"assistant-send", "POST", "/api/v1/assistant/messages", `{"message":"Add my Linux server","page":"/machines"}`, 202, "bearer", func(e *env) {
			e.svc.AssistantSendFunc = func(context.Context, domain.Actor, mcp.AssistantSendInput) (mcp.AssistantView, error) {
				v := conversation
				v.Messages, v.Plans, v.Thinking = v.Messages[:1], []mcp.ConfigPlanView{}, true
				return v, nil
			}
		}},
		{"assistant-unavailable", "POST", "/api/v1/assistant/messages", `{"message":"hello"}`, 503, "bearer", func(e *env) {
			e.svc.AssistantSendFunc = func(context.Context, domain.Actor, mcp.AssistantSendInput) (mcp.AssistantView, error) {
				return mcp.AssistantView{}, fmt.Errorf("no reasoning machine is online: %w", mcp.ErrUnavailable)
			}
		}},
		{"job-waiting", "GET", "/api/v1/jobs/JOB-42", "", 200, "bearer", waitingJob},
		{"error-assistant-secret", "POST", "/api/v1/assistant/messages", `{"message":"my key is sk-ant-abc"}`, 400, "bearer", func(e *env) {
			e.svc.AssistantSendFunc = func(context.Context, domain.Actor, mcp.AssistantSendInput) (mcp.AssistantView, error) {
				return mcp.AssistantView{}, fmt.Errorf("that looks like a secret; secrets go in a secure field: %w", mcp.ErrInvalidInput)
			}
		}},
	}
}
