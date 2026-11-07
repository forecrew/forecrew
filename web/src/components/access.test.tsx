import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const asMe = (me: Record<string, unknown>) => server.use(http.get("/api/v1/me", () => HttpResponse.json(me)))
const project = fx.project.id
// An Admin who is also project admin of app.
const projectAdmin = { ...fx.meAdmin, permissions: fx.meAdmin.permissions.filter((p) => p !== "project.manage"), projects: { [project]: ["project.manage", "project.read"] } }

describe("project access", () => {
  test("shows who has which role, directly or through a team, and who reaches it through the organization", async () => {
    asMe(fx.meAdmin)
    await renderApp("/projects/app")
    const section = (await screen.findByRole("heading", { name: "Access" })).closest("section")!
    const table = await within(section).findByRole("table")
    const marta = within(table).getByText("Marta Rossi").closest("tr")!
    expect(within(marta).getByText("maintainer")).toBeInTheDocument()
    expect(within(marta).getByText("direct")).toBeInTheDocument()
    const team = within(table).getByText("Mobile").closest("tr")!
    expect(within(team).getByText("through team Mobile")).toBeInTheDocument()
    const owner = within(table).getByText("Gianmaria").closest("tr")!
    expect(within(owner).getByText("through organization role")).toBeInTheDocument()
    expect(within(owner).queryByRole("button")).not.toBeInTheDocument() // read-only
  })

  test("adds and removes access for someone who manages the project", async () => {
    asMe(fx.meAdmin)
    const calls: string[] = []
    server.use(
      http.post("/api/v1/role-bindings", async ({ request }) => {
        calls.push(`add ${JSON.stringify(await request.json())}`)
        return HttpResponse.json(fx.roleBindingCreate, { status: 201 })
      }),
      http.delete("/api/v1/role-bindings/:id", ({ params }) => {
        calls.push(`remove ${String(params.id)}`)
        return HttpResponse.json(fx.roleBindingDelete)
      }),
    )
    const { user } = await renderApp("/projects/app")
    const form = await screen.findByRole("form", { name: "Add access" })
    await within(form).findByRole("option", { name: "Marta Rossi" })
    await user.selectOptions(within(form).getByLabelText("Who"), fx.members[1]!.user_id)
    await user.selectOptions(within(form).getByLabelText("Role on this project"), "viewer")
    await user.click(within(form).getByRole("button", { name: "Add access" }))
    await user.selectOptions(within(form).getByLabelText("Add"), "team")
    await within(form).findByRole("option", { name: "Mobile" })
    await user.selectOptions(within(form).getByLabelText("Who"), "mobile")
    await user.click(within(form).getByRole("button", { name: "Add access" }))
    await user.selectOptions(within(form).getByLabelText("Add"), "service_account")
    await within(form).findByRole("option", { name: "ci" })
    await user.click(within(form).getByRole("button", { name: "Add access" }))
    await user.click(await screen.findByRole("button", { name: "Remove maintainer of Marta Rossi" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Remove access" }))
    await waitFor(() => expect(calls).toHaveLength(4))
    expect(calls).toEqual([
      `add {"subject_kind":"user","subject":"${fx.members[1]!.user_id}","role":"viewer","project":"app"}`,
      'add {"subject_kind":"team","subject":"mobile","role":"viewer","project":"app"}',
      'add {"subject_kind":"service_account","subject":"ci","role":"viewer","project":"app"}',
      `remove ${fx.roleBindings[1]!.id}`,
    ])
  })

  test("a refused addition is shown", async () => {
    asMe(fx.meAdmin)
    server.use(http.post("/api/v1/role-bindings", errors.conflict))
    const { user } = await renderApp("/projects/app")
    await user.click(within(await screen.findByRole("form", { name: "Add access" })).getByRole("button", { name: "Add access" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("already approved")
  })

  test("a Viewer reads the access and has no controls", async () => {
    asMe({ ...fx.meViewer, projects: { [project]: ["project.read"] }, permissions: ["org.read", "project.read"] })
    await renderApp("/projects/app")
    const section = (await screen.findByRole("heading", { name: "Access" })).closest("section")!
    await within(section).findByRole("table")
    expect(within(section).queryByRole("button")).not.toBeInTheDocument()
    expect(screen.queryByRole("form", { name: "Add access" })).not.toBeInTheDocument()
  })

  test("a project admin without org.read manages access but cannot list people", async () => {
    asMe({ ...projectAdmin, permissions: [] })
    await renderApp("/projects/app")
    expect(await screen.findByText(/you need to see the organization's people/)).toBeInTheDocument()
    const section = screen.getByRole("heading", { name: "Access" }).closest("section")!
    expect(await within(section).findByRole("button", { name: "Remove maintainer of Marta Rossi" })).toBeInTheDocument()
    expect(within(section).queryByText("through organization role")).not.toBeInTheDocument()
  })

  test("with nothing to show it says who can see the project", async () => {
    asMe({ ...fx.meViewer, projects: { [project]: ["project.read"] }, permissions: ["project.read"] })
    server.use(http.get("/api/v1/role-bindings", () => HttpResponse.json([])))
    await renderApp("/projects/app")
    expect(await screen.findByText("Only organization admins can see this project.")).toBeInTheDocument()
  })
})

describe("permission-aware pages", () => {
  test("a Viewer is not offered what it may not do", async () => {
    asMe(fx.meViewer)
    await renderApp("/projects")
    await screen.findByRole("heading", { name: "Projects" })
    expect(screen.queryByRole("link", { name: "Onboard repository" })).not.toBeInTheDocument()
  })

  test("a Viewer has no New goal, no Start a council, no ad-hoc job and no Cancel", async () => {
    asMe({ ...fx.meViewer, projects: { [project]: ["project.read", "job.read", "goal.read"] } })
    const { unmount } = await renderApp("/goals")
    await screen.findByRole("heading", { name: "Goals" })
    expect(screen.queryByRole("link", { name: "New goal" })).not.toBeInTheDocument()
    unmount()
    const councils = await renderApp("/councils")
    await screen.findByRole("heading", { name: "Councils" })
    expect(screen.queryByRole("link", { name: "Start a council" })).not.toBeInTheDocument()
    councils.unmount()
    const jobs = await renderApp("/jobs")
    await screen.findByText("JOB-41")
    expect(screen.queryByRole("button", { name: /Cancel JOB/ })).not.toBeInTheDocument()
    jobs.unmount()
    await renderApp("/projects/app")
    expect(await screen.findByText(/Dispatching a job needs/)).toHaveTextContent("job.dispatch")
    expect(screen.queryByRole("button", { name: "Dispatch job" })).not.toBeInTheDocument()
  })

  test("the Home page of a Viewer has no New goal", async () => {
    asMe(fx.meViewer)
    await renderApp("/")
    await screen.findByRole("heading", { name: "Home" })
    expect(screen.queryByRole("link", { name: "New goal" })).not.toBeInTheDocument()
  })

  test("a Contributor may create goals, start councils and cancel jobs on its project", async () => {
    asMe(fx.meContributor)
    const { unmount } = await renderApp("/goals")
    expect(await screen.findByRole("link", { name: "New goal" })).toBeInTheDocument()
    unmount()
    await renderApp("/jobs")
    expect(await screen.findByRole("button", { name: "Cancel JOB-41" })).toBeInTheDocument()
  })

  test("a decision the caller may read but not resolve has no approve buttons and ignores the keys", async () => {
    server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gates.map((g) => ({ ...g, can_resolve: false })))))
    const { user } = await renderApp("/inbox")
    const gate = await screen.findByRole("article", { name: /GATE-9/ })
    expect(within(gate).getByText(/your role does not let you decide it/)).toBeInTheDocument()
    expect(within(gate).queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
    expect(within(gate).queryByLabelText("Note (optional)")).not.toBeInTheDocument()
    await user.keyboard("a")
    await user.keyboard("r")
    expect(screen.queryByLabelText("What should change?")).not.toBeInTheDocument()
  })

  test("a decision the caller may resolve has its buttons", async () => {
    await renderApp("/inbox")
    const gate = await screen.findByRole("article", { name: /GATE-9/ })
    expect(within(gate).getByRole("button", { name: "Approve" })).toBeInTheDocument()
  })
})
