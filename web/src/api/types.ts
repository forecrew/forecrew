// Types of the JSON in docs/api.md (the structs of internal/domain and
// internal/mcp). Fields the UI does not read are left out.

export type GoalStatus = "draft" | "spec_review" | "planned" | "in_progress" | "done" | "canceled"
export type TaskStatus = "open" | "in_progress" | "in_review" | "merge_ready" | "merged" | "closed"
export type JobStatus = "queued" | "running" | "succeeded" | "failed" | "canceled"
export type GateKind =
  | "profile_approval" | "spec_approval" | "merge_approval" | "test_plan_approval" | "needs_you"
  | "assumption_review" | "council_decision" | "knowledge_proposal" | "access_request" | "improvement_proposal"
  | "ticket_intake"
/** What opened a gate: a playbook step, or a job asking a human (`ask_human`). Absent means a playbook. */
export type GateOrigin = "playbook" | "ask_human"
export type GateStatus = "pending" | "approved" | "changes_requested" | "rejected"
export type ProjectStatus = "registered" | "onboarding" | "onboarded"
export type ChecksStatus = "failure" | "pending" | "success" | "none" | "unknown"

export type VerificationStatus = "passed" | "failed" | "incomplete"
export type TestRunStatus = "passed" | "failed" | "errored"

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

/** A permission of docs/contracts/permissions.md, such as "member.manage". */
export type Permission = string

/** A role the caller holds, on the organization or one project. */
export interface HeldRole {
  role: string
  scope: "organization" | "project"
  project_id?: string
  /** Set when the role comes through a team. */
  team_id?: string
}

/** Answer of POST /session and GET /me. */
export interface Session {
  /** Name of the token, when the caller uses one. */
  token?: string
  user?: { id: string; name: string; email?: string }
  /** The current organization; a session may not have picked one yet. */
  account?: Account
  /**
   * The organizations the session can switch to. A session from an
   * organization's own connection has only that one.
   */
  organizations?: { id: string; name: string; slug: string }[]
  roles?: HeldRole[]
  /** Held on the organization and, for project-scoped ones, on every project. */
  permissions?: Permission[]
  /** By project ID: the project-scoped permissions on each project where the caller has any. */
  projects?: Record<string, Permission[]>
  /** True in the enterprise build: custom roles can be edited. */
  enterprise?: boolean
}

/** A way to sign in, from POST /auth/discover. */
export interface SignInConnection {
  id: string
  kind: string
  name: string
  login_url: string
}

/** Answer of POST /auth/discover. */
export interface Discovery {
  connections: SignInConnection[]
  redirect?: string
  token_login: boolean
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

/** The verification of a task's newest pull request that has a report (get_goal). */
export interface VerificationRef {
  status: VerificationStatus
  sha: string
  /** The report is for the pull request's head commit. */
  current: boolean
  url: string
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
  verification?: VerificationRef
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
  /** Whether the caller may resolve the gate; the buttons are left out when not. */
  can_resolve?: boolean
  /** The improvement proposal of an improvement_proposal gate. */
  proposal?: Proposal
}

export type ProposalStatus = "proposed" | "accepted" | "rejected" | "snoozed" | "done" | "superseded"
export type ProposalCategory = "observability" | "testing" | "ci" | "security" | "docs" | "knowledge" | "tooling" | "workers" | "roles" | "cost"
export type ProposalDecision = "accept" | "reject" | "snooze" | "done"

export const PROPOSAL_STATUSES: ProposalStatus[] = ["proposed", "accepted", "rejected", "snoozed", "done", "superseded"]
export const PROPOSAL_CATEGORIES: ProposalCategory[] = ["observability", "testing", "ci", "security", "docs", "knowledge", "tooling", "workers", "roles", "cost"]

/** What accepting a proposal does: exactly one field is set. */
export interface ProposalAction {
  goal?: { title: string; body: string; platforms?: string[] }
  config_pr?: { files: { path: string; content: string }[] }
  setup?: { instructions: string; issue?: string }
}

/** An improvement proposal. Its text is the advisor's and what agents and repos wrote: show it as text. */
export interface Proposal {
  id: string
  project_id: string
  ref: string
  scope: "project" | "platform"
  category: ProposalCategory
  title: string
  rationale: string
  evidence: { kind: string; ref: string; detail: string }[]
  benefit: string
  effort: "s" | "m" | "l"
  risk: Level
  action: ProposalAction
  measure?: { detector: string; value: number }
  status: ProposalStatus
  snoozed_until?: string
  reason?: string
  outcome?: { before: number; after: number; measured_at: string }
  gate_ref?: string
  goal_ref?: string
  can_decide: boolean
  created_at: string
}

export interface GoalTicket {
  key: string
  url?: string
  connection: string
  status: string
  /** The last logical state written to the ticket. */
  state?: string
}

export interface GoalDetail {
  goal: Goal
  ref: string
  project: string
  url?: string
  ticket?: GoalTicket
  spec?: Spec
  tasks: Task[]
  gates: GateInfo[]
  assumptions?: Assumption[]
  cost_usd: number
}

export const LEVELS: Level[] = ["low", "medium", "high"]
export const ASSUMPTION_STATUSES: AssumptionStatus[] = ["unverified", "verified", "invalidated", "accepted"]

export type TraceKind = "goal" | "council" | "spec" | "decision" | "task" | "change" | "verification" | "merge" | "assumption"

/** One step of a trace (get_trace). Title, summary and attrs are agent text. */
export interface TraceNode {
  id: string
  kind: TraceKind
  title: string
  status?: string
  summary?: string
  url?: string
  at: string
  attrs?: Record<string, string>
}

export interface TraceEdge {
  from: string
  to: string
  label?: string
}

export interface Trace {
  /** The node asked for. */
  focus: string
  /** The goal the chain belongs to; absent for a record of no goal. */
  goal?: string
  nodes: TraceNode[]
  edges: TraceEdge[]
}

export type KnowledgeKind = "entry" | "note" | "generated"

export interface Provenance {
  author_role: string
  source: string
  created: string
  last_verified?: string
}

/** A search hit or a listed entry (search_knowledge, list_knowledge). Text is untrusted where `untrusted` says so. */
export interface KnowledgeResult {
  id: string
  kind: KnowledgeKind
  provider: string
  section: string
  path: string
  title: string
  source_url?: string
  snippet: string
  untrusted: boolean
  provenance: Provenance
  needs_reverify: boolean
  reverify_reason?: string
  scope?: "goal" | "project"
  goal_id?: string
  tags?: string[]
  expires_at?: string
}

export interface KnowledgeEntry extends KnowledgeResult {
  /** The whole Markdown. */
  body: string
}

export interface KnowledgeSection {
  name: string
  provider?: string
  provider_kind?: string
  instance?: string
  /** Repo globs, or the provider's locations. */
  paths: string[]
  owner?: string
  protected: boolean
}

export interface KnowledgeListing {
  sections: KnowledgeSection[]
  entries: KnowledgeEntry[]
}

export type KnowledgeAction = "add" | "edit" | "retire"
export type KnowledgeProposalStatus = "queued" | "open" | "accepted" | "rejected"

export interface KnowledgeProposal {
  id: string
  /** KB-<number> */
  ref: string
  number: number
  section: string
  path: string
  action: KnowledgeAction
  body: string
  reason: string
  proposed_by: string
  status: KnowledgeProposalStatus
  goal_ref?: string
  /** The pull request of a proposal to the repo provider. */
  url?: string
  /** The knowledge_proposal gate a human decides. */
  gate?: GateInfo
  created_at: string
}

export interface ProposeKnowledgeInput {
  project: string
  section: string
  path: string
  body: string
  reason: string
  action?: KnowledgeAction
}

export interface ProposeKnowledgeResult {
  proposal: string
  status: KnowledgeProposalStatus
  number: number
  url: string
}

export type IntegrationStatus = "proposed" | "approved" | "connected"

/** A tool the team uses outside the repo, from the approved profile. */
export interface Integration {
  tool: string
  instance?: string
  uses: string[]
  confidence: Level
  status: IntegrationStatus
  mapping?: Record<string, string[]>
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
  integrations?: Integration[]
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
  /** Absent or "agent" for an agent job. */
  kind?: "agent" | "command"
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
  /** The test runs of a command job's latest attempt added up. */
  tests?: TestSummary
  reason?: string
  /** Why a queued job waits, as a code to act on (the sentence is reason). */
  reason_code?: WaitReasonCode
  worker_name?: string
}

export interface TestCounts {
  passed: number
  failed: number
  skipped: number
  errored: number
}

export interface TestSummary extends TestCounts {
  runs: number
  status: TestRunStatus
}

/** A file a job produced (list_artifacts). */
export interface Artifact {
  id: string
  name: string
  content_type: string
  size: number
  sha256: string
  created_at: string
}

/** A failed or errored test case. Names and messages are written by the repo's tests: untrusted text. */
export interface TestFailure {
  name: string
  classname: string
  status: "failed" | "errored"
  message?: string
}

/** The latest attempt of one suite in one matrix cell of an environment (project_quality). */
export interface QualityCell {
  /** Empty for the worker's host. */
  environment: string
  cell: Record<string, string>
  suite: string
  status: TestRunStatus
  counts: TestCounts
  /** JOB-41 */
  job: string
  failures: TestFailure[]
  failures_truncated?: boolean
}

export interface QualityChange {
  number: number
  url: string
  branch: string
  sha: string
  task?: string
  cells: QualityCell[]
  verification?: VerificationInfo
}

export interface QualityInfo {
  project: string
  default_branch: string
  baseline?: { sha: string; cells: QualityCell[] }
  changes: QualityChange[]
}

export interface VerificationResult {
  environment: string
  cell: Record<string, string>
  suite: string
  required: boolean
  status: TestRunStatus | "missing"
  counts: TestCounts
  job?: string
}

export interface VerificationCriterion {
  status: "passed" | "failed" | "untested"
  /** Test -> its worst result. */
  tests: Record<string, string>
  untestable?: string
}

/** docs/contracts/verification.schema.json. */
export interface VerificationReport {
  version: number
  sha: string
  status: VerificationStatus
  results: VerificationResult[]
  criteria: Record<string, VerificationCriterion>
  new_tests: Record<string, boolean>
  flaky: { test: string; environments: string[] }[]
  environments: Record<string, { versions: Record<string, string> }>
  blocking: string[]
}

/** The answer of verification_report. */
export interface VerificationInfo {
  id: string
  change: number
  url: string
  sha: string
  status: VerificationStatus
  /** The report is for the pull request's head commit; otherwise it is stale. */
  current: boolean
  report: VerificationReport
  /** The rendered Markdown, when stored. */
  artifact_id?: string
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
  owner_kind?: "user" | "service_account"
  owner_id?: string
  /** read, write or admin; absent on a token from before scopes. */
  scopes?: Scope[]
  expires_at?: string
}

export type Scope = "read" | "write" | "admin"

/** Answer of POST /tokens: the secret is shown once. */
export interface CreatedToken {
  token: Token
  secret: string
}

/** An OAuth client the user authorized in this organization. */
export interface ConnectedApp {
  id: string
  name: string
  client: string
  scopes: string[]
  refreshed_at: string
  /** The app of this request. */
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
  project_id: string
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
  resolved_by?: string
  resolved_at?: string
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

export type MembershipStatus = "active" | "invited" | "suspended"
export type Source = "manual" | "invitation" | "jit" | "scim" | "idp"

/** A role a member holds directly. */
export interface MemberRole {
  role: string
  scope: "organization" | "project"
  /** Empty for a role on the organization. */
  project?: string
}

/** A member of the organization (GET /members). */
export interface Member {
  user_id: string
  name: string
  /** Only shown with member.manage. */
  email?: string
  status: MembershipStatus
  source: Source
  disabled?: boolean
  roles: MemberRole[]
  /** Team slugs. */
  teams: string[]
  last_seen_at?: string
  created_at: string
}

export interface Team {
  id: string
  name: string
  slug: string
  source: Source
  created_at: string
}

export interface TeamMember {
  team_id: string
  user_id: string
  source: Source
  created_at: string
}

export interface Role {
  id?: string
  name: string
  description?: string
  scope: "organization" | "project"
  permissions: Permission[]
  built_in: boolean
  custom: boolean
}

export interface RoleBinding {
  id: string
  subject_kind: "user" | "team" | "service_account"
  subject_id: string
  subject_name?: string
  role: string
  role_scope: "organization" | "project"
  /** Empty: the organization. */
  project?: string
  created_at: string
}

export interface ServiceAccount {
  id: string
  name: string
  description?: string
  disabled_at?: string
  created_at: string
}

export interface Invitation {
  id: string
  email?: string
  github_login?: string
  expires_at: string
  accepted_at?: string
  revoked_at?: string
  created_at: string
}

export interface CreatedInvitation {
  invitation: Invitation
  /** The link, shown once. */
  url: string
}

export type ConnectionKind = "github" | "google" | "microsoft" | "oidc"

/** A way to sign in (GET /connections). */
export interface Connection {
  id: string
  kind: ConnectionKind
  name: string
  issuer?: string
  /** Absent on an instance connection. */
  client_id?: string
  client_secret_ref?: string
  enabled: boolean
  /** From the server file: not changed here. */
  instance: boolean
  /** A test sign-in worked. */
  tested: boolean
  redirect_url: string
  test_url?: string
  created_at: string
}

export interface CreateConnectionInput {
  kind: ConnectionKind
  name: string
  issuer?: string
  client_id: string
  client_secret_ref: string
}

/** An entry of the audit log (GET /audit). */
export interface AuditEntry {
  id: string
  actor: string
  actor_name?: string
  credential?: string
  action: string
  subject_type: string
  subject_id?: string
  project_id?: string
  /** What the action recorded; text, never markup. */
  data?: Record<string, unknown>
  created_at: string
}

export interface AuditFilter {
  actor?: string
  action?: string
  subject_type?: string
  since?: string
  until?: string
}

/** A browser session of the signed-in person (GET /sessions). */
export interface BrowserSession {
  id: string
  created_at: string
  last_seen_at: string
  expires_at: string
  user_agent?: string
  ip?: string
  organization?: string
  connection?: string
  current: boolean
}

/** A sign-in linked to the signed-in person (GET /identities). */
export interface Identity {
  id: string
  connection: string
  kind: ConnectionKind
  login?: string
  email?: string
  last_used_at?: string
  created_at: string
}

/** The answer of POST /cli/authorize. */
export interface CliAuthorization {
  redirect: string
  code: string
}

/** A step of a change plan. Only the server writes steps; a secret_required one is a secure field. */
export interface ConfigStep {
  kind: "secret_required" | "connect"
  resource: string
  title: string
  detail?: string
  /** The name in the secret store of a secret_required step. */
  secret?: string
  /** The secret was saved before this plan: confirming uses it and takes no input. */
  reuses?: boolean
  filled: boolean
}

export type ConfigPlanStatus = "pending" | "awaiting_approval" | "applied" | "canceled" | "reverted" | "expired"
export type ConfigPlanSource = "form" | "assistant" | "yaml" | "cli" | "mcp"

/** One field of a change, as text for people. */
export interface ConfigFieldChange {
  path: string
  old?: string
  new?: string
}

/** One resource change of a plan; line is the change in one readable line, with its sign. */
export interface ConfigChange {
  op: "create" | "update" | "delete"
  kind: string
  name: string
  old?: unknown
  new?: unknown
  fields: ConfigFieldChange[]
  line: string
}

/** A change plan (docs/api.md, config), as the server shows it. */
export interface ConfigPlan {
  id: string
  status: ConfigPlanStatus
  source: ConfigPlanSource
  /** The MCP client that sent it. */
  client?: string
  title: string
  /** The sentence that asked for it. */
  request?: string
  /** "now", or "pr:owner/name" for repo files. */
  applies: string
  changes: ConfigChange[]
  steps: ConfigStep[]
  needs: Permission[]
  affects: string
  cost: string
  undo: string
  reversible: boolean
  lowers_protection: string[]
  /** Typed to confirm a plan that lowers protection. */
  confirm_word?: string
  /** Text the plan draws on that nobody verified. */
  untrusted?: string
  created_by: string
  conversation_id?: string
  reverts?: string
  expires_at: string
  applied_by?: string
  applied_at?: string
  pr_url?: string
  created_at: string
  updated_at: string
  can_confirm: boolean
  /** The configuration moved since the plan was made: it has to be rebuilt. */
  stale: boolean
}

/** A value Forecrew generated, shown once and stored nowhere. */
export interface ShowOnceValue {
  label: string
  value: string
}

/** The answer of applying a plan. */
export interface ConfigApplied {
  plan: ConfigPlan
  show_once: ShowOnceValue[]
}

/** The answer of asking for approval of a plan. */
export interface ConfigConfirmation {
  status: string
  plan: ConfigPlan
  confirm_url?: string
  message: string
}

/** A setting: its kind, name and spec (docs/contracts/config-resources.md). */
export interface ConfigResource {
  kind: string
  name: string
  spec: Record<string, unknown>
  scope?: string
  origin?: "file" | "database"
}

/** A change someone asks for: a spec, or delete. */
export interface ConfigWanted {
  kind: string
  name: string
  spec?: unknown
  delete?: boolean
}

export interface AssistantMessage {
  id: string
  conversation_id: string
  author: "user" | "assistant"
  body: string
  /** The job of the turn a person's message started; Stop cancels it. */
  job_id?: string
  created_at: string
}

export interface AssistantConversation {
  id: string
  title: string
  created_at: string
  updated_at: string
}

/** A conversation with its messages and the plans its turns made. */
export interface AssistantView extends AssistantConversation {
  messages: AssistantMessage[]
  plans: ConfigPlan[]
  /** A turn's job is still running. */
  thinking: boolean
}

export type LocationStatus = "online" | "offline" | "paused" | "pending" | "revoked"

/** A row of the Machines overview: a location, or a computer that runs jobs directly. */
export interface FleetLocation {
  id: string
  kind: "location" | "computer"
  name: string
  runs: string
  status: LocationStatus
  in_use: number
  /** How many it can run at once; 0 is no limit. */
  capacity: number
  cost: string
  /** What a computer could not do, in words: an agent it failed to install, with what to do. */
  problems?: string[]
}

export type WaitReasonCode =
  | "booting"
  | "queue"
  | "location_offline"
  | "no_machine_type"
  | "machine_limit"
  | "budget"
  | "policy"
  | "trust_zone"
  | "agent_install"
  | "no_connection"
  | "private_code"

export interface FleetWaitingJob {
  ref: string
  role?: string
  code: WaitReasonCode
  sentence: string
}

export interface FleetRunningJob {
  ref: string
  role?: string
  machine: string
  location: string
  minutes: number
}

export type FleetRouting = "own_only" | "own_first" | "hosted"

/** The Machines page (GET /fleet): sentences are written by the server. */
export interface FleetOverview {
  headline: string
  working: number
  waiting: number
  spend?: { amount: number; limit?: number; currency: string }
  routing: FleetRouting
  locations: FleetLocation[]
  queue: FleetWaitingJob[]
  running: FleetRunningJob[]
}

export interface ProviderCheck {
  name: string
  status: "ok" | "warn" | "fail"
  detail?: string
}

/** What a fleet agent said about its host: unchecked, so shown as plain text. */
export interface ProviderCapabilities {
  os?: string[]
  arch?: string[]
  max_cpu?: number
  max_memory_mb?: number
  max_disk_gb?: number
  max_machines?: number
  checks?: ProviderCheck[]
  host?: string
  cloud_account?: string
  region?: string
  agent_version?: string
}

export interface Provider {
  id: string
  name: string
  kind: string
  mode: string
  status: "pending" | "active" | "paused" | "revoked"
  last_seen_at?: string
  capabilities: ProviderCapabilities
  limits: { max_machines?: number; monthly_budget?: number }
  network_allow: string[]
  tags: Record<string, string>
  created_at: string
  online: boolean
  ownership: "own" | "hosted"
}

export interface Shape {
  id: string
  provider_id: string
  name: string
  os: string
  arch: string
  cpu: number
  memory_mb: number
  disk_gb: number
  gpu: number
  /** The pool of its machines: its trust zone. */
  pool: string
  labels: string[]
  image: string
  price_per_minute: number
  monthly_budget?: number
  warm_min: number
  max: number
  idle_seconds: number
  note: string
  enabled: boolean
  provider: string
  ownership: "own" | "hosted"
}

export interface Machine {
  id: string
  name: string
  shape: string
  provider: string
  state: "requested" | "booting" | "ready" | "busy" | "draining" | "destroyed" | "failed"
  job_ref?: string
  error?: string
  created_at: string
}

export type JoinStatus = "waiting" | "claimed" | "confirmed" | "used" | "expired"

/** A join code without its secrets; facts are what the machine said, unchecked. */
export interface JoinCode {
  id: string
  status: JoinStatus
  kind?: string
  facts: ProviderCapabilities
  facts_hash?: string
  expires_at: string
  created_at: string
}

export interface CreatedJoinCode {
  join_code: JoinCode
  code: string
}

/** A signed image manifest (docs/images.md), registered by a person. */
export interface ImageManifest {
  name: string
  version: string
  os: string
  arch: string
  format: string
  uri: string
  digest: string
  signature: string
  built_at: string
  contents: Record<string, unknown>
}

// Trackers (docs/contracts/tracker.md).

export type TicketState = "queued" | "in_progress" | "in_review" | "done" | "canceled"
export const TICKET_STATES: TicketState[] = ["queued", "in_progress", "in_review", "done", "canceled"]
export type TicketLinkStatus = "waiting" | "queued" | "active" | "done" | "canceled" | "dismissed"
export type TicketWaitReason = "no_route" | "many_projects" | "unknown_person" | "not_permitted"

export interface TrackerTriggers {
  assignee?: string
  label?: string
  state?: string
}

/** A connection to a tracker; config never holds a secret, only store: references. */
export interface TrackerConnection {
  id: string
  name: string
  kind: string
  config: Record<string, unknown>
  triggers: TrackerTriggers
  created_at: string
  updated_at: string
}

export interface TrackerMatch {
  project?: string
  types?: string[]
  labels?: string[]
  components?: string[]
}

/** A route with the names of what it joins. warning is the tracker's own words: untrusted. */
export interface TrackerRoute {
  id: string
  connection_id: string
  position: number
  match: TrackerMatch
  project_id: string
  goal_kind?: string
  playbook?: string
  states: Partial<Record<TicketState, string>> | null
  warning?: string
  connection: string
  project: string
}

export interface TrackerIdentity {
  id: string
  connection_id: string
  external_id: string
  external_name?: string
  user_id: string
  created_at: string
}

export interface TrackerCheck {
  name: string
  ok: boolean
  detail?: string
}

export interface TrackerState {
  name: string
  closed?: boolean
}

export interface TrackerTest {
  connection: string
  ok: boolean
  checks: TrackerCheck[]
  states?: TrackerState[]
}

/** A ticket Forecrew was handed. title, handed_by and key are ticket text: untrusted. */
export interface TicketInfo {
  id: string
  connection_id: string
  key: string
  title: string
  url?: string
  status: TicketLinkStatus
  reason?: TicketWaitReason
  project_id?: string
  goal_id?: string
  handed_by?: string
  state?: TicketState
  created_at: string
  updated_at: string
  connection: string
  project?: string
  gate_ref?: string
  goal_ref?: string
  /** trackers.max_active_goals, on a queued ticket. */
  limit?: number
}

export interface PersonSuggestion {
  user: string
  name: string
  reason: string
}

export interface UnmappedPerson {
  connection: string
  external_id: string
  external_name?: string
  tickets: string[]
  suggestions: PersonSuggestion[]
}

export interface UnmappedPeople {
  people: UnmappedPerson[]
  problems?: string[]
}

/** A provider of the model catalog (GET /models/catalog). */
export interface ModelProvider {
  name: string
  title: string
  adapter: string
  base_url?: string
  key_env: string
  homepage: string
  /** A country code, region (the account picks it) or local (your own machines). */
  country: string
  router?: boolean
}

export interface CatalogModel {
  id: string
  maker: string
  tier: string
  context: number
  price: { input: number; output: number }
}

/** A role a suggestion fills: the tier it is written for, and whether it writes code. */
export interface ModelRoleNeed {
  name: string
  tier: string
  writes: boolean
}

export interface ModelsCatalog {
  makers: { name: string; title: string; prefixes: string[] }[]
  providers: ModelProvider[]
  models: CatalogModel[]
  presets: string[]
  roles: ModelRoleNeed[]
}

/** A key check (no model) or a suitability test of a model connection. What a provider listed and an agent's error are untrusted text. */
export interface ModelCheck {
  id: string
  connection: string
  model?: string
  job_id: string
  status: string
  key?: { status: string; detail?: string; models?: string[]; agent_version?: string }
  test?: { abilities: Record<string, boolean>; level: "code" | "read" | "none"; tokens: number; duration_ms: number; turns: number; cost_usd: number; agent_version?: string; error?: string }
  stale?: boolean
  created_at: string
  updated_at: string
}

/** The estimate of a suitability test, and the test once it runs. */
export interface ModelTest {
  connection: string
  model: string
  max_turns: number
  timeout_seconds: number
  estimate: { input_tokens: number; output_tokens: number; cost_usd?: number; basis: string; worst_input_tokens?: number; worst_cost_usd?: number; worst_basis: string }
  test?: ModelCheck
}

export interface ModelsSuggestion {
  preset: string
  tiers: Record<string, Record<string, string>>
  roles: Record<string, { connection: string; tier: string }>
  notes: string[]
  plan?: ConfigPlan
}

/** The spec of a model_connection resource. */
export interface ModelConnectionSpec {
  vendor: string
  adapter: string
  base_url?: string
  key_env?: string
  secret_ref?: string
  enabled: boolean
}

/** The spec of a model_assignment resource: tier -> vendor -> model, role -> connection and tier. */
export interface ModelAssignmentSpec {
  tiers?: Record<string, Record<string, string>>
  roles?: Record<string, { connection: string; tier: string }>
  project_overrides?: boolean
}
