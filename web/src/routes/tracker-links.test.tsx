import { screen } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const as = (...perms: string[]) => server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => !perms.includes(p)) })))

describe("where trackers are met outside Settings", () => {
  const withIntegration = (tool: string) =>
    server.use(http.get("/api/v1/projects/*", () => HttpResponse.json({ ...fx.project, profile: { ...fx.project.profile, integrations: [{ tool, instance: "jira.acme.local", uses: ["issues"], confidence: "high", status: "proposed" }] } })))

  test("the integrations of a project lead to the connect form", async () => {
    withIntegration("Jira")
    server.use(http.get("/api/v1/tracker-connections", () => HttpResponse.json([])))
    const { user } = await renderApp("/knowledge?project=app&tab=integrations")
    const link = await screen.findByRole("link", { name: "Connect Jira as a tracker" })
    expect(link).toHaveAttribute("href", "/settings/integrations?connect=jira&instance=jira.acme.local")
    await user.click(link)
    expect(await screen.findByRole("dialog")).toBeInTheDocument()
  })

  test("no link where a connection exists, and the permission is named where it is missing", async () => {
    withIntegration("Jira")
    await renderApp("/knowledge?project=app&tab=integrations")
    expect(await screen.findByText("jira.acme.local")).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: /as a tracker/ })).toBeNull()
  })

  test("a reader is told what connecting needs", async () => {
    withIntegration("Linear")
    as("trackers.manage")
    server.use(http.get("/api/v1/tracker-connections", () => HttpResponse.json([])))
    await renderApp("/knowledge?project=app&tab=integrations")
    expect(await screen.findByText(/Connecting Linear as a tracker needs/)).toHaveTextContent("trackers.manage")
  })

  test("other tools get no link", async () => {
    withIntegration("Confluence")
    await renderApp("/knowledge?project=app&tab=integrations")
    expect(await screen.findByText("Confluence")).toBeInTheDocument()
    expect(screen.queryByText(/as a tracker/)).toBeNull()
  })
})
