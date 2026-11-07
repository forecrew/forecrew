import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import type { ConfigPlan } from "@/api/types"
import { PlanCard } from "@/components/PlanCard"
import { fx, server } from "@/test/server"
import { renderPiece } from "@/test/render"

const plan = (over: Partial<ConfigPlan> = {}) => ({ ...(fx.configPlan as unknown as ConfigPlan), ...over })
const lowers = fx.configPlanLowers as unknown as ConfigPlan
const needsSecret = fx.configPlanNeedsSecret as unknown as ConfigPlan

test("draws what changes, what it affects, costs and undoes, and when it expires", async () => {
  await renderPiece(<PlanCard plan={plan()} />)
  const card = screen.getByRole("region", { name: "Change plan: Add machine types to Home server" })
  expect(within(card).getByText("Applies now")).toBeInTheDocument()
  expect(within(card).getByText("You asked: Add my Linux server")).toBeInTheDocument()
  const changes = within(card).getByRole("region", { name: "Changes" })
  expect(changes).toHaveTextContent("Machine type linux-small (2 CPU, 4 GB)")
  expect(changes).toHaveTextContent("limits.max_machines: 0 → 3")
  expect(within(card).getByText("Free. It is your machine.")).toBeInTheDocument()
  expect(within(card).getByText("Can be reverted from Change history.")).toBeInTheDocument()
  expect(card).toHaveTextContent("Valid for 24 h · needs: Manage machines")
})

test("confirms in one step and becomes a receipt with a link to the history", async () => {
  let body: unknown
  server.use(http.post("/api/v1/config/plans/:plan/apply", async ({ request }) => ((body = await request.json()), HttpResponse.json(fx.configPlanApplied))))
  const { user } = await renderPiece(<PlanCard plan={plan()} />)
  await user.click(screen.getByRole("button", { name: "Confirm" }))
  expect(await screen.findByText("Applied")).toBeInTheDocument()
  expect(body).toEqual({})
  expect(screen.getByRole("link", { name: "Change history" })).toHaveAttribute("href", "/settings/history")
  // what the server generated is shown once, with a Copy button
  expect(screen.getByRole("region", { name: "SCIM token" })).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Confirm" })).toBeNull()
})

test("a person without the permission asks for approval instead", async () => {
  const { user } = await renderPiece(<PlanCard plan={plan({ can_confirm: false })} />)
  expect(screen.queryByRole("button", { name: "Confirm" })).toBeNull()
  await user.click(screen.getByRole("button", { name: "Ask for approval" }))
  expect(await screen.findByText("A person with Manage machines confirms it in the Inbox.")).toBeInTheDocument()
  expect(screen.getByText("Waiting for approval")).toBeInTheDocument()
})

test("a stale plan is rebuilt, never applied", async () => {
  let rebuilt: unknown
  server.use(http.post("/api/v1/config/plans", async ({ request }) => ((rebuilt = await request.json()), HttpResponse.json(plan({ title: "Rebuilt" }), { status: 201 }))))
  const onRebuilt = vi.fn()
  const { user } = await renderPiece(<PlanCard plan={plan({ stale: true })} onRebuilt={onRebuilt} />)
  expect(screen.queryByRole("button", { name: "Confirm" })).toBeNull()
  await user.click(screen.getByRole("button", { name: "Rebuild plan" }))
  await waitFor(() => expect(onRebuilt).toHaveBeenCalled())
  expect((rebuilt as { changes: { kind: string; name: string }[] }).changes.map((c) => `${c.kind}/${c.name}`)).toEqual(["shape/linux-small", "shape/linux-medium", "provider/home-server"])
  expect(screen.getByRole("region", { name: "Change plan: Rebuilt" })).toBeInTheDocument()
})

test("a plan that lowers protection needs the word typed, and says why", async () => {
  let body: unknown
  server.use(http.post("/api/v1/config/plans/:plan/apply", async ({ request }) => ((body = await request.json()), HttpResponse.json(fx.configPlanApplied))))
  const { user } = await renderPiece(<PlanCard plan={lowers} />)
  expect(screen.getByRole("list", { name: "Lowers protection" })).toHaveTextContent("Adds Forecrew Cloud as a fallback")
  expect(screen.getByText("Based on text from README.md, which is not verified")).toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Confirm" }))
  const dialog = await screen.findByRole("dialog", { name: /Confirm: Let jobs use/ })
  const apply = within(dialog).getByRole("button", { name: "Confirm and apply" })
  expect(apply).toBeDisabled()
  await user.type(within(dialog).getByLabelText(/Type acme to confirm/), "acm")
  expect(apply).toBeDisabled()
  await user.type(within(dialog).getByLabelText(/Type acme to confirm/), "e")
  await user.click(apply)
  await waitFor(() => expect(body).toEqual({ word: "acme" }))
})

test("the word typed is gone when the dialog is closed and opened again", async () => {
  const { user } = await renderPiece(<PlanCard plan={lowers} />)
  await user.click(screen.getByRole("button", { name: "Confirm" }))
  await user.type(await screen.findByLabelText(/Type acme to confirm/), "acme")
  await user.click(screen.getByRole("button", { name: "Keep it as it is" }))
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
  await user.click(screen.getByRole("button", { name: "Confirm" }))
  expect(await screen.findByLabelText(/Type acme to confirm/)).toHaveValue("")
  await user.type(screen.getByLabelText(/Type acme to confirm/), "acm")
  await user.keyboard("{Escape}")
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
  await user.click(screen.getByRole("button", { name: "Confirm" }))
  expect(await screen.findByLabelText(/Type acme to confirm/)).toHaveValue("")
})

test("a plan that lowers protection is not confirmed from a phone", async () => {
  vi.stubGlobal("matchMedia", () => ({ matches: true }))
  await renderPiece(<PlanCard plan={lowers} />)
  expect(screen.queryByRole("button", { name: "Confirm" })).toBeNull()
  expect(screen.getByText(/cannot be confirmed from a phone/)).toBeInTheDocument()
  vi.unstubAllGlobals()
})

test("Confirm waits for the secure field, which only ever shows Saved afterwards", async () => {
  const saved = { ...needsSecret, steps: needsSecret.steps.map((s) => (s.kind === "secret_required" ? { ...s, filled: true } : s)) }
  let value: unknown
  server.use(http.put("/api/v1/config/plans/:plan/secrets/:secret", async ({ request }) => ((value = await request.json()), HttpResponse.json(saved))))
  const { user } = await renderPiece(<PlanCard plan={needsSecret} />)
  expect(screen.getByRole("button", { name: "Confirm" })).toBeDisabled()
  expect(screen.getByText("Fill the secure field first.")).toBeInTheDocument()
  const field = screen.getByRole("form", { name: "Secure field: Okta client secret" })
  const input = within(field).getByLabelText("Okta client secret")
  expect(input).toHaveAttribute("type", "password")
  await user.type(input, "s3cret")
  await user.click(within(field).getByRole("button", { name: "Save" }))
  expect(await within(field).findByText(/Okta client secret: Saved/)).toBeInTheDocument()
  expect(value).toEqual({ value: "s3cret" })
  expect(screen.queryByDisplayValue("s3cret")).toBeNull()
  expect(screen.getByRole("button", { name: "Confirm" })).toBeEnabled()
})

test("a secret that cannot be saved says why and keeps the plan open", async () => {
  server.use(http.put("/api/v1/config/plans/:plan/secrets/:secret", () => HttpResponse.json({ error: { code: "conflict", message: "already saved" } }, { status: 409 })))
  const { user } = await renderPiece(<PlanCard plan={needsSecret} />)
  await user.type(screen.getByLabelText("Okta client secret"), "x")
  await user.click(screen.getByRole("button", { name: "Save" }))
  expect(await screen.findByText("already saved")).toBeInTheDocument()
})

test("a failed apply says nothing changed and offers to try again", async () => {
  server.use(http.post("/api/v1/config/plans/:plan/apply", () => HttpResponse.json({ error: { code: "conflict", message: "moved since" } }, { status: 409 })))
  const { user } = await renderPiece(<PlanCard plan={plan()} />)
  await user.click(screen.getByRole("button", { name: "Confirm" }))
  expect(await screen.findByText("Nothing changed. moved since")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Confirm" })).toBeEnabled()
})

test("cancel, edit and YAML", async () => {
  const onEdit = vi.fn()
  const { user } = await renderPiece(<PlanCard plan={plan()} onEdit={onEdit} />)
  await user.click(screen.getByRole("button", { name: "Edit" }))
  expect(onEdit).toHaveBeenCalled()
  await user.click(screen.getByRole("button", { name: "View as YAML" }))
  const dialog = await screen.findByRole("dialog")
  expect(dialog).toHaveTextContent("kind: shape")
  expect(dialog).toHaveTextContent("name: linux-small")
  await user.keyboard("{Escape}")
  server.use(http.post("/api/v1/config/plans/:plan/cancel", () => HttpResponse.json(plan({ status: "canceled" }))))
  await user.click(screen.getByRole("button", { name: "Cancel" }))
  expect(await screen.findByText("Canceled")).toBeInTheDocument()
})

test("an MCP plan says where it came from, and a repo plan says it opens a PR", async () => {
  await renderPiece(<PlanCard plan={plan({ source: "mcp", client: "Claude Desktop", applies: "pr:acme/app", can_confirm: false, status: "awaiting_approval" })} />)
  expect(screen.getByText("Plan from Claude Desktop")).toBeInTheDocument()
  expect(screen.getByText("Opens a PR on acme/app")).toBeInTheDocument()
  expect(screen.getByText("Waiting for approval")).toBeInTheDocument()
})

test("an expired plan shows its state and no buttons", async () => {
  await renderPiece(<PlanCard plan={plan({ status: "expired" })} />)
  expect(screen.getByText("Expired")).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Confirm" })).toBeNull()
})
