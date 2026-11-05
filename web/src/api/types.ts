// Types of the JSON in docs/api.md (the structs of internal/domain and
// internal/mcp). Fields the UI does not read are left out.

export type GoalStatus = "draft" | "spec_review" | "planned" | "in_progress" | "done" | "canceled"
export type TaskStatus = "open" | "in_progress" | "in_review" | "merge_ready" | "merged" | "closed"
export type JobStatus = "queued" | "running" | "succeeded" | "failed" | "canceled"
export type GateKind =
  | "profile_approval" | "spec_approval" | "merge_approval" | "test_plan_approval" | "needs_you"
  | "assumption_review" | "council_decision" | "knowledge_proposal" | "access_request" | "improvement_proposal"
/** What opened a gate: a playbook step, or a job asking a human (`ask_human`). Absent means a playbook. */
export type GateOrigin = "playbook" | "ask_human"
export type GateStatus = "pending" | "approved" | "changes_requested" | "rejected"
export type ProjectStatus = "registered" | "onboarding" | "onboarded"
export type ChecksStatus = "failure" | "pending" | "success" | "none" | "unknown"

export const GOAL_STATUSES: GoalStatus[] = ["draft", "spec_review", "planned", "in_progress", "done", "canceled"]
export const JOB_STATUSES: JobStatus[] = ["queued", "running", "succeeded", "failed", "canceled"]

/** An organization (stored as an account). */
export interface Account {
  id: string
  name: string
  slug: string
  created_by?: string
  max_token_ttl?: string
  created_at: string
}

/** Answer of POST /session and GET /me. */
export interface Session {
  token: string
  account: Account
}

export interface Goal {
  id: string
  project_id: string
  /** GOAL-<number> */
  number: number
  title: string
  body: string
  platforms?: string[]
  status: GoalStatus
  issue_number?: number
  created_by: string
  created_at: string
  updated_at: string
}

export interface Spec {
  id: string
  number: number
  revision: number
  /** Markdown. */
  body: string
  created_at: string
}

export interface Change {
  number: number
  url: string
  state: "open" | "merged" | "closed"
  checks?: ChecksStatus
}

export interface Task {
  id: string
  ref: string
  number: number
  seq: number
  title: string
  body: string
  status: TaskStatus
  url?: string
  /** Refs of the pending gates waiting for a human. */
  needs: string[]
  changes: Change[]
  iterations: number
  cost_usd: number
}

export interface Gate {
  id: string
  number: number
  kind: GateKind
  origin?: GateOrigin
  status: GateStatus
  title: string
  /** Markdown shown to the reviewer. */
  summary: string
  resolved_by?: string
  feedback?: string
  resolved_at?: string
  created_at: string
}

/** A gate with where to find what it is about (list_pending_gates, goal gates). */
export interface GateInfo extends Gate {
  ref: string
  project: string
  goal_ref?: string
  task_ref?: string
  url?: string
}

export interface GoalDetail {
  goal: Goal
  ref: string
  project: string
  url?: string
  spec?: Spec
  tasks: Task[]
  gates: GateInfo[]
  cost_usd: number
}

export interface ProfileComponent {
  path: string
  stack: string
  build?: string
  test?: string
  platforms?: string[]
  needs?: string[]
}

/** The approved .forecrew/profile.yaml (docs/contracts/profile.schema.json). */
export interface Profile {
  version: number
  name: string
  summary: string
  languages?: string[]
  components?: ProfileComponent[]
  platforms?: string[]
  open_questions?: string[]
}

export interface Project {
  id: string
  name: string
  repo: string
  default_branch: string
  status: ProjectStatus
  profile?: Profile
  created_at: string
  updated_at: string
}

export interface OnboardResult {
  project: Project
  run_id: string
}

export interface JobSpec {
  repo: string
  base_ref: string
  branch: string
  role: string
  runtime: string
  model: string
  prompt: string
  timeout: string
}

export interface Usage {
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
}

export interface Job {
  id: string
  project_id: string
  /** JOB-<number> */
  number: number
  status: JobStatus
  role: string
  runtime: string
  needs: string[]
  goal_id?: string
  task_id?: string
  spec: JobSpec
  attempts: number
  max_attempts: number
  /** Set while the job runs. */
  worker_id?: string
  /** A released job waits until then before it is leased again. */
  run_after?: string
  /** Set while the job runs. */
  lease_expires_at?: string
  started_at?: string
  finished_at?: string
  /** Structured output of a finished job. */
  output?: unknown
  /** The commit pushed to the job's branch, if any. */
  head_sha?: string
  error?: string
  usage: Usage
  cost_usd: number
  created_at: string
  updated_at: string
}

/** The answer of job_status. */
export interface JobInfo extends Job {
  ref: string
  reason?: string
  worker_name?: string
}

export type JobEventType = "status" | "log" | "message" | "tool_call" | "tool_result" | "usage" | "error"

export interface JobEvent {
  seq: number
  time: string
  type: JobEventType
  text?: string
  data?: unknown
}

export interface JobLogs {
  events: JobEvent[]
  next_seq: number
}

/** An API and MCP token, without any secret. */
export interface Token {
  id: string
  name: string
  created_at: string
  last_used_at?: string
  revoked_at?: string
  /** The token of this request. */
  current?: boolean
}

/** The answer of revoke_token. */
export interface RevokeResult {
  token: Token
  /** The caller revoked the token it used: the next request gets 401. */
  self: boolean
  message: string
}

export interface Worker {
  id: string
  name: string
  /** Pool the worker leases jobs from, set by an admin; empty means build. */
  pool: string
  /** The worker of a fleet machine; false on every static worker. */
  ephemeral: boolean
  labels: string[]
  version: string
  max_jobs: number
  last_seen_at?: string
  online: boolean
  running_jobs: string[]
}

export interface OnboardInput {
  repo: string
  name?: string
  default_branch?: string
}

export interface CreateGoalInput {
  project: string
  title: string
  body: string
  platforms?: string[]
}

export interface DispatchJobInput {
  project: string
  role: string
  prompt: string
  needs?: string[]
  goal?: string
  task?: string
  base_ref?: string
  branch?: string
  /** Worker pool; defaults to the role's pool. */
  pool?: string
  feedback?: string
  include_repo_facts?: boolean
}

export type CouncilStatus = "running" | "decided" | "failed" | "canceled"
export type CouncilStage = "proposal" | "critique" | "revision" | "synthesis"
export type Level = "low" | "medium" | "high"
export type AssumptionStatus = "unverified" | "verified" | "invalidated" | "accepted"

export const COUNCIL_STAGES: CouncilStage[] = ["proposal", "critique", "revision", "synthesis"]
export const COUNCIL_KINDS = ["adhoc", "product", "technical", "test_design", "incident", "onboarding"] as const

export interface BriefOption {
  name: string
  summary: string
  pros: string[]
  cons: string[]
  cost: Level
  risk: Level
}

export interface BriefAssumption {
  statement: string
  confidence: Level
  impact: Level
  verify_by: "test" | "ask-human" | "research"
}

/** The decision brief a council ends with. Agent text: show it as text. */
export interface CouncilBrief {
  question: string
  options: BriefOption[]
  recommendation: string
  votes: { member: string; option: string; reason: string }[]
  dissent: { member: string; position: string }[]
  assumptions: BriefAssumption[]
  open_questions: string[]
  reversible: boolean
  budget_exhausted: boolean
}

export interface SourceCheck {
  source: string
  kind: "path" | "kb" | "url" | "job"
  status: "ok" | "missing" | "unreachable" | "refused"
  detail?: string
}

/** One member's output at one stage. `content` is agent JSON of any shape: read it defensively. */
export interface Contribution {
  id: string
  /** role@runtime */
  member: string
  role: string
  runtime: string
  stage: CouncilStage
  round: number
  content: unknown
  evidence_check: SourceCheck[] | null
  created_at: string
}

export interface Council {
  id: string
  project_id: string
  goal_id?: string
  /** COUNCIL-<number> */
  ref: string
  number: number
  kind: string
  question: string
  constraints: string[] | null
  status: CouncilStatus
  brief?: CouncilBrief
  run_id?: string
  created_at: string
  updated_at: string
}

export interface Decision {
  id: string
  project_id: string
  /** DEC-<number> */
  ref: string
  number: number
  goal_id?: string
  question: string
  options: { name: string; summary?: string }[] | null
  choice: string
  reasons: string
  dissent?: string
  /** user:ana, github:ana or policy. */
  decided_by: string
  council_id?: string
  gate_id?: string
  /** Refs of the assumptions it rests on. */
  assumptions: string[]
  created_at: string
}

export interface CouncilDetail extends Council {
  contributions: Contribution[]
  gate?: GateInfo
  /** The decision that ended it, as stored: it has no ref or assumption refs. */
  decision?: Omit<Decision, "ref" | "assumptions">
}

export interface Assumption {
  id: string
  /** A-<number> */
  ref: string
  number: number
  made_by: string
  statement: string
  confidence: Level
  impact: Level
  verify_by: BriefAssumption["verify_by"]
  status: AssumptionStatus
  evidence?: string
  /** The job (JOB-41), else the council's ID, else the spec it came from. */
  source?: string
  dependents: { kind: string; id: string }[]
  created_at: string
}

export interface StartCouncilInput {
  project: string
  question: string
  kind?: string
  constraints?: string[]
}

export interface ErrorBody {
  error: { code: string; message: string }
}
