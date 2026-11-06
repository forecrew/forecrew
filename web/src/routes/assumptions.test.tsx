import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

/** Records the query of every list and the bodies of the resolutions. */
function registerApi() {
  const lists: URLSearchParams[] = []
  const resolves: { ref: string; body: unknown }[] = []
  server.use(
    http.get("/api/v1/assumptions", ({ request }) => {
      lists.push(new URL(request.url).searchParams)
      return HttpResponse.json(fx.assumptionRegister)
    }),
    http.post("/api/v1/assumptions/:ref/resolve", async ({ params, request }) => {
      resolves.push({ ref: String(params.ref), body: await request.json() })
      return HttpResponse.json(fx.assumptionRegisterResolve)
    }),
  )
  return { lists, resolves }
}

describe("assumptions page", () => {
  test("is in the navigation and the command menu", async () => {
    const { user, router } = await renderApp("/")
    await user.click(await screen.findByRole("link", { name: "Assumptions" }))
    await screen.findByRole("heading", { level: 1, name: "Assumptions" })
    expect(router.state.location.pathname).toBe("/assumptions")
    await user.keyboard("{Control>}k{/Control}")
    expect(await screen.findByRole("option", { name: "Assumptions" })).toBeInTheDocument()
  })

  test("lists the register of every project with what rests on each item", async () => {
    const { lists } = registerApi()
    await renderApp("/assumptions")
    const list = await screen.findByRole("list", { name: "Assumptions" })
    // one list call per project: the register answers a project at a time
    await waitFor(() => expect(lists.map((l) => l.get("project")).sort()).toEqual(["app", "docs"]))
    expect(within(list).getAllByRole("article")).toHaveLength(6) // 3 per project
    const first = within(list).getAllByRole("article")[0]!
    expect(within(first).getByText("A-4")).toBeInTheDocument()
    expect(within(first).getByText("unverified")).toBeInTheDocument()
    expect(within(first).getByText("high impact")).toBeInTheDocument()
    expect(within(first).getByText("GOAL-3, TASK-12")).toBeInTheDocument()
    expect(within(first).getByText(/architect/)).toBeInTheDocument()
    expect(within(first).getByText("(JOB-41)")).toBeInTheDocument()
    expect(within(first).getByText("app")).toBeInTheDocument()
    expect(within(first).getByRole("link", { name: "Why A-4?" })).toHaveAttribute("href", "/trace/A-4")
  })

  test("shows the evidence of a resolved item and no moves on a final one", async () => {
    registerApi()
    await renderApp("/assumptions?project=app")
    const accepted = (await screen.findAllByRole("article", { name: /Admins/ }))[0]!
    expect(within(accepted).getByText(/Ana accepts the risk\./)).toBeInTheDocument()
    expect(within(accepted).queryByRole("button", { name: "Accept A-5" })).not.toBeInTheDocument()
    expect(within(accepted).getByRole("button", { name: "Verify A-5" })).toBeInTheDocument()
    const invalidated = screen.getAllByRole("article", { name: /mailer retries/ })[0]!
    expect(within(invalidated).getByText(/It does not retry/)).toBeInTheDocument()
    expect(within(invalidated).queryByRole("group")).not.toBeInTheDocument()
    expect(within(invalidated).getByText("nothing yet")).toBeInTheDocument()
  })

  test("filters by project, status and impact", async () => {
    const { lists } = registerApi()
    const { user, router } = await renderApp("/assumptions?project=app")
    await screen.findByRole("list", { name: "Assumptions" })
    expect(lists.at(-1)!.get("project")).toBe("app")
    expect(within(screen.getByRole("list", { name: "Assumptions" })).queryByText("app")).not.toBeInTheDocument() // the project is the filter
    await user.selectOptions(screen.getByLabelText("Status"), "unverified")
    await waitFor(() => expect(lists.at(-1)!.get("status")).toBe("unverified"))
    await user.selectOptions(screen.getByLabelText("Impact"), "high")
    await waitFor(() => expect(lists.at(-1)!.get("impact")).toBe("high"))
    expect(router.state.location.search).toMatchObject({ project: "app", status: "unverified", impact: "high" })
    await user.selectOptions(screen.getByLabelText("Status"), "")
    await waitFor(() => expect(lists.at(-1)!.get("status")).toBeNull())
  })

  test("resolves inline with evidence", async () => {
    const { resolves } = registerApi()
    const { user } = await renderApp("/assumptions?project=app")
    const a4 = (await screen.findAllByRole("article", { name: /existing mailer/ }))[0]!
    await user.click(within(a4).getByRole("button", { name: "Invalidate A-4" }))
    await user.click(within(a4).getByRole("button", { name: "Confirm invalidate" }))
    expect(within(a4).getByText("Evidence is required to verify or invalidate.")).toBeInTheDocument()
    await user.type(within(a4).getByLabelText("Evidence for A-4"), "The mailer is retired.")
    await user.click(within(a4).getByRole("button", { name: "Confirm invalidate" }))
    await waitFor(() => expect(resolves).toEqual([{ ref: "A-4", body: { status: "invalidated", evidence: "The mailer is retired." } }]))
  })

  test("shows agent text as text, and the empty and error states", async () => {
    registerApi()
    const { container, unmount } = await renderApp("/assumptions?project=app")
    expect((await screen.findAllByText("Admins <b>never</b> sign in with a password."))[0]).toBeInTheDocument()
    expect(container.querySelector("b")).toBeNull()
    unmount()
    server.use(http.get("/api/v1/assumptions", () => HttpResponse.json(fx.assumptionRegisterEmpty)))
    const second = await renderApp("/assumptions?project=app&status=verified")
    expect(await screen.findByText("No assumptions match.")).toBeInTheDocument()
    second.unmount()
    server.use(http.get("/api/v1/assumptions", errors.invalidInput))
    await renderApp("/assumptions?project=app")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })
})
