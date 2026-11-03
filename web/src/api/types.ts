// Types of the JSON in docs/api.md (the structs of internal/domain and
// internal/mcp). Fields the UI does not read are left out.

export type GoalStatus = "draft" | "spec_review" | "planned" | "in_progress" | "done" | "canceled"
export type TaskStatus = "open" | "in_progress" | "in_review" | "merge_ready" | "merged" | "closed"
export type JobStatus = "queued" | "running" | "succeeded" | "failed" | "canceled"
export type GateKind = "profile_approval" | "spec_approval" | "merge_approval" | "test_plan_approval" | "needs_you"
export type GateStatus = "pending" | "approved" | "changes_requested" | "rejected"
export type ProjectStatus = "registered" | "onboarding" | "onboarded"
export type ChecksStatus = "failure" | "pending" | "success" | "none" | "unknown"

export const GOAL_STATUSES: GoalStatus[] = ["draft", "spec_review", "planned", "in_progress", "done", "canceled"]
export const JOB_STATUSES: JobStatus[] = ["queued", "running", "succeeded", "failed", "canceled"]

export interface Account {
  id: string
  name: string
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

export interface ErrorBody {
  error: { code: string; message: string }
}
