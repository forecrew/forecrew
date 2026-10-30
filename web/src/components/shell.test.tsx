import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

describe("navigation", () => {
  test("has a sidebar with every section and marks the current page", async () => {
    await renderApp("/inbox")
    const nav = await screen.findByRole("navigation", { name: "Main" })
    for (const name of ["Inbox", "Jobs", "Workers"]) {
      expect(within(nav).getByRole("link", { name: new RegExp(`^${name}`) })).toBeInTheDocument()
    }
    expect(within(nav).getByRole("link", { name: /^Inbox/ })).toHaveAttribute("aria-current", "page")
  })

  test("has a skip link and a main landmark", async () => {
    await renderApp("/")
    expect(await screen.findByRole("link", { name: "Skip to content" })).toHaveAttribute("href", "#main")
    expect(screen.getByRole("main")).toHaveAttribute("id", "main")
  })

  test("does not show an inbox count when nothing is pending", async () => {
    server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)))
    await renderApp("/")
    const nav = await screen.findByRole("navigation", { name: "Main" })
    await screen.findByText(/Nothing needs you/)
    expect(within(nav).getByRole("link", { name: /^Inbox/ })).not.toHaveTextContent("pending")
  })

  test("the menu button opens the navigation on a narrow screen and closes it on navigation", async () => {
    const { user, router } = await renderApp("/")
    await user.click(await screen.findByRole("button", { name: "Open menu" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("link", { name: /Workers/ }))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    expect(router.state.location.pathname).toBe("/workers")
  })
})
