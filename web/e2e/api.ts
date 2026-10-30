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
}

/**
 * Answers /api/v1 from the fixtures. The session starts logged out when
 * loggedIn is false and turns on after POST /session. Gate decisions remove
 * the gate from the list; canceling a job turns the job page into the canceled job.
 */
export async function mockApi(page: Page, opts: { loggedIn?: boolean } = {}): Promise<Calls> {
  let loggedIn = opts.loggedIn ?? true
  let gates = fixture("gates") as { ref: string }[]
  let canceled = false
  const calls: Calls = { log: [] }

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
    const routes: Record<string, string> = {
      "GET /me": "me",
      "GET /projects": "projects",
      "GET /goals": "goals",
      "GET /jobs": "jobs",
      "GET /workers": "workers",
    }
    const known = routes[`${req.method()} ${path}`]
    if (known) return json(route, fixture(known))
    if (req.method() === "GET" && path === "/gates") return json(route, gates)
    if (req.method() === "GET" && path.startsWith("/projects/")) return json(route, fixture("project"))
    if (req.method() === "GET" && /^\/goals\/[^/]+$/.test(path)) return json(route, fixture("goal"))
    if (req.method() === "GET" && /^\/jobs\/[^/]+\/logs$/.test(path)) {
      return json(route, url.searchParams.get("after_seq") === "5" ? fixture("job-logs-caught-up") : fixture("job-logs"))
    }
    if (req.method() === "GET" && /^\/jobs\/[^/]+$/.test(path)) return json(route, fixture(canceled ? "job-cancel" : "job"))
    if (req.method() === "POST" && path === "/goals") return json(route, fixture("goal-create"), 201)
    if (req.method() === "POST" && path === "/projects") return json(route, fixture("project-onboard"), 202)
    if (req.method() === "POST" && path === "/jobs") return json(route, fixture("job-dispatch"), 201)
    return json(route, fixture("error-not-found"), 404)
  })
  return calls
}
