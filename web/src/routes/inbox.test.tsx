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
const item = (title: string) => within(screen.getByRole("list", { name: "Pending decisions" })).getByRole("button", { name: new RegExp(title) })

test("lists the pending gates and shows the first as a decision", async () => {
  await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  expect(within(first).getByText("Merge approval")).toBeInTheDocument()
  expect(within(first).getByText("app")).toBeInTheDocument()
  expect(within(first).getByText(/GOAL-3/)).toBeInTheDocument()
  expect(within(first).getByText(/TASK-12/)).toBeInTheDocument()
  expect(within(first).getByText("Adds email sign-in. Tests pass.")).toBeInTheDocument()
  expect(within(first).getByText(/45 min/)).toBeInTheDocument()
  expect(within(first).getByRole("link", { name: /Open on GitHub/ })).toHaveAttribute("href", "https://github.com/acme/app/pull/9")
  expect(screen.getAllByRole("article")).toHaveLength(1)
  const list = screen.getByRole("list", { name: "Pending decisions" })
  expect(within(list).getAllByRole("button")).toHaveLength(3)
  expect(item("Merge PR #9")).toHaveAttribute("aria-current", "true")
  expect(screen.getByText(/Keyboard:/)).toBeInTheDocument()
})

test("has filter chips by kind that narrow the list", async () => {
  const { user } = await renderApp("/inbox")
  await screen.findByRole("article", { name: /GATE-9/ })
  const chips = screen.getByRole("group", { name: "Filter decisions" })
  expect(within(chips).getByRole("button", { name: "All 3" })).toHaveAttribute("aria-pressed", "true")
  await user.click(within(chips).getByRole("button", { name: "Specs" }))
  expect(within(chips).getByRole("button", { name: "Specs" })).toHaveAttribute("aria-pressed", "true")
  expect(within(screen.getByRole("list", { name: "Pending decisions" })).getAllByRole("button")).toHaveLength(1)
  expect(card("GATE-10")).toBeInTheDocument()
  await user.click(within(chips).getByRole("button", { name: "All 3" }))
  expect(within(screen.getByRole("list", { name: "Pending decisions" })).getAllByRole("button")).toHaveLength(3)
})

test("puts the actions before the summary and the feedback box last", async () => {
  await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  const approve = within(first).getByRole("button", { name: "Approve" })
  const summary = within(first).getByText("Adds email sign-in. Tests pass.")
  const note = within(first).getByLabelText("Note (optional)")
  expect(approve.compareDocumentPosition(summary) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  expect(summary.compareDocumentPosition(note) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
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
  expect(screen.getByRole("article", { name: /GATE-10/ })).toBeInTheDocument()
  expect(within(screen.getByRole("list", { name: "Pending decisions" })).getAllByRole("button")).toHaveLength(2)
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
  await screen.findByRole("article", { name: /GATE-9/ })
  await user.click(item("Developer job failed"))
  const gate = screen.getByRole("article", { name: /GATE-11/ })
  expect(within(gate).getByText("Needs you")).toBeInTheDocument()
  expect(within(gate).getByRole("button", { name: "Approve and retry" })).toBeInTheDocument()
  expect(within(gate).queryByRole("button", { name: "Request changes" })).not.toBeInTheDocument()
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
    expect(item("Merge PR #9")).toHaveAttribute("aria-current", "true")
    await user.keyboard("j")
    expect(item("Approve the spec")).toHaveAttribute("aria-current", "true")
    expect(item("Merge PR #9")).not.toHaveAttribute("aria-current")
    expect(card("GATE-10")).toBeInTheDocument()
    await user.keyboard("jj") // stops at the last one
    expect(item("Developer job failed")).toHaveAttribute("aria-current", "true")
    await user.keyboard("kkk") // and at the first
    expect(item("Merge PR #9")).toHaveAttribute("aria-current", "true")
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

  test("clicking a list item selects it", async () => {
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-9/ })
    await user.click(item("Approve the spec"))
    expect(card("GATE-10")).toBeInTheDocument()
    expect(item("Approve the spec")).toHaveAttribute("aria-current", "true")
  })
})

test("untrusted summary text is shown as text", async () => {
  const { container, user } = await renderApp("/inbox")
  await screen.findByRole("article", { name: /GATE-9/ })
  await user.click(item("Developer job failed"))
  const gate = screen.getByRole("article", { name: /GATE-11/ })
  expect(within(gate).getByText(/<script>alert\(1\)<\/script>/)).toBeInTheDocument()
  expect(container.querySelector("script")).toBeNull()
})

test("the shown decision has the primary Approve button, and it follows the selection", async () => {
  const { user } = await renderApp("/inbox")
  await screen.findByRole("article", { name: /GATE-9/ })
  const approve = (ref: string) => within(card(ref)).getByRole("button", { name: /^Approve/ })
  expect(approve("GATE-9").classList.contains("bg-primary")).toBe(true)
  await user.keyboard("j")
  expect(approve("GATE-10").classList.contains("bg-primary")).toBe(true)
})

test("J and K on an empty list do nothing", async () => {
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)))
  const { user } = await renderApp("/inbox")
  await screen.findByText(/Nothing needs you/)
  await user.keyboard("jkjak")
  expect(screen.queryByRole("article")).not.toBeInTheDocument()
})

test("asks for the project of the address and words the empty state", async () => {
  const seen: string[] = []
  server.use(
    http.get("/api/v1/gates", ({ request }) => {
      seen.push(new URL(request.url).search)
      return HttpResponse.json(fx.gatesEmpty)
    }),
  )
  await renderApp("/inbox?project=app")
  expect(await screen.findByText("Nothing needs you in this project.")).toBeInTheDocument()
  expect(seen).toContain("?project=app")
})

test("opens with the decision named in the address selected", async () => {
  await renderApp("/inbox?gate=GATE-10")
  expect(await screen.findByRole("article", { name: /GATE-10/ })).toBeInTheDocument()
  expect(item("Approve the spec")).toHaveAttribute("aria-current", "true")
})

test("puts Open on GitHub at the right end of the action row", async () => {
  await renderApp("/inbox")
  const first = await screen.findByRole("article", { name: /GATE-9/ })
  const link = within(first).getByRole("link", { name: /Open on GitHub/ })
  const approve = within(first).getByRole("button", { name: "Approve" })
  expect(link.parentElement).toBe(approve.parentElement)
  expect(link.className).toContain("ml-auto")
})

describe("verification on merge cards", () => {
  test("shows the status, environments, traceability and the report link beside the summary", async () => {
    const asked: string[] = []
    server.use(
      http.get("/api/v1/changes/:change/verification", ({ params }) => {
        asked.push(decodeURIComponent(String(params.change)))
        return HttpResponse.json(fx.verification)
      }),
    )
    await renderApp("/inbox")
    const first = await screen.findByRole("article", { name: /GATE-9/ })
    const panel = await within(first).findByRole("region", { name: "GATE-9 verification" })
    expect(asked).toEqual(["https://github.com/acme/app/pull/9"])
    expect(within(panel).getByRole("list", { name: "Blocking reasons" })).toHaveTextContent("AC-3 has no test")
    expect(within(panel).getByText(/Environments:/).parentElement).toHaveTextContent("linux (go go1.26.1")
    const table = within(panel).getByRole("region", { name: /criteria table/ })
    expect(within(table).getByText("AC-2").closest("tr")).toHaveTextContent("auth::TestLinkExpires")
    expect(within(panel).getByRole("link", { name: "Download report" })).toHaveAttribute("href", expect.stringMatching(/^\/api\/v1\/artifacts\//))
    const summary = within(first).getByText("Adds email sign-in. Tests pass.")
    expect(summary.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  test("asks only for merge approvals", async () => {
    const asked: string[] = []
    server.use(
      http.get("/api/v1/changes/:change/verification", ({ params }) => {
        asked.push(String(params.change))
        return HttpResponse.json(fx.verification)
      }),
    )
    const { user } = await renderApp("/inbox")
    await screen.findByRole("region", { name: "GATE-9 verification" })
    await user.click(item("Approve the spec"))
    await screen.findByRole("article", { name: /GATE-10/ })
    expect(screen.queryByRole("region", { name: /verification/ })).not.toBeInTheDocument()
    expect(asked).toHaveLength(1)
  })

  test("shows nothing when the pull request has no report yet", async () => {
    server.use(http.get("/api/v1/changes/:change/verification", errors.notFound))
    await renderApp("/inbox")
    const first = await screen.findByRole("article", { name: /GATE-9/ })
    await waitFor(() => expect(within(first).queryByRole("alert")).not.toBeInTheDocument())
    expect(within(first).queryByRole("region", { name: /verification/ })).not.toBeInTheDocument()
  })

  test("says when the verification cannot be loaded", async () => {
    server.use(http.get("/api/v1/changes/:change/verification", errors.invalidInput))
    await renderApp("/inbox")
    const first = await screen.findByRole("article", { name: /GATE-9/ })
    expect(await within(first).findByRole("alert")).toHaveTextContent(/Could not load the verification/)
  })
})
