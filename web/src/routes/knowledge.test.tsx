import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

/** Records the query of every listing and search. */
function knowledgeApi() {
  const lists: URLSearchParams[] = []
  const searches: URLSearchParams[] = []
  server.use(
    http.get("/api/v1/knowledge/entries", ({ request }) => {
      const q = new URL(request.url).searchParams
      lists.push(q)
      return HttpResponse.json(q.get("kind") === "note" ? fx.knowledgeNotes : fx.knowledgeEntries)
    }),
    http.get("/api/v1/knowledge", ({ request }) => {
      searches.push(new URL(request.url).searchParams)
      return HttpResponse.json(fx.knowledgeSearch)
    }),
  )
  return { lists, searches }
}

describe("knowledge page: browsing", () => {
  test("is in the navigation and the command menu", async () => {
    const { user, router } = await renderApp("/")
    await user.click(await screen.findByRole("link", { name: "Knowledge" }))
    await screen.findByRole("heading", { level: 1, name: "Knowledge" })
    expect(router.state.location.pathname).toBe("/knowledge")
    await user.keyboard("{Control>}k{/Control}")
    expect(await screen.findByRole("option", { name: "Knowledge" })).toBeInTheDocument()
  })

  test("asks for a project when there are several and none is chosen", async () => {
    await renderApp("/knowledge")
    expect(await screen.findByText(/Choose a project in the sidebar/)).toBeInTheDocument()
    expect(screen.queryByRole("group", { name: "Knowledge views" })).not.toBeInTheDocument()
  })

  test("uses the only project without a choice", async () => {
    server.use(http.get("/api/v1/projects", () => HttpResponse.json([fx.projects[0]])))
    const { lists } = knowledgeApi()
    await renderApp("/knowledge")
    await screen.findByRole("list", { name: "Entries" })
    expect(lists[0]!.get("project")).toBe("app")
  })

  test("lists the sections with their provider, location and owner", async () => {
    knowledgeApi()
    await renderApp("/knowledge?project=app")
    const list = await screen.findByRole("region", { name: "Sections" })
    const arch = await within(list).findByRole("button", { name: /architecture/ })
    expect(arch).toHaveTextContent("repo")
    expect(arch).toHaveTextContent("docs/adr/**")
    expect(arch).toHaveTextContent("owner architect")
    const conv = within(list).getByRole("button", { name: /conventions/ })
    expect(conv).toHaveTextContent("wiki (confluence, acme.atlassian.net)")
    expect(conv).toHaveTextContent("ENG/Conventions")
    expect(within(list).getByRole("button", { name: /vision/ })).toHaveTextContent("protected")
    expect(within(list).getByRole("button", { name: /generated/ })).toHaveTextContent("built by forecrewd")
  })

  test("narrows the entries to a section", async () => {
    const { lists } = knowledgeApi()
    const { user, router } = await renderApp("/knowledge?project=app")
    const sections = await screen.findByRole("region", { name: "Sections" })
    await user.click(within(sections).getByRole("button", { name: /vision/ }))
    await waitFor(() => expect(lists.at(-1)!.get("section")).toBe("vision"))
    expect(router.state.location.search).toMatchObject({ section: "vision" })
    expect(within(sections).getByRole("button", { name: /vision/ })).toHaveAttribute("aria-pressed", "true")
    await user.click(within(sections).getByRole("button", { name: /vision/ }))
    await waitFor(() => expect(lists.at(-1)!.get("section")).toBeNull())
  })

  test("shows an entry with its provenance, freshness and source", async () => {
    knowledgeApi()
    const { user } = await renderApp("/knowledge?project=app")
    const list = await screen.findByRole("list", { name: "Entries" })
    // The first entry is open; a stale one shows its badge.
    const first = screen.getByRole("region", { name: "Jobs wait in Postgres" })
    expect(within(first).getByText("Open the source").closest("a")).toHaveAttribute("href", "https://github.com/acme/app/blob/main/docs/adr/0001-queue.md")
    expect(within(first).getByText("Written by").nextElementSibling).toHaveTextContent("architect (JOB-12)")
    expect(within(first).getByText("untrusted text")).toBeInTheDocument()
    expect(within(first).getByText(/Jobs wait in a Postgres table, leased by workers\./)).toBeInTheDocument()
    expect(within(first).queryByText("needs re-verify")).not.toBeInTheDocument()

    await user.click(within(list).getByRole("button", { name: /Why Forecrew/ }))
    const stale = screen.getByRole("region", { name: "Why Forecrew" })
    expect(within(stale).getByText("needs re-verify")).toBeInTheDocument()
    expect(within(stale).getByText(/abc123 changed internal\/queue/)).toBeInTheDocument()
    const verified = within(stale).getByText("Last verified").nextElementSibling!
    expect(verified).toHaveTextContent("1 h ago") // 100 minutes ago
    expect(within(stale).queryByText("untrusted text")).not.toBeInTheDocument()
    // the list marks it too
    expect(within(within(list).getByRole("button", { name: /Why Forecrew/ })).getByText("needs re-verify")).toBeInTheDocument()
  })

  test("shows entry text as text", async () => {
    knowledgeApi()
    const { user, container } = await renderApp("/knowledge?project=app&kind=entry")
    const list = await screen.findByRole("list", { name: "Entries" })
    await user.click(within(list).getByRole("button", { name: /flaky/ }))
    const entry = screen.getByRole("region", { name: "The spool is <i>flaky</i>" })
    expect(within(entry).getByText(/<script>alert\(1\)<\/script>/)).toBeInTheDocument()
    expect(container.querySelector("i, script")).toBeNull()
  })

  test("searches with a kind and a provider filter and shows the excerpt", async () => {
    const { searches } = knowledgeApi()
    const { user, router } = await renderApp("/knowledge?project=app")
    await screen.findByRole("list", { name: "Entries" })
    await user.type(screen.getByLabelText("Search knowledge"), "postgres")
    await user.selectOptions(screen.getByLabelText("Kind"), "entry")
    await user.selectOptions(screen.getByLabelText("Provider"), "repo")
    await user.click(screen.getByRole("button", { name: "Search" }))
    const results = await screen.findByRole("list", { name: "Search results" })
    await waitFor(() => expect(searches.at(-1)!.get("q")).toBe("postgres"))
    expect(searches.at(-1)!.get("kind")).toBe("entry")
    expect(searches.at(-1)!.get("provider")).toBe("repo")
    expect(router.state.location.search).toMatchObject({ q: "postgres", kind: "entry", provider: "repo" })
    expect(within(results).getAllByRole("button")).toHaveLength(fx.knowledgeSearch.length)
    const view = screen.getByRole("region", { name: fx.knowledgeSearch[0]!.title })
    expect(within(view).getByText("Excerpt")).toBeInTheDocument()
    // The match is highlighted; other markup in the excerpt stays text.
    expect(within(view).getByText("Postgres").tagName).toBe("MARK")
    expect(view).toHaveTextContent("Jobs wait in a Postgres table, <i>not</i> a broker.")
    expect(view).not.toHaveTextContent("<b>")
    expect(view.querySelector("i")).toBeNull()
    await user.click(screen.getByRole("button", { name: "Clear search" }))
    await screen.findByRole("list", { name: "Entries" })
    expect(screen.getByLabelText("Search knowledge")).toHaveValue("")
  })

  test("says when a search finds nothing or an area is empty", async () => {
    server.use(http.get("/api/v1/knowledge", () => HttpResponse.json(fx.knowledgeSearchEmpty)))
    const first = await renderApp("/knowledge?project=app&q=zebra")
    expect(await screen.findByText("Nothing found for “zebra”.")).toBeInTheDocument()
    first.unmount()
    server.use(http.get("/api/v1/knowledge/entries", () => HttpResponse.json(fx.knowledgeEntriesEmpty)))
    const second = await renderApp("/knowledge?project=app&section=security")
    expect(await screen.findByText("No entries here yet.")).toBeInTheDocument()
    second.unmount()
    server.use(http.get("/api/v1/knowledge/entries", errors.invalidInput))
    await renderApp("/knowledge?project=app")
    expect((await screen.findAllByText("Could not load this")).length).toBeGreaterThan(0)
  })

  test("switches between the views and ignores an unknown one", async () => {
    knowledgeApi()
    const { user, router } = await renderApp("/knowledge?project=app&tab=nonsense")
    const views = await screen.findByRole("group", { name: "Knowledge views" })
    expect(within(views).getByRole("button", { name: "Entries" })).toHaveAttribute("aria-pressed", "true")
    await user.click(within(views).getByRole("button", { name: "Agent notes" }))
    await waitFor(() => expect(router.state.location.search).toMatchObject({ tab: "notes" }))
    expect(within(views).getByRole("button", { name: "Agent notes" })).toHaveAttribute("aria-pressed", "true")
    await user.click(within(views).getByRole("button", { name: "Entries" }))
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty("tab"))
  })
})

describe("knowledge page: lists", () => {
  test("asks for the sections alone, with one entry, and does not repeat it", async () => {
    const lists: URLSearchParams[] = []
    server.use(
      http.get("/api/v1/knowledge/entries", ({ request }) => {
        lists.push(new URL(request.url).searchParams)
        return HttpResponse.json(fx.knowledgeEntries)
      }),
    )
    await renderApp("/knowledge?project=app")
    await screen.findByRole("list", { name: "Entries" })
    expect(lists.filter((l) => l.get("limit") === "1")).toHaveLength(1)
    expect(lists.filter((l) => l.get("limit") === null).length).toBeGreaterThan(0)
  })

  test("says when a list has the most the API gives", async () => {
    const many = Array.from({ length: 50 }, (_, i) => ({ ...fx.knowledgeEntries.entries[1]!, id: `e${i}`, title: `Entry ${i}` }))
    server.use(http.get("/api/v1/knowledge/entries", () => HttpResponse.json({ ...fx.knowledgeEntries, entries: many })))
    await renderApp("/knowledge?project=app")
    expect(await screen.findByText("Showing the first 50. Narrow the list to see others.")).toBeInTheDocument()
  })

  test("a short list says nothing about a cap", async () => {
    await renderApp("/knowledge?project=app")
    await screen.findByRole("list", { name: "Entries" })
    expect(screen.queryByText(/Showing the first/)).not.toBeInTheDocument()
  })

  test("a search asks for a page and says when it is full", async () => {
    const searches: URLSearchParams[] = []
    const many = Array.from({ length: 50 }, (_, i) => ({ ...fx.knowledgeSearch[0]!, id: `s${i}`, title: `Hit ${i}` }))
    server.use(
      http.get("/api/v1/knowledge", ({ request }) => {
        searches.push(new URL(request.url).searchParams)
        return HttpResponse.json(many)
      }),
    )
    await renderApp("/knowledge?project=app&q=postgres")
    expect(await screen.findByText(/Showing the first 50/)).toBeInTheDocument()
    expect(searches[0]!.get("limit")).toBe("50")
  })
})
