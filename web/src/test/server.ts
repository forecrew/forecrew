import { http, HttpResponse } from "msw"
import { setupServer } from "msw/node"
import assumptionRegister from "./fixtures/assumption-register.json"
import assumptionRegisterEmpty from "./fixtures/assumption-register-empty.json"
import assumptionRegisterResolve from "./fixtures/assumption-register-resolve.json"
import assumptions from "./fixtures/assumptions.json"
import gatesProposal from "./fixtures/gates-proposal.json"
import proposalDecide from "./fixtures/proposal-decide.json"
import proposals from "./fixtures/proposals.json"
import assumptionResolve from "./fixtures/assumption-resolve.json"
import councilConstraint from "./fixtures/council-constraint.json"
import councilConstraintConflict from "./fixtures/council-constraint-conflict.json"
import councilDecided from "./fixtures/council-decided.json"
import councilFailed from "./fixtures/council-failed.json"
import councilPending from "./fixtures/council-pending.json"
import councilRunning from "./fixtures/council-running.json"
import councilStart from "./fixtures/council-start.json"
import councils from "./fixtures/councils.json"
import councilsEmpty from "./fixtures/councils-empty.json"
import decisions from "./fixtures/decisions.json"
import decisionsEmpty from "./fixtures/decisions-empty.json"
import gatesCouncil from "./fixtures/gates-council.json"
import knowledgeEntries from "./fixtures/knowledge-entries.json"
import knowledgeEntriesEmpty from "./fixtures/knowledge-entries-empty.json"
import knowledgeNoteDelete from "./fixtures/knowledge-note-delete.json"
import knowledgeNotes from "./fixtures/knowledge-notes.json"
import knowledgeProposals from "./fixtures/knowledge-proposals.json"
import knowledgeProposalsEmpty from "./fixtures/knowledge-proposals-empty.json"
import knowledgePropose from "./fixtures/knowledge-propose.json"
import knowledgeSearch from "./fixtures/knowledge-search.json"
import knowledgeSearchEmpty from "./fixtures/knowledge-search-empty.json"
import trace from "./fixtures/trace.json"
import gateApprove from "./fixtures/gate-approve.json"
import gateReject from "./fixtures/gate-reject.json"
import gateRequestChanges from "./fixtures/gate-request-changes.json"
import gates from "./fixtures/gates.json"
import goal from "./fixtures/goal.json"
import goalCreate from "./fixtures/goal-create.json"
import goals from "./fixtures/goals.json"
import job from "./fixtures/job.json"
import jobArtifacts from "./fixtures/job-artifacts.json"
import jobArtifactsEmpty from "./fixtures/job-artifacts-empty.json"
import jobCommand from "./fixtures/job-command.json"
import quality from "./fixtures/quality.json"
import qualityBaselineOnly from "./fixtures/quality-baseline-only.json"
import qualityEmpty from "./fixtures/quality-empty.json"
import verification from "./fixtures/verification.json"
import jobCancel from "./fixtures/job-cancel.json"
import jobCancelConflict from "./fixtures/job-cancel-conflict.json"
import jobDispatch from "./fixtures/job-dispatch.json"
import jobLogs from "./fixtures/job-logs.json"
import jobLogsAllKinds from "./fixtures/job-logs-all-kinds.json"
import jobLogsCaughtUp from "./fixtures/job-logs-caught-up.json"
import jobLogsEmpty from "./fixtures/job-logs-empty.json"
import jobs from "./fixtures/jobs.json"
import authDiscover from "./fixtures/auth-discover.json"
import me from "./fixtures/me.json"
import project from "./fixtures/project.json"
import projectMinimalProfile from "./fixtures/project-minimal-profile.json"
import projectOnboard from "./fixtures/project-onboard.json"
import projectUnknownCommands from "./fixtures/project-unknown-commands.json"
import projects from "./fixtures/projects.json"
import session from "./fixtures/session.json"
import tokens from "./fixtures/tokens.json"
import tokensEmpty from "./fixtures/tokens-empty.json"
import connectedApps from "./fixtures/connected-apps.json"
import tokenRevoke from "./fixtures/token-revoke.json"
import tokenRevokeSelf from "./fixtures/token-revoke-self.json"
import workers from "./fixtures/workers.json"
import audit from "./fixtures/audit.json"
import auditEmpty from "./fixtures/audit-empty.json"
import assistantConversation from "./fixtures/assistant-conversation.json"
import assistantConversations from "./fixtures/assistant-conversations.json"
import assistantSend from "./fixtures/assistant-send.json"
import assistantThinking from "./fixtures/assistant-thinking.json"
import assistantUnavailable from "./fixtures/assistant-unavailable.json"
import configExport from "./fixtures/config-export.json"
import configPlanApplied from "./fixtures/config-plan-applied.json"
import configPlanApproval from "./fixtures/config-plan-approval.json"
import configPlanCannotConfirm from "./fixtures/config-plan-cannot-confirm.json"
import configPlanLowers from "./fixtures/config-plan-lowers.json"
import configPlanNeedsSecret from "./fixtures/config-plan-needs-secret.json"
import configPlanRevert from "./fixtures/config-plan-revert.json"
import configPlanStale from "./fixtures/config-plan-stale.json"
import configPlan from "./fixtures/config-plan.json"
import configPlansEmpty from "./fixtures/config-plans-empty.json"
import configPlansInbox from "./fixtures/config-plans-inbox.json"
import configPlans from "./fixtures/config-plans.json"
import configResourcesFleetPolicy from "./fixtures/config-resources-fleet-policy.json"
import configResourcesProvider from "./fixtures/config-resources-provider.json"
import errorAssistantSecret from "./fixtures/error-assistant-secret.json"
import fleetEmpty from "./fixtures/fleet-empty.json"
import fleet from "./fixtures/fleet.json"
import imageBuildStart from "./fixtures/image-build-start.json"
import imageBuildsDone from "./fixtures/image-builds-done.json"
import imageBuildsEmpty from "./fixtures/image-builds-empty.json"
import imageBuildsFailed from "./fixtures/image-builds-failed.json"
import imageBuildsRequested from "./fixtures/image-builds-requested.json"
import imageBuildsRunning from "./fixtures/image-builds-running.json"
import imageRegister from "./fixtures/image-register.json"
import jobWaiting from "./fixtures/job-waiting.json"
import joinCodeCreate from "./fixtures/join-code-create.json"
import joinCodes from "./fixtures/join-codes.json"
import joinConfirm from "./fixtures/join-confirm.json"
import machines from "./fixtures/machines.json"
import providers from "./fixtures/providers.json"
import shapes from "./fixtures/shapes.json"
import configPlanApply from "./fixtures/config-plan-apply.json"
import configPlanCancel from "./fixtures/config-plan-cancel.json"
import configPlanConnection from "./fixtures/config-plan-connection.json"
import configPlanSecret from "./fixtures/config-plan-secret.json"
import connectionCreate from "./fixtures/connection-create.json"
import connectionEnable from "./fixtures/connection-enable.json"
import connectionEnableUntested from "./fixtures/connection-enable-untested.json"
import connections from "./fixtures/connections.json"
import identities from "./fixtures/identities.json"
import invitationCreate from "./fixtures/invitation-create.json"
import invitationRevoke from "./fixtures/invitation-revoke.json"
import invitations from "./fixtures/invitations.json"
import meAdmin from "./fixtures/me-admin.json"
import meContributor from "./fixtures/me-contributor.json"
import meOwnerEnterprise from "./fixtures/me-owner-enterprise.json"
import meViewer from "./fixtures/me-viewer.json"
import memberLastOwner from "./fixtures/member-last-owner.json"
import memberSuspend from "./fixtures/member-suspend.json"
import members from "./fixtures/members.json"
import organization from "./fixtures/organization.json"
import roleBindingCreate from "./fixtures/role-binding-create.json"
import roleBindingDelete from "./fixtures/role-binding-delete.json"
import roleBindings from "./fixtures/role-bindings.json"
import roleCreate from "./fixtures/role-create.json"
import roleDeleteInUse from "./fixtures/role-delete-in-use.json"
import roleUpdate from "./fixtures/role-update.json"
import roles from "./fixtures/roles.json"
import rolesCustom from "./fixtures/roles-custom.json"
import serviceAccountCreate from "./fixtures/service-account-create.json"
import serviceAccounts from "./fixtures/service-accounts.json"
import sessions from "./fixtures/sessions.json"
import teamCreate from "./fixtures/team-create.json"
import teamMembers from "./fixtures/team-members.json"
import teamsEmpty from "./fixtures/teams-empty.json"
import teams from "./fixtures/teams.json"
import tokenCreate from "./fixtures/token-create.json"
import tokensScopes from "./fixtures/tokens-scopes.json"
import errorConflict from "./fixtures/error-conflict.json"
import errorInvalidInput from "./fixtures/error-invalid-input.json"
import errorNotFound from "./fixtures/error-not-found.json"
import errorUnauthorized from "./fixtures/error-unauthorized.json"
import gatesEmpty from "./fixtures/gates-empty.json"
import goalsEmpty from "./fixtures/goals-empty.json"
import jobFailed from "./fixtures/job-failed.json"
import jobQueued from "./fixtures/job-queued.json"
import jobsEmpty from "./fixtures/jobs-empty.json"
import configPlanTracker from "./fixtures/config-plan-tracker.json"
import configPlanTrackerApply from "./fixtures/config-plan-tracker-apply.json"
import configPlanTrackerSecret from "./fixtures/config-plan-tracker-secret.json"
import ticketAccept from "./fixtures/ticket-accept.json"
import ticketDismiss from "./fixtures/ticket-dismiss.json"
import ticketsWaiting from "./fixtures/tickets-waiting.json"
import trackerConnections from "./fixtures/tracker-connections.json"
import trackerIdentities from "./fixtures/tracker-identities.json"
import trackerIdentityMap from "./fixtures/tracker-identity-map.json"
import trackerRoutes from "./fixtures/tracker-routes.json"
import trackerTest from "./fixtures/tracker-test.json"
import trackerTestFailed from "./fixtures/tracker-test-failed.json"
import trackerConnectionsEmpty from "./fixtures/tracker-connections-empty.json"
import trackerUnmappedProblem from "./fixtures/tracker-unmapped-problem.json"
import ticketsWaitingEmpty from "./fixtures/tickets-waiting-empty.json"
import trackerUnmapped from "./fixtures/tracker-unmapped.json"
import projectsEmpty from "./fixtures/projects-empty.json"
import workersEmpty from "./fixtures/workers-empty.json"
import configPlanModelAuthorization from "./fixtures/config-plan-model-authorization.json"
import configPlanModelConnection from "./fixtures/config-plan-model-connection.json"
import configResourcesModelAssignment from "./fixtures/config-resources-model-assignment.json"
import configResourcesModelAuthorization from "./fixtures/config-resources-model-authorization.json"
import configResourcesModelConnection from "./fixtures/config-resources-model-connection.json"
import jobWaitingPrivateCode from "./fixtures/job-waiting-private-code.json"
import modelCheck from "./fixtures/model-check.json"
import modelChecks from "./fixtures/model-checks.json"
import modelChecksEmpty from "./fixtures/model-checks-empty.json"
import modelTestEstimate from "./fixtures/model-test-estimate.json"
import modelsCatalog from "./fixtures/models-catalog.json"
import modelsSuggest from "./fixtures/models-suggest.json"

// The API as the Go golden test (internal/api) recorded it. Tests only use
// these files, never hand-written payloads: see docs/ui.md.
export const handlers = [
  http.post("/api/v1/session", () => HttpResponse.json(session)),
  http.delete("/api/v1/session", () => new HttpResponse(null, { status: 204 })),
  http.post("/api/v1/auth/discover", () => HttpResponse.json(authDiscover)),
  http.get("/api/v1/me", () => HttpResponse.json(me)),
  http.get("/api/v1/projects", () => HttpResponse.json(projects)),
  http.post("/api/v1/projects", () => HttpResponse.json(projectOnboard, { status: 202 })),
  http.get("/api/v1/projects/*", () => HttpResponse.json(project)),
  http.get("/api/v1/goals", () => HttpResponse.json(goals)),
  http.post("/api/v1/goals", () => HttpResponse.json(goalCreate, { status: 201 })),
  http.get("/api/v1/goals/:ref", () => HttpResponse.json(goal)),
  http.get("/api/v1/jobs", () => HttpResponse.json(jobs)),
  http.post("/api/v1/jobs", () => HttpResponse.json(jobDispatch, { status: 201 })),
  http.post("/api/v1/jobs/:ref/cancel", () => HttpResponse.json(jobCancel)),
  http.get("/api/v1/jobs/:ref/logs", ({ request }) =>
    HttpResponse.json(new URL(request.url).searchParams.get("after_seq") === String(jobLogs.next_seq) ? jobLogsCaughtUp : jobLogs),
  ),
  http.get("/api/v1/jobs/:ref/artifacts", () => HttpResponse.json(jobArtifactsEmpty)),
  http.get("/api/v1/jobs/:ref", () => HttpResponse.json(job)),
  http.get("/api/v1/quality", () => HttpResponse.json(quality)),
  http.get("/api/v1/changes/:change/verification", () => HttpResponse.json(verification)),
  http.get("/api/v1/workers", () => HttpResponse.json(workers)),
  http.put("/api/v1/session/organization", () => HttpResponse.json(me)),
  http.delete("/api/v1/sessions", () => new HttpResponse(null, { status: 204 })),
  http.get("/api/v1/sessions", () => HttpResponse.json(sessions)),
  http.delete("/api/v1/sessions/:id", () => new HttpResponse(null, { status: 204 })),
  http.get("/api/v1/identities", () => HttpResponse.json(identities)),
  http.post("/api/v1/cli/authorize", () => HttpResponse.json({ redirect: "http://127.0.0.1:5555/callback?code=fcl_abc&state=st", code: "fcl_abc" })),
  http.post("/api/v1/tokens", () => HttpResponse.json(tokenCreate, { status: 201 })),
  http.get("/api/v1/members", () => HttpResponse.json(members)),
  http.post("/api/v1/members/:id/suspend", () => HttpResponse.json(memberSuspend)),
  http.post("/api/v1/members/:id/resume", () => HttpResponse.json(members[1])),
  http.delete("/api/v1/members/:id", () => HttpResponse.json(members[1])),
  http.patch("/api/v1/organization", () => HttpResponse.json(organization)),
  http.get("/api/v1/invitations", () => HttpResponse.json(invitations)),
  http.post("/api/v1/invitations", () => HttpResponse.json(invitationCreate, { status: 201 })),
  http.post("/api/v1/invitations/:id/revoke", () => HttpResponse.json(invitationRevoke)),
  http.get("/api/v1/teams", () => HttpResponse.json(teams)),
  http.post("/api/v1/teams", () => HttpResponse.json(teamCreate, { status: 201 })),
  http.delete("/api/v1/teams/:slug", () => HttpResponse.json(teams[0])),
  http.get("/api/v1/teams/:slug/members", () => HttpResponse.json(teamMembers)),
  http.post("/api/v1/teams/:slug/members", () => HttpResponse.json(teamMembers[0], { status: 201 })),
  http.delete("/api/v1/teams/:slug/members/:user", () => HttpResponse.json(teamMembers[0])),
  http.get("/api/v1/roles", () => HttpResponse.json(roles)),
  http.get("/api/v1/role-bindings", ({ request }) => {
    const project = new URL(request.url).searchParams.get("project")
    return HttpResponse.json(project ? roleBindings.filter((b) => b.project === project) : roleBindings)
  }),
  http.post("/api/v1/role-bindings", () => HttpResponse.json(roleBindingCreate, { status: 201 })),
  http.delete("/api/v1/role-bindings/:id", () => HttpResponse.json(roleBindingDelete)),
  http.get("/api/v1/service-accounts", () => HttpResponse.json(serviceAccounts)),
  http.post("/api/v1/service-accounts", () => HttpResponse.json(serviceAccountCreate, { status: 201 })),
  http.post("/api/v1/service-accounts/:name/:action", () => HttpResponse.json(serviceAccounts[0])),
  http.get("/api/v1/fleet", () => HttpResponse.json(fleet)),
  http.get("/api/v1/providers", () => HttpResponse.json(providers)),
  http.get("/api/v1/shapes", () => HttpResponse.json(shapes)),
  http.get("/api/v1/machines", () => HttpResponse.json(machines)),
  http.get("/api/v1/join-codes", () => HttpResponse.json(joinCodes)),
  http.post("/api/v1/join-codes", () => HttpResponse.json(joinCodeCreate, { status: 201 })),
  http.post("/api/v1/join-codes/:join/confirm", () => HttpResponse.json(joinConfirm)),
  http.get("/api/v1/providers/:provider/image-builds", () => HttpResponse.json(imageBuildsEmpty)),
  http.post("/api/v1/providers/:provider/image-builds", () => HttpResponse.json(imageBuildStart, { status: 202 })),
  http.get("/api/v1/images", () => HttpResponse.json([])),
  http.post("/api/v1/images", () => HttpResponse.json(imageRegister, { status: 201 })),
  http.get("/api/v1/config/resources", ({ request }) => {
    const byKind: Record<string, unknown> = {
      fleet_policy: configResourcesFleetPolicy,
      model_connection: configResourcesModelConnection,
      model_assignment: configResourcesModelAssignment,
      model_authorization: configResourcesModelAuthorization,
    }
    return HttpResponse.json(byKind[new URL(request.url).searchParams.get("kind") ?? ""] ?? configResourcesProvider)
  }),
  http.get("/api/v1/models/catalog", () => HttpResponse.json(modelsCatalog)),
  http.get("/api/v1/model-checks", () => HttpResponse.json(modelChecks)),
  http.post("/api/v1/model-connections/:connection/check", () => HttpResponse.json(modelCheck, { status: 202 })),
  // With run the test starts: the recorded estimate, and the queued check as its test.
  http.post("/api/v1/model-connections/:connection/tests", async ({ request }) => {
    const body = (await request.json()) as { run?: boolean }
    return HttpResponse.json(body.run ? { ...modelTestEstimate, test: modelCheck } : modelTestEstimate)
  }),
  http.post("/api/v1/models/suggest", () => HttpResponse.json(modelsSuggest)),
  http.get("/api/v1/config/export", () => HttpResponse.json(configExport)),
  http.get("/api/v1/config/plans", ({ request }) => HttpResponse.json(new URL(request.url).searchParams.get("inbox") ? configPlansEmpty : configPlans)),
  http.get("/api/v1/config/plans/:plan", () => HttpResponse.json(configPlan)),
  http.post("/api/v1/config/plans/:plan/request-approval", () => HttpResponse.json(configPlanApproval)),
  http.post("/api/v1/config/plans/:plan/revert", () => HttpResponse.json(configPlanRevert, { status: 201 })),
  http.post("/api/v1/config/import", () => HttpResponse.json(configPlan, { status: 201 })),
  http.get("/api/v1/assistant/conversations", () => HttpResponse.json(assistantConversations)),
  http.get("/api/v1/assistant/conversations/:id", () => HttpResponse.json(assistantConversation)),
  http.post("/api/v1/assistant/messages", () => HttpResponse.json(assistantSend, { status: 202 })),
  http.post("/api/v1/config/plans", () => HttpResponse.json(configPlanConnection, { status: 201 })),
  http.put("/api/v1/config/plans/:plan/secrets/:secret", () => HttpResponse.json(configPlanSecret)),
  http.post("/api/v1/config/plans/:plan/apply", () => HttpResponse.json(configPlanApply)),
  http.post("/api/v1/config/plans/:plan/cancel", () => HttpResponse.json(configPlanCancel)),
  http.get("/api/v1/connections", () => HttpResponse.json(connections)),
  http.post("/api/v1/connections", () => HttpResponse.json(connectionCreate, { status: 201 })),
  http.post("/api/v1/connections/:name/enable", () => HttpResponse.json(connectionEnable)),
  http.post("/api/v1/connections/:name/disable", () => HttpResponse.json(connections[1])),
  http.get("/api/v1/audit", () => HttpResponse.json(audit)),
  http.get("/api/v1/tokens", () => HttpResponse.json(tokens)),
  http.get("/api/v1/connected-apps", () => HttpResponse.json([])),
  http.post("/api/v1/tokens/:id/revoke", ({ params }) =>
    HttpResponse.json(params.id === tokens.find((t) => t.current)?.id ? tokenRevokeSelf : tokenRevoke),
  ),
  // The fixtures belong to the project app; the others have none.
  http.get("/api/v1/councils", ({ request }) => HttpResponse.json(new URL(request.url).searchParams.get("project") === "app" ? councils : [])),
  http.post("/api/v1/councils", () => HttpResponse.json(councilStart, { status: 202 })),
  http.get("/api/v1/councils/:ref", ({ params }) => {
    const byRef: Record<string, unknown> = { "COUNCIL-2": councilRunning, "COUNCIL-3": councilDecided, "COUNCIL-4": councilFailed, "COUNCIL-5": councilPending }
    const found = byRef[String(params.ref)]
    return found ? HttpResponse.json(found) : HttpResponse.json(errorNotFound, { status: 404 })
  }),
  http.post("/api/v1/councils/:ref/constraints", () => HttpResponse.json(councilConstraint)),
  http.get("/api/v1/assumptions", () => HttpResponse.json(assumptions)),
  http.post("/api/v1/assumptions/:ref/resolve", () => HttpResponse.json(assumptionResolve)),
  http.get("/api/v1/proposals", ({ request }) => HttpResponse.json(new URL(request.url).searchParams.get("project") === "app" ? proposals : [])),
  http.post("/api/v1/proposals/:ref/decide", () => HttpResponse.json(proposalDecide)),
  http.get("/api/v1/trace", () => HttpResponse.json(trace)),
  http.get("/api/v1/knowledge/entries", ({ request }) => {
    const q = new URL(request.url).searchParams
    const kind = q.get("kind")
    const section = q.get("section")
    return HttpResponse.json(
      kind === "note" ? knowledgeNotes : { ...knowledgeEntries, entries: knowledgeEntries.entries.filter((e) => !section || e.section === section) },
    )
  }),
  http.get("/api/v1/knowledge/proposals", () => HttpResponse.json(knowledgeProposals)),
  http.post("/api/v1/knowledge/proposals", () => HttpResponse.json(knowledgePropose, { status: 201 })),
  http.get("/api/v1/knowledge", () => HttpResponse.json(knowledgeSearch)),
  http.delete("/api/v1/knowledge/notes/:id", () => HttpResponse.json(knowledgeNoteDelete)),
  http.get("/api/v1/decisions", ({ request }) => HttpResponse.json(new URL(request.url).searchParams.get("project") === "app" ? decisions : [])),
  http.get("/api/v1/tracker-connections", () => HttpResponse.json(trackerConnections)),
  http.post("/api/v1/tracker-connections/:name/test", () => HttpResponse.json(trackerTest)),
  http.get("/api/v1/tracker-routes", () => HttpResponse.json(trackerRoutes)),
  http.get("/api/v1/tracker-identities", () => HttpResponse.json(trackerIdentities)),
  http.post("/api/v1/tracker-identities", () => HttpResponse.json(trackerIdentityMap, { status: 201 })),
  http.get("/api/v1/tracker-people/unmapped", () => HttpResponse.json(trackerUnmapped)),
  http.get("/api/v1/tickets/waiting", () => HttpResponse.json(ticketsWaiting)),
  http.post("/api/v1/tickets/:ticket/accept", () => HttpResponse.json(ticketAccept)),
  http.post("/api/v1/tickets/:ticket/dismiss", () => HttpResponse.json(ticketDismiss)),
  http.get("/api/v1/gates", () => HttpResponse.json(gates)),
  http.post("/api/v1/gates/:ref/approve", () => HttpResponse.json(gateApprove)),
  http.post("/api/v1/gates/:ref/request-changes", () => HttpResponse.json(gateRequestChanges)),
  http.post("/api/v1/gates/:ref/reject", () => HttpResponse.json(gateReject)),
]

export const server = setupServer(...handlers)

/** The recorded payloads, for tests that change what the API answers. */
export const fx = {
  authDiscover, me, session, projects, projectsEmpty, project, projectOnboard, goals, goalsEmpty, goal, goalCreate, jobs, jobsEmpty, job,
  jobQueued, jobFailed, jobDispatch, jobCancel, jobLogs, jobLogsCaughtUp, jobLogsEmpty, jobLogsAllKinds, projectMinimalProfile, projectUnknownCommands, workers, workersEmpty, gates, gatesEmpty, gateApprove,
  connectedApps, gateRequestChanges, gateReject, jobArtifacts, jobArtifactsEmpty, jobCommand, quality, qualityBaselineOnly, qualityEmpty, verification, tokens, tokensEmpty, tokenRevoke, tokenRevokeSelf,
  councils, councilsEmpty, councilRunning, councilDecided, councilFailed, councilPending, councilStart, councilConstraint,
  assumptions, assumptionResolve, decisions, decisionsEmpty, gatesCouncil, gatesProposal, proposals, proposalDecide,
  trace, assumptionRegister, assumptionRegisterEmpty, assumptionRegisterResolve, knowledgeEntries, knowledgeEntriesEmpty, knowledgeNotes,
  knowledgeNoteDelete, knowledgeProposals, knowledgeProposalsEmpty, knowledgePropose, knowledgeSearch, knowledgeSearchEmpty,
  assistantConversation, assistantConversations, assistantSend, assistantThinking, assistantUnavailable, configExport, configPlanApplied, configPlanApproval, configPlanCannotConfirm, configPlanLowers, configPlanNeedsSecret, configPlanRevert, configPlanStale, configPlan, configPlansEmpty, configPlansInbox, configPlans, configResourcesFleetPolicy, configResourcesProvider, errorAssistantSecret, fleetEmpty, fleet, imageBuildStart, imageBuildsDone, imageBuildsEmpty, imageBuildsFailed, imageBuildsRequested, imageBuildsRunning, imageRegister, jobWaiting, joinCodeCreate, joinCodes, joinConfirm, machines, providers, shapes,
  audit, auditEmpty, configPlanApply, configPlanCancel, configPlanConnection, configPlanSecret, connectionCreate, connectionEnable, connectionEnableUntested, connections, identities, invitationCreate, invitationRevoke, invitations,
  meAdmin, meContributor, meOwnerEnterprise, meViewer, roleCreate, roleDeleteInUse, roleUpdate, rolesCustom, memberLastOwner, memberSuspend, members, organization, roleBindingCreate, roleBindingDelete, roleBindings, roles,
  serviceAccountCreate, serviceAccounts, sessions, teamCreate, teamMembers, teamsEmpty, teams, tokenCreate, tokensScopes,
  configPlanTracker, configPlanTrackerApply, configPlanTrackerSecret, ticketAccept, ticketDismiss, ticketsWaiting, ticketsWaitingEmpty, trackerConnections, trackerConnectionsEmpty,
  trackerIdentities, trackerIdentityMap, trackerRoutes, trackerTest, trackerTestFailed, trackerUnmapped, trackerUnmappedProblem,
  configPlanModelAuthorization, configPlanModelConnection, configResourcesModelAssignment, configResourcesModelAuthorization, configResourcesModelConnection,
  jobWaitingPrivateCode, modelCheck, modelChecks, modelChecksEmpty, modelTestEstimate, modelsCatalog, modelsSuggest,
}

/** The recorded error answers. */
export const errors = {
  unauthorized: () => HttpResponse.json(errorUnauthorized, { status: 401 }),
  notFound: () => HttpResponse.json(errorNotFound, { status: 404 }),
  invalidInput: () => HttpResponse.json(errorInvalidInput, { status: 400 }),
  conflict: () => HttpResponse.json(errorConflict, { status: 409 }),
  lastOwner: () => HttpResponse.json(memberLastOwner, { status: 409 }),
  untestedConnection: () => HttpResponse.json(connectionEnableUntested, { status: 409 }),
  councilEnded: () => HttpResponse.json(councilConstraintConflict, { status: 409 }),
  cancelConflict: () => HttpResponse.json(jobCancelConflict, { status: 409 }),
}

/** Answers every call to a path with the recorded 401. */
export const unauthorized = errors.unauthorized
