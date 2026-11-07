import type { ConfigResource, ModelAssignmentSpec, ModelCheck, ModelConnectionSpec, ModelProvider, ModelRoleNeed } from "@/api/types"
import type { Tone } from "@/lib/format"
import { humanName } from "@/lib/format"

/** A connection as the page shows it: its spec, with where it comes from. */
export interface Connection {
  name: string
  spec: ModelConnectionSpec
  /** Set in the server file: read-only here. */
  fromFile: boolean
}

export const connectionsOf = (resources: ConfigResource[] | undefined): Connection[] =>
  (resources ?? []).map((r) => ({ name: r.name, spec: r.spec as unknown as ModelConnectionSpec, fromFile: r.origin === "file" }))

/** The organization's assignment: its own row, else the server file's. */
export const organizationAssignment = (resources: ConfigResource[] | undefined): { spec: ModelAssignmentSpec; fromFile: boolean } => {
  const r = (resources ?? []).find((x) => x.name === "organization")
  return { spec: (r?.spec ?? {}) as ModelAssignmentSpec, fromFile: r?.origin === "file" }
}

/** A project's overrides, if it has any. */
export const projectAssignment = (resources: ConfigResource[] | undefined, project: string): ModelAssignmentSpec | undefined =>
  (resources ?? []).find((x) => x.name === `project:${project}`)?.spec as ModelAssignmentSpec | undefined

/** Whether a base URL is on your own machine or network: the code stays there. */
export function localAddress(url: string | undefined): boolean {
  if (!url) return false
  let host: string
  try {
    host = new URL(url).hostname.replace(/^\[|\]$/g, "")
  } catch {
    return false
  }
  if (host === "localhost" || host.endsWith(".localhost") || host === "::1") return true
  const v4 = host.split(".").map(Number)
  if (v4.length === 4 && v4.every((n) => Number.isInteger(n) && n >= 0 && n <= 255)) {
    const [a, b] = v4 as [number, number, number, number]
    return a === 10 || a === 127 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168)
  }
  return /^f[cd][0-9a-f]{2}:/i.test(host)
}

/** The provider of a connection in the catalog, by its vendor. */
export const providerOf = (providers: ModelProvider[] | undefined, vendor: string) => providers?.find((p) => p.name === vendor)

/** The name people read for a connection: its provider's, or the connection's own. */
export const connectionTitle = (c: Connection, providers?: ModelProvider[]) => {
  const p = providerOf(providers, c.spec.vendor)
  const title = p?.title.replace(/ API$/, "") ?? humanName(c.name)
  return c.name === c.spec.vendor || !p ? title : `${title} (${c.name})`
}

let regions: Intl.DisplayNames | undefined
/** A country code in words: CN is China. */
export function countryName(code: string): string {
  try {
    regions ??= new Intl.DisplayNames(["en"], { type: "region" })
    return regions.of(code) ?? code
  } catch {
    return code
  }
}

/** Where the provider of a connection runs, in words. */
export function whereText(c: Connection, provider?: ModelProvider): string {
  if (localAddress(c.spec.base_url) || provider?.country === "local") return "Local, code stays here"
  if (!provider) return "Another endpoint"
  if (provider.country === "region") return "In the region of its endpoint"
  return countryName(provider.country)
}

/**
 * A model's name as its connection's agent takes it: opencode names models provider/model, so a
 * name the provider listed gets the connection's vendor in front.
 */
export const qualifiedModel = (c: Connection, model: string) => (c.spec.adapter === "opencode" && !model.includes("/") ? `${c.spec.vendor}/${model}` : model)

/** The last key check of a connection. */
export const keyCheckOf = (checks: ModelCheck[] | undefined, connection: string) => checks?.find((c) => c.connection === connection && !c.model)

/** The last test of each model of a connection. */
export const testsOf = (checks: ModelCheck[] | undefined, connection: string) => (checks ?? []).filter((c) => c.connection === connection && c.model)

/** What a key check found, in words. */
export function keyStatus(check: ModelCheck | undefined): { text: string; tone: Tone } {
  switch (check?.status) {
    case undefined:
      return { text: "Not checked yet", tone: "neutral" }
    case "queued":
      return { text: "Checking…", tone: "neutral" }
    case "reachable":
      return { text: "Works", tone: "good" }
    case "refused":
      return { text: "Key refused", tone: "bad" }
    case "unreachable":
      return { text: "Cannot reach it", tone: "bad" }
    case "wrong_address":
      return { text: "Wrong address", tone: "bad" }
    case "unsupported":
      return { text: "No list of models: enter them by hand", tone: "attention" }
  }
  return { text: "Check failed", tone: "bad" }
}

/** What fixes a failed key check. */
export function keyAdvice(check: ModelCheck | undefined): string {
  switch (check?.status) {
    case "refused":
      return "Enter the key again: the provider did not accept it."
    case "unreachable":
      return "Check the address, and that a worker can reach it. A model on your own network needs local_model_endpoints on that worker."
    case "wrong_address":
      return "The address answers, but not as a model API: check it under More options."
  }
  return ""
}

/** What a model cannot do, in words, from the abilities its test found. */
const MISSING: Record<string, string> = {
  read: "Can't read files",
  edit: "Can't edit files",
  command: "Can't run commands",
  mcp: "Can't call tools reliably",
  json: "Can't answer in the required format",
}

/** What a tested model may do, in words. */
export function levelText(t: ModelCheck | undefined): { text: string; tone: Tone } {
  if (!t) return { text: "Not tested yet", tone: "neutral" }
  if (t.status === "queued") return { text: "Testing…", tone: "neutral" }
  if (t.status === "failed" || !t.test) return { text: "The test did not run", tone: "bad" }
  if (t.test.level === "code") return { text: "Writes code", tone: "good" }
  const missing = Object.keys(MISSING).filter((a) => !t.test?.abilities[a])
  if (t.test.level === "read") return { text: `Reading roles only: ${missing.map((a) => MISSING[a]!.toLowerCase()).join(", ")}`, tone: "attention" }
  return { text: missing.length ? missing.map((a) => MISSING[a]).join(", ") : "Did not pass", tone: "bad" }
}

/** The groups of "Who uses what": people choose by what a role does. */
export const ROLE_GROUPS = ["Writes code", "Reviews", "Plans", "Quick tasks"] as const
export type RoleGroup = (typeof ROLE_GROUPS)[number]

export function groupOf(r: ModelRoleNeed): RoleGroup {
  if (r.writes) return "Writes code"
  if (r.name.includes("review")) return "Reviews"
  if (r.tier === "fast") return "Quick tasks"
  return "Plans"
}

/** The model a role uses under an assignment, or "" when none is set. */
export function modelOf(a: ModelAssignmentSpec, role: string, connections: Connection[]): { connection: string; tier: string; model: string } | undefined {
  const r = a.roles?.[role]
  if (!r) return undefined
  const vendor = connections.find((c) => c.name === r.connection)?.spec.vendor ?? ""
  return { connection: r.connection, tier: r.tier, model: a.tiers?.[r.tier]?.[vendor] ?? "" }
}

/** The presets of a suggestion, in words. */
export const PRESETS: Record<string, string> = { balanced: "Balanced", low_cost: "Low cost", local_only: "Local only" }

/** The names a connection may take: lowercase letters, digits and -. */
export const CONNECTION_NAME = /^[a-z0-9][a-z0-9-]{0,62}$/

/** The variable an opencode provider reads its key from: uppercase, ending in _KEY. */
export const KEY_ENV = /^[A-Z][A-Z0-9_]*_KEY$/
