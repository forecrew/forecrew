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

const tokensTable = async () => within(await screen.findByRole("region", { name: "Tokens table" })).getByRole("table")

describe("your account", () => {
  test("is in the account menu and the command menu, and /tokens leads here", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)))
    const { user, router } = await renderApp("/tokens")
    await screen.findByRole("heading", { name: "Your account" })
    expect(router.state.location.pathname).toBe("/account")
    await user.keyboard("{Control>}k{/Control}")
    expect(await screen.findByRole("option", { name: "Your account" })).toBeInTheDocument()
    expect(screen.getByRole("option", { name: "Settings" })).toBeInTheDocument()
  })

  test("lists the tokens with scopes, expiry, last use and the revoked ones", async () => {
    server.use(http.get("/api/v1/tokens", () => HttpResponse.json(fx.tokensScopes)))
    await renderApp("/account")
    const table = await tokensTable()
    const laptop = within(table).getByText("laptop").closest("tr")!
    expect(within(laptop).getByText("this session")).toBeInTheDocument()
    expect(within(laptop).getByText("write")).toBeInTheDocument()
    expect(within(laptop).getByText("1 min ago")).toBeInTheDocument() // last used
    expect(within(laptop).getByText(/^in \d+ d$/)).toBeInTheDocument() // expires
    expect(within(laptop).getByRole("button", { name: "Revoke laptop" })).toBeInTheDocument()
    const old = within(table).getByText("old-laptop").closest("tr")!
    expect(within(old).getByText("expired")).toBeInTheDocument()
    // a service account's token is not a personal one
    expect(within(table).queryByText("ci-box")).not.toBeInTheDocument()
  })

  test("a token from before scopes has none recorded, and one without expiry never expires", async () => {
    await renderApp("/account")
    const table = await tokensTable()
    const old = within(table).getByText("old-laptop").closest("tr")!
    expect(within(old).getByText("not recorded")).toBeInTheDocument()
    expect(within(old).getByText("never", { selector: "span" })).toBeInTheDocument()
    const ci = within(table).getByText("ci-box").closest("tr")!
    expect(within(ci).getByText("revoked")).toBeInTheDocument()
    expect(within(ci).queryByRole("button")).not.toBeInTheDocument()
  })

  test("shows empty and error states of the tokens", async () => {
    server.use(http.get("/api/v1/tokens", () => HttpResponse.json(fx.tokensEmpty)))
    const { unmount } = await renderApp("/account")
    expect(await screen.findByText("No tokens.")).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/tokens", errors.invalidInput))
    await renderApp("/account")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })

  test("creates a token with a name, scope and expiry, shown once with Copy", async () => {
    let body: unknown
    server.use(
      http.post("/api/v1/tokens", async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(fx.tokenCreate, { status: 201 })
      }),
    )
    const { user } = await renderApp("/account")
    await user.click(await screen.findByRole("button", { name: "Create token" }))
    expect(screen.getByText(/Use lowercase letters/)).toBeInTheDocument()
    await user.type(screen.getByLabelText("Token name"), "ci-laptop")
    await user.selectOptions(screen.getByLabelText("Scopes"), "write")
    await user.selectOptions(screen.getByLabelText("Expires after"), "720h")
    await user.click(screen.getByRole("button", { name: "Create token" }))
    const card = await screen.findByRole("region", { name: "Token ci-laptop" })
    expect(within(card).getByLabelText("Token")).toHaveValue(fx.tokenCreate.secret)
    expect(body).toEqual({ name: "ci-laptop", scopes: ["write"], ttl: "720h" })
    // The secret is plain text in a read-only field and goes when dismissed.
    await user.click(within(card).getByRole("button", { name: "Done" }))
    expect(screen.queryByLabelText("Token")).not.toBeInTheDocument()
  })

  test("a failing create shows the server's message", async () => {
    server.use(http.post("/api/v1/tokens", errors.invalidInput))
    const { user } = await renderApp("/account")
    await user.type(await screen.findByLabelText("Token name"), "x")
    await user.click(screen.getByRole("button", { name: "Create token" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("feedback is required")
  })

  test("says what the SSO token limit is when the organization has one", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.meAdmin, account: { ...fx.meAdmin.account, max_token_ttl: "168h0m0s" } })))
    await renderApp("/account")
    expect(await screen.findByText(/limits personal tokens to 7 days/)).toBeInTheDocument()
  })

  test("lists your sign-ins and your sessions, and ends one", async () => {
    server.use(
      http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)),
      http.get("/api/v1/sessions", () => HttpResponse.json(fx.sessions.map((s, i) => ({ ...s, current: i === 0 })))),
    )
    const ended: string[] = []
    server.use(
      http.delete("/api/v1/sessions/:id", ({ params }) => {
        ended.push(String(params.id))
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const { user } = await renderApp("/account")
    const signins = await screen.findByRole("region", { name: "Sign-ins table" })
    expect(within(signins).getByText("github")).toBeInTheDocument()
    expect(within(signins).getByText("ana")).toBeInTheDocument()
    const sessions = await screen.findByRole("region", { name: "Sessions table" })
    expect(within(sessions).getByText(/Firefox on Linux/)).toBeInTheDocument()
    expect(within(sessions).getByText("this session")).toBeInTheDocument()
    expect(within(sessions).getAllByRole("button")).toHaveLength(1)
    await user.click(within(sessions).getByRole("button", { name: "End session, Safari on iPhone" }))
    await waitFor(() => expect(ended).toEqual([fx.sessions[1]!.id]))
  })

  test("a failing end of a session is shown", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)), http.delete("/api/v1/sessions/:id", errors.notFound))
    const { user } = await renderApp("/account")
    await user.click((await screen.findAllByRole("button", { name: /End session,/ }))[0]!)
    expect(await screen.findByRole("alert")).toHaveTextContent("not found")
  })

  test("a token login has no linked sign-in and no sessions to manage", async () => {
    let asked = false
    server.use(
      http.get("/api/v1/sessions", () => {
        asked = true
        return HttpResponse.json([])
      }),
    )
    await renderApp("/account")
    expect(await screen.findByText(/You use a token, which is not linked to a sign-in/)).toBeInTheDocument()
    await screen.findByRole("heading", { name: "Personal tokens" })
    expect(screen.queryByRole("heading", { name: "Sessions" })).not.toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Sign out everywhere" })).not.toBeInTheDocument()
    expect(asked).toBe(false)
  })

  test("a person with no browser session left is told so", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)), http.get("/api/v1/sessions", () => HttpResponse.json([])))
    await renderApp("/account")
    expect(await screen.findByText("No browser sessions.")).toBeInTheDocument()
  })

  test("a person without sign-ins linked is told so", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)), http.get("/api/v1/identities", () => HttpResponse.json([])))
    await renderApp("/account")
    expect(await screen.findByText("No sign-in is linked to your account.")).toBeInTheDocument()
  })

  test("Sign out everywhere ends every session", async () => {
    let everywhere = false
    server.use(
      http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)),
      http.delete("/api/v1/sessions", () => {
        everywhere = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const { user, router } = await renderApp("/account")
    await user.click(await screen.findByRole("button", { name: "Sign out everywhere" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
    expect(everywhere).toBe(true)
  })

  test("revokes another token after confirming, and shows it as revoked", async () => {
    const ids = recordRevokes()
    const { user, router } = await renderApp("/account")
    await user.click(await screen.findByRole("button", { name: "Revoke old-laptop" }))
    const dialog = await screen.findByRole("dialog", { name: "Revoke old-laptop?" })
    expect(within(dialog).queryByText(/You will be logged out/)).not.toBeInTheDocument()
    expect(ids).toEqual([])
    await user.click(within(dialog).getByRole("button", { name: "Revoke old-laptop" }))
    await waitFor(() => expect(ids).toEqual([fx.tokens[1]!.id]))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    const row = (await tokensTable()).querySelector("tr:nth-child(2)") as HTMLElement
    await waitFor(() => expect(within(row).getByText("revoked")).toBeInTheDocument())
    expect(within(row).queryByRole("button")).not.toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/account")
  })

  test("keeping the token makes no call", async () => {
    const ids = recordRevokes()
    const { user } = await renderApp("/account")
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
    const { user, router } = await renderApp("/account")
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
    const { user, router } = await renderApp("/account")
    await user.click(await screen.findByRole("button", { name: "Revoke laptop" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Revoke laptop" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
  })

  test("shows the API message when the revoke fails", async () => {
    server.use(http.post("/api/v1/tokens/:id/revoke", errors.notFound))
    const { user } = await renderApp("/account")
    await user.click(await screen.findByRole("button", { name: "Revoke old-laptop" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Revoke old-laptop" }))
    expect(await screen.findByText("goal GOAL-99: not found")).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Keep token" }))
    await user.click(await screen.findByRole("button", { name: "Revoke old-laptop" }))
    await screen.findByRole("dialog")
    expect(screen.queryByText("goal GOAL-99: not found")).not.toBeInTheDocument()
  })
  test("lists the connected apps and disconnects one", async () => {
    let apps = fx.connectedApps
    const ids: string[] = []
    server.use(
      http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)),
      http.get("/api/v1/connected-apps", () => HttpResponse.json(apps)),
      http.post("/api/v1/connected-apps/:id/revoke", ({ params }) => {
        ids.push(String(params.id))
        const app = apps[0]
        apps = []
        return HttpResponse.json(app)
      }),
    )
    const { user } = await renderApp("/account")
    const row = (await screen.findByText("Claude")).closest("tr")!
    expect(within(row).getByText("read, write")).toBeInTheDocument()
    await user.click(within(row).getByRole("button", { name: "Disconnect Claude" }))
    await user.click(await screen.findByRole("button", { name: "Disconnect Claude" }))
    await waitFor(() => expect(ids).toEqual([fx.connectedApps[0]!.id]))
    expect(await screen.findByText("No connected apps.")).toBeInTheDocument()
  })

  test("a token login has no connected apps to show", async () => {
    await renderApp("/account")
    await screen.findByRole("heading", { name: "Personal tokens" })
    expect(screen.queryByRole("heading", { name: "Connected apps" })).not.toBeInTheDocument()
  })
})
