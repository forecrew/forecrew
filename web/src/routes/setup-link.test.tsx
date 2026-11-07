import { screen } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const signedOut = () => server.use(http.get("/api/v1/me", errors.unauthorized))
const github = fx.authDiscover.connections[0]!

test("the setup link checks its token and offers the sign-in, with no session", async () => {
  signedOut()
  const tokens: unknown[] = []
  server.use(
    http.post("/auth/setup", async ({ request }) => {
      tokens.push(await request.json())
      return HttpResponse.json({ organization: "Acme Labs" })
    }),
  )
  const { router } = await renderApp("/setup-link#fcu_secret")
  expect(await screen.findByText("Acme Labs")).toBeInTheDocument()
  expect(screen.getByRole("link", { name: "Sign in with github" })).toHaveAttribute("href", github.login_url)
  expect(tokens).toEqual([{ token: "fcu_secret" }])
  // The token does not stay in the address.
  expect(router.state.location.hash).toBe("")
})

test("a link that was used or expired says so with the server's sentence", async () => {
  signedOut()
  server.use(http.post("/auth/setup", () => HttpResponse.json({ error: "This setup link has expired or was already used. Create a new one." }, { status: 404 })))
  await renderApp("/setup-link#fcu_old")
  expect(await screen.findByRole("alert")).toHaveTextContent("This setup link has expired or was already used. Create a new one.")
  expect(screen.queryByRole("link", { name: "Sign in with github" })).toBeNull()
})

test("an address without a token explains what is missing, and an answer that is not JSON keeps the generic sentence", async () => {
  signedOut()
  const { unmount } = await renderApp("/setup-link")
  expect(await screen.findByRole("alert")).toHaveTextContent("needs the link the installer printed")
  unmount()
  server.use(http.post("/auth/setup", () => new HttpResponse("nope", { status: 502 })))
  await renderApp("/setup-link#fcu_x")
  expect(await screen.findByRole("alert")).toHaveTextContent("expired or was already used")
})

test("a server that cannot be reached, and a server with no sign-in set up, are said", async () => {
  signedOut()
  server.use(http.post("/auth/setup", () => HttpResponse.error()))
  const { unmount } = await renderApp("/setup-link#fcu_x")
  expect(await screen.findByRole("alert")).toHaveTextContent("Cannot reach the server")
  unmount()
  server.use(
    http.post("/auth/setup", () => HttpResponse.json({ organization: "Acme" })),
    http.post("/api/v1/auth/discover", () => HttpResponse.json({ connections: [], token_login: false })),
  )
  const second = await renderApp("/setup-link#fcu_x")
  expect(await screen.findByText(/No way to sign in is set up/)).toBeInTheDocument()
  second.unmount()
  server.use(http.post("/api/v1/auth/discover", () => HttpResponse.json({ error: { code: "internal", message: "x" } }, { status: 500 })))
  await renderApp("/setup-link#fcu_x")
  expect(await screen.findByText(/could not be loaded/)).toBeInTheDocument()
})
