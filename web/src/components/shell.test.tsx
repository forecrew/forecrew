import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

describe("navigation", () => {
  test("has a sidebar with every section and marks the current page", async () => {
    await renderApp("/goals")
    const nav = await screen.findByRole("navigation", { name: "Main" })
    for (const name of ["Home", "Inbox", "Goals", "Projects", "Jobs", "Workers"]) {
      expect(within(nav).getByRole("link", { name: new RegExp(`^${name}`) })).toBeInTheDocument()
    }
    expect(within(nav).getByRole("link", { name: "Goals" })).toHaveAttribute("aria-current", "page")
    expect(within(nav).getByRole("link", { name: /^Home/ })).not.toHaveAttribute("aria-current")
  })

  test("keeps Tokens at the bottom of the sidebar, with the app name and who is signed in", async () => {
    await renderApp("/")
    const aside = (await screen.findByText("Forecrew", { selector: "aside p" })).closest("aside") as HTMLElement
    expect(within(aside).getByRole("link", { name: "Tokens" })).toHaveAttribute("href", "/tokens")
    expect(within(aside).getByRole("button", { name: /Log out/ })).toBeInTheDocument()
    expect(await within(aside).findByText(/Signed in as/)).toBeInTheDocument()
  })

  test("the project switcher stays on the page and sets the project filter in the address", async () => {
    const { user, router } = await renderApp("/workers")
    const aside = (await screen.findByText("Forecrew", { selector: "aside p" })).closest("aside") as HTMLElement
    const switcher = await within(aside).findByLabelText("Switch project")
    await user.selectOptions(switcher, "app")
    await waitFor(() => expect(router.state.location.search).toEqual({ project: "app" }))
    expect(router.state.location.pathname).toBe("/workers")
    expect(within(aside).getByLabelText("Switch project")).toHaveValue("app")
    // the lists that filter by project keep it when opened from the sidebar
    expect(within(aside).getByRole("link", { name: "Jobs" })).toHaveAttribute("href", "/jobs?project=app")
    expect(within(aside).getByRole("link", { name: "Projects" })).toHaveAttribute("href", "/projects")
    await user.click(within(aside).getByRole("link", { name: "Goals" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/goals"))
    expect(router.state.location.search).toEqual({ project: "app" })
    await user.selectOptions(within(aside).getByLabelText("Switch project"), "")
    await waitFor(() => expect(router.state.location.search).toEqual({}))
    expect(router.state.location.pathname).toBe("/goals")
  })

  test("has a bottom bar with Home, Inbox, Jobs and the menu for narrow screens", async () => {
    await renderApp("/")
    const bar = await screen.findByRole("navigation", { name: "Quick" })
    for (const [name, href] of [["Home", "/"], ["Inbox", "/inbox"], ["Jobs", "/jobs"]] as const) {
      expect(within(bar).getByRole("link", { name: new RegExp(`^${name}`) })).toHaveAttribute("href", href)
    }
    expect(within(bar).getByRole("button", { name: "Menu" })).toBeInTheDocument()
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
    await screen.findAllByText("Nothing needs you")
    expect(within(nav).getByRole("link", { name: /^Inbox/ })).not.toHaveTextContent("pending")
  })

  test("the menu button opens the navigation on a narrow screen and closes it on navigation", async () => {
    const { user, router } = await renderApp("/")
    await user.click(await screen.findByRole("button", { name: "Menu" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("link", { name: /Workers/ }))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    expect(router.state.location.pathname).toBe("/workers")
  })
})
