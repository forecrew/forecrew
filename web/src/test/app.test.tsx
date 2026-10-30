import { screen, waitFor, within } from "@testing-library/react"
import { http } from "msw"
import { errors, server } from "./server"
import { renderApp } from "./render"

describe("login and session", () => {
  test("a visit without a session goes to the login page and back after login", async () => {
    server.use(http.get("/api/v1/me", errors.unauthorized))
    const { user, router } = await renderApp("/inbox")
    expect(await screen.findByRole("heading", { name: "Log in to Forecrew" })).toBeInTheDocument()
    expect(router.state.location.search).toEqual({ redirect: "/inbox" })

    server.resetHandlers() // the session is valid from now on
    await user.type(screen.getByLabelText("API token"), "fcm_secret")
    await user.click(screen.getByRole("button", { name: "Log in" }))
    expect(await screen.findByRole("heading", { name: "Inbox" })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/inbox")
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
    await screen.findByRole("heading", { name: "Inbox" })
    expect(body).toEqual({ token: "fcm_secret" })
    expect(type).toBe("application/json")
    expect(JSON.stringify({ ...localStorage })).not.toContain("fcm_secret")
    expect(JSON.stringify({ ...sessionStorage })).not.toContain("fcm_secret")
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
    await screen.findByRole("heading", { name: "Inbox" })
    expect(router.state.location.pathname).toBe("/inbox")
  })

  test("any 401 later goes back to the login page", async () => {
    const { router, queryClient } = await renderApp("/inbox")
    await screen.findByRole("heading", { name: "Inbox" })
    server.use(http.get("/api/v1/gates", errors.unauthorized))
    await queryClient.invalidateQueries({ queryKey: ["gates"] })
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
    expect(router.state.location.search).toEqual({ redirect: "/inbox" })
  })

  test("log out clears the session and shows the login page", async () => {
    let called = false
    server.use(
      http.delete("/api/v1/session", () => {
        called = true
        return new Response(null, { status: 204 })
      }),
    )
    const { user, router } = await renderApp("/")
    await screen.findByRole("heading", { name: "Inbox" })
    const aside = screen.getByText(/Signed in as/).closest("div")!
    expect(within(aside).getByText("laptop")).toBeInTheDocument()
    await user.click(within(aside).getByRole("button", { name: /Log out/ }))
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
    expect(await screen.findByRole("heading", { name: "Inbox" })).toBeInTheDocument()
  })
})

test("an unknown address shows a not found page", async () => {
  await renderApp("/nowhere")
  expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument()
})
