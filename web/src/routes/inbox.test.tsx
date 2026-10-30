import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

/** An API whose gate decisions remove the gate from the list, and records the bodies. */
function inboxApi() {
  let pending = [...fx.gates]
  const calls: { ref: string; decision: string; body: unknown }[] = []
  server.use(
    http.get("/api/v1/gates", () => HttpResponse.json(pending)),
    http.post("/api/v1/gates/:ref/:decision", async ({ params, request }) => {
      calls.push({ ref: String(params.ref), decision: String(params.decision), body: await request.json() })
      pending = pending.filter((g) => g.ref !== params.ref)
      return HttpResponse.json(fx.gateApprove)
    }),
  )
  return calls
}

const card = (ref: string) => screen.getByRole("article", { name: new RegExp(ref) })

test("shows the pending gates as decision cards", async () => {
  await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  expect(within(first).getByText("Merge approval")).toBeInTheDocument()
  expect(within(first).getByText("app")).toBeInTheDocument()
  expect(within(first).getByText(/GOAL-3/)).toBeInTheDocument()
  expect(within(first).getByText(/TASK-12/)).toBeInTheDocument()
  expect(within(first).getByText("Adds email sign-in. Tests pass.")).toBeInTheDocument()
  expect(within(first).getByText(/45 min/)).toBeInTheDocument()
  expect(within(first).getByRole("link", { name: /Open on GitHub/ })).toHaveAttribute("href", "https://github.com/acme/app/pull/9")
  expect(screen.getAllByRole("article")).toHaveLength(3)
})

test("shows the pending count in the sidebar", async () => {
  await renderApp("/inbox")
  const nav = await screen.findByRole("navigation", { name: "Main" })
  await waitFor(() => expect(within(nav).getByRole("link", { name: /Inbox/ })).toHaveTextContent("3 pending"))
})

test("shows an empty state, a loading state and an error", async () => {
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)))
  const { unmount } = await renderApp("/inbox")
  expect(await screen.findByText(/Nothing needs you/)).toBeInTheDocument()
  unmount()

  server.use(http.get("/api/v1/gates", errors.invalidInput))
  const { user } = await renderApp("/inbox")
  expect(screen.getByRole("status")).toHaveTextContent("Loading")
  expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gates)))
  await user.click(screen.getByRole("button", { name: "Try again" }))
  expect(await screen.findByRole("article", { name: /GATE-9/ })).toBeInTheDocument()
})

test("approves with an optional note and the list updates", async () => {
  const calls = inboxApi()
  const { user } = await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  await user.type(within(first).getByLabelText("Note (optional)"), "Looks good")
  await user.click(within(first).getByRole("button", { name: "Approve" }))
  await waitFor(() => expect(screen.queryByRole("article", { name: /GATE-9/ })).not.toBeInTheDocument())
  expect(calls).toEqual([{ ref: "GATE-9", decision: "approve", body: { feedback: "Looks good" } }])
  expect(screen.getAllByRole("article")).toHaveLength(2)
})

test("approves without a note", async () => {
  const calls = inboxApi()
  const { user } = await renderApp("/inbox")
  await user.click(within(await screen.findByRole("article", { name: /GATE-9/ })).getByRole("button", { name: "Approve" }))
  await waitFor(() => expect(calls).toHaveLength(1))
  expect(calls[0]?.body).toEqual({})
})

test("request changes needs feedback", async () => {
  const calls = inboxApi()
  const { user } = await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  await user.click(within(first).getByRole("button", { name: "Request changes" }))
  const feedback = within(first).getByLabelText("What should change?")
  expect(feedback).toHaveFocus()

  await user.click(within(first).getByRole("button", { name: "Send changes request" }))
  expect(within(first).getByText("Feedback is required to request changes.")).toBeInTheDocument()
  expect(calls).toEqual([])

  await user.type(feedback, "Add a test")
  await user.click(within(first).getByRole("button", { name: "Send changes request" }))
  await waitFor(() => expect(calls).toEqual([{ ref: "GATE-9", decision: "request-changes", body: { feedback: "Add a test" } }]))
  await waitFor(() => expect(screen.queryByRole("article", { name: /GATE-9/ })).not.toBeInTheDocument())
})

test("cancel closes the request changes form", async () => {
  const { user } = await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  await user.click(within(first).getByRole("button", { name: "Request changes" }))
  await user.click(within(first).getByRole("button", { name: "Cancel" }))
  expect(within(first).getByRole("button", { name: "Approve" })).toBeInTheDocument()
})

test("reject asks for confirmation and takes an optional reason", async () => {
  const calls = inboxApi()
  const { user } = await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  await user.click(within(first).getByRole("button", { name: "Reject" }))
  expect(calls).toEqual([])
  await user.type(within(first).getByLabelText("Why stop? (optional)"), "Not wanted")
  await user.click(within(first).getByRole("button", { name: "Confirm reject" }))
  await waitFor(() => expect(calls).toEqual([{ ref: "GATE-9", decision: "reject", body: { feedback: "Not wanted" } }]))
})

test("a needs-you gate offers retry and reject, not request changes", async () => {
  const { user } = await renderApp("/inbox")
  const gate = await screen.findByRole("article", { name: /GATE-11/ })
  expect(within(gate).getByText("Needs you")).toBeInTheDocument()
  expect(within(gate).getByRole("button", { name: "Approve and retry" })).toBeInTheDocument()
  expect(within(gate).queryByRole("button", { name: "Request changes" })).not.toBeInTheDocument()
  await user.click(gate)
  await user.keyboard("r")
  expect(within(gate).queryByLabelText("What should change?")).not.toBeInTheDocument()
})

test("a refused decision shows the error on the card", async () => {
  server.use(http.post("/api/v1/gates/:ref/approve", errors.conflict))
  const { user } = await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  await user.click(within(first).getByRole("button", { name: "Approve" }))
  expect(await within(first).findByRole("alert")).toHaveTextContent("already approved")
  expect(screen.getByRole("article", { name: /GATE-9/ })).toBeInTheDocument()
})

describe("keyboard", () => {
  test("J and K move between cards", async () => {
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-9/ })
    expect(card("GATE-9")).toHaveAttribute("aria-current", "true")
    await user.keyboard("j")
    expect(card("GATE-10")).toHaveAttribute("aria-current", "true")
    expect(card("GATE-9")).not.toHaveAttribute("aria-current")
    await user.keyboard("jj") // stops at the last card
    expect(card("GATE-11")).toHaveAttribute("aria-current", "true")
    await user.keyboard("kkk") // and at the first
    expect(card("GATE-9")).toHaveAttribute("aria-current", "true")
  })

  test("A approves the selected card with its note", async () => {
    const calls = inboxApi()
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-9/ })
    await user.keyboard("j")
    await user.type(within(card("GATE-10")).getByLabelText("Note (optional)"), "ok a")
    await user.click(document.body) // leave the field
    await user.keyboard("a")
    await waitFor(() => expect(calls).toEqual([{ ref: "GATE-10", decision: "approve", body: { feedback: "ok a" } }]))
  })

  test("R opens request changes on the selected card", async () => {
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-9/ })
    await user.keyboard("r")
    expect(within(card("GATE-9")).getByLabelText("What should change?")).toHaveFocus()
    // keys typed in the field are text, not shortcuts
    await user.keyboard("jka")
    expect(within(card("GATE-9")).getByLabelText("What should change?")).toHaveValue("jka")
    expect(card("GATE-9")).toHaveAttribute("aria-current", "true")
  })

  test("shortcuts with a modifier are left to the browser", async () => {
    const calls = inboxApi()
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-9/ })
    await user.keyboard("{Control>}a{/Control}")
    expect(calls).toEqual([])
  })

  test("clicking a card selects it", async () => {
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-9/ })
    await user.click(within(card("GATE-10")).getByText("Spec approval"))
    expect(card("GATE-10")).toHaveAttribute("aria-current", "true")
  })
})

test("untrusted summary text is shown as text", async () => {
  const { container } = await renderApp("/inbox")
  const gate = await screen.findByRole("article", { name: /GATE-11/ })
  expect(within(gate).getByText(/<script>alert\(1\)<\/script>/)).toBeInTheDocument()
  expect(container.querySelector("script")).toBeNull()
})

test("only the selected card has the primary Approve button", async () => {
  const { user } = await renderApp("/inbox")
  await screen.findByRole("article", { name: /GATE-9/ })
  const variant = (ref: string) => within(card(ref)).getByRole("button", { name: /^Approve/ }).getAttribute("data-variant")
  expect([variant("GATE-9"), variant("GATE-10")]).toEqual(["default", "outline"])
  await user.keyboard("j")
  expect([variant("GATE-9"), variant("GATE-10")]).toEqual(["outline", "default"])
})

test("J and K on an empty list do nothing", async () => {
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)))
  const { user } = await renderApp("/inbox")
  await screen.findByText(/Nothing needs you/)
  await user.keyboard("jkjak")
  expect(screen.queryByRole("article")).not.toBeInTheDocument()
})

test("filters the inbox by project through the query string", async () => {
  const seen: string[] = []
  server.use(
    http.get("/api/v1/gates", ({ request }) => {
      seen.push(new URL(request.url).search)
      return HttpResponse.json(fx.gates)
    }),
  )
  const { user, router } = await renderApp("/inbox")
  await screen.findByRole("article", { name: /GATE-9/ })
  await user.selectOptions(await screen.findByLabelText("Project"), "app")
  await waitFor(() => expect(seen).toContain("?project=app"))
  expect(router.state.location.search).toEqual({ project: "app" })
  await user.selectOptions(screen.getByLabelText("Project"), "")
  await waitFor(() => expect(router.state.location.search).toEqual({}))
})

test("reads the project filter from the address and words the empty state", async () => {
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)))
  await renderApp("/inbox?project=app")
  expect(await screen.findByLabelText("Project")).toHaveValue("app")
  expect(await screen.findByText("Nothing needs you in this project.")).toBeInTheDocument()
})
