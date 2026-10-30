import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

describe("project list", () => {
  test("lists projects with repository and status", async () => {
    await renderApp("/projects")
    const row = (await screen.findByRole("link", { name: "app" })).closest("tr")!
    expect(within(row).getByRole("link", { name: /acme\/app/ })).toHaveAttribute("href", "https://github.com/acme/app")
    expect(within(row).getByText("onboarded")).toBeInTheDocument()
    expect(screen.getByText("onboarding")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "Onboard repository" })).toHaveAttribute("href", "/projects/new")
  })

  test("shows empty and error states", async () => {
    server.use(http.get("/api/v1/projects", () => HttpResponse.json(fx.projectsEmpty)))
    const { unmount } = await renderApp("/projects")
    expect(await screen.findByText(/No projects yet/)).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/projects", errors.invalidInput))
    await renderApp("/projects")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })
})

describe("onboard repository form", () => {
  test("onboards owner/name and opens the project", async () => {
    let body: unknown
    server.use(
      http.post("/api/v1/projects", async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(fx.projectOnboard, { status: 202 })
      }),
    )
    const { user, router } = await renderApp("/projects/new")
    await user.type(await screen.findByLabelText("Repository"), " acme/docs ")
    await user.click(screen.getByRole("button", { name: "Onboard repository" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/projects/docs"))
    expect(body).toEqual({ repo: "acme/docs" })
  })

  test.each(["", "acme", "acme/", "a b/c", "https://github.com/acme/app", "acme/app/extra"])("refuses %j", async (value) => {
    let called = false
    server.use(http.post("/api/v1/projects", () => ((called = true), HttpResponse.json(fx.projectOnboard))))
    const { user } = await renderApp("/projects/new")
    const input = await screen.findByLabelText("Repository")
    if (value) await user.type(input, value)
    await user.click(screen.getByRole("button", { name: "Onboard repository" }))
    expect(screen.getByText(/Enter the repository as owner\/name/)).toBeInTheDocument()
    expect(input).toHaveAttribute("aria-invalid", "true")
    expect(called).toBe(false)
  })

  test("shows the server's refusal", async () => {
    server.use(http.post("/api/v1/projects", errors.conflict))
    const { user } = await renderApp("/projects/new")
    await user.type(await screen.findByLabelText("Repository"), "acme/app")
    await user.click(screen.getByRole("button", { name: "Onboard repository" }))
    expect(await screen.findByText(/already approved/)).toBeInTheDocument()
  })
})

describe("project page", () => {
  test("shows the approved profile, goals and the ad-hoc job form", async () => {
    await renderApp("/projects/app")
    expect(await screen.findByRole("heading", { name: "app", level: 1 })).toBeInTheDocument()
    expect(screen.getByText("onboarded")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: /acme\/app/ })).toHaveAttribute("href", "https://github.com/acme/app")
    expect(screen.getByText("A web app with a Go API.")).toBeInTheDocument()
    expect(screen.getByText("Go, TypeScript")).toBeInTheDocument()
    expect(screen.getAllByText("linux, macos").length).toBeGreaterThan(0) // project platforms and a component

    const components = screen.getByRole("region", { name: "Components table" })
    const root = within(components).getByText("go build ./...").closest("tr")!
    expect(within(root).getByText("go test ./...")).toBeInTheDocument()
    expect(within(root).getAllByText("go")).toHaveLength(2) // the stack and the needs tag
    const web = within(components).getByText("pnpm build").closest("tr")!
    expect(within(web).getAllByText("node")).toHaveLength(2)

    expect(await screen.findByRole("link", { name: /GOAL-3 Sign in/ })).toHaveAttribute("href", "/goals/GOAL-3")
    expect(screen.getByRole("heading", { name: "Run ad-hoc job" })).toBeInTheDocument()
  })

  test("says when there is no profile yet", async () => {
    const { profile: _profile, ...bare } = fx.project
    server.use(http.get("/api/v1/projects/*", () => HttpResponse.json({ ...bare, status: "onboarding" })))
    const { unmount } = await renderApp("/projects/docs")
    expect(await screen.findByText(/Onboarding is running/)).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/projects/*", () => HttpResponse.json({ ...bare, status: "registered" })))
    await renderApp("/projects/docs")
    expect(await screen.findByText("No approved profile yet.")).toBeInTheDocument()
  })

  test("shows a profile without components and no goals", async () => {
    server.use(
      http.get("/api/v1/projects/*", () => HttpResponse.json(fx.projectMinimalProfile)),
      http.get("/api/v1/goals", () => HttpResponse.json(fx.goalsEmpty)),
    )
    await renderApp("/projects/app")
    expect(await screen.findByText("Small.")).toBeInTheDocument()
    expect(screen.queryByRole("region", { name: "Components table" })).not.toBeInTheDocument()
    expect(await screen.findByText("No goals for this project.")).toBeInTheDocument()
  })

  test("shows unknown commands as such", async () => {
    server.use(
      http.get("/api/v1/projects/*", () => HttpResponse.json(fx.projectUnknownCommands)),
    )
    await renderApp("/projects/app")
    expect(await screen.findAllByText("unknown")).toHaveLength(2)
  })

  test("shows an error for an unknown project", async () => {
    server.use(http.get("/api/v1/projects/*", errors.notFound))
    await renderApp("/projects/nope")
    expect(await screen.findByText(/GOAL-99: not found/)).toBeInTheDocument()
  })

  test("requests the project by owner/name too", async () => {
    let path = ""
    server.use(
      http.get("/api/v1/projects/*", ({ request }) => {
        path = new URL(request.url).pathname
        return HttpResponse.json(fx.project)
      }),
    )
    await renderApp("/projects/acme%2Fapp")
    await screen.findByRole("heading", { name: "app", level: 1 })
    expect(path).toBe("/api/v1/projects/acme/app")
  })

  describe("ad-hoc job form", () => {
    test("dispatches a job and opens it", async () => {
      let body: unknown
      server.use(
        http.post("/api/v1/jobs", async ({ request }) => {
          body = await request.json()
          return HttpResponse.json(fx.jobDispatch, { status: 201 })
        }),
      )
      const { user, router } = await renderApp("/projects/app")
      await screen.findByRole("heading", { name: "app", level: 1 })
      expect(screen.getByLabelText("Role")).toHaveValue("reviewer")
      await user.type(screen.getByLabelText("Prompt"), "Look at the login flow.")
      await user.type(screen.getByLabelText("Needs"), "linux, docker")
      await user.click(screen.getByRole("button", { name: "Run job" }))
      await waitFor(() => expect(router.state.location.pathname).toBe("/jobs/JOB-42"))
      expect(body).toEqual({ project: "app", role: "reviewer", prompt: "Look at the login flow.", needs: ["linux", "docker"] })
    })

    test("needs a role and a prompt", async () => {
      let called = false
      server.use(http.post("/api/v1/jobs", () => ((called = true), HttpResponse.json(fx.jobDispatch))))
      const { user } = await renderApp("/projects/app")
      await screen.findByRole("heading", { name: "app", level: 1 })
      await user.clear(screen.getByLabelText("Role"))
      await user.click(screen.getByRole("button", { name: "Run job" }))
      expect(screen.getByText("Enter a role.")).toBeInTheDocument()
      expect(screen.getByText("Tell the job what to do.")).toBeInTheDocument()
      expect(called).toBe(false)
    })

    test("shows the server's refusal", async () => {
      server.use(http.post("/api/v1/jobs", errors.invalidInput))
      const { user } = await renderApp("/projects/app")
      await screen.findByRole("heading", { name: "app", level: 1 })
      await user.type(screen.getByLabelText("Prompt"), "x")
      await user.click(screen.getByRole("button", { name: "Run job" }))
      expect(await screen.findByRole("alert")).toHaveTextContent("feedback is required")
    })
  })
})
