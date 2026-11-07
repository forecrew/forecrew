import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

describe("navigation", () => {
  test("has a sidebar with every section and marks the current page", async () => {
    await renderApp("/goals")
    const nav = await screen.findByRole("navigation", { name: "Main" })
    for (const name of ["Home", "Inbox", "Goals", "Projects", "Jobs", "Quality", "Workers"]) {
      expect(within(nav).getByRole("link", { name: new RegExp(`^${name}`) })).toBeInTheDocument()
    }
    expect(within(nav).getByRole("link", { name: "Goals" })).toHaveAttribute("aria-current", "page")
    expect(within(nav).getByRole("link", { name: /^Home/ })).not.toHaveAttribute("aria-current")
  })

  test("keeps Settings and the account menu at the bottom of the sidebar, with the app name", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)))
    await renderApp("/")
    const aside = (await screen.findByText("Forecrew", { selector: "aside p" })).closest("aside") as HTMLElement
    expect(await within(aside).findByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings")
    expect(within(aside).getByRole("button", { name: "Account menu" })).toHaveTextContent("ana")
    expect(within(aside).queryByRole("link", { name: "Tokens" })).not.toBeInTheDocument()
  })

  test("a token session shows the token name in the account menu", async () => {
    await renderApp("/")
    const aside = (await screen.findByText("Forecrew", { selector: "aside p" })).closest("aside") as HTMLElement
    expect(await within(aside).findByRole("button", { name: "Account menu" })).toHaveTextContent("laptop")
  })

  test("the account menu leads to Your account and signs out", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)))
    const { user, router } = await renderApp("/")
    await user.click(await screen.findByRole("button", { name: "Account menu" }))
    expect(await screen.findByRole("menuitem", { name: "Sign out everywhere" })).toBeInTheDocument()
    await user.click(screen.getByRole("menuitem", { name: "Your account" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/account"))
  })

  test("sign out everywhere ends every session and shows the login page", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)))
    let everywhere = false
    server.use(
      http.delete("/api/v1/sessions", () => {
        everywhere = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const { user, router } = await renderApp("/")
    await user.click(await screen.findByRole("button", { name: "Account menu" }))
    await user.click(await screen.findByRole("menuitem", { name: "Sign out everywhere" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"))
    expect(everywhere).toBe(true)
  })

  test("a Contributor has no Settings link", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meContributor)))
    await renderApp("/")
    const aside = (await screen.findByText("Forecrew", { selector: "aside p" })).closest("aside") as HTMLElement
    await within(aside).findByRole("button", { name: "Account menu" })
    expect(within(aside).queryByRole("link", { name: "Settings" })).not.toBeInTheDocument()
  })

  test("with one organization the sidebar names it and has no switcher", async () => {
    server.use(http.get("/api/v1/me", () => HttpResponse.json(fx.meAdmin)))
    await renderApp("/")
    const aside = (await screen.findByText("Forecrew", { selector: "aside p" })).closest("aside") as HTMLElement
    expect(await within(aside).findByText("acme")).toBeInTheDocument()
    expect(within(aside).queryByLabelText("Switch organization")).not.toBeInTheDocument()
  })

  test("with several organizations the switcher moves the session and starts over in the other", async () => {
    const other = { id: "018f0000-0000-7000-8000-000000000002", name: "Other", slug: "other" }
    const me = { ...fx.meAdmin, organizations: [...fx.meAdmin.organizations, other] }
    let current = me
    const picked: unknown[] = []
    server.use(
      http.get("/api/v1/me", () => HttpResponse.json(current)),
      http.put("/api/v1/session/organization", async ({ request }) => {
        picked.push(await request.json())
        current = { ...me, account: { ...me.account, ...other, created_at: me.account.created_at } }
        return HttpResponse.json(current)
      }),
    )
    const { user, router } = await renderApp("/goals")
    const aside = (await screen.findByText("Forecrew", { selector: "aside p" })).closest("aside") as HTMLElement
    const select = await within(aside).findByLabelText("Switch organization")
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["acme", "Other"])
    await user.selectOptions(select, "other")
    await waitFor(() => expect(router.state.location.pathname).toBe("/"))
    expect(picked).toEqual([{ organization: "other" }])
    await waitFor(() => expect(within(aside).getByLabelText("Switch organization")).toHaveValue("other"))
  })

  test("a refused switch is shown and the session stays where it is", async () => {
    const other = { id: "018f0000-0000-7000-8000-000000000002", name: "Other", slug: "other" }
    server.use(
      http.get("/api/v1/me", () => HttpResponse.json({ ...fx.meAdmin, organizations: [...fx.meAdmin.organizations, other] })),
      http.put("/api/v1/session/organization", errors.invalidInput),
    )
    const { user, router } = await renderApp("/goals")
    await user.selectOptions(await screen.findByLabelText("Switch organization"), "other")
    expect(await screen.findByRole("alert")).toHaveTextContent("feedback is required")
    expect(router.state.location.pathname).toBe("/goals")
  })

  test("a session from an organization's own connection has one organization and no switcher", async () => {
    // /me lists only the organization of that connection.
    server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.meAdmin, organizations: fx.meAdmin.organizations.slice(0, 1) })))
    await renderApp("/")
    await screen.findByRole("button", { name: "Account menu" })
    expect(screen.queryByLabelText("Switch organization")).not.toBeInTheDocument()
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
