import { screen, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

test("lists workers with pool, status, labels, version and running jobs", async () => {
  await renderApp("/workers")
  const row = (await screen.findByText("build-box-1")).closest("tr")!
  expect(within(row).getByText("build")).toBeInTheDocument()
  expect(within(row).getByText("online")).toBeInTheDocument()
  expect(within(row).getByRole("list", { name: "Labels of build-box-1" })).toHaveTextContent("runtime:claude-code")
  expect(within(row).getByText("v0.3.1")).toBeInTheDocument()
  expect(within(row).getByRole("link", { name: "JOB-41" })).toHaveAttribute("href", "/jobs/JOB-41")
  const idle = screen.getByText("mac-mini").closest("tr")!
  expect(within(idle).getByText("control")).toBeInTheDocument()
  expect(within(idle).getByText("offline")).toBeInTheDocument()
  expect(within(idle).getByText("idle")).toBeInTheDocument()
  expect(within(idle).getByText("2 h ago")).toBeInTheDocument()
})

test("explains how to add a worker", async () => {
  await renderApp("/workers")
  const section = (await screen.findByRole("heading", { name: "Add a worker" })).closest("section")!
  expect(within(section).getByText("forecrew worker add build-box-1 --pool build")).toBeInTheDocument()
  expect(section).toHaveTextContent("install.sh")
  expect(section).toHaveTextContent(`--server ${window.location.origin}`)
})

test("shows build for a worker stored without a pool", async () => {
  server.use(http.get("/api/v1/workers", () => HttpResponse.json([{ ...fx.workers[0], pool: "" }])))
  await renderApp("/workers")
  const row = (await screen.findByText("build-box-1")).closest("tr")!
  expect(within(row).getByText("build")).toBeInTheDocument()
})

test("shows the empty state with the instructions", async () => {
  server.use(http.get("/api/v1/workers", () => HttpResponse.json(fx.workersEmpty)))
  await renderApp("/workers")
  expect(await screen.findByText(/No workers yet/)).toBeInTheDocument()
  expect(screen.getByRole("heading", { name: "Add a worker" })).toBeInTheDocument()
})

test("shows an error", async () => {
  server.use(http.get("/api/v1/workers", errors.invalidInput))
  await renderApp("/workers")
  expect(await screen.findByText("Could not load this")).toBeInTheDocument()
})
