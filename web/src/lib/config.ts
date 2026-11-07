import type { ConfigChange, ConfigPlan } from "@/api/types"
import { toYaml } from "@/lib/yaml"

/** What a permission lets a person do, in the words of the interface. */
const WORDS: Record<string, string> = {
  "worker.manage": "Manage machines",
  "billing.manage": "Manage budgets",
  "org.manage": "Manage the organization",
  "connection.manage": "Manage sign-in",
  "member.manage": "Manage people",
  "team.manage": "Manage teams",
  "role.manage": "Manage roles",
  "pool.manage": "Manage trust zones",
  "project.manage": "Manage the project",
  "config.apply": "Make change plans",
}
export const permissionWords = (p: string) => WORDS[p] ?? p

/** Where a plan applies: now, or in a pull request, never ambiguous. */
export function appliesText(applies: string): string {
  return applies.startsWith("pr:") ? `Opens a PR on ${applies.slice(3)}` : "Applies now"
}

/** The resources of a plan after it, as YAML for people. */
export function planYaml(plan: ConfigPlan): string {
  return plan.changes
    .map((c: ConfigChange) => (c.op === "delete" ? `# removed: ${c.kind}/${c.name}\n` : toYaml({ kind: c.kind, name: c.name, spec: c.new })))
    .join("---\n")
}

/** What the plan would say of itself in the history: where it came from. */
export function sourceText(plan: Pick<ConfigPlan, "source" | "client">): string {
  switch (plan.source) {
    case "mcp":
      return plan.client || "an MCP client"
    case "assistant":
      return "assistant"
    case "yaml":
      return "YAML"
    case "cli":
      return "CLI"
    default:
      return "form"
  }
}

/** True on a phone, where plans that lower protection are not confirmed. */
export function narrowScreen(): boolean {
  return typeof window.matchMedia === "function" && window.matchMedia("(max-width: 767px)").matches
}
