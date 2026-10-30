import { http, HttpResponse } from "msw"
import { ApiError, api, setUnauthorizedHandler } from "./client"
import { errors, fx, server } from "@/test/server"

afterEach(() => setUnauthorizedHandler(() => {}))

test("returns the JSON body", async () => {
  expect(await api.listGates()).toEqual(fx.gates)
})

test("turns an error body into an ApiError", async () => {
  server.use(http.get("/api/v1/goals/:ref", errors.notFound))
  const err = await api.getGoal("GOAL-99").catch((e: unknown) => e)
  expect(err).toBeInstanceOf(ApiError)
  expect(err).toMatchObject({ status: 404, code: "not_found", message: "goal GOAL-99: not found" })
})

test("copes with an error that is not the API's", async () => {
  server.use(http.get("/api/v1/workers", () => new HttpResponse("<html>bad gateway</html>", { status: 502 })))
  await expect(api.listWorkers()).rejects.toMatchObject({ status: 502, code: "internal", message: "Request failed (502)" })
})

test("reports an unreachable server", async () => {
  server.use(http.get("/api/v1/workers", () => HttpResponse.error()))
  await expect(api.listWorkers()).rejects.toMatchObject({ status: 0, code: "network" })
})

test("calls the unauthorized handler on every 401 except login's", async () => {
  const handler = vi.fn()
  setUnauthorizedHandler(handler)
  server.use(http.get("/api/v1/me", errors.unauthorized), http.post("/api/v1/session", errors.unauthorized), http.delete("/api/v1/session", errors.unauthorized))
  await expect(api.me()).rejects.toMatchObject({ status: 401 })
  expect(handler).toHaveBeenCalledTimes(1)
  await expect(api.login("x")).rejects.toMatchObject({ status: 401 })
  await expect(api.logout()).rejects.toMatchObject({ status: 401 })
  expect(handler).toHaveBeenCalledTimes(1)
})

test("sends JSON on state changes, even without a body, and nothing on reads", async () => {
  const seen: { method: string; type: string | null; body: string }[] = []
  server.use(
    http.all("/api/v1/*", async ({ request }) => {
      seen.push({ method: request.method, type: request.headers.get("content-type"), body: await request.text() })
      return request.method === "DELETE" ? new HttpResponse(null, { status: 204 }) : HttpResponse.json({})
    }),
  )
  await api.me()
  await api.logout()
  await api.approve("GATE-1")
  await api.reject("GATE-1")
  await api.requestChanges("GATE-1", "more")
  expect(seen).toEqual([
    { method: "GET", type: null, body: "" },
    { method: "DELETE", type: "application/json", body: "" },
    { method: "POST", type: "application/json", body: "{}" },
    { method: "POST", type: "application/json", body: "{}" },
    { method: "POST", type: "application/json", body: '{"feedback":"more"}' },
  ])
})

test("builds query strings without empty filters", async () => {
  const urls: string[] = []
  server.use(
    http.get("/api/v1/*", ({ request }) => {
      urls.push(new URL(request.url).pathname + new URL(request.url).search)
      return HttpResponse.json([])
    }),
  )
  await api.listGoals()
  await api.listGoals({ project: "app", status: "draft" })
  await api.listJobs({ project: "a b", status: "", limit: 5 })
  await api.jobLogs("JOB-1", 7, 10)
  await api.jobLogs("JOB-1")
  expect(urls).toEqual([
    "/api/v1/goals",
    "/api/v1/goals?project=app&status=draft",
    "/api/v1/jobs?project=a+b&limit=5",
    "/api/v1/jobs/JOB-1/logs?after_seq=7&limit=10",
    "/api/v1/jobs/JOB-1/logs?limit=500",
  ])
})

test("escapes refs and keeps owner/name project paths", async () => {
  const paths: string[] = []
  server.use(http.get("/api/v1/*", ({ request }) => (paths.push(new URL(request.url).pathname), HttpResponse.json({}))))
  await api.getProject("acme/app")
  await api.getProject("my app")
  await api.getJob("JOB 1")
  expect(paths).toEqual(["/api/v1/projects/acme/app", "/api/v1/projects/my%20app", "/api/v1/jobs/JOB%201"])
})

test("login and logout", async () => {
  expect(await api.login("fcm_x")).toEqual(fx.session)
  await expect(api.logout()).resolves.toBeUndefined()
  expect(await api.dispatchJob({ project: "app", role: "reviewer", prompt: "x" })).toEqual(fx.jobDispatch)
})
