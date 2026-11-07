import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { leave } from "@/lib/redirect"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const signedOut = () => server.use(http.get("/api/v1/me", errors.unauthorized))
const github = fx.authDiscover.connections[0]!

describe("the sign-in page", () => {
  test("shows one button per way to sign in and keeps the page to return to", async () => {
    signedOut()
    server.use(http.post("/api/v1/auth/discover", () => HttpResponse.json({ ...fx.authDiscover, token_login: false })))
    await renderApp("/login?redirect=%2Finbox%3Fproject%3Dapp")
    const link = await screen.findByRole("link", { name: "Sign in with github" })
    expect(link).toHaveAttribute("href", `${github.login_url}?redirect=%2Finbox%3Fproject%3Dapp`)
  })

  test("an address on another site is never carried", async () => {
    signedOut()
    server.use(http.post("/api/v1/auth/discover", () => HttpResponse.json({ ...fx.authDiscover, token_login: false })))
    await renderApp("/login?redirect=%2F%2Fevil.example")
    expect(await screen.findByRole("link", { name: "Sign in with github" })).toHaveAttribute("href", `${github.login_url}?redirect=%2F`)
  })

  test("asks for the email first and lists the connections of the company that owns the domain", async () => {
    signedOut()
    const bodies: unknown[] = []
    server.use(
      http.post("/api/v1/auth/discover", async ({ request }) => {
        const body = (await request.json()) as { email?: string }
        bodies.push(body)
        return HttpResponse.json({
          connections: body.email ? [...fx.authDiscover.connections, { id: "o1", kind: "oidc", name: "okta", login_url: "/auth/o1/login?org=acme" }] : fx.authDiscover.connections,
          token_login: false,
        })
      }),
    )
    const { user } = await renderApp("/login?org=acme")
    await screen.findByRole("link", { name: "Sign in with github" })
    expect(screen.queryByRole("link", { name: "Sign in with okta" })).not.toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Continue" }))
    expect(screen.getByText("Enter your email address.")).toBeInTheDocument()
    await user.type(screen.getByLabelText("Email"), "ana@acme.test")
    await user.click(screen.getByRole("button", { name: "Continue" }))
    expect(await screen.findByRole("link", { name: "Sign in with okta" })).toHaveAttribute("href", "/auth/o1/login?org=acme")
    expect(bodies).toEqual([{ org: "acme" }, { org: "acme", email: "ana@acme.test" }])
  })

  test("a domain that requires its own sign-in leads straight to it", async () => {
    signedOut()
    server.use(
      http.post("/api/v1/auth/discover", async ({ request }) => {
        const body = (await request.json()) as { email?: string }
        return HttpResponse.json({ connections: fx.authDiscover.connections, token_login: false, ...(body.email ? { redirect: "/auth/o1/login" } : {}) })
      }),
    )
    const { user } = await renderApp("/login")
    await user.type(await screen.findByLabelText("Email"), "ana@acme.test")
    await user.click(screen.getByRole("button", { name: "Continue" }))
    const status = await screen.findByRole("status")
    expect(status).toHaveTextContent("Your company requires its own sign-in for ana@acme.test")
    expect(within(status).getByRole("link", { name: "Continue with your company sign-in" })).toHaveAttribute("href", "/auth/o1/login")
  })

  test("an invitation says so", async () => {
    signedOut()
    await renderApp("/login?invite=fci_x")
    expect(await screen.findByText(/You were invited/)).toBeInTheDocument()
  })
})

describe("allowing the command line", () => {
  const address = "/cli-login?redirect_uri=http%3A%2F%2F127.0.0.1%3A5555%2Fcallback&state=st&code_challenge=abc&host=laptop"

  test("asks you to confirm, then sends the browser back to the CLI and shows the code", async () => {
    const go = vi.spyOn(leave, "to").mockImplementation(() => {})
    let body: unknown
    server.use(
      http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)),
      http.post("/api/v1/cli/authorize", async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ redirect: "http://127.0.0.1:5555/callback?code=fcl_abc&state=st", code: "fcl_abc" })
      }),
    )
    const { user } = await renderApp(address)
    expect(await screen.findByText(/asks to act as ana in/)).toHaveTextContent("laptop")
    await user.click(screen.getByRole("button", { name: "Allow" }))
    await waitFor(() => expect(go).toHaveBeenCalledWith("http://127.0.0.1:5555/callback?code=fcl_abc&state=st"))
    expect(body).toEqual({ redirect_uri: "http://127.0.0.1:5555/callback", state: "st", code_challenge: "abc" })
    expect(await screen.findByLabelText("Code")).toHaveValue("fcl_abc")
    go.mockRestore()
  })

  test("names the organization it was asked for", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)))
    await renderApp(`${address}&organization=beta`)
    expect(await screen.findByText(/asks to act as ana in/)).toHaveTextContent("beta")
  })

  test("a refusal is shown and Cancel goes home", async () => {
    server.use(http.post("/api/v1/cli/authorize", errors.invalidInput))
    const { user, router } = await renderApp(address)
    await user.click(await screen.findByRole("button", { name: "Allow" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("feedback is required")
    await user.click(screen.getByRole("button", { name: "Cancel" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/"))
  })

  test("an address that is not a loopback one is not a login", async () => {
    await renderApp("/cli-login?redirect_uri=http%3A%2F%2Fevil.example%3A5555%2Fcallback&code_challenge=abc")
    expect(await screen.findByRole("alert")).toHaveTextContent("not a login that forecrew login started")
    expect(screen.queryByRole("button", { name: "Allow" })).not.toBeInTheDocument()
  })

  test("a page that is not an address at all is refused too", async () => {
    await renderApp("/cli-login")
    expect(await screen.findByRole("alert")).toBeInTheDocument()
  })
})
