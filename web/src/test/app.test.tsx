import { screen, waitFor } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "./server"
import { renderApp } from "./render"

describe("login and session", () => {
  test("a visit without a session goes to the login page and back after login", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized))
    const { user, router } = await renderApp("/goals?status=draft")
    expect(await screen.findByRole("heading", { name: "Log in to Forecrew" })).toBeInTheDocument()
    expect(router.state.location.search).toEqual({ redirect: "/goals?status=draft" })

    server.resetHandlers() // the session is valid from now on
    await user.type(await screen.findByLabelText("API token"), "fcm_secret")
    await user.click(screen.getByRole("button", { name: "Log in" }))
    expect(await screen.findByRole("heading", { name: "Goals" })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/goals")
    expect(router.state.location.search).toEqual({ status: "draft" })
  })

  test("login sends the token as JSON and never stores it", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized))
    let body: unknown
    let type: string | null = null
    server.use(
      http.post("/api/v1/session", async ({ request }) => {
        body = await request.json()
        type = request.headers.get("content-type")
        return (await import("msw")).HttpResponse.json((await import("./server")).fx.session)
      }),
    )
    const { user } = await renderApp("/login")
    await user.type(await screen.findByLabelText("API token"), "  fcm_secret ")
    await user.click(screen.getByRole("button", { name: "Log in" }))
    await screen.findByRole("heading", { name: "Home" })
    expect(body).toEqual({ token: "fcm_secret" })
    expect(type).toBe("application/json")
    expect(JSON.stringify({ ...localStorage })).not.toContain("fcm_secret")
    expect(JSON.stringify({ ...sessionStorage })).not.toContain("fcm_secret")
  })

  test("the sign-in page links to the connections and passes the invitation on", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized))
    let body: unknown
    server.use(
      http.post("/api/v1/auth/discover", async ({ request }) => {
        body = await request.json()
        return (await import("msw")).HttpResponse.json({ ...(await import("./server")).fx.authDiscover, token_login: false })
      }),
    )
    await renderApp("/login?invite=fci_x")
    const link = await screen.findByRole("link", { name: "Sign in with github" })
    expect(link).toHaveAttribute("href", expect.stringContaining("/auth/"))
    expect(body).toEqual({ invite: "fci_x" })
    expect(screen.queryByLabelText("API token")).not.toBeInTheDocument()
  })

  test("the sign-in page says when the server has no way to sign in", async () => {
    server.use(
      http.get("/api/v1/me", errors.unauthorized),
      http.post("/api/v1/auth/discover", async () => (await import("msw")).HttpResponse.json({ connections: [], token_login: false })),
    )
    await renderApp("/login")
    expect(await screen.findByRole("alert")).toHaveTextContent("No way to sign in is set up")
  })

  test("the sign-in page offers another try when its options do not load", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized), http.post("/api/v1/auth/discover", errors.invalidInput))
    const { user } = await renderApp("/login")
    expect(await screen.findByRole("alert")).toHaveTextContent("could not be loaded")
    server.resetHandlers()
    server.use(http.get("/api/v1/me", errors.unauthorized))
    await user.click(screen.getByRole("button", { name: "Try again" }))
    expect(await screen.findByRole("link", { name: "Sign in with github" })).toBeInTheDocument()
  })

  test("an empty token is refused without a request", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized))
    const { user } = await renderApp("/login")
    await user.click(await screen.findByRole("button", { name: "Log in" }))
    expect(screen.getByText("Paste your API token.")).toBeInTheDocument()
  })

  test("a refused token shows an error and stays on the login page", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized), http.post("/api/v1/session", errors.unauthorized))
    const { user, router } = await renderApp("/login")
    await user.type(await screen.findByLabelText("API token"), "fcm_wrong")
    await user.click(screen.getByRole("button", { name: "Log in" }))
    expect(await screen.findByText(/That token was not accepted/)).toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/login")
  })

  test("a failing login shows the server message", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized), http.post("/api/v1/session", errors.invalidInput))
    const { user } = await renderApp("/login")
    await user.type(await screen.findByLabelText("API token"), "x")
    await user.click(screen.getByRole("button", { name: "Log in" }))
    expect(await screen.findByText(/feedback is required/)).toBeInTheDocument()
  })

  test("a redirect to another site is ignored", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized))
    const { user, router } = await renderApp("/login?redirect=//evil.example")
    server.resetHandlers()
    await user.type(await screen.findByLabelText("API token"), "fcm_secret")
    await user.click(screen.getByRole("button", { name: "Log in" }))
    await screen.findByRole("heading", { name: "Home" })
    expect(router.state.location.pathname).toBe("/")
  })

  test("any 401 later goes back to the login page", async () => {
    const { router, queryClient } = await renderApp("/workers")
    await screen.findByRole("heading", { name: "Workers" })
    server.use(http.get("/api/v1/workers", errors.unauthorized))
    await queryClient.invalidateQueries({ queryKey: ["workers"] })
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
    expect(router.state.location.search).toEqual({ redirect: "/workers" })
  })

  test("sign out clears the session and shows the login page", async () => {
    let called = false
    server.use(
      http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)),
      http.delete("/api/v1/session", () => {
        called = true
        return new Response(null, { status: 204 })
      }),
    )
    const { user, router } = await renderApp("/")
    await screen.findByRole("heading", { name: "Home" })
    await user.click(await screen.findByRole("button", { name: "Account menu" }))
    await user.click(await screen.findByRole("menuitem", { name: "Sign out" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
    expect(called).toBe(true)
  })

  test("a server error while checking the session is shown, not treated as a logout", async () => {
    server.use(http.get("/api/v1/me", errors.conflict))
    const { router } = await renderApp("/")
    expect(await screen.findByText("Something went wrong")).toBeInTheDocument()
    expect(screen.getByText(/already approved/)).toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/")
  })

  test("the error page can try again", async () => {
    server.use(http.get("/api/v1/me", errors.conflict))
    const { user } = await renderApp("/")
    await screen.findByText("Something went wrong")
    server.resetHandlers()
    await user.click(screen.getByRole("button", { name: "Try again" }))
    expect(await screen.findByRole("heading", { name: "Home" })).toBeInTheDocument()
  })
})

test("an unknown address shows a not found page", async () => {
  await renderApp("/nowhere")
  expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument()
})
