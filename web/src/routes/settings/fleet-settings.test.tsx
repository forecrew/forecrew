import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const as = (...perms: string[]) => server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => !perms.includes(p)) })))
const plan = (calls: { body?: unknown }) => http.post("/api/v1/config/plans", async ({ request }) => ((calls.body = await request.json()), HttpResponse.json(fx.configPlan, { status: 201 })))

describe("policies", () => {
  test("reads like sentences with one control each", async () => {
    await renderApp("/settings/policies")
    expect(await screen.findByLabelText("Jobs run on")).toHaveValue("own_only")
    expect(screen.getByLabelText("Run at most (machines at once)")).toHaveValue("20")
    expect(screen.getByLabelText("Stop at (a month)")).toHaveValue("200")
    expect(screen.getByLabelText("Machines can also reach")).toHaveValue("registry.acme.test")
    expect(screen.getByRole("link", { name: "Policies" })).toBeInTheDocument()
  })

  test("a change is a plan, and unchanged limits are kept", async () => {
    const calls: { body?: unknown } = {}
    server.use(plan(calls))
    const { user } = await renderApp("/settings/policies")
    await user.selectOptions(await screen.findByLabelText("Jobs run on"), "own_first")
    await user.click(screen.getByRole("button", { name: "Review the change" }))
    expect(await screen.findByRole("region", { name: /Change plan/ })).toBeInTheDocument()
    expect(calls.body).toMatchObject({ source: "form", changes: [{ kind: "fleet_policy", name: "fleet", spec: { routing: "own_first", quotas: { concurrent_machines: 20, monthly_budget: 200 }, egress: ["registry.acme.test"], plan: "free" } }] })
  })

  test("an Admin sees the budgets read-only, and a plan from them leaves the budgets alone", async () => {
    const calls: { body?: { changes: { spec: { quotas: Record<string, number> } }[] } } = {}
    server.use(plan(calls))
    as("billing.manage")
    const { user } = await renderApp("/settings/policies")
    expect(await screen.findByLabelText("Stop at (a month)")).toBeDisabled()
    expect(screen.getByLabelText("Run at most (machines at once)")).toBeDisabled()
    expect(screen.getAllByText("Needs Manage budgets to change.")).toHaveLength(2)
    await user.selectOptions(screen.getByLabelText("Jobs run on"), "hosted")
    await user.click(screen.getByRole("button", { name: "Review the change" }))
    await screen.findByRole("region", { name: /Change plan/ })
    expect(calls.body?.changes[0]?.spec.quotas).toEqual({ concurrent_machines: 20, monthly_budget: 200 })
  })

  test("Billing changes budgets and nothing else", async () => {
    const calls: { body?: { changes: { spec: { quotas: Record<string, number>; routing: string } }[] } } = {}
    server.use(plan(calls))
    as("worker.manage", "worker.read")
    const { user } = await renderApp("/settings/policies")
    expect(await screen.findByLabelText("Jobs run on")).toBeDisabled()
    await user.clear(screen.getByLabelText("Stop at (a month)"))
    await user.type(screen.getByLabelText("Stop at (a month)"), "300")
    await user.click(screen.getByRole("button", { name: "Review the change" }))
    await screen.findByRole("region", { name: /Change plan/ })
    expect(calls.body?.changes[0]?.spec.quotas.monthly_budget).toBe(300)
    expect(calls.body?.changes[0]?.spec.routing).toBe("own_only")
  })

  test("a member reads it and has no button", async () => {
    as("worker.manage", "billing.manage")
    await renderApp("/settings/policies")
    expect(await screen.findByLabelText("Jobs run on")).toBeDisabled()
    expect(screen.queryByRole("button", { name: "Review the change" })).toBeNull()
  })

  test("a setting from the server file says so", async () => {
    server.use(http.get("/api/v1/config/resources", () => HttpResponse.json([{ ...fx.configResourcesFleetPolicy[0], origin: "file" }])))
    await renderApp("/settings/policies")
    expect(await screen.findByText(/Managed by server file/)).toBeInTheDocument()
    expect(screen.getByLabelText("Jobs run on")).toBeDisabled()
  })

  test("trust zones list the machine types of each zone in the words of the interface", async () => {
    const { user } = await renderApp("/settings/policies")
    await user.click(await screen.findByRole("button", { name: "Trust zones" }))
    expect((await screen.findByText("Reasoning", { selector: "span" })).closest("li")).toHaveTextContent("Linux Reasoning")
    expect(screen.getByText("Build", { selector: "span" }).closest("li")).toHaveTextContent("Linux Small")
  })

  test("a rejected plan says why, in place", async () => {
    server.use(http.post("/api/v1/config/plans", () => HttpResponse.json({ error: { code: "invalid_input", message: "egress host is not valid" } }, { status: 400 })))
    const { user } = await renderApp("/settings/policies")
    await user.click(await screen.findByRole("button", { name: "Review the change" }))
    expect(await screen.findByText("egress host is not valid")).toBeInTheDocument()
  })
})

describe("change history", () => {
  test("lists the plans by day with who, where from, state and whether they can be reverted", async () => {
    await renderApp("/settings/history")
    const row = (await screen.findByRole("button", { name: "Add machine types to Home server" })).closest("li")!
    expect(row).toHaveTextContent("assistant")
    expect(row).toHaveTextContent("Applied")
    expect(row).toHaveTextContent("Can revert")
    expect(screen.getByRole("button", { name: "Raise budget to 200" }).closest("li")).toHaveTextContent("Reverted")
    expect(screen.getByRole("button", { name: "Add Windows test machine type" }).closest("li")).toHaveTextContent("Claude Desktop")
    expect(screen.getByRole("tab", { name: "All activity" })).toBeInTheDocument()
  })

  test("a row opens the plan as it was confirmed, with YAML before and after", async () => {
    const { user } = await renderApp("/settings/history")
    await user.click(await screen.findByRole("button", { name: "Add machine types to Home server" }))
    const sheet = await screen.findByRole("dialog", { name: "Add machine types to Home server" })
    expect(sheet).toHaveTextContent("Add my Linux server")
    expect(sheet).toHaveTextContent("shape/linux-small, shape/linux-medium, provider/home-server")
    expect(within(sheet).getByRole("heading", { name: "YAML before" }).nextElementSibling).toHaveTextContent("limits:")
    expect(within(sheet).getByRole("heading", { name: "YAML after" }).nextElementSibling).toHaveTextContent("kind: shape")
  })

  test("revert makes a new plan, shown in the same card, and says what cannot come back", async () => {
    const { user } = await renderApp("/settings/history")
    await user.click(await screen.findByRole("button", { name: "Add machine types to Home server" }))
    const sheet = await screen.findByRole("dialog")
    await user.click(within(sheet).getByRole("button", { name: "Revert" }))
    const card = await within(sheet).findByRole("region", { name: "Change plan: Revert: Add machine types to Home server" })
    expect(card).toHaveTextContent("A machine that was destroyed does not come back.")
    expect(within(sheet).queryByRole("button", { name: "Revert" })).toBeNull()
  })

  test("a failing revert says why", async () => {
    server.use(http.post("/api/v1/config/plans/:plan/revert", () => HttpResponse.json({ error: { code: "conflict", message: "moved since" } }, { status: 409 })))
    const { user } = await renderApp("/settings/history")
    await user.click(await screen.findByRole("button", { name: "Add machine types to Home server" }))
    await user.click(await screen.findByRole("button", { name: "Revert" }))
    expect(await screen.findByText("moved since")).toBeInTheDocument()
  })

  test("a member sees their own changes and no audit log, and cannot revert", async () => {
    as("audit.read", "worker.manage")
    const { user } = await renderApp("/settings/history")
    await user.click(await screen.findByRole("button", { name: "Add machine types to Home server" }))
    expect(screen.queryByRole("tab", { name: "All activity" })).toBeNull()
    expect(screen.queryByRole("button", { name: "Revert" })).toBeNull()
  })

  test("has an empty state", async () => {
    server.use(http.get("/api/v1/config/plans", () => HttpResponse.json(fx.configPlansEmpty)))
    await renderApp("/settings/history")
    expect(await screen.findByText(/No changes yet/)).toBeInTheDocument()
  })

  test("exports everything as YAML and imports YAML as a plan", async () => {
    let imported: unknown
    server.use(http.post("/api/v1/config/import", async ({ request }) => ((imported = await request.json()), HttpResponse.json(fx.configPlan, { status: 201 }))))
    const { user } = await renderApp("/settings/history")
    await user.click(await screen.findByRole("button", { name: "Export YAML" }))
    expect(await screen.findByText(/name: home-server/)).toBeInTheDocument()
    await user.keyboard("{Escape}")
    await user.click(screen.getByRole("button", { name: "Import" }))
    const dialog = await screen.findByRole("dialog", { name: "Import YAML" })
    await user.click(within(dialog).getByLabelText("YAML"))
    await user.paste("kind: provider")
    await user.click(within(dialog).getByRole("button", { name: "Make a plan" }))
    expect(await within(dialog).findByRole("region", { name: /Change plan/ })).toBeInTheDocument()
    await waitFor(() => expect(imported).toEqual({ yaml: "kind: provider" }))
  })

  test("the audit log is the other tab", async () => {
    await renderApp("/settings/history?tab=activity")
    expect(await screen.findByRole("tab", { name: "All activity", selected: true })).toBeInTheDocument()
  })
})
