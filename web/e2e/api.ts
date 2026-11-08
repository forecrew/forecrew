import { readFileSync } from "node:fs"
import { join } from "node:path"
import type { Page, Route } from "@playwright/test"

const dir = join(import.meta.dirname, "../src/test/fixtures")

/** A recorded API answer (see docs/ui.md for how the fixtures are made). */
export const fixture = (name: string) => JSON.parse(readFileSync(join(dir, `${name}.json`), "utf8")) as unknown

const json = (route: Route, body: unknown, status = 200) =>
  route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) })

export interface Calls {
  /** "METHOD /path" of every API call, in order, with the JSON body of writes. */
  log: { call: string; body?: unknown }[]
  /**
   * The image build of the Windows location (win-host) as the page reads it: empty, requested, running, done or failed,
   * the name of a recorded answer. Starting a build sets requested; a test moves it on, as the connector would.
   */
  build: { stage: string }
}

/**
 * Answers /api/v1 from the fixtures. The session starts logged out when
 * loggedIn is false and turns on after POST /session. Gate decisions remove
 * the gate from the list; canceling a job turns the job page into the canceled job.
 * Signing in goes through the fake GitHub connection: its login address signs
 * the page in and sends it back to the page it came from. `me` names the
 * fixture of who is signed in (me, me-admin, me-viewer, me-contributor).
 * `lowering` makes the assistant's and the forms' plan one that lowers
 * protection (the second confirmation); `inboxPlans` puts plans in the Inbox. `trackerFails` makes the test of a tracker connection
 * fail on its certificate. A plan for a tracker connection or rule is the recorded one with its secure field.
 * A join code reports as soon as it is made. JOB-42 is a waiting job, JOB-43 one waiting for a model connection.
 * `build` is where the image build of the Windows location starts (see Calls.build).
 * `noModels` starts with no AI provider: confirming the DeepSeek plan connects it and its key check passes;
 * confirming the suggested preset moves the developer to it, and JOB-41 then runs on deepseek-chat.
 */
export async function mockApi(page: Page, opts: { loggedIn?: boolean; gates?: string; me?: string; lowering?: boolean; inboxPlans?: boolean; roles?: string; trackerFails?: boolean; noModels?: boolean; build?: string } = {}): Promise<Calls> {
  let loggedIn = opts.loggedIn ?? true
  let gates = fixture(opts.gates ?? "gates") as { ref: string }[]
  let canceled = false
  let revoked = false
  // The fleet starts empty. An assistant plan confirmed in the page adds the location.
  let asked = false
  let applied = false
  let joined = false
  // The AI models flow: no provider, then DeepSeek connected, then the preset applied.
  let modelsConnected = !opts.noModels
  let presetApplied = false
  const modelPlans = () => [fixture("config-plan-model-connection"), fixture("config-plan-model-authorization"), (fixture("models-suggest") as { plan: object }).plan] as { id: string; steps: { filled?: boolean }[] }[]
  const calls: Calls = { log: [], build: { stage: opts.build ?? "empty" } }
  const newPlan = () => fixture(opts.lowering ? "config-plan-lowers" : "config-plan") as { id: string }
  // Every recorded plan, found by its ID.
  const planById = (id: string) => {
    const names = ["config-plan-tracker", "config-plan", "config-plan-lowers", "config-plan-revert", "config-plan-needs-secret", "config-plan-secret", "config-plan-connection", "config-plan-stale", "config-plan-cannot-confirm", "config-plan-approval"]
    const all = [...modelPlans(), ...names.map((n) => fixture(n)), ...(fixture("config-plans") as unknown[]), ...(fixture("config-plans-inbox") as unknown[])] as { id: string }[]
    return all.find((p) => p.id === id)
  }

  // The fake GitHub: the server's own sign-in routes are outside /api/v1.
  await page.route("**/auth/*/login*", async (route) => {
    loggedIn = true
    const back = new URL(route.request().url()).searchParams.get("redirect") ?? "/"
    await route.fulfill({ status: 302, headers: { location: back } })
  })

  await page.route("**/api/v1/**", async (route) => {
    const req = route.request()
    const url = new URL(req.url())
    const path = url.pathname.replace("/api/v1", "")
    const call = `${req.method()} ${path}${url.search}`
    const body = req.method() === "GET" ? undefined : (req.postDataJSON() as unknown)
    calls.log.push({ call, body })

    if (req.method() === "POST" && path === "/session") {
      loggedIn = true
      return json(route, fixture("session"))
    }
    if (req.method() === "DELETE" && path === "/session") {
      loggedIn = false
      return route.fulfill({ status: 204 })
    }
    if (req.method() === "POST" && path === "/auth/discover") return json(route, { ...(fixture("auth-discover") as object), token_login: false })
    if (!loggedIn) return json(route, fixture("error-unauthorized"), 401)

    const decision = /^\/gates\/([^/]+)\/(approve|request-changes|reject)$/.exec(path)
    if (req.method() === "POST" && decision) {
      gates = gates.filter((g) => g.ref !== decision[1])
      return json(route, fixture(decision[2] === "approve" ? "gate-approve" : decision[2] === "reject" ? "gate-reject" : "gate-request-changes"))
    }
    if (req.method() === "POST" && /^\/jobs\/[^/]+\/cancel$/.test(path)) {
      canceled = true
      return json(route, fixture("job-cancel"))
    }
    if (req.method() === "GET" && path === "/tokens") {
      const tokens = fixture("tokens") as { name: string; revoked_at?: string }[]
      return json(route, tokens.map((t) => (revoked && t.name === "old-laptop" ? { ...t, revoked_at: "2026-03-01T12:00:00Z" } : t)))
    }
    const revokeMatch = /^\/tokens\/([^/]+)\/revoke$/.exec(path)
    if (req.method() === "POST" && revokeMatch) {
      const self = (fixture("tokens") as { id: string; current?: boolean }[]).some((t) => t.id === revokeMatch[1] && t.current)
      if (self) loggedIn = false
      else revoked = true
      return json(route, fixture(self ? "token-revoke-self" : "token-revoke"))
    }
    const bindings = fixture("role-bindings") as { project?: string }[]
    if (req.method() === "GET" && path === "/role-bindings" && url.searchParams.get("project")) {
      return json(route, bindings.filter((b) => b.project === url.searchParams.get("project")))
    }
    const routes: Record<string, string> = {
      "GET /me": opts.me ?? "me",
      "GET /members": "members",
      "GET /teams": "teams",
      "GET /roles": opts.roles ?? "roles",
      "GET /role-bindings": "role-bindings",
      "GET /service-accounts": "service-accounts",
      "GET /invitations": "invitations",
      "GET /connections": "connections",
      "GET /audit": "audit",
      "GET /sessions": "sessions",
      "GET /identities": "identities",
      "GET /projects": "projects",
      "GET /goals": "goals",
      "GET /jobs": "jobs",
      "GET /workers": "workers",
      "GET /providers": "providers",
      "GET /shapes": "shapes",
      "GET /machines": "machines",
      "GET /config/export": "config-export",
      "GET /assistant/conversations": "assistant-conversations",
      "GET /trace": "trace",
      "GET /assumptions": "assumptions",
      "GET /tracker-connections": "tracker-connections",
      "GET /tracker-routes": "tracker-routes",
      "GET /tracker-identities": "tracker-identities",
      "GET /tracker-people/unmapped": "tracker-unmapped",
    }
    if (req.method() === "GET" && path === "/join-codes") {
      // The machine reports as soon as its code is made: the code the page waits for is the claimed one.
      const codes = fixture("join-codes") as { id: string }[]
      return json(route, joined ? [{ ...codes[0], id: (fixture("join-code-create") as { join_code: { id: string } }).join_code.id }, ...codes] : codes)
    }
    const known = routes[`${req.method()} ${path}`]
    if (known) return json(route, fixture(known))
    if (req.method() === "GET" && path === "/gates") return json(route, gates)
    // AI models.
    if (req.method() === "GET" && path === "/models/catalog") return json(route, fixture("models-catalog"))
    if (req.method() === "GET" && path === "/model-checks") return json(route, modelsConnected ? fixture("model-checks") : [])
    if (req.method() === "POST" && /^\/model-connections\/[^/]+\/check$/.test(path)) return json(route, fixture("model-check"), 202)
    if (req.method() === "POST" && /^\/model-connections\/[^/]+\/tests$/.test(path)) {
      const run = (body as { run?: boolean } | undefined)?.run
      return json(route, run ? { ...(fixture("model-test-estimate") as object), test: fixture("model-check") } : fixture("model-test-estimate"))
    }
    if (req.method() === "POST" && path === "/models/suggest") return json(route, fixture("models-suggest"))
    const modelKind = (body as { changes?: { kind: string }[] } | undefined)?.changes?.[0]?.kind
    if (req.method() === "POST" && path === "/config/plans" && modelKind === "model_connection") return json(route, fixture("config-plan-model-connection"), 201)
    if (req.method() === "POST" && path === "/config/plans" && modelKind === "model_authorization") return json(route, fixture("config-plan-model-authorization"), 201)
    const modelPlan = /^\/config\/plans\/([^/]+)\/(apply|secrets\/.+)$/.exec(path)
    const ofModels = modelPlan ? modelPlans().find((p) => p.id === modelPlan[1]) : undefined
    if (modelPlan && ofModels) {
      if (modelPlan[2] !== "apply") return json(route, { ...ofModels, steps: ofModels.steps.map((st) => ({ ...st, filled: true })) })
      if (ofModels.id === (fixture("config-plan-model-connection") as { id: string }).id) modelsConnected = true
      else presetApplied = true
      return json(route, { plan: { ...ofModels, status: "applied", steps: ofModels.steps.map((st) => ({ ...st, filled: true })) }, show_once: [] })
    }
    if (req.method() === "GET" && path === "/config/resources" && url.searchParams.get("kind")?.startsWith("model_")) {
      const kind = url.searchParams.get("kind")!
      if (kind === "model_connection") return json(route, modelsConnected ? fixture("config-resources-model-connection") : [])
      return json(route, modelsConnected ? fixture(`config-resources-${kind.replace("_", "-")}`) : [])
    }
    if (req.method() === "GET" && path === "/jobs/JOB-43") return json(route, fixture("job-waiting-private-code"))
    if (req.method() === "GET" && path === "/jobs/JOB-41" && presetApplied) {
      const job = fixture("job") as { spec: object }
      return json(route, { ...job, spec: { ...job.spec, model: "deepseek-chat" } })
    }
    // Trackers: the test of a connection passes unless the page asks for the failing one.
    if (req.method() === "POST" && /^\/tracker-connections\/[^/]+\/test$/.test(path)) return json(route, fixture(opts.trackerFails ? "tracker-test-failed" : "tracker-test"))
    if (req.method() === "GET" && path === "/tickets/waiting") return json(route, fixture("tickets-waiting"))
    if (req.method() === "POST" && path === "/tracker-identities") return json(route, fixture("tracker-identity-map"), 201)
    if (req.method() === "POST" && /^\/tickets\/[^/]+\/accept$/.test(path)) return json(route, fixture("ticket-accept"))
    if (req.method() === "POST" && /^\/tickets\/[^/]+\/dismiss$/.test(path)) return json(route, fixture("ticket-dismiss"))
    if (req.method() === "POST" && path === "/config/plans" && (body as { changes?: { kind: string }[] } | undefined)?.changes?.[0]?.kind?.startsWith("tracker_")) return json(route, fixture("config-plan-tracker"), 201)
    if (req.method() === "GET" && /^\/teams\/[^/]+\/members$/.test(path)) return json(route, fixture("team-members"))
    if (req.method() === "POST" && path === "/invitations") return json(route, fixture("invitation-create"), 201)
    if (req.method() === "POST" && path === "/tokens") return json(route, fixture("token-create"), 201)
    if (req.method() === "PATCH" && path === "/organization") return json(route, fixture("organization"))
    // The council fixtures belong to the project app.
    if (req.method() === "GET" && (path === "/councils" || path === "/decisions")) {
      return json(route, url.searchParams.get("project") === "app" ? fixture(path === "/councils" ? "councils" : "decisions") : [])
    }
    if (req.method() === "POST" && path === "/assistant/messages") {
      asked = true
      return json(route, fixture("assistant-send"), 202)
    }
    if (req.method() === "GET" && path.startsWith("/assistant/conversations/")) {
      const view = fixture("assistant-conversation") as { messages: unknown[]; plans: unknown[] }
      const plan = (applied ? (fixture("config-plan-applied") as { plan: object }).plan : newPlan()) as object
      return json(route, { ...view, plans: asked ? [{ ...plan, source: "assistant", conversation_id: "018f0000-0000-7000-8000-000000000740" }] : [] })
    }
    if (req.method() === "POST" && /^\/config\/plans\/[^/]+\/apply$/.test(path)) {
      applied = true
      return json(route, fixture("config-plan-applied"))
    }
    if (/^\/providers\/[^/]+\/image-builds$/.test(path)) {
      if (req.method() === "POST") {
        calls.build.stage = "requested"
        return json(route, fixture("image-build-start"), 202)
      }
      return json(route, fixture(`image-builds-${calls.build.stage}`))
    }
    if (req.method() === "GET" && path === "/images") return json(route, [])
    if (req.method() === "GET" && path === "/fleet") return json(route, fixture(applied ? "fleet" : "fleet-empty"))
    if (req.method() === "GET" && path === "/config/plans") return json(route, url.searchParams.get("inbox") ? (opts.inboxPlans ? fixture("config-plans-inbox") : []) : fixture("config-plans"))
    const planPath = /^\/config\/plans\/([^/]+)(?:\/(cancel|revert|request-approval|secrets\/[^/]+))?$/.exec(path)
    if (planPath) {
      const [, id, action] = planPath
      if (req.method() === "GET" && !action) {
        const found = planById(id!)
        return found ? json(route, found) : json(route, fixture("error-not-found"), 404)
      }
      if (req.method() === "POST" && action === "cancel") return json(route, fixture("config-plan-cancel"))
      if (req.method() === "POST" && action === "revert") return json(route, fixture("config-plan-revert"), 201)
      if (req.method() === "POST" && action === "request-approval") return json(route, fixture("config-plan-approval"))
      if (req.method() === "PUT" && action?.startsWith("secrets/")) return json(route, fixture(id === (fixture("config-plan-tracker") as { id: string }).id ? "config-plan-tracker-secret" : "config-plan-secret"))
    }
    if (req.method() === "POST" && path === "/config/import") return json(route, fixture("config-plan"), 201)
    if (req.method() === "POST" && /^\/join-codes\/[^/]+\/confirm$/.test(path)) return json(route, fixture("join-confirm"))
    if (req.method() === "POST" && path === "/images") return json(route, fixture("image-register"), 201)
    if (req.method() === "GET" && path === "/connected-apps") return json(route, fixture("connected-apps"))
    if (req.method() === "GET" && path === "/config/resources") return json(route, fixture(url.searchParams.get("kind") === "fleet_policy" ? "config-resources-fleet-policy" : "config-resources-provider"))
    if (req.method() === "POST" && path === "/join-codes") {
      joined = true
      return json(route, fixture("join-code-create"), 201)
    }
    if (req.method() === "POST" && path === "/config/plans") return json(route, newPlan(), 201)
    const council = /^\/councils\/COUNCIL-(\d)$/.exec(path)
    if (req.method() === "GET" && council) {
      const names: Record<string, string> = { "2": "running", "3": "decided", "4": "failed", "5": "pending" }
      return json(route, names[council[1]!] ? fixture(`council-${names[council[1]!]}`) : fixture("error-not-found"), names[council[1]!] ? 200 : 404)
    }
    if (req.method() === "GET" && path === "/assumptions") return json(route, fixture("assumptions"))
    if (req.method() === "GET" && path === "/quality") return json(route, fixture("quality"))
    if (req.method() === "GET" && /^\/changes\/[^/]+\/verification$/.test(path)) return json(route, fixture("verification"))
    if (req.method() === "GET" && /^\/jobs\/[^/]+\/artifacts$/.test(path)) return json(route, fixture("job-artifacts"))
    if (req.method() === "GET" && path.startsWith("/projects/")) return json(route, fixture("project"))
    if (req.method() === "GET" && /^\/goals\/[^/]+$/.test(path)) return json(route, fixture("goal"))
    if (req.method() === "GET" && /^\/jobs\/[^/]+\/logs$/.test(path)) {
      return json(route, url.searchParams.get("after_seq") === "5" ? fixture("job-logs-caught-up") : fixture("job-logs"))
    }
    if (req.method() === "GET" && path === "/jobs/JOB-42") return json(route, fixture("job-waiting"))
    if (req.method() === "GET" && /^\/jobs\/[^/]+$/.test(path)) return json(route, fixture(canceled ? "job-cancel" : "job"))
    if (req.method() === "GET" && path === "/knowledge/entries") return json(route, fixture(url.searchParams.get("kind") === "note" ? "knowledge-notes" : "knowledge-entries"))
    if (req.method() === "GET" && path === "/knowledge") return json(route, fixture("knowledge-search"))
    if (req.method() === "GET" && path === "/knowledge/proposals") return json(route, fixture("knowledge-proposals"))
    if (req.method() === "POST" && path === "/knowledge/proposals") return json(route, fixture("knowledge-propose"), 201)
    if (req.method() === "DELETE" && path.startsWith("/knowledge/notes/")) return json(route, fixture("knowledge-note-delete"))
    if (req.method() === "POST" && /^\/assumptions\/[^/]+\/resolve$/.test(path)) return json(route, fixture("assumption-resolve"))
    if (req.method() === "POST" && path === "/goals") return json(route, fixture("goal-create"), 201)
    if (req.method() === "POST" && path === "/projects") return json(route, fixture("project-onboard"), 202)
    if (req.method() === "POST" && path === "/jobs") return json(route, fixture("job-dispatch"), 201)
    return json(route, fixture("error-not-found"), 404)
  })
  return calls
}
