import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

function recordRevokes() {
  const ids: string[] = []
  let revoked = false
  server.use(
    http.get("/api/v1/tokens", () =>
      HttpResponse.json(revoked ? fx.tokens.map((t) => (t.name === "old-laptop" ? { ...t, revoked_at: "2026-03-01T12:00:00Z" } : t)) : fx.tokens),
    ),
    http.post("/api/v1/tokens/:id/revoke", ({ params }) => {
      ids.push(String(params.id))
      revoked = true
      return HttpResponse.json(fx.tokenRevoke)
    }),
  )
  return ids
}

describe("tokens page", () => {
  test("is in the navigation and the command menu", async () => {
    const { user, router } = await renderApp("/")
    await user.click(await screen.findByRole("link", { name: "Tokens" }))
    await screen.findByRole("heading", { name: "Tokens" })
    expect(router.state.location.pathname).toBe("/tokens")
    await user.keyboard("{Control>}k{/Control}")
    expect(await screen.findByRole("option", { name: "Tokens" })).toBeInTheDocument()
  })

  test("lists the tokens with their times, the current session and the revoked ones", async () => {
    await renderApp("/tokens")
    const table = await screen.findByRole("table")
    const laptop = within(table).getByText("laptop").closest("tr")!
    expect(within(laptop).getByText("this session")).toBeInTheDocument()
    expect(within(laptop).getByText("1 min ago")).toBeInTheDocument() // last used
    expect(within(laptop).getByRole("button", { name: "Revoke laptop" })).toBeInTheDocument()

    const old = within(table).getByText("old-laptop").closest("tr")!
    expect(within(old).queryByText("this session")).not.toBeInTheDocument()
    expect(within(old).getByText("no")).toBeInTheDocument()

    const ci = within(table).getByText("ci-box").closest("tr")!
    expect(within(ci).getByText("revoked")).toBeInTheDocument()
    expect(within(ci).getByText("never")).toBeInTheDocument() // never used
    expect(within(ci).queryByRole("button")).not.toBeInTheDocument()
  })

  test("says how to create tokens and links the runbook", async () => {
    await renderApp("/tokens")
    expect(await screen.findByText("forecrew token create")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: /MCP token runbook/ })).toHaveAttribute(
      "href",
      "https://github.com/forecrew/forecrew/blob/main/docs/runbooks/mcp-token.md",
    )
  })

  test("shows empty and error states", async () => {
    server.use(http.get("/api/v1/tokens", () => HttpResponse.json(fx.tokensEmpty)))
    const { unmount } = await renderApp("/tokens")
    expect(await screen.findByText("No tokens.")).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/tokens", errors.invalidInput))
    await renderApp("/tokens")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })

  test("revokes another token after confirming, and shows it as revoked", async () => {
    const ids = recordRevokes()
    const { user, router } = await renderApp("/tokens")
    await user.click(await screen.findByRole("button", { name: "Revoke old-laptop" }))
    const dialog = await screen.findByRole("dialog", { name: "Revoke old-laptop?" })
    expect(within(dialog).queryByText(/You will be logged out/)).not.toBeInTheDocument()
    expect(ids).toEqual([])
    await user.click(within(dialog).getByRole("button", { name: "Revoke old-laptop" }))
    await waitFor(() => expect(ids).toEqual([fx.tokens[1]!.id]))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    const row = screen.getByText("old-laptop").closest("tr")!
    await waitFor(() => expect(within(row).getByText("revoked")).toBeInTheDocument())
    expect(within(row).queryByRole("button")).not.toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/tokens")
  })

  test("keeping the token makes no call", async () => {
    const ids = recordRevokes()
    const { user } = await renderApp("/tokens")
    await user.click(await screen.findByRole("button", { name: "Revoke old-laptop" }))
    await user.click(await screen.findByRole("button", { name: "Keep token" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    expect(ids).toEqual([])
  })

  test("warns before revoking the current token, then goes to the login page", async () => {
    let loggedOut = false
    server.use(http.delete("/api/v1/session", () => {
      loggedOut = true
      return new HttpResponse(null, { status: 204 })
    }))
    const { user, router } = await renderApp("/tokens")
    await user.click(await screen.findByRole("button", { name: "Revoke laptop" }))
    const dialog = await screen.findByRole("dialog", { name: "Revoke laptop?" })
    expect(dialog).toHaveTextContent("You will be logged out")
    await user.click(within(dialog).getByRole("button", { name: "Revoke laptop" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
    expect(loggedOut).toBe(true)
    expect(await screen.findByLabelText("API token")).toBeInTheDocument()
  })

  test("still goes to the login page when clearing the cookie fails", async () => {
    server.use(http.delete("/api/v1/session", errors.unauthorized))
    const { user, router } = await renderApp("/tokens")
    await user.click(await screen.findByRole("button", { name: "Revoke laptop" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Revoke laptop" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
  })

  test("shows the API message when the revoke fails", async () => {
    server.use(http.post("/api/v1/tokens/:id/revoke", errors.notFound))
    const { user } = await renderApp("/tokens")
    await user.click(await screen.findByRole("button", { name: "Revoke old-laptop" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Revoke old-laptop" }))
    expect(await screen.findByText("goal GOAL-99: not found")).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Keep token" }))
    await user.click(await screen.findByRole("button", { name: "Revoke old-laptop" }))
    await screen.findByRole("dialog")
    expect(screen.queryByText("goal GOAL-99: not found")).not.toBeInTheDocument()
  })
})
