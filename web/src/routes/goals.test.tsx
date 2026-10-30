import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

describe("goal list", () => {
  test("lists goals with project, status and stage", async () => {
    await renderApp("/goals")
    const row = (await screen.findByRole("link", { name: /GOAL-3/ })).closest("tr")!
    expect(within(row).getByText("app")).toBeInTheDocument()
    expect(within(row).getByText("in progress")).toBeInTheDocument()
    expect(within(row).getByText("building")).toBeInTheDocument()
    expect(screen.getAllByRole("row")).toHaveLength(4)
    expect(screen.getByRole("link", { name: "New goal" })).toHaveAttribute("href", "/goals/new")
  })

  test("filters by project and status through the query string", async () => {
    const seen: string[] = []
    server.use(
      http.get("/api/v1/goals", ({ request }) => {
        seen.push(new URL(request.url).search)
        return HttpResponse.json(fx.goals)
      }),
    )
    const { user, router } = await renderApp("/goals")
    await screen.findByRole("link", { name: /GOAL-3/ })
    await user.selectOptions(await screen.findByLabelText("Switch project"), "app")
    await user.selectOptions(screen.getByLabelText("Status"), "draft")
    await waitFor(() => expect(seen).toContain("?project=app&status=draft"))
    expect(router.state.location.search).toEqual({ project: "app", status: "draft" })
    await user.selectOptions(screen.getByLabelText("Status"), "")
    await waitFor(() => expect(router.state.location.search).toEqual({ project: "app" }))
  })

  test("reads the filters from the address", async () => {
    await renderApp("/goals?status=done")
    expect(await screen.findByLabelText("Status")).toHaveValue("done")
  })

  test("shows empty, loading and error states", async () => {
    server.use(http.get("/api/v1/goals", () => HttpResponse.json(fx.goalsEmpty)))
    const { unmount } = await renderApp("/goals")
    expect(screen.getByRole("status")).toHaveTextContent("Loading")
    expect(await screen.findByText(/No goals yet/)).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/goals", errors.invalidInput))
    await renderApp("/goals")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })
})

describe("new goal form", () => {
  test("creates a goal with optional platforms and opens it", async () => {
    let body: unknown
    server.use(
      http.post("/api/v1/goals", async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(fx.goalCreate, { status: 201 })
      }),
    )
    const { user, router } = await renderApp("/goals/new")
    await user.selectOptions(await screen.findByLabelText("Project"), "app")
    await user.type(screen.getByLabelText("Title"), " Dark mode ")
    await user.type(screen.getByLabelText("Description"), "Follow the system setting.")
    await user.type(screen.getByLabelText("Platforms"), "web, ios")
    await user.click(screen.getByRole("button", { name: "Create goal" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/goals/GOAL-4"))
    expect(body).toEqual({ project: "app", title: "Dark mode", body: "Follow the system setting.", platforms: ["web", "ios"] })
  })

  test("needs a project and a title", async () => {
    let called = false
    server.use(http.post("/api/v1/goals", () => ((called = true), HttpResponse.json(fx.goalCreate))))
    const { user } = await renderApp("/goals/new")
    await user.click(await screen.findByRole("button", { name: "Create goal" }))
    expect(screen.getByText("Choose a project.")).toBeInTheDocument()
    expect(screen.getByText("Give the goal a title.")).toBeInTheDocument()
    expect(screen.getByLabelText("Title")).toHaveAttribute("aria-invalid", "true")
    expect(called).toBe(false)
  })

  test("shows the server's refusal", async () => {
    server.use(http.post("/api/v1/goals", errors.invalidInput))
    const { user } = await renderApp("/goals/new")
    await user.selectOptions(await screen.findByLabelText("Project"), "app")
    await user.type(screen.getByLabelText("Title"), "x")
    await user.click(screen.getByRole("button", { name: "Create goal" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("feedback is required")
  })
})

describe("goal page", () => {
  test("shows status, stage, spec, tasks, gates and cost", async () => {
    await renderApp("/goals/GOAL-3")
    expect(await screen.findByRole("heading", { name: "GOAL-3 · Sign in with an email link" })).toBeInTheDocument()
    expect(screen.getByText("Stage: building")).toBeInTheDocument()
    expect(screen.getByText("Total cost $1.75")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: /Goal issue/ })).toHaveAttribute("href", "https://github.com/acme/app/issues/5")
    expect(screen.getByRole("link", { name: "app" })).toHaveAttribute("href", "/projects/app")
    expect(screen.getByText("Platforms: web")).toBeInTheDocument()
    expect(screen.getByRole("heading", { name: "Description" })).toBeInTheDocument()
    expect(screen.getByText("Let users sign in without a password.")).toBeInTheDocument()

    const spec = screen.getByRole("region", { name: /^Spec/ })
    expect(within(spec).getByText(/revision 2/)).toBeInTheDocument()
    expect(within(spec).getByRole("heading", { name: "Acceptance criteria" })).toBeInTheDocument()
    expect(within(spec).getByText(/AC-1: A user receives/)).toBeInTheDocument()
    expect(within(spec).getByRole("heading", { name: "Assumptions" })).toBeInTheDocument()
    expect(within(spec).getByRole("heading", { name: "Open questions" })).toBeInTheDocument()

    const tasks = screen.getByRole("region", { name: "Tasks table" })
    const row = within(tasks).getByText("Send the sign-in link").closest("tr")!
    expect(within(row).getByText("in review")).toBeInTheDocument()
    expect(within(row).getByText("needs GATE-9")).toBeInTheDocument()
    expect(within(row).getByRole("link", { name: /Issue/ })).toHaveAttribute("href", "https://github.com/acme/app/issues/6")
    expect(within(row).getByRole("link", { name: /PR #9/ })).toHaveAttribute("href", "https://github.com/acme/app/pull/9")
    expect(within(row).getByText("checks failing")).toBeInTheDocument()
    expect(within(row).getByText("2")).toBeInTheDocument()
    expect(within(row).getByText("$0.75")).toBeInTheDocument()
    const second = within(tasks).getByText("Verify the link").closest("tr")!
    expect(within(second).getByText("nothing")).toBeInTheDocument()
    expect(within(second).getAllByText("none")).toHaveLength(2)

    const gates = screen.getByRole("heading", { name: "Gates" }).closest("section")!
    expect(within(gates).getByText("pending")).toBeInTheDocument()
    expect(within(gates).getByText("changes requested")).toBeInTheDocument()
    expect(within(gates).getByText("by mcp:laptop")).toBeInTheDocument()
    expect(within(gates).getByText("Say what happens to expired links.")).toBeInTheDocument()
  })

  test("lists the jobs of the goal with their task, worker and cost", async () => {
    const seen: string[] = []
    const [first, ...others] = fx.jobs
    server.use(
      http.get("/api/v1/jobs", ({ request }) => {
        seen.push(new URL(request.url).search)
        return HttpResponse.json([{ ...first, task_id: fx.goal.tasks[0]!.id }, ...others])
      }),
    )
    await renderApp("/goals/GOAL-3")
    const jobs = (await screen.findByRole("heading", { name: "Jobs" })).closest("section")!
    const row = (await within(jobs).findByRole("link", { name: /JOB-41 developer/ })).closest("tr")!
    expect(within(row).getByRole("link", { name: /JOB-41/ })).toHaveAttribute("href", "/jobs/JOB-41")
    expect(within(row).getByText("running")).toBeInTheDocument()
    expect(within(row).getByText("TASK-12")).toBeInTheDocument()
    await waitFor(() => expect(within(row).getByText("build-box-1")).toBeInTheDocument())
    expect(within(row).getByText("$0.12")).toBeInTheDocument()
    const queued = within(jobs).getByRole("link", { name: /JOB-42/ }).closest("tr")!
    expect(within(queued).getAllByText("none")).toHaveLength(2) // no task, no worker
    expect(seen[0]).toBe("?goal=GOAL-3&limit=500")
  })

  test("shows when the goal has no jobs, and an error", async () => {
    server.use(http.get("/api/v1/jobs", () => HttpResponse.json(fx.jobsEmpty)))
    const { unmount } = await renderApp("/goals/GOAL-3")
    expect(await screen.findByText("No jobs yet.")).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/jobs", errors.invalidInput))
    await renderApp("/goals/GOAL-3")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })

  test("decides the pending gates of the goal like the inbox does", async () => {
    const calls: { ref: string; decision: string; body: unknown }[] = []
    server.use(
      http.post("/api/v1/gates/:ref/:decision", async ({ params, request }) => {
        calls.push({ ref: String(params.ref), decision: String(params.decision), body: await request.json() })
        return HttpResponse.json(fx.gateApprove)
      }),
    )
    const { user } = await renderApp("/goals/GOAL-3")
    const waiting = (await screen.findByRole("heading", { name: "Waiting for you" })).closest("section")!
    const card = within(waiting).getByRole("article", { name: /GATE-9/ })
    expect(within(card).getByRole("button", { name: "Request changes" })).toBeInTheDocument()
    expect(within(card).getByRole("button", { name: "Reject" })).toBeInTheDocument()
    await user.type(within(card).getByLabelText("Note (optional)"), "Ship it")
    await user.click(within(card).getByRole("button", { name: "Approve" }))
    await waitFor(() => expect(calls).toEqual([{ ref: "GATE-9", decision: "approve", body: { feedback: "Ship it" } }]))
  })

  test("has no waiting section when no gate is pending", async () => {
    server.use(http.get("/api/v1/goals/:ref", () => HttpResponse.json({ ...fx.goal, gates: fx.goal.gates.filter((g) => g.status !== "pending") })))
    await renderApp("/goals/GOAL-3")
    await screen.findByRole("heading", { name: "Gates" })
    expect(screen.queryByRole("heading", { name: "Waiting for you" })).not.toBeInTheDocument()
  })

  test("copes with a goal without spec, tasks or gates", async () => {
    const { spec: _spec, ...rest } = fx.goal
    server.use(http.get("/api/v1/goals/:ref", () => HttpResponse.json({ ...rest, tasks: [], gates: [], url: undefined })))
    await renderApp("/goals/GOAL-3")
    expect(await screen.findByText(/No spec yet/)).toBeInTheDocument()
    expect(screen.getByText("No tasks yet.")).toBeInTheDocument()
    expect(screen.getByText("No gates.")).toBeInTheDocument()
  })

  test("shows a goal body with HTML as text", async () => {
    const detail = structuredClone(fx.goal)
    detail.goal.body = "Do <script>alert(1)</script> this"
    server.use(http.get("/api/v1/goals/:ref", () => HttpResponse.json(detail)))
    const { container } = await renderApp("/goals/GOAL-3")
    expect(await screen.findByText(/Do <script>alert\(1\)<\/script> this/)).toBeInTheDocument()
    expect(container.querySelector("script")).toBeNull()
  })

  test("omits the description of a goal without one", async () => {
    const detail = structuredClone(fx.goal)
    detail.goal.body = ""
    server.use(http.get("/api/v1/goals/:ref", () => HttpResponse.json(detail)))
    await renderApp("/goals/GOAL-3")
    await screen.findByText("Total cost $1.75")
    expect(screen.queryByRole("heading", { name: "Description" })).not.toBeInTheDocument()
  })

  test("shows each kind of check status", async () => {
    const detail = structuredClone(fx.goal)
    const change = detail.tasks[0]!.changes[0]!
    detail.tasks[0]!.changes = ["success", "pending", "none", "unknown"].map((checks, i) => ({ ...change, number: i + 20, checks }))
    server.use(http.get("/api/v1/goals/:ref", () => HttpResponse.json(detail)))
    await renderApp("/goals/GOAL-3")
    for (const text of ["checks passing", "checks running", "no checks", "checks unknown"]) {
      expect(await screen.findByText(text)).toBeInTheDocument()
    }
  })

  test("shows loading and error states", async () => {
    server.use(http.get("/api/v1/goals/:ref", errors.notFound))
    await renderApp("/goals/GOAL-99")
    expect(screen.getByRole("status")).toBeInTheDocument()
    expect(await screen.findByText(/GOAL-99: not found/)).toBeInTheDocument()
  })

  test("agent text in the spec is shown as text", async () => {
    const detail = structuredClone(fx.goal)
    detail.spec.body = "Hello <script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[click](javascript:alert(1))"
    server.use(http.get("/api/v1/goals/:ref", () => HttpResponse.json(detail)))
    const { container } = await renderApp("/goals/GOAL-3")
    expect(await screen.findByText(/Hello <script>alert\(1\)<\/script>/)).toBeInTheDocument()
    expect(container.querySelector("script, img")).toBeNull()
    expect(container.querySelector("a[href^='javascript']")).toBeNull()
  })
})
