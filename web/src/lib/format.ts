import type { CouncilStatus, GateKind, GateOrigin, GoalStatus, JobStatus, Level, ProjectStatus, TaskStatus } from "@/api/types"

/** "5 min", "2 h", "3 d": how long ago an RFC 3339 time was. */
export function age(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "never"
  return span(Math.max(0, Math.round((now - Date.parse(iso)) / 1000)))
}

/** "in 5 min": how long until an RFC 3339 time; "now" once it has passed. */
export function until(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "never"
  const text = span(Math.max(0, Math.round((Date.parse(iso) - now) / 1000)))
  return text === "just now" ? "now" : `in ${text}`
}

function span(s: number): string {
  if (s < 60) return "just now"
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h`
  return `${Math.floor(h / 24)} d`
}

/** 12345 as "12,345". */
export const count = (n: number | undefined) => (n ?? 0).toLocaleString("en-US")

export function cost(usd: number | undefined): string {
  return `$${(usd ?? 0).toFixed(2)}`
}

export const goalRef = (number: number) => `GOAL-${number}`
export function councilTone(s: CouncilStatus): Tone {
  return s === "decided" ? "good" : s === "failed" ? "bad" : s === "running" ? "attention" : "neutral"
}
export function levelTone(l: Level): Tone {
  return l === "high" ? "bad" : l === "medium" ? "attention" : "neutral"
}

/** The council a council_decision gate is about, read from its title ("COUNCIL-5: ..."). */
export const councilOfGate = (title: string) => /^(COUNCIL-\d+):/.exec(title)?.[1]
/** The assumption an assumption_review gate is about, read from its title ("Review A-7: ..."). */
export const assumptionOfGate = (title: string) => /^Review (A-\d+):/.exec(title)?.[1]

/** What the devil's advocate's dissent in a brief starts with (internal/councils DevilsAdvocateDissent). */
export const MANDATED_DISSENT = "Devil's advocate (mandated dissent): "

export const decisionRef = (number: number) => `DEC-${number}`
export const jobRef = (number: number) => `JOB-${number}`

export type Stage = "spec" | "planning" | "building" | "review" | "done" | "canceled"

/** The stage a goal is in, from its status: the list API gives nothing finer. */
export function stageOf(status: GoalStatus): Stage {
  switch (status) {
    case "draft":
      return "spec"
    case "spec_review":
      return "spec"
    case "planned":
      return "planning"
    case "in_progress":
      return "building"
    default:
      return status
  }
}

const GATE_KINDS: Record<GateKind, string> = {
  profile_approval: "Profile approval",
  spec_approval: "Spec approval",
  merge_approval: "Merge approval",
  test_plan_approval: "Test plan approval",
  needs_you: "Needs you",
  assumption_review: "Assumption review",
  council_decision: "Council decision",
  knowledge_proposal: "Knowledge proposal",
  access_request: "Access request",
  improvement_proposal: "Improvement proposal",
}
export const gateKindLabel = (k: GateKind) => GATE_KINDS[k]

/** A job asking a human (`ask_human`) is a question, kept apart from the escalations of a playbook. */
export const isQuestion = (g: { kind: GateKind; origin?: GateOrigin }) => g.kind === "needs_you" && g.origin === "ask_human"
export const gateLabel = (g: { kind: GateKind; origin?: GateOrigin }) => (isQuestion(g) ? "Question" : gateKindLabel(g.kind))

export const words = (s: string) => s.replaceAll("_", " ")

export type Tone = "good" | "attention" | "bad" | "neutral"

export function goalTone(s: GoalStatus): Tone {
  return s === "done" ? "good" : s === "canceled" ? "neutral" : s === "spec_review" ? "attention" : "neutral"
}
export function taskTone(s: TaskStatus): Tone {
  return s === "merged" ? "good" : s === "closed" ? "neutral" : s === "in_review" || s === "merge_ready" ? "attention" : "neutral"
}
export function jobTone(s: JobStatus): Tone {
  return s === "succeeded" ? "good" : s === "failed" ? "bad" : s === "queued" ? "attention" : "neutral"
}
export function projectTone(s: ProjectStatus): Tone {
  return s === "onboarded" ? "good" : s === "onboarding" ? "attention" : "neutral"
}

/** Splits text into its non-empty lines, trimmed. */
export function splitLines(s: string): string[] {
  return s.split("\n").map((l) => l.trim()).filter(Boolean)
}

/** Splits a comma or space separated list ("web, ios") into its words. */
export function splitList(s: string): string[] {
  return s.split(/[\s,]+/).filter(Boolean)
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

/** Whether s is a ref of the kind ("GOAL", "TASK": GOAL-3, any case, from 1) or a UUID, as the API takes them. */
export function isRefOrId(kind: "GOAL" | "TASK", s: string): boolean {
  const [prefix, num, ...rest] = s.split("-")
  if (prefix?.toUpperCase() === kind && rest.length === 0 && /^\d+$/.test(num ?? "") && Number(num) >= 1) return true
  return UUID.test(s)
}
