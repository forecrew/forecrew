import type { TicketInfo, TicketState, TrackerConnection, TrackerRoute, TrackerState, TrackerTriggers } from "@/api/types"

/** The trackers the connect form sets up. GitHub issues and boards are connected from the command line or by asking the assistant. */
export const FORM_KINDS = [
  { value: "jira", label: "Jira" },
  { value: "linear", label: "Linear" },
] as const
export type FormKind = (typeof FORM_KINDS)[number]["value"]

const KIND_LABEL: Record<string, string> = { jira: "Jira", linear: "Linear", github: "GitHub issues", github_project: "GitHub Projects" }
export const kindLabel = (kind: string) => KIND_LABEL[kind] ?? kind

/** A string list of a connection's config: Jira's projects, Linear's teams, GitHub's repos. */
export function configList(c: TrackerConnection, key: string): string[] {
  const v = c.config[key]
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []
}

/** Where the connection points, in words: the host of a Jira, the teams of a Linear. */
export function addressOf(c: TrackerConnection): string {
  switch (c.kind) {
    case "jira": {
      const url = typeof c.config.base_url === "string" ? c.config.base_url : ""
      try {
        return new URL(url).host
      } catch {
        return url
      }
    }
    case "linear":
      return `teams ${configList(c, "teams").join(", ")}`
    case "github":
      return configList(c, "repos").join(", ")
    case "github_project":
      return `${String(c.config.owner ?? "")} board ${String(c.config.project ?? "")}`
    default:
      return ""
  }
}

/** The tracker's own projects or teams a rule can match on. */
export function scopesOf(c: TrackerConnection): string[] {
  return c.kind === "jira" ? configList(c, "projects") : c.kind === "linear" ? configList(c, "teams") : c.kind === "github" ? configList(c, "repos") : []
}

/** How a person hands a ticket over, in words. */
export function triggerWords(t: TrackerTriggers): string {
  const parts: string[] = []
  if (t.label) parts.push(`labeled ${t.label}`)
  if (t.assignee) parts.push(`assigned to ${t.assignee}`)
  if (t.state) parts.push(`moved to ${t.state}`)
  return parts.join(" or ")
}

/** "Tickets from ENG labeled forecrew go to app": a route as a sentence. */
export function routeSentence(r: Pick<TrackerRoute, "match" | "project">): string {
  const m = r.match
  const from = m.project ? `from ${m.project}` : "from any project"
  const rest = [
    m.types?.length ? `of type ${m.types.join(" or ")}` : "",
    m.labels?.length ? `labeled ${m.labels.join(" or ")}` : "",
    m.components?.length ? `in ${m.components.join(" or ")}` : "",
  ].filter(Boolean)
  return `Tickets ${[from, ...rest].join(" ")} go to ${r.project}`
}

/** Why a ticket waits in the Inbox, as a sentence. */
export function reasonWords(t: Pick<TicketInfo, "status" | "reason" | "handed_by" | "limit">): string {
  if (t.status === "queued") {
    return t.limit ? `Waiting for a free slot: ${t.limit} goals from tickets are already running in this project.` : "Waiting for a free slot in its project."
  }
  switch (t.reason) {
    case "unknown_person":
      return t.handed_by ? `${t.handed_by} isn't in Forecrew. Start this anyway?` : "Whoever handed this over isn't in Forecrew. Start this anyway?"
    case "no_route":
      return "No rule says where this ticket goes."
    case "many_projects":
      return "Rules for more than one project match this ticket."
    case "not_permitted":
      return "The person who handed it over can't create goals in that project."
    default:
      return "Waiting for a person to accept it."
  }
}

/** A ticket's link, only when it is a web address: the URL comes from the tracker. */
export const safeUrl = (url: string | undefined) => (url && /^https?:\/\//i.test(url) ? url : undefined)

/** The words of a ticket's state on its tracker, as Forecrew wrote it. */
export const stateWords: Record<TicketState, string> = {
  queued: "queued",
  in_progress: "in progress",
  in_review: "in review",
  done: "done",
  canceled: "canceled",
}

export const MAPPED_STATES: TicketState[] = ["in_progress", "in_review", "done", "canceled"]

/**
 * Suggests the tracker's state for each logical one from the names it has:
 * "In Progress" for in progress, "Code Review" for in review, and of the
 * closed states "Done" for done and "Won't Do" for canceled. A name no
 * guess fits is left out: that state is not written to the ticket.
 */
export function suggestStates(states: TrackerState[]): Partial<Record<TicketState, string>> {
  const pick = (re: RegExp, closed: boolean) => states.find((s) => Boolean(s.closed) === closed && re.test(s.name))?.name
  const out: Partial<Record<TicketState, string>> = {}
  const set = (k: TicketState, v: string | undefined) => {
    if (v) out[k] = v
  }
  set("in_progress", pick(/progress|doing|started|building/i, false))
  set("in_review", pick(/review/i, false))
  set("done", pick(/done|complete|closed|resolved|shipped|finished/i, true) ?? states.find((s) => s.closed && !/cancel|won'?t|reject|duplicate|invalid/i.test(s.name))?.name)
  set("canceled", pick(/cancel|won'?t|reject|duplicate|invalid/i, true))
  return out
}

/** Whether an address is one a hosted server cannot reach: a private network or a name without a public domain. */
export function looksPrivate(url: string): boolean {
  let host: string
  try {
    host = new URL(url).hostname.toLowerCase()
  } catch {
    return false
  }
  if (host === "localhost" || host.endsWith(".local") || host.endsWith(".internal") || host.endsWith(".lan") || host.endsWith(".corp") || host.endsWith(".intranet")) return true
  if (!host.includes(".") && !host.includes(":")) return true
  const v4 = /^(\d+)\.(\d+)\.\d+\.\d+$/.exec(host)
  if (v4) {
    const [a, b] = [Number(v4[1]), Number(v4[2])]
    return a === 10 || a === 127 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || (a === 169 && b === 254)
  }
  return host === "[::1]" || host.startsWith("[fc") || host.startsWith("[fd") || host.startsWith("[fe80")
}

/** What to do about a failed check of a connection test, in words. */
export function checkHelp(name: string, detail: string | undefined): string {
  switch (name) {
    case "address":
      return /private|refused/i.test(detail ?? "")
        ? "This server does not reach private addresses. On a hosted server the tracker must be reachable from outside; a self-hosted server in the same network turns on trackers.private_addresses."
        : "Forecrew could not reach the address. Check it, and that the tracker is up."
    case "certificate":
      return "The server's certificate is not trusted. Add your company's certificate below."
    case "login":
      return "The token was refused or has expired. Make a new token for the dedicated account and connect again."
    case "projects":
      return "The dedicated account cannot see, comment on or move tickets of a project or team of this connection. Give it access in the tracker."
    case "transitions":
      return "The tracker has no finished status, so Forecrew cannot move a ticket to done."
    case "routes":
      return "A rule maps a status the tracker does not have. Remove the rule and add it again."
    default:
      return ""
  }
}
