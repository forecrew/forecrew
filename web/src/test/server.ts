import { http, HttpResponse } from "msw"
import { setupServer } from "msw/node"
import gateApprove from "./fixtures/gate-approve.json"
import gateReject from "./fixtures/gate-reject.json"
import gateRequestChanges from "./fixtures/gate-request-changes.json"
import gates from "./fixtures/gates.json"
import goal from "./fixtures/goal.json"
import goalCreate from "./fixtures/goal-create.json"
import goals from "./fixtures/goals.json"
import job from "./fixtures/job.json"
import jobDispatch from "./fixtures/job-dispatch.json"
import jobLogs from "./fixtures/job-logs.json"
import jobLogsAllKinds from "./fixtures/job-logs-all-kinds.json"
import jobLogsCaughtUp from "./fixtures/job-logs-caught-up.json"
import jobLogsEmpty from "./fixtures/job-logs-empty.json"
import jobs from "./fixtures/jobs.json"
import me from "./fixtures/me.json"
import project from "./fixtures/project.json"
import projectMinimalProfile from "./fixtures/project-minimal-profile.json"
import projectOnboard from "./fixtures/project-onboard.json"
import projectUnknownCommands from "./fixtures/project-unknown-commands.json"
import projects from "./fixtures/projects.json"
import session from "./fixtures/session.json"
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
  http.get("/api/v1/me", () => HttpResponse.json(me)),
  http.get("/api/v1/projects", () => HttpResponse.json(projects)),
  http.post("/api/v1/projects", () => HttpResponse.json(projectOnboard, { status: 202 })),
  http.get("/api/v1/projects/*", () => HttpResponse.json(project)),
  http.get("/api/v1/goals", () => HttpResponse.json(goals)),
  http.post("/api/v1/goals", () => HttpResponse.json(goalCreate, { status: 201 })),
  http.get("/api/v1/goals/:ref", () => HttpResponse.json(goal)),
  http.get("/api/v1/jobs", () => HttpResponse.json(jobs)),
  http.post("/api/v1/jobs", () => HttpResponse.json(jobDispatch, { status: 201 })),
  http.get("/api/v1/jobs/:ref/logs", ({ request }) =>
    HttpResponse.json(new URL(request.url).searchParams.get("after_seq") === String(jobLogs.next_seq) ? jobLogsCaughtUp : jobLogs),
  ),
  http.get("/api/v1/jobs/:ref", () => HttpResponse.json(job)),
  http.get("/api/v1/workers", () => HttpResponse.json(workers)),
  http.get("/api/v1/gates", () => HttpResponse.json(gates)),
  http.post("/api/v1/gates/:ref/approve", () => HttpResponse.json(gateApprove)),
  http.post("/api/v1/gates/:ref/request-changes", () => HttpResponse.json(gateRequestChanges)),
  http.post("/api/v1/gates/:ref/reject", () => HttpResponse.json(gateReject)),
]

export const server = setupServer(...handlers)

/** The recorded payloads, for tests that change what the API answers. */
export const fx = {
  me, session, projects, projectsEmpty, project, projectOnboard, goals, goalsEmpty, goal, goalCreate, jobs, jobsEmpty, job,
  jobQueued, jobFailed, jobDispatch, jobLogs, jobLogsCaughtUp, jobLogsEmpty, jobLogsAllKinds, projectMinimalProfile, projectUnknownCommands, workers, workersEmpty, gates, gatesEmpty, gateApprove,
  gateRequestChanges, gateReject,
}

/** The recorded error answers. */
export const errors = {
  unauthorized: () => HttpResponse.json(errorUnauthorized, { status: 401 }),
  notFound: () => HttpResponse.json(errorNotFound, { status: 404 }),
  invalidInput: () => HttpResponse.json(errorInvalidInput, { status: 400 }),
  conflict: () => HttpResponse.json(errorConflict, { status: 409 }),
}

/** Answers every call to a path with the recorded 401. */
export const unauthorized = errors.unauthorized
