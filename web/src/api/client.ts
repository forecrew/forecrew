import type {
  CreateGoalInput, DispatchJobInput, ErrorBody, Gate, GateInfo, Goal, GoalDetail, GoalStatus, Job, JobInfo,
  JobLogs, JobStatus, OnboardInput, OnboardResult, Project, Session, Worker,
} from "./types"

/** An error answer of the API. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = "ApiError"
    this.status = status
    this.code = code
  }
}

let onUnauthorized: () => void = () => {}

/** Sets what happens on every 401 (the app goes back to the login page). */
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn
}

async function request<T>(method: string, path: string, body?: unknown, opts: { login?: boolean } = {}): Promise<T> {
  const init: RequestInit = { method, headers: {}, credentials: "same-origin" }
  // The API only takes a session cookie on state changes with this type.
  if (method !== "GET") {
    ;(init.headers as Record<string, string>)["Content-Type"] = "application/json"
    if (body !== undefined) init.body = JSON.stringify(body)
  }
  let res: Response
  try {
    res = await fetch(path, init)
  } catch {
    throw new ApiError(0, "network", "Cannot reach the server")
  }
  if (res.ok) {
    return (res.status === 204 ? undefined : await res.json()) as T
  }
  let code = "internal"
  let message = `Request failed (${res.status})`
  try {
    const e = (await res.json()) as ErrorBody
    code = e.error.code
    message = e.error.message
  } catch {
    // not an API error body: keep the generic message
  }
  if (res.status === 401 && !opts.login) onUnauthorized()
  throw new ApiError(res.status, code, message)
}

function query(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "" && v !== 0) q.set(k, String(v))
  const s = q.toString()
  return s ? `?${s}` : ""
}

const seg = encodeURIComponent
// Project names may be owner/name: the server takes the rest of the path.
const projectPath = (name: string) => name.split("/").map(seg).join("/")

export const api = {
  login: (token: string) => request<Session>("POST", "/api/v1/session", { token }, { login: true }),
  logout: () => request<void>("DELETE", "/api/v1/session", undefined, { login: true }),
  me: () => request<Session>("GET", "/api/v1/me"),

  listProjects: () => request<Project[]>("GET", "/api/v1/projects"),
  getProject: (name: string) => request<Project>("GET", `/api/v1/projects/${projectPath(name)}`),
  onboardProject: (input: OnboardInput) => request<OnboardResult>("POST", "/api/v1/projects", input),

  listGoals: (f: { project?: string; status?: GoalStatus | "" } = {}) =>
    request<Goal[]>("GET", `/api/v1/goals${query(f)}`),
  getGoal: (ref: string) => request<GoalDetail>("GET", `/api/v1/goals/${seg(ref)}`),
  createGoal: (input: CreateGoalInput) => request<Goal>("POST", "/api/v1/goals", input),

  listJobs: (f: { project?: string; goal?: string; status?: JobStatus | ""; limit?: number } = {}) =>
    request<Job[]>("GET", `/api/v1/jobs${query(f)}`),
  getJob: (ref: string) => request<JobInfo>("GET", `/api/v1/jobs/${seg(ref)}`),
  jobLogs: (ref: string, afterSeq = 0, limit = 500) =>
    request<JobLogs>("GET", `/api/v1/jobs/${seg(ref)}/logs${query({ after_seq: afterSeq, limit })}`),
  dispatchJob: (input: DispatchJobInput) => request<Job>("POST", "/api/v1/jobs", input),

  listWorkers: () => request<Worker[]>("GET", "/api/v1/workers"),

  listGates: () => request<GateInfo[]>("GET", "/api/v1/gates"),
  approve: (ref: string, feedback?: string) =>
    request<Gate>("POST", `/api/v1/gates/${seg(ref)}/approve`, feedback ? { feedback } : {}),
  requestChanges: (ref: string, feedback: string) =>
    request<Gate>("POST", `/api/v1/gates/${seg(ref)}/request-changes`, { feedback }),
  reject: (ref: string, feedback?: string) =>
    request<Gate>("POST", `/api/v1/gates/${seg(ref)}/reject`, feedback ? { feedback } : {}),
}
