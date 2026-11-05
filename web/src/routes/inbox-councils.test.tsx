import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

/** Gates of the new kinds: GATE-12 council decision, 13 assumption review, 14 a job's question, 11 an escalation. */
function inboxApi() {
  let pending = [...fx.gatesCouncil]
  const gateCalls: { ref: string; decision: string; body: unknown }[] = []
  const resolveCalls: { ref: string; body: unknown }[] = []
  server.use(
    http.get("/api/v1/gates", () => HttpResponse.json(pending)),
    http.post("/api/v1/gates/:ref/:decision", async ({ params, request }) => {
      gateCalls.push({ ref: String(params.ref), decision: String(params.decision), body: await request.json() })
      pending = pending.filter((g) => g.ref !== params.ref)
      return HttpResponse.json(fx.gateApprove)
    }),
    http.post("/api/v1/assumptions/:ref/resolve", async ({ params, request }) => {
      resolveCalls.push({ ref: String(params.ref), body: await request.json() })
      pending = pending.filter((g) => !g.title.startsWith(`Review ${String(params.ref)}:`))
      return HttpResponse.json(fx.assumptionResolve)
    }),
  )
  return { gateCalls, resolveCalls }
}

const card = (ref: string) => screen.getByRole("article", { name: new RegExp(ref) })
const item = (title: RegExp | string) => within(screen.getByRole("list", { name: "Pending decisions" })).getByRole("button", { name: title })

describe("council decision card", () => {
  test("shows the brief with options, votes, dissent and assumptions", async () => {
    inboxApi()
    await renderApp("/inbox?gate=GATE-12")
    const gate = await screen.findByRole("article", { name: /GATE-12/ })
    expect(within(gate).getByText("Council decision")).toBeInTheDocument()
    const brief = within(await within(gate).findByRole("region", { name: "Options" }))
    expect(brief.getAllByText("postgres-queue").length).toBeGreaterThan(0)
    expect(brief.getByText("recommended")).toBeInTheDocument()
    expect(within(gate).getByText(/Job data should not share/)).toBeInTheDocument()
    expect(within(gate).getByRole("region", { name: "Mandated dissent" })).toBeInTheDocument()
    expect(within(gate).getByText("Fewer than 50 workers.")).toBeInTheDocument()
    expect(within(gate).getByRole("link", { name: /Open the whole debate \(COUNCIL-5\)/ })).toHaveAttribute("href", "/councils/COUNCIL-5")
    // its own summary is the fallback, not shown next to the brief
    expect(within(gate).queryByText(/The brief of COUNCIL-5/)).not.toBeInTheDocument()
  })

  test("approves the recommendation", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-12")
    const gate = await screen.findByRole("article", { name: /GATE-12/ })
    await user.click(within(gate).getByRole("button", { name: "Approve recommendation" }))
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-12", decision: "approve", body: {} }]))
  })

  test("request changes sends the feedback as the constraint for one more round", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-12")
    const gate = await screen.findByRole("article", { name: /GATE-12/ })
    await user.click(within(gate).getByRole("button", { name: "Request changes" }))
    await user.click(within(gate).getByRole("button", { name: "Send constraint" }))
    expect(within(gate).getByText("Feedback is required to request changes.")).toBeInTheDocument()
    await user.type(within(gate).getByLabelText("Constraint for one more round"), "No new services")
    await user.click(within(gate).getByRole("button", { name: "Send constraint" }))
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-12", decision: "request-changes", body: { feedback: "No new services" } }]))
  })

  test("rejects", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-12")
    const gate = await screen.findByRole("article", { name: /GATE-12/ })
    await user.click(within(gate).getByRole("button", { name: "Reject" }))
    await user.click(within(gate).getByRole("button", { name: "Confirm reject" }))
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-12", decision: "reject", body: {} }]))
  })

  test("a council that had its human round can only be approved or rejected", async () => {
    inboxApi()
    const twice = structuredClone(fx.councilPending)
    const synthesis = twice.contributions.find((k) => k.stage === "synthesis")!
    twice.contributions.push({ ...synthesis, id: "second", round: 2 })
    server.use(http.get("/api/v1/councils/:ref", () => HttpResponse.json(twice)))
    const { user } = await renderApp("/inbox?gate=GATE-12")
    const gate = await screen.findByRole("article", { name: /GATE-12/ })
    await waitFor(() => expect(within(gate).queryByRole("button", { name: "Request changes" })).not.toBeInTheDocument())
    await user.keyboard("r")
    expect(within(gate).queryByLabelText("Constraint for one more round")).not.toBeInTheDocument()
    expect(within(gate).getByText(/had its human round/)).toBeInTheDocument()
    expect(within(gate).getByRole("button", { name: "Approve recommendation" })).toBeInTheDocument()
  })

  test("falls back to the gate's summary while the council cannot be read", async () => {
    inboxApi()
    server.use(http.get("/api/v1/councils/:ref", () => HttpResponse.json({ error: { code: "not_found", message: "no" } }, { status: 404 })))
    await renderApp("/inbox?gate=GATE-12")
    const gate = await screen.findByRole("article", { name: /GATE-12/ })
    expect(within(gate).getByText(/The brief of COUNCIL-5/)).toBeInTheDocument()
  })
})

describe("assumption review card", () => {
  test("shows the statement, impact, source and dependents", async () => {
    inboxApi()
    await renderApp("/inbox?gate=GATE-13")
    const gate = await screen.findByRole("article", { name: /GATE-13/ })
    const a = within(await within(gate).findByRole("group", { name: "Assumption A-7" }))
    expect(within(gate).getByText("Assumption review")).toBeInTheDocument()
    expect(a.getByText(/Fewer than 50 workers will poll the queue/)).toBeInTheDocument()
    expect(a.getByText("impact high")).toBeInTheDocument()
    expect(a.getByText("check by ask-human")).toBeInTheDocument()
    expect(a.getByText("JOB-41")).toBeInTheDocument()
    expect(a.getByText("GOAL-3")).toBeInTheDocument()
    expect(a.getByText("TASK-12")).toBeInTheDocument()
  })

  test("accept approves the gate", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-13")
    const gate = await screen.findByRole("article", { name: /GATE-13/ })
    await user.click(within(gate).getByRole("button", { name: "Accept" }))
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-13", decision: "approve", body: {} }]))
  })

  test("invalidate rejects the gate with the reason", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-13")
    const gate = await screen.findByRole("article", { name: /GATE-13/ })
    await user.click(within(gate).getByRole("button", { name: "Invalidate" }))
    await user.type(within(gate).getByLabelText("Why is it wrong? (optional)"), "We run 80")
    await user.click(within(gate).getByRole("button", { name: "Confirm invalidate" }))
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-13", decision: "reject", body: { feedback: "We run 80" } }]))
  })

  test("verify needs evidence and resolves the assumption", async () => {
    const { gateCalls, resolveCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-13")
    const gate = await screen.findByRole("article", { name: /GATE-13/ })
    await user.click(within(gate).getByRole("button", { name: "Verify" }))
    await user.click(within(gate).getByRole("button", { name: "Confirm verified" }))
    expect(within(gate).getByText("Evidence is required to verify.")).toBeInTheDocument()
    expect(resolveCalls).toEqual([])
    await user.type(within(gate).getByLabelText("Evidence that it holds"), "Load test with 80 workers.")
    await user.click(within(gate).getByRole("button", { name: "Confirm verified" }))
    await waitFor(() => expect(resolveCalls).toEqual([{ ref: "A-7", body: { status: "verified", evidence: "Load test with 80 workers." } }]))
    expect(gateCalls).toEqual([])
    await waitFor(() => expect(screen.queryByRole("article", { name: /GATE-13/ })).not.toBeInTheDocument())
  })

  test("shows a refused verification on the card", async () => {
    inboxApi()
    server.use(http.post("/api/v1/assumptions/:ref/resolve", () => HttpResponse.json({ error: { code: "invalid_input", message: "verified needs evidence" } }, { status: 400 })))
    const { user } = await renderApp("/inbox?gate=GATE-13")
    const gate = await screen.findByRole("article", { name: /GATE-13/ })
    await user.click(within(gate).getByRole("button", { name: "Verify" }))
    await user.type(within(gate).getByLabelText("Evidence that it holds"), "ok")
    await user.click(within(gate).getByRole("button", { name: "Confirm verified" }))
    expect(await within(gate).findByRole("alert")).toHaveTextContent("verified needs evidence")
  })

  test("request changes asks for what to check", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-13")
    const gate = await screen.findByRole("article", { name: /GATE-13/ })
    await user.click(within(gate).getByRole("button", { name: "Request changes" }))
    await user.type(within(gate).getByLabelText("What should be checked?"), "Count the workers")
    await user.click(within(gate).getByRole("button", { name: "Send changes request" }))
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-13", decision: "request-changes", body: { feedback: "Count the workers" } }]))
  })

  test("falls back to the gate's summary when the assumption is not in the register", async () => {
    inboxApi()
    server.use(http.get("/api/v1/assumptions", () => HttpResponse.json([])))
    await renderApp("/inbox?gate=GATE-13")
    const gate = await screen.findByRole("article", { name: /GATE-13/ })
    expect(within(gate).getByText(/high-impact assumption made by architect/)).toBeInTheDocument()
  })
})

describe("a job's question", () => {
  test("is kept apart from escalations, in its own filter and with its own words", async () => {
    inboxApi()
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article")
    const chips = screen.getByRole("group", { name: "Filter decisions" })
    expect(within(chips).getByRole("button", { name: "Needs you" })).toBeInTheDocument()
    await user.click(within(chips).getByRole("button", { name: "Questions" }))
    const list = within(screen.getByRole("list", { name: "Pending decisions" }))
    expect(list.getAllByRole("button")).toHaveLength(1)
    const gate = card("GATE-14")
    expect(within(gate).getByText("Question")).toBeInTheDocument()
    expect(within(gate).queryByText("Needs you")).not.toBeInTheDocument()
    expect(within(gate).getByText(/expire after 15 minutes or 1 hour/)).toBeInTheDocument()
    expect(within(gate).queryByRole("button", { name: "Request changes" })).not.toBeInTheDocument()
    await user.click(within(chips).getByRole("button", { name: "Needs you" }))
    expect(list.getAllByRole("button")).toHaveLength(1)
    expect(card("GATE-11")).toBeInTheDocument()
  })

  test("the answer is the approval's feedback and is required", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-14")
    const gate = await screen.findByRole("article", { name: /GATE-14/ })
    await user.click(within(gate).getByRole("button", { name: "Send answer" }))
    expect(within(gate).getByText("Write the answer first: it is what the job gets.")).toBeInTheDocument()
    expect(gateCalls).toEqual([])
    await user.type(within(gate).getByLabelText("Your answer"), "15 minutes")
    await user.click(within(gate).getByRole("button", { name: "Send answer" }))
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-14", decision: "approve", body: { feedback: "15 minutes" } }]))
  })

  test("declining rejects", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox?gate=GATE-14")
    const gate = await screen.findByRole("article", { name: /GATE-14/ })
    await user.click(within(gate).getByRole("button", { name: "Decline" }))
    await user.click(within(gate).getByRole("button", { name: "Confirm decline" }))
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-14", decision: "reject", body: {} }]))
  })
})

describe("keyboard on the new cards", () => {
  test("J and K move over them, A approves the council brief", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-12/ })
    expect(item(/COUNCIL-5/)).toHaveAttribute("aria-current", "true")
    await user.keyboard("j")
    expect(card("GATE-13")).toBeInTheDocument()
    await user.keyboard("k")
    await within(card("GATE-12")).findByRole("region", { name: "Options" })
    await user.keyboard("a")
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-12", decision: "approve", body: {} }]))
  })

  test("A accepts the selected assumption", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-12/ })
    await user.keyboard("j")
    await within(card("GATE-13")).findByRole("group", { name: "Assumption A-7" })
    await user.keyboard("a")
    await waitFor(() => expect(gateCalls).toEqual([{ ref: "GATE-13", decision: "approve", body: {} }]))
  })

  test("R opens the constraint field of a council and the check field of an assumption", async () => {
    inboxApi()
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-12/ })
    await user.keyboard("r")
    expect(within(card("GATE-12")).getByLabelText("Constraint for one more round")).toHaveFocus()
    await user.click(document.body) // leave the field
    await user.keyboard("j")
    await user.keyboard("r")
    expect(within(card("GATE-13")).getByLabelText("What should be checked?")).toHaveFocus()
  })

  test("A on a question without an answer asks for one, and R does nothing", async () => {
    const { gateCalls } = inboxApi()
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-12/ })
    await user.keyboard("jj")
    const gate = card("GATE-14")
    await user.keyboard("a")
    expect(await within(gate).findByRole("alert")).toHaveTextContent("Write the answer first")
    expect(gateCalls).toEqual([])
    await user.keyboard("r")
    expect(within(gate).queryByLabelText("What should change?")).not.toBeInTheDocument()
    // moving away clears the message
    await user.keyboard("k")
    await user.keyboard("j")
    expect(within(card("GATE-14")).queryByRole("alert")).not.toBeInTheDocument()
  })
})

test("the other new gate kinds are labelled and filterable", async () => {
  const others = [
    { ...fx.gatesCouncil[0]!, ref: "GATE-20", number: 20, kind: "knowledge_proposal", title: "Add an ADR" },
    { ...fx.gatesCouncil[0]!, ref: "GATE-21", number: 21, kind: "access_request", title: "Read the wiki" },
    { ...fx.gatesCouncil[0]!, ref: "GATE-22", number: 22, kind: "improvement_proposal", title: "Faster tests" },
  ]
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(others)))
  await renderApp("/inbox")
  await screen.findByRole("article", { name: /GATE-20/ })
  const chips = screen.getByRole("group", { name: "Filter decisions" })
  for (const label of ["Knowledge", "Access", "Improvements"]) expect(within(chips).getByRole("button", { name: label })).toBeInTheDocument()
  const list = within(screen.getByRole("list", { name: "Pending decisions" }))
  expect(list.getByRole("button", { name: /Knowledge proposal/ })).toBeInTheDocument()
  expect(list.getByRole("button", { name: /Access request/ })).toBeInTheDocument()
  expect(list.getByRole("button", { name: /Improvement proposal/ })).toBeInTheDocument()
})
