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
    await user.selectOptions(await screen.findByLabelText("Project"), "app")
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
