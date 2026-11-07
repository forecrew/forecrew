import { screen, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const fresh = () => server.use(http.get("/api/v1/fleet", () => HttpResponse.json(fx.fleetEmpty)), http.get("/api/v1/projects", () => HttpResponse.json(fx.projectsEmpty)))

test("first run asks where jobs run before which repository", async () => {
  fresh()
  await renderApp("/setup")
  const where = await screen.findByRole("list", { name: "Where should agents work?" })
  expect(within(where).getByRole("link", { name: /A computer of mine/ })).toHaveAttribute("href", "/machines/add?kind=computer")
  expect(within(where).getByRole("link", { name: /Decide later/ })).toHaveAttribute("href", "/setup?tab=repo")
  expect(within(where).queryByRole("link", { name: /Forecrew Cloud/ })).toBeNull()
  expect(screen.queryByRole("link", { name: "Pick a repository" })).toBeNull()
  expect(screen.getByRole("heading", { name: /1\. Where should agents work\?/ }).closest("li")).toHaveAttribute("aria-current", "step")
})

test("deciding later moves on to the repository, and a repository to the first goal", async () => {
  fresh()
  const { unmount } = await renderApp("/setup?tab=repo")
  expect(await screen.findByRole("link", { name: "Pick a repository" })).toHaveAttribute("href", "/projects/new")
  unmount()
  server.use(http.get("/api/v1/fleet", () => HttpResponse.json(fx.fleet)), http.get("/api/v1/projects", () => HttpResponse.json(fx.projects)))
  await renderApp("/setup")
  expect(await screen.findByRole("link", { name: "Describe a first goal" })).toHaveAttribute("href", "/goals/new")
  expect(screen.getByRole("heading", { name: /1\. Where should agents work\?/ })).toHaveTextContent("(done)")
})

test("Home welcomes a first run and points to the setup", async () => {
  fresh()
  await renderApp("/")
  const welcome = await screen.findByRole("region", { name: "Welcome" })
  expect(within(welcome).getByRole("link", { name: "Set up Forecrew" })).toHaveAttribute("href", "/setup")
})

test("Home does not welcome someone who cannot add machines, or an organization that has them", async () => {
  fresh()
  server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meViewer)))
  const { unmount } = await renderApp("/")
  await screen.findByText(/Nothing needs you|decision/)
  expect(screen.queryByRole("region", { name: "Welcome" })).toBeNull()
  unmount()
  server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.me)), http.get("/api/v1/fleet", () => HttpResponse.json(fx.fleet)))
  await renderApp("/")
  await screen.findByText(/decisions? need/)
  expect(screen.queryByRole("region", { name: "Welcome" })).toBeNull()
})
