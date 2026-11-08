import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

function plans(plan: object = fx.configPlanModelAuthorization) {
  const bodies: { title: string; changes: { kind: string; name: string; spec?: Record<string, unknown>; delete?: boolean }[] }[] = []
  server.use(
    http.post("/api/v1/config/plans", async ({ request }) => {
      bodies.push((await request.json()) as (typeof bodies)[number])
      return HttpResponse.json(plan, { status: 201 })
    }),
  )
  return bodies
}

const section = () => screen.findByRole("region", { name: "Setup · AI models" })

describe("the project's AI models in Setup", () => {
  test("shows what it inherits and its own choices", async () => {
    await renderApp("/projects/app")
    const s = await section()
    const table = await within(s).findByRole("region", { name: "Models of the project table" })
    const rows = within(table).getAllByRole("row")
    expect(rows.find((r) => r.textContent?.startsWith("Developer"))).toHaveTextContent("claude-sonnet-4-5 on claudeOrganization")
    expect(rows.find((r) => r.textContent?.startsWith("Reviewer"))).toHaveTextContent("no fast model on home-ollamaThis project")
    expect(within(s).getByRole("link", { name: "Organization settings, AI models" })).toHaveAttribute("href", "/settings/models")
  })

  test("says which connections may receive its code, and authorizes one with the second confirmation", async () => {
    const bodies = plans()
    const { user } = await renderApp("/projects/app")
    const s = await section()
    const list = await within(s).findByRole("list", { name: "Who may receive the code" })
    const items = within(list).getAllByRole("listitem")
    expect(items[0]).toHaveTextContent("Server file: allowed")
    expect(items[1]).toHaveTextContent("Allowed: the code of acme/app goes to DeepSeek API (China)")
    expect(items[2]).toHaveTextContent("Local: the code stays on your network")
    // Local only by the server's flag: an Ollama at a public address needs authorizing.
    expect(items[3]).toHaveTextContent("Not allowed: jobs with private code on it wait")
    expect(within(items[3]!).getByRole("button", { name: "Authorize" })).toBeInTheDocument()
    await user.click(within(items[1]!).getByRole("button", { name: "Revoke" }))
    await waitFor(() => expect(bodies[0]?.changes).toEqual([{ kind: "model_authorization", name: "deepseek@app", delete: true }]))
    const card = await screen.findByRole("region", { name: "Change plan: Let deepseek receive the code of app" })
    expect(within(card).getByText("The code of acme/app goes to DeepSeek API (in CN) through connection deepseek.")).toBeInTheDocument()
    expect(within(card).getByText("Sends the private code of project app through connection deepseek")).toBeInTheDocument()
  })

  test("authorizes a connection that may not receive the code yet", async () => {
    server.use(http.get("/api/v1/config/resources", ({ request }) => {
      const kind = new URL(request.url).searchParams.get("kind")
      return HttpResponse.json(kind === "model_connection" ? fx.configResourcesModelConnection : kind === "model_assignment" ? fx.configResourcesModelAssignment : [])
    }))
    const bodies = plans()
    const { user } = await renderApp("/projects/app")
    const s = await section()
    const list = await within(s).findByRole("list", { name: "Who may receive the code" })
    const deepseek = within(list).getAllByRole("listitem")[1]!
    expect(deepseek).toHaveTextContent("Not allowed: jobs with private code on it wait")
    await user.click(within(deepseek).getByRole("button", { name: "Authorize" }))
    await waitFor(() => expect(bodies[0]?.changes).toEqual([{ kind: "model_authorization", name: "deepseek@app", spec: {} }]))
  })

  test("chooses a model for the project and puts a role back on the organization's", async () => {
    const bodies = plans(fx.configPlan)
    const { user } = await renderApp("/projects/app")
    const s = await section()
    const form = await within(s).findByRole("form", { name: "Choose a model for this project" })
    await user.selectOptions(within(form).getByLabelText("Agent"), "developer")
    await user.selectOptions(within(form).getByLabelText("Provider"), "deepseek")
    await user.click(within(form).getByRole("button", { name: "Review the change" }))
    await waitFor(() =>
      expect(bodies[0]?.changes).toEqual([
        { kind: "model_assignment", name: "project:app", spec: { roles: { reviewer: { connection: "home-ollama", tier: "fast" }, developer: { connection: "deepseek", tier: "strong" } } } },
      ]),
    )
    await user.click(screen.getByRole("button", { name: "Edit" }))
    await user.click(within(s).getByRole("button", { name: "Undo: use the organization's model for Reviewer" }))
    await waitFor(() => expect(bodies[1]?.changes).toEqual([{ kind: "model_assignment", name: "project:app", delete: true }]))
  })

  test("someone who may not assign reads, with the controls disabled or gone", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meViewer)))
    await renderApp("/projects/app")
    const s = await section()
    await within(s).findByRole("list", { name: "Who may receive the code" })
    expect(within(s).queryByRole("button", { name: "Revoke" })).toBeNull()
    expect(within(s).getByRole("button", { name: "Choose a model for this project" })).toBeDisabled()
    expect(within(s).getByText(/Authorizing or revoking needs/)).toBeInTheDocument()
  })

  test("is absent without models.read", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => !p.startsWith("models.")) })))
    await renderApp("/projects/app")
    await screen.findByRole("heading", { name: "Profile" })
    expect(screen.queryByRole("region", { name: "Setup · AI models" })).toBeNull()
  })

  test("someone who may assign the project's models but not read the organization's is told why", async () => {
    const appID = fx.project.id
    server.use(
      http.get("/api/v1/me", () =>
        HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => !p.startsWith("models.")), projects: { [appID]: ["models.assign"] } }),
      ),
    )
    await renderApp("/projects/app")
    const s = await section()
    expect(within(s).getByText(/you cannot see them: that needs/)).toBeInTheDocument()
    expect(within(s).queryByRole("list", { name: "Who may receive the code" })).toBeNull()
  })

  test("says when the organization keeps projects on its models", async () => {
    server.use(http.get("/api/v1/config/resources", ({ request }) => {
      const kind = new URL(request.url).searchParams.get("kind")
      if (kind === "model_assignment") return HttpResponse.json([{ kind, name: "organization", spec: { project_overrides: false, roles: {} }, origin: "database" }])
      return HttpResponse.json(kind === "model_connection" ? fx.configResourcesModelConnection : [])
    }))
    await renderApp("/projects/app")
    const s = await section()
    expect(await within(s).findByText("The organization keeps every project on its own models.")).toBeInTheDocument()
  })
})
