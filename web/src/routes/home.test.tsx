import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"
import { StageBar } from "./home"
import { render } from "@testing-library/react"

const panel = async (name: string) => (await screen.findByRole("heading", { name })).closest("[data-slot=card]") as HTMLElement

test("has the command bar, the New goal button, the date and the headline", async () => {
  const { user } = await renderApp("/")
  expect(await screen.findByRole("heading", { level: 1, name: "Home" })).toBeInTheDocument()
  expect(await screen.findByText(/1 March/)).toBeInTheDocument()
  expect(await screen.findByText("3 decisions need you")).toBeInTheDocument()
  expect(screen.getByRole("link", { name: "New goal" })).toHaveAttribute("href", "/goals/new")
  await user.click(screen.getByRole("button", { name: /Search or run a command/ }))
  expect(await screen.findByRole("dialog")).toBeInTheDocument()
})

test("shows the three status tiles", async () => {
  server.use(http.get("/api/v1/jobs", ({ request }) => HttpResponse.json(new URL(request.url).searchParams.get("status") === "running" ? [fx.jobs[0]] : fx.jobs)))
  await renderApp("/")
  const tile = await panel("Decisions waiting")
  expect(await within(tile).findByText("3")).toBeInTheDocument()
  expect(within(tile).getByText(/Oldest waiting/)).toHaveTextContent("45 min")

  const agents = await panel("Agents working")
  expect(await within(agents).findByText("1")).toBeInTheDocument()
  expect(within(agents).getByText("jobs running")).toBeInTheDocument()

  const workers = await panel("Workers online")
  expect(await within(workers).findByText("1 of 2")).toBeInTheDocument()
  expect(within(workers).getByText("Some offline")).toBeInTheDocument()
})

test("lists what needs you with Review or Decide", async () => {
  await renderApp("/")
  const list = await panel("Needs you")
  expect(await within(list).findByText("Merge PR #9")).toBeInTheDocument()
  expect(within(list).getByText("Merge approval")).toBeInTheDocument()
  expect(within(list).getByRole("link", { name: "Review GATE-9" })).toHaveAttribute("href", "/inbox?gate=GATE-9")
  expect(within(list).getByRole("link", { name: "Decide GATE-11" })).toHaveAttribute("href", "/inbox?gate=GATE-11")
  // the outline variant's border must win over the base transparent one
  const review = within(list).getByRole("link", { name: "Review GATE-9" })
  expect(review).toHaveClass("border-border")
  expect(review).not.toHaveClass("border-transparent")
  expect(within(list).getByRole("link", { name: "Open inbox" })).toHaveAttribute("href", "/inbox")
})

test("shows active goals with a stage", async () => {
  await renderApp("/")
  const goals = await panel("Active goals")
  expect(await within(goals).findByRole("link", { name: /GOAL-3 Sign in/ })).toHaveAttribute("href", "/goals/GOAL-3")
  expect(within(goals).getByText("building")).toBeInTheDocument()
  expect(within(goals).getByText("spec")).toBeInTheDocument()
  expect(within(goals).queryByText(/Export to CSV/)).not.toBeInTheDocument() // done
  expect(within(goals).getByRole("link", { name: "See all goals" })).toHaveAttribute("href", "/goals")
})

test("shows live activity and the fleet", async () => {
  await renderApp("/")
  const live = await panel("Live activity")
  expect(await within(live).findByRole("link", { name: /JOB-41 developer/ })).toBeInTheDocument()
  expect(within(live).getByRole("link", { name: "Watch" })).toHaveAttribute("href", "/jobs?status=running")
  const fleet = await panel("Fleet")
  expect(await within(fleet).findByText("build-box-1")).toBeInTheDocument()
  expect(within(fleet).getByText("1 of 1 jobs")).toBeInTheDocument()
  expect(within(fleet).getByText("mac-mini")).toBeInTheDocument()
  expect(within(fleet).getByText("offline")).toBeInTheDocument()
})

test("shows calm states when there is nothing to do", async () => {
  server.use(
    http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)),
    http.get("/api/v1/goals", () => HttpResponse.json(fx.goalsEmpty)),
    http.get("/api/v1/jobs", () => HttpResponse.json(fx.jobsEmpty)),
    http.get("/api/v1/workers", () => HttpResponse.json(fx.workersEmpty)),
  )
  await renderApp("/")
  expect((await screen.findAllByText("Nothing needs you")).length).toBe(2)
  expect(screen.getByText("All clear")).toBeInTheDocument()
  expect(screen.getByText("No active goals")).toBeInTheDocument()
  expect(screen.getAllByText("No jobs running")).toHaveLength(2)
  expect(screen.getByText("0 of 0")).toBeInTheDocument()
  expect(screen.getAllByText("No workers")).toHaveLength(2)
})

test("says so when all workers are healthy or all offline, and when one decision waits", async () => {
  server.use(
    http.get("/api/v1/gates", () => HttpResponse.json(fx.gates.slice(0, 1))),
    http.get("/api/v1/workers", () => HttpResponse.json(fx.workers.map((w) => ({ ...w, online: true })))),
  )
  const { unmount } = await renderApp("/")
  expect(await screen.findByText("1 decision needs you")).toBeInTheDocument()
  expect(await screen.findByText("Healthy")).toBeInTheDocument()
  unmount()
  server.use(http.get("/api/v1/workers", () => HttpResponse.json(fx.workers.map((w) => ({ ...w, online: false })))))
  await renderApp("/")
  expect(await screen.findByText("All offline")).toBeInTheDocument()
})

test("each panel shows its own error", async () => {
  server.use(http.get("/api/v1/workers", errors.invalidInput), http.get("/api/v1/gates", errors.invalidInput), http.get("/api/v1/goals", errors.invalidInput), http.get("/api/v1/jobs", errors.invalidInput))
  await renderApp("/")
  await waitFor(() => expect(screen.getAllByText(/Could not load: feedback is required/)).toHaveLength(7))
})

test("panels show a loading state first", async () => {
  await renderApp("/")
  expect((await screen.findAllByText("Loading…")).length).toBeGreaterThan(0)
  await screen.findByText("3 decisions need you")
})

test("the stage bar fills segments up to the goal's stage", () => {
  const filled = (status: Parameters<typeof StageBar>[0]["status"]) => {
    const { container, unmount } = render(<StageBar status={status} />)
    const n = container.querySelectorAll("span.bg-primary").length
    unmount()
    return n
  }
  expect(filled("draft")).toBe(1)
  expect(filled("spec_review")).toBe(1)
  expect(filled("planned")).toBe(2)
  expect(filled("in_progress")).toBe(3)
  expect(filled("done")).toBe(4)
  expect(filled("canceled")).toBe(1)
})
