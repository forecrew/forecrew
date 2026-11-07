import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const without = (...perms: string[]) => HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => !perms.includes(p)) })
const as = (...perms: string[]) => server.use(http.get("/api/v1/me", () => without(...perms)))

test("says in one sentence whether jobs have somewhere to run, and shows locations, waiting jobs and running jobs", async () => {
  await renderApp("/machines")
  expect(await screen.findByText(/2 jobs waiting: Only Mac Mini can run this/)).toBeInTheDocument()
  expect(screen.getByText("2 machines")).toBeInTheDocument()
  expect(screen.getByText("CHF 41 of 200")).toBeInTheDocument()
  const home = (await screen.findByRole("link", { name: "Home Server" })).closest("tr")!
  expect(home).toHaveTextContent("Online")
  expect(home).toHaveTextContent("2 of 3")
  expect(home).toHaveTextContent("Free")
  expect(screen.getByText("Mac Mini").closest("tr")).toHaveTextContent("Offline")
  // a computer that runs jobs directly is an ordinary row, with no link
  expect(screen.getByText("build-box-1").closest("tr")).toHaveTextContent("Linux, directly")
  expect(screen.queryByRole("link", { name: "build-box-1" })).toBeNull()
  const waiting = screen.getByRole("region", { name: "Waiting jobs table" })
  expect(waiting).toHaveTextContent("Only Mac Mini can run this, and it is offline.")
})

test("offers the fix for what only a person can fix, and nothing for what fixes itself", async () => {
  await renderApp("/machines")
  const waiting = await screen.findByRole("region", { name: "Waiting jobs table" })
  expect(within(waiting).getByRole("button", { name: "How to bring it back" })).toBeInTheDocument()
  expect(within(waiting).getByRole("link", { name: "Raise the limit" })).toHaveAttribute("href", "/settings/policies")
})

test("a member asks an admin instead of fixing, and a viewer sees only the sentence", async () => {
  as("worker.manage", "billing.manage")
  const { user } = await renderApp("/machines")
  const waiting = await screen.findByRole("region", { name: "Waiting jobs table" })
  expect(within(waiting).getAllByRole("button", { name: "Ask an admin" })).toHaveLength(2)
  expect(screen.queryByRole("link", { name: "Add machines" })).toBeNull()
  await user.click(within(waiting).getAllByRole("button", { name: "Ask an admin" })[0]!)
  expect(await screen.findByRole("region", { name: "Assistant" })).toBeInTheDocument()
  expect(screen.getByLabelText("Say what you want")).toHaveValue("Why is my location offline and how do I bring it back?")
})

test("a viewer has no fix buttons", async () => {
  as("worker.manage", "billing.manage", "config.apply")
  await renderApp("/machines")
  const waiting = await screen.findByRole("region", { name: "Waiting jobs table" })
  expect(within(waiting).queryAllByRole("button")).toHaveLength(0)
  expect(within(waiting).queryAllByRole("link")).toHaveLength(2) // only the two jobs
})

test("hides the spend tile while everything is free, and Waiting and Running when empty", async () => {
  server.use(http.get("/api/v1/fleet", () => HttpResponse.json({ ...fx.fleet, spend: undefined, queue: [], running: [], waiting: 0, working: 0, headline: "Everything is fine. 0 jobs running, none waiting." })))
  await renderApp("/machines")
  expect(await screen.findByText("Everything is fine. 0 jobs running, none waiting.")).toBeInTheDocument()
  expect(screen.queryByText("Spend this month")).toBeNull()
  expect(screen.queryByRole("heading", { name: "Waiting" })).toBeNull()
  expect(screen.queryByRole("heading", { name: "Running now" })).toBeNull()
})

test("an empty fleet shows the three ways to connect and the assistant", async () => {
  server.use(http.get("/api/v1/fleet", () => HttpResponse.json(fx.fleetEmpty)))
  await renderApp("/machines")
  expect(await screen.findByText("No machines yet. Agents need somewhere to work.")).toBeInTheDocument()
  const cards = screen.getByRole("list", { name: "What do you want to connect?" })
  expect(within(cards).getByRole("link", { name: /A computer or server/ })).toHaveAttribute("href", "/machines/add?kind=computer")
  expect(within(cards).getByRole("link", { name: /My AWS account/ })).toBeInTheDocument()
  expect(within(cards).queryByRole("link", { name: /Forecrew Cloud/ })).toBeNull()
  expect(screen.getByText(/Or just say what you have/)).toBeInTheDocument()
})

test("the machine types tab lists what can run, and a row opens its settings", async () => {
  const { user } = await renderApp("/machines?tab=types")
  const row = (await screen.findByRole("button", { name: "Linux Small" })).closest("tr")!
  expect(row).toHaveTextContent("Home Server")
  expect(row).toHaveTextContent("2 CPU · 4 GB")
  expect(screen.getByText("Linux Large").closest("tr")).toHaveTextContent("0.04/min")
  await user.click(screen.getByRole("button", { name: "Linux Small" }))
  const sheet = await screen.findByRole("dialog", { name: "Linux Small" })
  expect(sheet).toHaveTextContent("ubuntu-24-04")
  expect(within(sheet).getByRole("switch", { name: "On" })).toBeChecked()
})

test("a machine type is changed as a plan, and its budget is read-only without Manage budgets", async () => {
  server.use(http.get("/api/v1/config/resources", () => HttpResponse.json([{ kind: "shape", name: "linux-small", spec: { provider: "home-server", max: 3, enabled: true } }])))
  let body: { changes: { spec: Record<string, unknown> }[] } | undefined
  server.use(http.post("/api/v1/config/plans", async ({ request }) => ((body = (await request.json()) as typeof body), HttpResponse.json(fx.configPlan, { status: 201 }))))
  as("billing.manage")
  const { user } = await renderApp("/machines?tab=types")
  await user.click(await screen.findByRole("button", { name: "Linux Small" }))
  const sheet = await screen.findByRole("dialog", { name: "Linux Small" })
  await user.click(within(sheet).getByRole("button", { name: "More options" }))
  expect(within(sheet).getByLabelText("Monthly limit")).toBeDisabled()
  expect(within(sheet).getByText("Needs Manage budgets to change.")).toBeInTheDocument()
  await user.clear(within(sheet).getByLabelText("Machines at once"))
  await user.type(within(sheet).getByLabelText("Machines at once"), "5")
  await user.click(within(sheet).getByRole("button", { name: "Review the change" }))
  expect(await within(sheet).findByRole("region", { name: /Change plan/ })).toBeInTheDocument()
  expect(body?.changes[0]?.spec).toMatchObject({ max: 5, enabled: true, provider: "home-server" })
  expect(body?.changes[0]?.spec).not.toHaveProperty("monthly_budget")
})

test("machine types of Forecrew Cloud and for a member are read-only", async () => {
  as("worker.manage")
  const { user } = await renderApp("/machines?tab=types")
  await user.click(await screen.findByRole("button", { name: "Linux Small" }))
  const sheet = await screen.findByRole("dialog", { name: "Linux Small" })
  expect(within(sheet).getByRole("switch", { name: "On" })).toHaveAttribute("aria-disabled", "true")
  expect(within(sheet).queryByRole("button", { name: "Review the change" })).toBeNull()
})

test("the machine types tab has an empty state", async () => {
  server.use(http.get("/api/v1/shapes", () => HttpResponse.json([])))
  await renderApp("/machines?tab=types")
  expect(await screen.findByText(/Machine types appear when you add a location/)).toBeInTheDocument()
})

test("a failing overview shows the error", async () => {
  server.use(http.get("/api/v1/fleet", errors.invalidInput))
  await renderApp("/machines")
  expect(await screen.findByText("Could not load this")).toBeInTheDocument()
})

test("the old workers address leads to Machines", async () => {
  const { router } = await renderApp("/workers")
  await waitFor(() => expect(router.state.location.pathname).toBe("/machines"))
})

test("a location shows its checks as plain text, its limits and its types", async () => {
  await renderApp("/machines/018f0000-0000-7000-8000-000000000700")
  expect(await screen.findByRole("heading", { name: "Home Server" })).toBeInTheDocument()
  expect(screen.getByText(/Linux · 16 CPU · 64 GB/)).toBeInTheDocument()
  expect(screen.getByRole("heading", { name: "Checks" })).toBeInTheDocument()
  expect(screen.getByText("Available")).toBeInTheDocument()
  expect(screen.getByLabelText("Machines at once")).toHaveValue("3")
  expect(screen.getByLabelText("Monthly limit")).toBeEnabled() // the Owner may manage budgets
  expect(screen.getByText("Linux Small")).toBeInTheDocument()
})

test("a location is paused with a plan", async () => {
  let body: { changes: { spec: { paused?: boolean } }[] } | undefined
  server.use(http.post("/api/v1/config/plans", async ({ request }) => ((body = (await request.json()) as typeof body), HttpResponse.json(fx.configPlan, { status: 201 }))))
  const { user } = await renderApp("/machines/018f0000-0000-7000-8000-000000000700")
  await user.click(await screen.findByRole("button", { name: "Pause" }))
  expect(await screen.findByRole("region", { name: /Change plan/ })).toBeInTheDocument()
  expect(body?.changes[0]?.spec.paused).toBe(true)
})

test("an offline location says when it was seen and what to try", async () => {
  await renderApp("/machines/018f0000-0000-7000-8000-000000000701")
  expect(await screen.findByText("This location is offline")).toBeInTheDocument()
  expect(screen.getByRole("alert")).toHaveTextContent("Last seen 4 h ago")
})

test("a location another person may only read has no controls", async () => {
  as("worker.manage", "billing.manage")
  await renderApp("/machines/018f0000-0000-7000-8000-000000000700")
  await screen.findByRole("heading", { name: "Home Server" })
  expect(screen.queryByRole("button", { name: "Pause" })).toBeNull()
  expect(screen.queryByRole("button", { name: "Review the change" })).toBeNull()
  expect(screen.getByLabelText("Machines at once")).toBeDisabled()
})

test("a location is removed with a plan that says so", async () => {
  let body: { changes: { delete?: boolean }[] } | undefined
  server.use(http.post("/api/v1/config/plans", async ({ request }) => ((body = (await request.json()) as typeof body), HttpResponse.json(fx.configPlan, { status: 201 }))))
  const { user } = await renderApp("/machines/018f0000-0000-7000-8000-000000000700")
  await user.click(await screen.findByRole("button", { name: "Remove location…" }))
  await screen.findByRole("region", { name: /Change plan/ })
  expect(body?.changes[0]?.delete).toBe(true)
})

test("an unknown location says so", async () => {
  await renderApp("/machines/nope")
  expect(await screen.findByText("Location not found")).toBeInTheDocument()
})

test("a location managed by the server file says so and cannot be changed", async () => {
  server.use(http.get("/api/v1/config/resources", () => HttpResponse.json([{ kind: "provider", name: "home-server", origin: "file", spec: { kind: "microvm", limits: { max_machines: 3 } } }])))
  await renderApp("/machines/018f0000-0000-7000-8000-000000000700")
  expect(await screen.findByText("Managed by server file")).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Pause" })).toBeNull()
})
