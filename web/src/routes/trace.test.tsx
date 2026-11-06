import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import type { Trace } from "@/api/types"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const trace = fx.trace as Trace

function recordResolves() {
  const calls: { ref: string; body: unknown }[] = []
  server.use(
    http.post("/api/v1/assumptions/:ref/resolve", async ({ params, request }) => {
      calls.push({ ref: String(params.ref), body: await request.json() })
      return HttpResponse.json(fx.assumptionRegisterResolve)
    }),
  )
  return calls
}

describe("trace page", () => {
  test("shows the chain from the goal to the merge, with assumptions under what they affect", async () => {
    await renderApp("/trace/TASK-12")
    await screen.findByRole("heading", { level: 1, name: "Why TASK-12?" })
    const chain = screen.getByRole("list", { name: "Trace" })
    const steps = within(chain)
      .getAllByRole("listitem")
      .filter((li) => li.parentElement === chain)
    expect(steps.map((li) => within(li).getAllByText(/^(GOAL|COUNCIL|SPEC|DEC|TASK)-\d+$|^acme\/app#9$|^(verification|merge):acme\/app#9$/)[0]!.textContent)).toEqual([
      "GOAL-3", "COUNCIL-1", "SPEC-1", "SPEC-2", "DEC-1", "DEC-2", "TASK-12", "acme/app#9", "verification:acme/app#9", "merge:acme/app#9",
    ])
    // where each step came from
    expect(within(steps[3]!).getByText("revised SPEC-1")).toBeInTheDocument()
    expect(within(steps[8]!).getByText("verified acme/app#9")).toBeInTheDocument()
    // assumptions sit under the nodes they affect
    const task = within(steps[6]!).getByRole("list", { name: "Assumptions of TASK-12" })
    expect(within(task).getByRole("button", { name: /A-4/ })).toBeInTheDocument()
    expect(within(within(steps[0]!).getByRole("list", { name: "Assumptions of GOAL-3" })).getByRole("button", { name: /A-4/ })).toBeInTheDocument()
    expect(within(within(steps[3]!).getByRole("list", { name: "Assumptions of SPEC-2" })).getByRole("button", { name: /A-5/ })).toBeInTheDocument()
  })

  test("opens on the node asked for and shows another when clicked", async () => {
    const { user } = await renderApp("/trace/TASK-12")
    const detail = await screen.findByRole("region", { name: "Send the sign-in link" })
    expect(within(detail).getByText("merged")).toBeInTheDocument()
    expect(within(detail).getByRole("link", { name: /Open on GitHub/ })).toHaveAttribute("href", "https://github.com/acme/app/issues/6")
    expect(within(detail).getByRole("link", { name: "Open GOAL-3" })).toHaveAttribute("href", "/goals/GOAL-3")
    expect(within(detail).queryByRole("link", { name: "Trace TASK-12" })).not.toBeInTheDocument()

    const chain = screen.getByRole("list", { name: "Trace" })
    await user.click(within(chain).getByRole("button", { name: /Pull request #9/ }))
    const pr = screen.getByRole("region", { name: "Pull request #9" })
    expect(within(pr).getByText("agent/task-12")).toBeInTheDocument()
    expect(within(pr).getByRole("link", { name: /Open on GitHub/ })).toHaveAttribute("href", "https://github.com/acme/app/pull/9")
    expect(within(pr).getByRole("link", { name: "Trace acme/app#9" })).toBeInTheDocument()

    // A decision shows who decided and why.
    await user.click(within(chain).getByRole("button", { name: /COUNCIL-1/ }))
    const council = screen.getByRole("region", { name: "Which mailer?" })
    expect(within(council).getByText("Use the existing mailer.")).toBeInTheDocument()

    // Verification and merge have no ref to trace from.
    await user.click(within(chain).getByRole("button", { name: /Verification of #9/ }))
    expect(screen.getByRole("region", { name: "Verification of #9" })).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: /^Trace / })).not.toBeInTheDocument()
  })

  test("follows a link to trace another node", async () => {
    const { user, router } = await renderApp("/trace/TASK-12")
    await user.click(await screen.findByRole("button", { name: /Spec revision 2/ }))
    await user.click(screen.getByRole("link", { name: "Trace SPEC-2" }))
    await screen.findByRole("heading", { level: 1, name: "Why SPEC-2?" })
    expect(router.state.location.pathname).toBe("/trace/SPEC-2")
    // The page starts from the focus of the answer again: the fixture's.
    expect(await screen.findByRole("region", { name: "Send the sign-in link" })).toBeInTheDocument()
  })

  test("shows agent text as text", async () => {
    const { user, container } = await renderApp("/trace/TASK-12")
    const chain = await screen.findByRole("list", { name: "Trace" })
    await user.click(within(chain).getByRole("button", { name: /Admins/ }))
    expect(screen.getByRole("region", { name: /Admins <b>never<\/b>/ })).toBeInTheDocument()
    expect(container.querySelector("b")).toBeNull()
  })

  test("verifies an assumption with its evidence, in place", async () => {
    const calls = recordResolves()
    const { user } = await renderApp("/trace/TASK-12")
    const chain = await screen.findByRole("list", { name: "Trace" })
    await user.click(within(chain).getAllByRole("button", { name: /A-4/ })[0]!)
    const detail = screen.getByRole("region", { name: "Links are sent by the existing mailer." })
    expect(within(detail).getByText("high")).toBeInTheDocument() // impact
    await user.click(within(detail).getByRole("button", { name: "Verify A-4" }))
    // Evidence is required to verify.
    await user.click(within(detail).getByRole("button", { name: "Confirm verify" }))
    expect(within(detail).getByText("Evidence is required to verify or invalidate.")).toBeInTheDocument()
    expect(calls).toEqual([])
    await user.type(within(detail).getByLabelText("Evidence for A-4"), "  The mailer is in production.  ")
    await user.click(within(detail).getByRole("button", { name: "Confirm verify" }))
    await waitFor(() => expect(calls).toEqual([{ ref: "A-4", body: { status: "verified", evidence: "The mailer is in production." } }]))
    await waitFor(() => expect(within(detail).queryByLabelText("Evidence for A-4")).not.toBeInTheDocument())
  })

  test("accepts without evidence and invalidates with a warning", async () => {
    const calls = recordResolves()
    const { user } = await renderApp("/trace/TASK-12")
    const chain = await screen.findByRole("list", { name: "Trace" })
    await user.click(within(chain).getAllByRole("button", { name: /A-4/ })[0]!)
    const detail = screen.getByRole("region", { name: "Links are sent by the existing mailer." })
    await user.click(within(detail).getByRole("button", { name: "Invalidate A-4" }))
    expect(within(detail).getByText(/sends unmerged dependent tasks back/)).toBeInTheDocument()
    await user.click(within(detail).getByRole("button", { name: "Cancel" }))
    await user.click(within(detail).getByRole("button", { name: "Accept A-4" }))
    expect(within(detail).getByLabelText("Evidence for A-4 (optional)")).toBeInTheDocument()
    await user.click(within(detail).getByRole("button", { name: "Confirm accept" }))
    await waitFor(() => expect(calls).toEqual([{ ref: "A-4", body: { status: "accepted" } }]))
  })

  test("offers only the moves an assumption allows and shows a failure", async () => {
    server.use(http.post("/api/v1/assumptions/:ref/resolve", errors.conflict))
    const { user } = await renderApp("/trace/TASK-12")
    const chain = await screen.findByRole("list", { name: "Trace" })
    await user.click(within(chain).getByRole("button", { name: /A-5/ }))
    const detail = screen.getByRole("region", { name: /Admins/ })
    expect(within(detail).getByText("Ana accepts the risk.")).toBeInTheDocument()
    expect(within(detail).queryByRole("button", { name: /Accept A-5/ })).not.toBeInTheDocument()
    await user.click(within(detail).getByRole("button", { name: "Verify A-5" }))
    await user.type(within(detail).getByLabelText("Evidence for A-5"), "checked")
    await user.click(within(detail).getByRole("button", { name: "Confirm verify" }))
    expect(await within(detail).findByRole("alert")).toBeInTheDocument()
  })

  test("a record of no goal is a chain of one", async () => {
    const one: Trace = { focus: "A-6", nodes: [{ ...trace.nodes.find((n) => n.id === "A-4")!, id: "A-6", status: "invalidated" }], edges: [] }
    server.use(http.get("/api/v1/trace", () => HttpResponse.json(one)))
    await renderApp("/trace/A-6")
    expect(await screen.findByText("No goal chain to show: this record has no goal, or you may not read it.")).toBeInTheDocument()
    const detail = screen.getByRole("region", { name: /Links are sent/ })
    expect(within(detail).queryByRole("button", { name: /Verify/ })).not.toBeInTheDocument() // invalidated: final
    expect(within(screen.getByRole("list", { name: "Trace" })).getByRole("button", { name: /A-6/ })).toBeInTheDocument()
  })

  test("shows empty, missing and error states", async () => {
    server.use(http.get("/api/v1/trace", () => HttpResponse.json({ focus: "GOAL-1", nodes: [], edges: [] })))
    const first = await renderApp("/trace/GOAL-1")
    expect(await screen.findByText("Nothing to trace.")).toBeInTheDocument()
    first.unmount()
    server.use(http.get("/api/v1/trace", errors.notFound))
    await renderApp("/trace/GOAL-99")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })
})

describe("Why links", () => {
  test("the goal page links the trace of the goal and of each task", async () => {
    await renderApp("/goals/GOAL-3")
    expect(await screen.findByRole("link", { name: "Why GOAL-3?" })).toHaveAttribute("href", "/trace/GOAL-3")
    const tasks = screen.getByRole("region", { name: "Tasks table" })
    expect(within(tasks).getByRole("link", { name: "Why TASK-12?" })).toHaveAttribute("href", "/trace/TASK-12")
  })

  test("a gate links the trace of its task, else of its goal", async () => {
    await renderApp("/inbox")
    const first = await screen.findByRole("article", { name: /GATE-9/ })
    expect(within(first).getByRole("link", { name: "Why TASK-12?" })).toHaveAttribute("href", "/trace/TASK-12")
  })

  test("a gate of a goal links the trace of the goal", async () => {
    const { user } = await renderApp("/inbox")
    await screen.findByRole("article", { name: /GATE-9/ })
    await user.click(within(screen.getByRole("list", { name: "Pending decisions" })).getByRole("button", { name: /Approve the spec/ }))
    expect(within(screen.getByRole("article", { name: /GATE-10/ })).getByRole("link", { name: "Why GOAL-4?" })).toHaveAttribute("href", "/trace/GOAL-4")
  })

  test("a gate of neither has no link", async () => {
    server.use(http.get("/api/v1/gates", () => HttpResponse.json([{ ...fx.gates[2]!, task_ref: undefined, goal_ref: undefined }])))
    await renderApp("/inbox")
    const g = await screen.findByRole("article", { name: /GATE-11/ })
    expect(within(g).queryByRole("link", { name: /Why/ })).not.toBeInTheDocument()
  })

  test("a job links the trace of its task, else of its goal", async () => {
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json({ ...fx.job, task_id: fx.goal.tasks[0]!.id })))
    const first = await renderApp("/jobs/JOB-41")
    expect(await screen.findByRole("link", { name: "Why TASK-12?" })).toHaveAttribute("href", "/trace/TASK-12")
    first.unmount()
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json({ ...fx.job, task_id: undefined })))
    await renderApp("/jobs/JOB-41")
    expect(await screen.findByRole("link", { name: "Why GOAL-3?" })).toHaveAttribute("href", "/trace/GOAL-3")
  })

  test("a job of no goal has no link", async () => {
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json({ ...fx.job, goal_id: undefined, task_id: undefined })))
    await renderApp("/jobs/JOB-41")
    await screen.findByRole("heading", { level: 1, name: /JOB-41/ })
    expect(screen.queryByRole("link", { name: /Why/ })).not.toBeInTheDocument()
  })
})
