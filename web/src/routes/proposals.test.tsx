import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

function proposalApi() {
  const lists: URLSearchParams[] = []
  const decisions: { ref: string; body: Record<string, string> }[] = []
  server.use(
    http.get("/api/v1/proposals", ({ request }) => {
      const q = new URL(request.url).searchParams
      lists.push(q)
      return HttpResponse.json(q.get("project") === "app" ? fx.proposals : [])
    }),
    http.post("/api/v1/proposals/:ref/decide", async ({ params, request }) => {
      decisions.push({ ref: String(params.ref), body: (await request.json()) as Record<string, string> })
      return HttpResponse.json(fx.proposalDecide)
    }),
  )
  return { lists, decisions }
}

const article = (name: RegExp) => screen.findByRole("article", { name })

describe("proposals page", () => {
  test("is in the navigation and the command menu", async () => {
    const { user, router } = await renderApp("/")
    await user.click(await screen.findByRole("link", { name: "Proposals" }))
    await screen.findByRole("heading", { level: 1, name: "Proposals" })
    expect(router.state.location.pathname).toBe("/proposals")
    await user.keyboard("{Control>}k{/Control}")
    expect(await screen.findByRole("option", { name: "Proposals" })).toBeInTheDocument()
  })

  test("lists proposals with their outcome and where to decide them", async () => {
    await renderApp("/proposals")
    const open = within(await article(/Scan dependencies/))
    expect(open.getByText("PROP-3")).toBeInTheDocument()
    expect(open.getByText("Start a goal: Add a scan")).toBeInTheDocument()
    expect(open.getByRole("link", { name: "Decide in the Inbox" })).toHaveAttribute("href", "/inbox?gate=GATE-30")
    const done = within(await article(/macOS worker/))
    expect(done.getByText("workers: 4 before, 0 after")).toBeInTheDocument()
    const rejected = within(await article(/Build on Windows/))
    expect(rejected.getByText("We ship Linux only")).toBeInTheDocument()
    expect(rejected.getByText(".forecrew/matrix.yaml")).toBeInTheDocument()
    expect(rejected.queryByRole("link", { name: "Decide in the Inbox" })).not.toBeInTheDocument()
  })

  test("filters by status and category", async () => {
    const { lists } = proposalApi()
    const { user } = await renderApp("/proposals")
    await article(/Scan dependencies/)
    await user.selectOptions(screen.getByLabelText("Status"), "done")
    await user.selectOptions(screen.getByLabelText("Category"), "workers")
    await waitFor(() => expect(lists.some((q) => q.get("status") === "done" && q.get("category") === "workers")).toBe(true))
  })

  test("marks the setup of an accepted proposal done, and shows a platform issue", async () => {
    const { decisions } = proposalApi()
    const { user } = await renderApp("/proposals")
    const setup = within(await article(/Connect Sentry/))
    expect(setup.getByText("Issue to file on the Forecrew repo")).toBeInTheDocument()
    expect(setup.getByText(/a change to Forecrew itself/)).toBeInTheDocument()
    await user.click(setup.getByRole("button", { name: "Mark as done" }))
    await waitFor(() => expect(decisions).toEqual([{ ref: "PROP-4", body: { decision: "done" } }]))
  })

  test("says why marking done failed", async () => {
    server.use(http.post("/api/v1/proposals/:ref/decide", () => HttpResponse.json(errors.conflict, { status: 409 })))
    const { user } = await renderApp("/proposals")
    const setup = within(await article(/Connect Sentry/))
    await user.click(setup.getByRole("button", { name: "Mark as done" }))
    expect(await setup.findByRole("alert")).toBeInTheDocument()
  })

  test("says when nothing matches", async () => {
    server.use(http.get("/api/v1/proposals", () => HttpResponse.json([])))
    await renderApp("/proposals")
    expect(await screen.findByText("No proposals match.")).toBeInTheDocument()
  })
})

describe("proposal cards in the inbox", () => {
  const card = () => screen.findByRole("article", { name: /GATE-30/ })

  test("show the evidence, benefit, effort, risk and action as text, not the raw summary", async () => {
    server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesProposal)))
    await renderApp("/inbox")
    const c = within(await card())
    expect(c.getByText("Vulnerable packages show up before release.")).toBeInTheDocument()
    expect(c.getByText("Start a goal: Add a scan")).toBeInTheDocument()
    expect(c.getByText("Evidence (1)")).toBeInTheDocument()
    expect(c.getByRole("button", { name: "Accept" })).toBeInTheDocument()
    expect(c.queryByText(/untrusted-data/)).not.toBeInTheDocument()
  })

  test("need a reason to reject", async () => {
    const rejects: unknown[] = []
    server.use(
      http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesProposal)),
      http.post("/api/v1/gates/:ref/reject", async ({ request }) => {
        rejects.push(await request.json())
        return HttpResponse.json(fx.gateReject)
      }),
    )
    const { user } = await renderApp("/inbox")
    const c = within(await card())
    await user.click(c.getByRole("button", { name: "Reject" }))
    await user.click(c.getByRole("button", { name: "Confirm reject" }))
    expect(await c.findByText("A reason is required to reject.")).toBeInTheDocument()
    expect(rejects).toEqual([])
    await user.type(c.getByLabelText(/Why not/), "We use another scanner")
    await user.click(c.getByRole("button", { name: "Confirm reject" }))
    await waitFor(() => expect(rejects).toEqual([{ feedback: "We use another scanner" }]))
  })

  test("snooze until the end of a date", async () => {
    const { decisions } = proposalApi()
    server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesProposal)))
    const { user } = await renderApp("/inbox")
    const c = within(await card())
    await user.click(c.getByRole("button", { name: "Snooze" }))
    await user.click(c.getByRole("button", { name: "Confirm snooze" }))
    expect(await c.findByText("Pick a date.")).toBeInTheDocument()
    await user.type(c.getByLabelText("Snooze until"), "2030-01-02")
    await user.click(c.getByRole("button", { name: "Confirm snooze" }))
    await waitFor(() => expect(decisions).toEqual([{ ref: "PROP-3", body: { decision: "snooze", until: "2030-01-02T23:59:59Z" } }]))
  })

  test("say why a snooze failed, and can be left", async () => {
    server.use(
      http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesProposal)),
      http.post("/api/v1/proposals/:ref/decide", () => HttpResponse.json(errors.conflict, { status: 409 })),
    )
    const { user } = await renderApp("/inbox")
    const c = within(await card())
    await user.click(c.getByRole("button", { name: "Snooze" }))
    await user.type(c.getByLabelText("Snooze until"), "2030-01-02")
    await user.click(c.getByRole("button", { name: "Confirm snooze" }))
    await waitFor(() => expect(c.getByLabelText("Snooze until")).toHaveAccessibleDescription())
    await user.click(c.getAllByRole("button", { name: "Cancel" })[0]!)
    expect(c.getByRole("button", { name: "Snooze" })).toBeInTheDocument()
  })
})

test("home counts the suggestions", async () => {
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesProposal)))
  await renderApp("/")
  const link = await screen.findByRole("link", { name: "Open proposals" })
  expect(link).toHaveAttribute("href", "/proposals")
  expect(within(link.parentElement!).getByText("1")).toBeInTheDocument()
})
