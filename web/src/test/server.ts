import { http, HttpResponse } from "msw"
import { setupServer } from "msw/node"
import assumptionRegister from "./fixtures/assumption-register.json"
import assumptionRegisterEmpty from "./fixtures/assumption-register-empty.json"
import assumptionRegisterResolve from "./fixtures/assumption-register-resolve.json"
import assumptions from "./fixtures/assumptions.json"
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
import tokenRevoke from "./fixtures/token-revoke.json"
import tokenRevokeSelf from "./fixtures/token-revoke-self.json"
import workers from "./fixtures/workers.json"
import errorConflict from "./fixtures/error-conflict.json"
import errorInvalidInput from "./fixtures/error-invalid-input.json"
import errorNotFound from "./fixtures/error-not-found.json"
import errorUnauthorized from "./fixtures/error-unauthorized.json"
import gatesEmpty from "./fixtures/gates-empty.json"
import goalsEmpty from "./fixtures/goals-empty.json"
import jobFailed from "./fixtures/job-failed.json"
import jobQueued from "./fixtures/job-queued.json"
import jobsEmpty from "./fixtures/jobs-empty.json"
import projectsEmpty from "./fixtures/projects-empty.json"
import workersEmpty from "./fixtures/workers-empty.json"

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
  http.get("/api/v1/tokens", () => HttpResponse.json(tokens)),
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
  gateRequestChanges, gateReject, jobArtifacts, jobArtifactsEmpty, jobCommand, quality, qualityBaselineOnly, qualityEmpty, verification, tokens, tokensEmpty, tokenRevoke, tokenRevokeSelf,
  councils, councilsEmpty, councilRunning, councilDecided, councilFailed, councilPending, councilStart, councilConstraint,
  assumptions, assumptionResolve, decisions, decisionsEmpty, gatesCouncil,
  trace, assumptionRegister, assumptionRegisterEmpty, assumptionRegisterResolve, knowledgeEntries, knowledgeEntriesEmpty, knowledgeNotes,
  knowledgeNoteDelete, knowledgeProposals, knowledgeProposalsEmpty, knowledgePropose, knowledgeSearch, knowledgeSearchEmpty,
}

/** The recorded error answers. */
export const errors = {
  unauthorized: () => HttpResponse.json(errorUnauthorized, { status: 401 }),
  notFound: () => HttpResponse.json(errorNotFound, { status: 404 }),
  invalidInput: () => HttpResponse.json(errorInvalidInput, { status: 400 }),
  conflict: () => HttpResponse.json(errorConflict, { status: 409 }),
  councilEnded: () => HttpResponse.json(councilConstraintConflict, { status: 409 }),
  cancelConflict: () => HttpResponse.json(jobCancelConflict, { status: 409 }),
}

/** Answers every call to a path with the recorded 401. */
export const unauthorized = errors.unauthorized
