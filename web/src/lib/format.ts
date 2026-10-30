import type { GateKind, GoalStatus, JobStatus, ProjectStatus, TaskStatus } from "@/api/types"

/** "5 min", "2 h", "3 d": how long ago an RFC 3339 time was. */
export function age(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "never"
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000))
  if (s < 60) return "just now"
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h`
  return `${Math.floor(h / 24)} d`
}

export function cost(usd: number | undefined): string {
  return `$${(usd ?? 0).toFixed(2)}`
}

export const goalRef = (number: number) => `GOAL-${number}`
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
  needs_you: "Needs you",
}
export const gateKindLabel = (k: GateKind) => GATE_KINDS[k]

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

/** Splits a comma or space separated list ("web, ios") into its words. */
export function splitList(s: string): string[] {
  return s.split(/[\s,]+/).filter(Boolean)
}
