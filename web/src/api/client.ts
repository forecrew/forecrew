import type {
  Account, ConfigPlan, AuditEntry, AuditFilter, BrowserSession, CliAuthorization, Connection, CreateConnectionInput, CreatedInvitation, CreatedToken, Identity,
  Invitation, Member, Role, RoleBinding, Scope, ServiceAccount, Team, TeamMember,
  Artifact, Assumption, AssumptionStatus, Council, CouncilDetail, CouncilStatus, Decision, StartCouncilInput,
  CreateGoalInput, Discovery, DispatchJobInput, ErrorBody, Gate, GateInfo, Goal, GoalDetail, GoalStatus, Job, JobInfo,
  JobLogs, JobStatus, KnowledgeKind, KnowledgeListing, KnowledgeProposal, KnowledgeProposalStatus, KnowledgeResult, Level,
  ConnectedApp, OnboardInput, OnboardResult, Project, ProposeKnowledgeInput, ProposeKnowledgeResult, QualityInfo, RevokeResult, Session, Token,
  Trace, VerificationInfo, Worker,
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

/** The most the lists of entries, search hits and proposals show: the API caps them. */
export const PAGE = 50

const seg = encodeURIComponent
/** Where an artifact is downloaded from. */
export const artifactUrl = (id: string) => `/api/v1/artifacts/${seg(id)}`
// Project names may be owner/name: the server takes the rest of the path.
const projectPath = (name: string) => name.split("/").map(seg).join("/")

export const api = {
  login: (token: string) => request<Session>("POST", "/api/v1/session", { token }, { login: true }),
  discover: (input: { org?: string; invite?: string; email?: string }) => request<Discovery>("POST", "/api/v1/auth/discover", input, { login: true }),
  logout: () => request<void>("DELETE", "/api/v1/session", undefined, { login: true }),
  me: () => request<Session>("GET", "/api/v1/me"),
  switchOrganization: (slug: string) => request<Session>("PUT", "/api/v1/session/organization", { organization: slug }),
  signOutEverywhere: () => request<void>("DELETE", "/api/v1/sessions"),
  listSessions: () => request<BrowserSession[]>("GET", "/api/v1/sessions"),
  endSession: (id: string) => request<void>("DELETE", `/api/v1/sessions/${seg(id)}`),
  listIdentities: () => request<Identity[]>("GET", "/api/v1/identities"),
  cliAuthorize: (input: { redirect_uri: string; state: string; code_challenge: string; organization?: string }) =>
    request<CliAuthorization>("POST", "/api/v1/cli/authorize", input),

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
  cancelJob: (ref: string) => request<JobInfo>("POST", `/api/v1/jobs/${seg(ref)}/cancel`),

  listArtifacts: (job: string) => request<Artifact[]>("GET", `/api/v1/jobs/${seg(job)}/artifacts`),

  getQuality: (project: string) => request<QualityInfo>("GET", `/api/v1/quality${query({ project })}`),
  /** change is a PR number (with project), a PR URL or a task ref. */
  getVerification: (change: string, project?: string) =>
    request<VerificationInfo>("GET", `/api/v1/changes/${seg(change)}/verification${query({ project })}`),

  listWorkers: () => request<Worker[]>("GET", "/api/v1/workers"),

  listTokens: () => request<Token[]>("GET", "/api/v1/tokens"),
  revokeToken: (id: string) => request<RevokeResult>("POST", `/api/v1/tokens/${seg(id)}/revoke`),
  createToken: (input: { name: string; scopes: Scope[]; ttl?: string; service_account?: string }) =>
    request<CreatedToken>("POST", "/api/v1/tokens", input),

  listMembers: () => request<Member[]>("GET", "/api/v1/members"),
  suspendMember: (id: string, suspend: boolean) => request<Member>("POST", `/api/v1/members/${seg(id)}/${suspend ? "suspend" : "resume"}`),
  removeMember: (id: string) => request<Member>("DELETE", `/api/v1/members/${seg(id)}`),
  updateOrganization: (input: { name?: string; slug?: string }) => request<Account>("PATCH", "/api/v1/organization", input),

  listInvitations: () => request<Invitation[]>("GET", "/api/v1/invitations"),
  createInvitation: (input: { email?: string; github_login?: string; roles: { role: string; project?: string }[]; ttl?: string }) =>
    request<CreatedInvitation>("POST", "/api/v1/invitations", input),
  revokeInvitation: (id: string) => request<Invitation>("POST", `/api/v1/invitations/${seg(id)}/revoke`),

  listTeams: () => request<Team[]>("GET", "/api/v1/teams"),
  createTeam: (input: { name: string; slug?: string }) => request<Team>("POST", "/api/v1/teams", input),
  deleteTeam: (slug: string) => request<Team>("DELETE", `/api/v1/teams/${seg(slug)}`),
  listTeamMembers: (slug: string) => request<TeamMember[]>("GET", `/api/v1/teams/${seg(slug)}/members`),
  addTeamMember: (slug: string, user: string) => request<TeamMember>("POST", `/api/v1/teams/${seg(slug)}/members`, { user }),
  removeTeamMember: (slug: string, user: string) => request<TeamMember>("DELETE", `/api/v1/teams/${seg(slug)}/members/${seg(user)}`),

  listRoles: () => request<Role[]>("GET", "/api/v1/roles"),
  listBindings: (f: { project?: string; subject_kind?: string; subject?: string } = {}) =>
    request<RoleBinding[]>("GET", `/api/v1/role-bindings${query(f)}`),
  createBinding: (input: { subject_kind: string; subject: string; role: string; project?: string }) =>
    request<RoleBinding>("POST", "/api/v1/role-bindings", input),
  deleteBinding: (id: string) => request<RoleBinding>("DELETE", `/api/v1/role-bindings/${seg(id)}`),

  listServiceAccounts: () => request<ServiceAccount[]>("GET", "/api/v1/service-accounts"),
  createServiceAccount: (input: { name: string; description?: string }) => request<ServiceAccount>("POST", "/api/v1/service-accounts", input),
  setServiceAccountDisabled: (name: string, disabled: boolean) =>
    request<ServiceAccount>("POST", `/api/v1/service-accounts/${seg(name)}/${disabled ? "disable" : "enable"}`),

  /** A plan of changes made from a form; it is applied with applyPlan once its secure fields are filled. */
  planConfig: (title: string, changes: { kind: string; name: string; spec: unknown }[]) =>
    request<ConfigPlan>("POST", "/api/v1/config/plans", { title, changes, source: "form" }),
  /** Saves a secure field of a plan; the value goes to the secret store and is never read back. */
  putPlanSecret: (plan: string, secret: string, value: string) =>
    request<ConfigPlan>("PUT", `/api/v1/config/plans/${seg(plan)}/secrets/${seg(secret)}`, { value }),
  applyPlan: (plan: string, word?: string) => request<{ plan: ConfigPlan }>("POST", `/api/v1/config/plans/${seg(plan)}/apply`, word ? { word } : {}),
  cancelPlan: (plan: string) => request<ConfigPlan>("POST", `/api/v1/config/plans/${seg(plan)}/cancel`),

  listConnections: () => request<Connection[]>("GET", "/api/v1/connections"),
  createConnection: (input: CreateConnectionInput) => request<Connection>("POST", "/api/v1/connections", input),
  setConnectionEnabled: (name: string, enabled: boolean) =>
    request<Connection>("POST", `/api/v1/connections/${seg(name)}/${enabled ? "enable" : "disable"}`),

  listAudit: (f: AuditFilter & { limit?: number } = {}) => request<AuditEntry[]>("GET", `/api/v1/audit${query({ ...f })}`),
  listConnectedApps: () => request<ConnectedApp[]>("GET", "/api/v1/connected-apps"),
  revokeConnectedApp: (id: string) => request<ConnectedApp>("POST", `/api/v1/connected-apps/${seg(id)}/revoke`),

  listCouncils: (f: { project: string; status?: CouncilStatus | "" }) => request<Council[]>("GET", `/api/v1/councils${query(f)}`),
  getCouncil: (ref: string) => request<CouncilDetail>("GET", `/api/v1/councils/${seg(ref)}`),
  startCouncil: (input: StartCouncilInput) => request<Council>("POST", "/api/v1/councils", input),
  addConstraint: (ref: string, constraint: string) =>
    request<Council>("POST", `/api/v1/councils/${seg(ref)}/constraints`, { constraint }),

  listAssumptions: (f: { project: string; goal?: string; status?: AssumptionStatus | ""; impact?: Level | "" }) =>
    request<Assumption[]>("GET", `/api/v1/assumptions${query(f)}`),
  resolveAssumption: (ref: string, status: AssumptionStatus, evidence?: string) =>
    request<Assumption>("POST", `/api/v1/assumptions/${seg(ref)}/resolve`, evidence ? { status, evidence } : { status }),
  listDecisions: (f: { project: string }) => request<Decision[]>("GET", `/api/v1/decisions${query(f)}`),

  getTrace: (ref: string) => request<Trace>("GET", `/api/v1/trace${query({ ref })}`),

  listKnowledge: (f: { project: string; section?: string; kind?: KnowledgeKind | ""; provider?: string; goal?: string; limit?: number }) =>
    request<KnowledgeListing>("GET", `/api/v1/knowledge/entries${query(f)}`),
  searchKnowledge: (f: { project: string; q: string; section?: string; kind?: KnowledgeKind | ""; provider?: string }) =>
    request<KnowledgeResult[]>("GET", `/api/v1/knowledge${query({ ...f, limit: PAGE })}`),
  listKnowledgeProposals: (f: { project: string; status?: KnowledgeProposalStatus | "" }) =>
    request<KnowledgeProposal[]>("GET", `/api/v1/knowledge/proposals${query(f)}`),
  proposeKnowledge: (input: ProposeKnowledgeInput) => request<ProposeKnowledgeResult>("POST", "/api/v1/knowledge/proposals", input),
  deleteNote: (id: string) => request<{ note: string; message: string }>("DELETE", `/api/v1/knowledge/notes/${seg(id)}`),

  listGates: (f: { project?: string } = {}) => request<GateInfo[]>("GET", `/api/v1/gates${query(f)}`),
  approve: (ref: string, feedback?: string) =>
    request<Gate>("POST", `/api/v1/gates/${seg(ref)}/approve`, feedback ? { feedback } : {}),
  requestChanges: (ref: string, feedback: string) =>
    request<Gate>("POST", `/api/v1/gates/${seg(ref)}/request-changes`, { feedback }),
  reject: (ref: string, feedback?: string) =>
    request<Gate>("POST", `/api/v1/gates/${seg(ref)}/reject`, feedback ? { feedback } : {}),
}
