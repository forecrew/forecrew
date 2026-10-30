import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const tile = async (name: string) => (await screen.findByRole("heading", { name })).closest("[data-slot=card]") as HTMLElement

test("shows the four tiles", async () => {
  await renderApp("/")
  const decisions = await tile("Decisions")
  expect(await within(decisions).findByText("3 decisions need you")).toBeInTheDocument()
  expect(within(decisions).getByText(/Oldest waiting/)).toHaveTextContent("45 min")
  expect(within(decisions).getByRole("link", { name: "Open the inbox" })).toHaveAttribute("href", "/inbox")

  const goals = await tile("Goals")
  expect(await within(goals).findByText("2 active")).toBeInTheDocument()
  expect(within(goals).getByRole("link", { name: /GOAL-3 Sign in/ })).toHaveAttribute("href", "/goals/GOAL-3")
  expect(within(goals).getByText("building")).toBeInTheDocument()
  expect(within(goals).getByText("spec")).toBeInTheDocument()
  expect(within(goals).queryByText(/Export to CSV/)).not.toBeInTheDocument() // done

  const jobs = await tile("Jobs")
  expect(within(jobs).getByRole("link", { name: "See running jobs" })).toHaveAttribute("href", "/jobs?status=running")

  const workers = await tile("Workers")
  expect(await within(workers).findByText("1 of 2 online")).toBeInTheDocument()
  expect(within(workers).getByText("Some offline")).toBeInTheDocument()
})

test("shows running jobs from the jobs API", async () => {
  server.use(http.get("/api/v1/jobs", ({ request }) => HttpResponse.json(new URL(request.url).searchParams.get("status") === "running" ? [fx.jobs[0]] : fx.jobs)))
  await renderApp("/")
  const jobs = await tile("Jobs")
  expect(await within(jobs).findByText("1 running")).toBeInTheDocument()
  expect(within(jobs).getByRole("link", { name: /JOB-41 developer/ })).toBeInTheDocument()
})

test("shows calm states when there is nothing to do", async () => {
  server.use(
    http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)),
    http.get("/api/v1/goals", () => HttpResponse.json(fx.goalsEmpty)),
    http.get("/api/v1/jobs", () => HttpResponse.json(fx.jobsEmpty)),
    http.get("/api/v1/workers", () => HttpResponse.json(fx.workersEmpty)),
  )
  await renderApp("/")
  expect(await screen.findByText("Nothing needs you")).toBeInTheDocument()
  expect(screen.getByText("All clear")).toBeInTheDocument()
  expect(screen.getByText("No active goals")).toBeInTheDocument()
  expect(screen.getByText("No jobs running")).toBeInTheDocument()
  expect(screen.getByText("0 of 0 online")).toBeInTheDocument()
  expect(screen.getByText("No workers")).toBeInTheDocument()
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

test("each tile shows its own error", async () => {
  server.use(http.get("/api/v1/workers", errors.invalidInput), http.get("/api/v1/gates", errors.invalidInput), http.get("/api/v1/goals", errors.invalidInput), http.get("/api/v1/jobs", errors.invalidInput))
  await renderApp("/")
  await waitFor(() => expect(screen.getAllByText(/Could not load: feedback is required/)).toHaveLength(4))
})

test("tiles show a loading state first", async () => {
  await renderApp("/")
  expect((await screen.findAllByText("Loading…")).length).toBeGreaterThan(0)
  await screen.findByText("3 decisions need you")
})
