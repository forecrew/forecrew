import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import type { Project } from "@/api/types"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

describe("knowledge page: proposals", () => {
  test("lists the proposals with where each is reviewed", async () => {
    await renderApp("/knowledge?project=app&tab=proposals")
    const list = await screen.findByRole("list", { name: "Knowledge proposals" })
    const [pr, gate, done] = within(list).getAllByRole("article")
    expect(within(pr!).getByText("KB-5")).toBeInTheDocument()
    expect(within(pr!).getByText("open")).toBeInTheDocument()
    expect(within(pr!).getByText("add")).toBeInTheDocument()
    expect(within(pr!).getByRole("link", { name: /Open the pull request/ })).toHaveAttribute("href", "https://github.com/acme/app/pull/12")
    expect(within(pr!).getByRole("link", { name: "GOAL-3" })).toHaveAttribute("href", "/goals/GOAL-3")
    expect(within(pr!).getByText(/Proposed by architect/)).toBeInTheDocument()
    // a protected section waits for a human at a gate
    expect(within(gate!).getByRole("link", { name: "Decide GATE-12" })).toHaveAttribute("href", "/inbox?gate=GATE-12")
    // an old one with neither shows no review
    expect(within(done!).getByText("accepted")).toBeInTheDocument()
    expect(within(done!).getByText("no review yet")).toBeInTheDocument()
  })

  test("shows the reason and the new text as text", async () => {
    const { user, container } = await renderApp("/knowledge?project=app&tab=proposals")
    const first = (await screen.findAllByRole("article"))[0]!
    expect(within(first).getByText(/<b>Please<\/b> review\./)).toBeInTheDocument()
    expect(container.querySelector("b")).toBeNull()
    await user.click(within(first).getByText("New text of docs/adr/0002-retries.md"))
    expect(within(first).getByText("Jobs retry three times.")).toBeInTheDocument()
  })

  test("a decided gate shows its outcome and filtering asks for a status", async () => {
    const calls: (string | null)[] = []
    const decided = structuredClone(fx.knowledgeProposals)
    decided[1]!.gate!.status = "approved"
    server.use(
      http.get("/api/v1/knowledge/proposals", ({ request }) => {
        calls.push(new URL(request.url).searchParams.get("status"))
        return HttpResponse.json(decided)
      }),
    )
    const { user } = await renderApp("/knowledge?project=app&tab=proposals")
    const list = await screen.findByRole("list", { name: "Knowledge proposals" })
    expect(within(list).getByText("GATE-12: approved")).toBeInTheDocument()
    await user.selectOptions(screen.getByLabelText("Status"), "open")
    await waitFor(() => expect(calls.at(-1)).toBe("open"))
  })

  test("says when there are none or the list fails", async () => {
    server.use(http.get("/api/v1/knowledge/proposals", () => HttpResponse.json(fx.knowledgeProposalsEmpty)))
    const first = await renderApp("/knowledge?project=app&tab=proposals")
    expect(await screen.findByText("No proposals.")).toBeInTheDocument()
    first.unmount()
    server.use(http.get("/api/v1/knowledge/proposals", errors.invalidInput))
    await renderApp("/knowledge?project=app&tab=proposals")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })
})

describe("knowledge page: capped lists", () => {
  test("proposals and notes say when they are full", async () => {
    const proposals = Array.from({ length: 50 }, (_, i) => ({ ...fx.knowledgeProposals[0]!, ref: `KB-${i + 10}` }))
    const notes = { ...fx.knowledgeNotes, entries: Array.from({ length: 50 }, (_, i) => ({ ...fx.knowledgeNotes.entries[1]!, id: `n${i}` })) }
    server.use(
      http.get("/api/v1/knowledge/proposals", () => HttpResponse.json(proposals)),
      http.get("/api/v1/knowledge/entries", ({ request }) => HttpResponse.json(new URL(request.url).searchParams.get("kind") === "note" ? notes : fx.knowledgeEntries)),
    )
    const first = await renderApp("/knowledge?project=app&tab=proposals")
    expect(await screen.findByText(/Showing the first 50/)).toBeInTheDocument()
    first.unmount()
    await renderApp("/knowledge?project=app&tab=notes")
    expect(await screen.findByText(/Showing the first 50/)).toBeInTheDocument()
  })
})

describe("knowledge page: propose a change", () => {
  function recordProposals() {
    const bodies: unknown[] = []
    server.use(
      http.post("/api/v1/knowledge/proposals", async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json(fx.knowledgePropose, { status: 201 })
      }),
    )
    return bodies
  }
  const fill = async (user: Awaited<ReturnType<typeof renderApp>>["user"], form: HTMLElement) => {
    await user.selectOptions(within(form).getByLabelText("Section"), "architecture")
    await user.type(within(form).getByLabelText("Path"), " docs/adr/0003-x.md ")
    await user.type(within(form).getByLabelText(/Entry/), "# X")
    await user.type(within(form).getByLabelText("Reason"), " Because. ")
  }

  test("offers the curated sections only", async () => {
    await renderApp("/knowledge?project=app&tab=proposals")
    const form = await screen.findByRole("form", { name: "Propose a change" })
    await waitFor(() => expect(within(form).getByRole("option", { name: "architecture" })).toBeInTheDocument())
    expect(within(form).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Choose a section", "architecture", "conventions", "vision", "Automatic", "add", "edit", "retire",
    ])
  })

  test("asks for what is missing", async () => {
    const bodies = recordProposals()
    const { user } = await renderApp("/knowledge?project=app&tab=proposals")
    const form = await screen.findByRole("form", { name: "Propose a change" })
    await user.click(within(form).getByRole("button", { name: "Propose" }))
    for (const msg of ["Choose a section.", "Give the path of the entry.", "Write the entry, or choose retire.", "Say why the knowledge should change."]) {
      expect(within(form).getByText(msg)).toBeInTheDocument()
    }
    expect(bodies).toEqual([])
    // retiring needs no text
    await user.selectOptions(within(form).getByLabelText("Action"), "retire")
    expect(within(form).queryByLabelText(/Entry/)).not.toBeInTheDocument()
  })

  test("sends the proposal and links the pull request", async () => {
    const bodies = recordProposals()
    const { user } = await renderApp("/knowledge?project=app&tab=proposals")
    const form = await screen.findByRole("form", { name: "Propose a change" })
    await waitFor(() => expect(within(form).getByRole("option", { name: "architecture" })).toBeInTheDocument())
    await fill(user, form)
    await user.click(within(form).getByRole("button", { name: "Propose" }))
    await waitFor(() => expect(bodies).toEqual([{ project: "app", section: "architecture", path: "docs/adr/0003-x.md", body: "# X", reason: "Because." }]))
    const done = await within(form).findByRole("status")
    expect(done).toHaveTextContent("KB-6 opened as pull request #13")
    expect(within(done).getByRole("link")).toHaveAttribute("href", "https://github.com/acme/app/pull/13")
    expect(within(form).getByLabelText("Path")).toHaveValue("")
  })

  test("sends a retirement without text and shows a refusal", async () => {
    const bodies = recordProposals()
    const { user } = await renderApp("/knowledge?project=app&tab=proposals")
    const form = await screen.findByRole("form", { name: "Propose a change" })
    await waitFor(() => expect(within(form).getByRole("option", { name: "architecture" })).toBeInTheDocument())
    await user.selectOptions(within(form).getByLabelText("Section"), "architecture")
    await user.selectOptions(within(form).getByLabelText("Action"), "retire")
    await user.type(within(form).getByLabelText("Path"), "docs/adr/0001-queue.md")
    await user.type(within(form).getByLabelText("Reason"), "Old.")
    await user.click(within(form).getByRole("button", { name: "Propose" }))
    await waitFor(() => expect(bodies).toEqual([{ project: "app", section: "architecture", path: "docs/adr/0001-queue.md", action: "retire", body: "", reason: "Old." }]))

    server.use(http.post("/api/v1/knowledge/proposals", errors.invalidInput))
    await user.type(within(form).getByLabelText("Path"), "docs/adr/0001-queue.md")
    await user.type(within(form).getByLabelText("Reason"), "Again.")
    await user.click(within(form).getByRole("button", { name: "Propose" }))
    expect(await within(form).findByRole("alert")).toBeInTheDocument()
  })
})

describe("knowledge page: agent notes", () => {
  test("lists the notes of the project with their scope, tags and expiry", async () => {
    const lists: URLSearchParams[] = []
    server.use(
      http.get("/api/v1/knowledge/entries", ({ request }) => {
        lists.push(new URL(request.url).searchParams)
        return HttpResponse.json(fx.knowledgeNotes)
      }),
    )
    await renderApp("/knowledge?project=app&tab=notes")
    const list = await screen.findByRole("list", { name: "Agent notes" })
    expect(lists.at(-1)!.get("kind")).toBe("note")
    expect(lists.at(-1)!.get("goal")).toBeNull()
    const [goalNote, projectNote] = within(list).getAllByRole("article")
    expect(within(goalNote!).getByText("goal note")).toBeInTheDocument()
    expect(within(goalNote!).getByText("tests")).toBeInTheDocument()
    expect(within(goalNote!).getByText(/by developer \(JOB-41\)/)).toBeInTheDocument()
    expect(within(goalNote!).getByText(/^expires/)).toHaveTextContent("expires in 33 h")
    expect(within(projectNote!).getByText("project note")).toBeInTheDocument()
    expect(within(projectNote!).getByText("Run make test, not go test.")).toBeInTheDocument()
    expect(within(projectNote!).queryByText(/expires/)).not.toBeInTheDocument()
  })

  test("shows note text as text", async () => {
    const { container } = await renderApp("/knowledge?project=app&tab=notes")
    const note = (await screen.findAllByRole("article"))[0]!
    expect(within(note).getByText(/<script>alert\(1\)<\/script>/)).toBeInTheDocument()
    expect(within(note).getByRole("heading", { name: "The spool is <i>flaky</i>" })).toBeInTheDocument()
    expect(container.querySelector("i, script")).toBeNull()
  })

  test("reads the notes of a goal", async () => {
    const lists: URLSearchParams[] = []
    server.use(
      http.get("/api/v1/knowledge/entries", ({ request }) => {
        lists.push(new URL(request.url).searchParams)
        return HttpResponse.json(fx.knowledgeNotes)
      }),
    )
    const { user, router } = await renderApp("/knowledge?project=app&tab=notes")
    await screen.findByRole("list", { name: "Agent notes" })
    await waitFor(() => expect(within(screen.getByLabelText("Notes of")).getAllByRole("option").length).toBeGreaterThan(1))
    await user.selectOptions(screen.getByLabelText("Notes of"), "GOAL-3")
    await waitFor(() => expect(lists.at(-1)!.get("goal")).toBe("GOAL-3"))
    expect(router.state.location.search).toMatchObject({ goal: "GOAL-3" })
    await user.selectOptions(screen.getByLabelText("Notes of"), "")
    await waitFor(() => expect(lists.at(-1)!.get("goal")).toBeNull())
  })

  test("the goal page links its notes", async () => {
    const { user, router } = await renderApp("/goals/GOAL-3")
    await user.click(await screen.findByRole("link", { name: "Agent notes" }))
    await screen.findByRole("heading", { level: 1, name: "Knowledge" })
    expect(router.state.location.search).toMatchObject({ project: "app", tab: "notes", goal: "GOAL-3" })
    expect(await screen.findByRole("list", { name: "Agent notes" })).toBeInTheDocument()
  })

  test("deletes a note after confirming", async () => {
    const deleted: string[] = []
    server.use(
      http.delete("/api/v1/knowledge/notes/:id", ({ params }) => {
        deleted.push(String(params.id))
        return HttpResponse.json(fx.knowledgeNoteDelete)
      }),
    )
    const { user } = await renderApp("/knowledge?project=app&tab=notes")
    await user.click(await screen.findByRole("button", { name: "Delete note The spool is <i>flaky</i>" }))
    const dialog = await screen.findByRole("dialog", { name: "Delete this note?" })
    expect(deleted).toEqual([])
    await user.click(within(dialog).getByRole("button", { name: "Keep note" }))
    expect(deleted).toEqual([])
    await user.click(await screen.findByRole("button", { name: "Delete note The spool is <i>flaky</i>" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete note" }))
    await waitFor(() => expect(deleted).toEqual([fx.knowledgeNotes.entries[0]!.id]))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
  })

  test("says when there are no notes", async () => {
    server.use(http.get("/api/v1/knowledge/entries", () => HttpResponse.json(fx.knowledgeEntriesEmpty)))
    await renderApp("/knowledge?project=app&tab=notes")
    expect(await screen.findByText("No notes.")).toBeInTheDocument()
  })
})

describe("knowledge page: integrations", () => {
  const withIntegrations = (): Project =>
    ({
      ...fx.project,
      profile: {
        ...fx.project.profile,
        integrations: [
          { tool: "confluence", instance: "acme.atlassian.net", uses: ["knowledge"], confidence: "high", status: "approved", mapping: { conventions: ["ENG/Conventions"] } },
          { tool: "mcp:sentry", uses: ["monitoring", "notifications"], confidence: "low", status: "proposed" },
          { tool: "slack", uses: ["notifications"], confidence: "medium", status: "connected" },
        ],
      },
    }) as Project

  test("lists the tools of the profile with their status", async () => {
    server.use(http.get("/api/v1/projects/*", () => HttpResponse.json(withIntegrations())))
    await renderApp("/knowledge?project=app&tab=integrations")
    const list = await screen.findByRole("list", { name: "Integrations" })
    const [confluence, sentry, slack] = within(list).getAllByRole("listitem")
    expect(within(confluence!).getByText("acme.atlassian.net")).toBeInTheDocument()
    expect(within(confluence!).getByText("approved")).toBeInTheDocument()
    expect(within(confluence!).getByText("high confidence")).toBeInTheDocument()
    expect(within(confluence!).getByText("ENG/Conventions")).toBeInTheDocument()
    expect(within(confluence!).getByText("Could be used for knowledge.")).toBeInTheDocument()
    expect(within(sentry!).getByText("proposed")).toBeInTheDocument()
    expect(within(sentry!).getByText("Could be used for monitoring, notifications.")).toBeInTheDocument()
    expect(within(slack!).getByText("connected")).toBeInTheDocument()
  })

  test("says when onboarding found none", async () => {
    await renderApp("/knowledge?project=app&tab=integrations")
    expect(await screen.findByText("Onboarding found no tools outside the repo.")).toBeInTheDocument()
  })
})
