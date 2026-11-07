import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const open = async (path = "/machines") => {
  const app = await renderApp(path)
  await app.user.click(await screen.findByRole("button", { name: "Ask" }))
  return { ...app, panel: await screen.findByRole("region", { name: "Assistant" }) }
}

test("the Ask button opens the panel docked beside the page, which stays usable", async () => {
  const { panel } = await open()
  expect(screen.getByRole("complementary", { name: "Assistant panel" })).toContainElement(panel)
  expect(screen.getByRole("heading", { name: "Machines" })).toBeInTheDocument()
  expect(within(panel).getByText(/Nothing changes until you confirm/)).toBeInTheDocument()
  expect(within(panel).getByText("Page: Machines")).toBeInTheDocument()
  expect(within(panel).getByText("Sees your configuration. Never your secrets.")).toBeInTheDocument()
  // starters match the page
  expect(within(panel).getByRole("button", { name: "Add my Linux server" })).toBeInTheDocument()
})

test("a message goes with the page, and the answer comes with its plan drawn by the server", async () => {
  let sent: unknown
  server.use(
    http.post("/api/v1/assistant/messages", async ({ request }) => ((sent = await request.json()), HttpResponse.json(fx.assistantSend, { status: 202 }))),
    http.get("/api/v1/assistant/conversations/:id", () => HttpResponse.json(fx.assistantConversation)),
  )
  const { user, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "Add my Linux server")
  await user.click(within(panel).getByRole("button", { name: "Send" }))
  expect(sent).toEqual({ message: "Add my Linux server", page: "/machines" })
  expect(await within(panel).findByText("two sizes")).toBeInTheDocument()
  // markdown is text: a script in the answer is shown, not run
  expect(within(panel).getByText(/<script>alert\(1\)<\/script>/)).toBeInTheDocument()
  // the plan is a card with a secure field and its step, never part of the message
  const plan = within(panel).getByRole("region", { name: "Change plan: Add Okta sign-in" })
  expect(within(plan).getByLabelText("Okta client secret")).toHaveAttribute("type", "password")
  expect(within(panel).getByLabelText("Say what you want")).toHaveValue("")
})

test("typing yes does nothing: only the card's button confirms", async () => {
  let applied = 0
  server.use(
    http.get("/api/v1/assistant/conversations/:id", () => HttpResponse.json({ ...fx.assistantConversation, plans: [{ ...fx.configPlan, source: "assistant" }] })),
    http.post("/api/v1/config/plans/:plan/apply", () => (applied++, HttpResponse.json(fx.configPlanApplied))),
    http.post("/api/v1/assistant/messages", () => HttpResponse.json(fx.assistantSend, { status: 202 })),
  )
  const { user, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "yes")
  await user.click(within(panel).getByRole("button", { name: "Send" }))
  await within(panel).findByRole("region", { name: /Change plan/ })
  expect(applied).toBe(0)
  await user.click(within(panel).getByRole("button", { name: "Confirm" }))
  await within(panel).findByText("Applied")
  expect(applied).toBe(1)
})

test("something that looks like a key is not sent, and says why", async () => {
  let calls = 0
  server.use(http.post("/api/v1/assistant/messages", () => (calls++, HttpResponse.json(fx.assistantSend, { status: 202 }))))
  const { user, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "my key is sk-ant-abcdefgh12345")
  expect(within(panel).getByText(/looks like a key/)).toBeInTheDocument()
  expect(within(panel).getByRole("button", { name: "Send" })).toBeDisabled()
  await user.keyboard("{Enter}")
  expect(calls).toBe(0)
})

test("a Linear key typed into the assistant is not sent either: the token goes in the secure field", async () => {
  let calls = 0
  server.use(http.post("/api/v1/assistant/messages", () => (calls++, HttpResponse.json(fx.assistantSend, { status: 202 }))))
  const { user, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "Connect Linear with lin_api_abcdefghijklmnopqrstuvwx")
  expect(within(panel).getByText(/looks like a key/)).toBeInTheDocument()
  await user.keyboard("{Enter}")
  expect(calls).toBe(0)
})

test("while it works it says so, polls, and can be stopped", async () => {
  let gets = 0
  const cancelled: string[] = []
  server.use(
    http.post("/api/v1/assistant/messages", () => HttpResponse.json(fx.assistantSend, { status: 202 })),
    http.get("/api/v1/assistant/conversations/:id", () => (gets++, HttpResponse.json(fx.assistantThinking))),
    http.post("/api/v1/jobs/:job/cancel", ({ params }) => (cancelled.push(String(params.job)), HttpResponse.json(fx.job))),
  )
  const { user, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "Add my Linux server{Enter}")
  expect(await within(panel).findByText(/Thinking/)).toBeInTheDocument()
  await user.click(within(panel).getByRole("button", { name: "Stop" }))
  await waitFor(() => expect(within(panel).queryByText(/Thinking/)).toBeNull())
  expect(gets).toBeGreaterThan(0)
  // Stop cancels the job of the person's last message, not only the spinner.
  expect(cancelled).toEqual(["018f0000-0000-7000-8000-000000000150"])
})

test("without a reasoning machine it says the assistant is down and that forms still work", async () => {
  server.use(http.post("/api/v1/assistant/messages", () => HttpResponse.json(fx.assistantUnavailable, { status: 503 })))
  const { user, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "hello{Enter}")
  expect(await within(panel).findByText("The assistant is not available")).toBeInTheDocument()
  expect(within(panel).getByText(/no reasoning machine is online. Every setting still works with forms/)).toBeInTheDocument()
})

test("another failure is shown in place", async () => {
  server.use(http.post("/api/v1/assistant/messages", () => HttpResponse.json(fx.errorAssistantSecret, { status: 400 })))
  const { user, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "hello{Enter}")
  expect(await within(panel).findByText(/secrets go in a secure field/)).toBeInTheDocument()
})

test("a starter prompt sends itself; New starts again; the page context can be removed", async () => {
  let sent: { page?: string } = {}
  server.use(
    http.post("/api/v1/assistant/messages", async ({ request }) => ((sent = (await request.json()) as typeof sent), HttpResponse.json(fx.assistantSend, { status: 202 }))),
    http.get("/api/v1/assistant/conversations/:id", () => HttpResponse.json(fx.assistantConversation)),
  )
  const { user, panel } = await open()
  await user.click(within(panel).getByRole("button", { name: "Remove the page as context" }))
  expect(within(panel).queryByText("Page: Machines")).toBeNull()
  await user.click(within(panel).getByRole("button", { name: "Add a Windows PC" }))
  await within(panel).findByText("two sizes")
  expect(sent.page).toBeUndefined()
  await user.click(within(panel).getByRole("button", { name: "New" }))
  expect(within(panel).queryByText("two sizes")).toBeNull()
  expect(within(panel).getByText(/Nothing changes until you confirm/)).toBeInTheDocument()
})

test("something to build is offered as a goal", async () => {
  const { user, router, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "build offline sync")
  await user.click(within(panel).getByRole("link", { name: "Start this as a goal" }))
  await waitFor(() => expect(router.state.location.pathname).toBe("/goals/new"))
  expect(await screen.findByLabelText("Title")).toHaveValue("build offline sync")
})

test("Escape closes the panel, the close button too, and the conversation stays", async () => {
  server.use(
    http.post("/api/v1/assistant/messages", () => HttpResponse.json(fx.assistantSend, { status: 202 })),
    http.get("/api/v1/assistant/conversations/:id", () => HttpResponse.json(fx.assistantConversation)),
  )
  const { user, panel } = await open()
  await user.type(within(panel).getByLabelText("Say what you want"), "Add my Linux server{Enter}")
  await within(panel).findByText("two sizes")
  await user.keyboard("{Escape}")
  await waitFor(() => expect(screen.queryByRole("region", { name: "Assistant" })).toBeNull())
  await user.click(screen.getByRole("button", { name: "Ask" }))
  expect(await screen.findByText("two sizes")).toBeInTheDocument()
  await user.click(screen.getByRole("button", { name: "Close the assistant" }))
  await waitFor(() => expect(screen.queryByRole("region", { name: "Assistant" })).toBeNull())
})

test("on a narrow screen it is a sheet", async () => {
  vi.stubGlobal("matchMedia", (q: string) => ({ matches: !q.includes("min-width"), addEventListener: () => {}, removeEventListener: () => {} }))
  const app = await renderApp("/machines")
  await app.user.click(await screen.findByRole("button", { name: "Ask" }))
  const dialog = await screen.findByRole("dialog", { name: "Assistant" })
  expect(within(dialog).getByRole("region", { name: "Assistant" })).toBeInTheDocument()
  expect(screen.queryByRole("complementary", { name: "Assistant panel" })).toBeNull()
  vi.unstubAllGlobals()
})

test("without config.read there is no Ask button, no command menu row and no panel", async () => {
  server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.meViewer, permissions: fx.meViewer.permissions.filter((p: string) => p !== "config.read") })))
  const { user } = await renderApp("/machines")
  await screen.findByRole("heading", { name: "Machines" })
  expect(screen.queryByRole("button", { name: /^Ask/ })).toBeNull()
  await user.keyboard("{Control>}k{/Control}")
  await screen.findByPlaceholderText(/Search pages/)
  expect(screen.queryByRole("option", { name: /Ask the assistant/ })).toBeNull()
  expect(screen.queryByRole("region", { name: "Assistant" })).toBeNull()
})
