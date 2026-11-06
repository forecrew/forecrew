import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const section = (name: string) => screen.getByRole("region", { name })

describe("council list", () => {
  test("lists the councils of every project, newest first, with their status", async () => {
    await renderApp("/councils")
    const table = await screen.findByRole("region", { name: "Councils table" })
    const rows = within(table).getAllByRole("row").slice(1)
    expect(rows).toHaveLength(4)
    expect(rows.map((r) => within(r).getByRole("link").textContent)).toEqual([
      expect.stringContaining("COUNCIL-5"),
      expect.stringContaining("COUNCIL-4"),
      expect.stringContaining("COUNCIL-3"),
      expect.stringContaining("COUNCIL-2"),
    ])
    expect(within(rows[1]!).getByText("failed")).toBeInTheDocument()
    expect(within(rows[2]!).getByText("decided")).toBeInTheDocument()
    expect(within(rows[3]!).getByText("running")).toBeInTheDocument()
    expect(within(rows[0]!).getByText("app")).toBeInTheDocument()
  })

  test("filters by project and by status", async () => {
    const seen: string[] = []
    server.use(
      http.get("/api/v1/councils", ({ request }) => {
        seen.push(new URL(request.url).search)
        return HttpResponse.json(fx.councils)
      }),
    )
    const { user } = await renderApp("/councils?project=app")
    await screen.findByRole("region", { name: "Councils table" })
    expect(seen).toEqual(["?project=app"])
    await user.selectOptions(screen.getByLabelText("Status"), "decided")
    await waitFor(() => expect(seen).toContain("?project=app&status=decided"))
  })

  test("does not say none were started when a filter hides them", async () => {
    server.use(http.get("/api/v1/councils", () => HttpResponse.json(fx.councilsEmpty)))
    await renderApp("/councils?project=app&status=failed")
    expect(await screen.findByText("No failed councils in this project.")).toBeInTheDocument()
    expect(screen.queryByText(/No councils yet/)).not.toBeInTheDocument()
  })

  test("says when there are none, and has the way to start one", async () => {
    server.use(http.get("/api/v1/councils", () => HttpResponse.json(fx.councilsEmpty)))
    const { user, router } = await renderApp("/councils?project=app")
    expect(await screen.findByText(/No councils yet/)).toBeInTheDocument()
    await user.click(screen.getByRole("link", { name: "Start a council" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/councils/new"))
    expect(router.state.location.search).toEqual({ project: "app" })
  })

  test("opens a council", async () => {
    const { user, router } = await renderApp("/councils")
    await user.click(await screen.findByRole("link", { name: /COUNCIL-3/ }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/councils/COUNCIL-3"))
  })
})

describe("council page", () => {
  test("a running council shows its question, members and the stages so far", async () => {
    await renderApp("/councils/COUNCIL-2")
    expect(await screen.findByRole("heading", { name: "COUNCIL-2" })).toBeInTheDocument()
    expect(screen.getAllByText("running").length).toBeGreaterThan(0)
    expect(within(section("Question")).getByText("Which queue should jobs use?")).toBeInTheDocument()
    expect(within(section("Question")).getByText("Keep it to one database")).toBeInTheDocument()
    const members = within(screen.getByRole("list", { name: "Members" }))
    expect(members.getByText("architect@claude-code")).toBeInTheDocument()
    expect(members.getByText("security@codex")).toBeInTheDocument()
    expect(members.getByText("devil's advocate")).toBeInTheDocument()
    const proposals = within(section("proposal stage"))
    expect(proposals.getAllByText(/Use the Postgres queue we already run/, { selector: "p" })).toHaveLength(3)
    expect(within(section("critique stage")).getByText(/Polling/, { selector: "li" })).toBeInTheDocument()
    expect(screen.queryByRole("region", { name: "revision stage" })).not.toBeInTheDocument()
    expect(within(section("Brief")).getByText("The brief is written after the revisions.")).toBeInTheDocument()
    expect(screen.queryByText("Mandated dissent")).not.toBeInTheDocument()
  })

  test("shows the source checks of a contribution", async () => {
    await renderApp("/councils/COUNCIL-2")
    const proposals = within(await screen.findByRole("region", { name: "proposal stage" }))
    expect(proposals.getByText("1 sources, all checked")).toBeInTheDocument()
    expect(proposals.getAllByText("1 sources, 1 failed").length).toBeGreaterThan(0)
    expect(proposals.getAllByText(/private address/).length).toBeGreaterThan(0)
  })

  test("a decided council shows the brief, the decision and its dissent apart from the mandated one", async () => {
    await renderApp("/councils/COUNCIL-3")
    const brief = within(await screen.findByRole("region", { name: "Brief" }))
    expect(brief.getByText("recommended")).toBeInTheDocument()
    expect(brief.getByText("postgres-queue", { selector: "strong" })).toBeInTheDocument()
    expect(brief.getByText("reversible")).toBeInTheDocument()
    expect(brief.getByText("Fewer than 50 workers.")).toBeInTheDocument()
    expect(brief.getByText("Who owns the Redis cluster?")).toBeInTheDocument()
    expect(brief.getByText(/Isolates job data/)).toBeInTheDocument()

    const dissent = within(brief.getByRole("region", { name: "Dissent" }))
    expect(dissent.getByText(/Job data should not share the application database/)).toBeInTheDocument()
    expect(dissent.queryByText(/Polling will not scale/)).not.toBeInTheDocument()
    const mandated = within(brief.getByRole("region", { name: "Mandated dissent" }))
    expect(mandated.getByText(/Polling will not scale past 50 workers/)).toBeInTheDocument()
    expect(mandated.queryByText(/mandated dissent\)/)).not.toBeInTheDocument()

    const decision = within(section("Decision"))
    expect(decision.getByText("DEC-5")).toBeInTheDocument()
    expect(decision.getByText(/decided by user:ana/)).toBeInTheDocument()
    expect(decision.getByText(/GATE-10/)).toBeInTheDocument()
    expect(screen.getByRole("region", { name: "revision stage" })).toBeInTheDocument()
    // it ended: no way to add a constraint
    expect(screen.queryByLabelText("Add a constraint")).not.toBeInTheDocument()
  })

  test("a failed council says so", async () => {
    await renderApp("/councils/COUNCIL-4")
    expect(await screen.findByText("This council failed")).toBeInTheDocument()
    expect(screen.getByText("Its brief was rejected.")).toBeInTheDocument()
    expect(screen.queryByLabelText("Add a constraint")).not.toBeInTheDocument()
  })

  test("a council waiting for a human links to its decision in the Inbox", async () => {
    const { user, router } = await renderApp("/councils/COUNCIL-5")
    await user.click(await screen.findByRole("link", { name: "Decide in the Inbox" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/inbox"))
    expect(router.state.location.search).toEqual({ gate: "GATE-12" })
  })

  test("agent text is plain text, never markup", async () => {
    const { container } = await renderApp("/councils/COUNCIL-2")
    await screen.findByRole("region", { name: "critique stage" })
    expect(screen.getByText(/A <b>busy<\/b> table leaks timing/, { selector: "li" })).toBeInTheDocument()
    expect(container.querySelector("main b, article b, section b")).toBeNull()
  })

  test("the raw output is one click away", async () => {
    const { user } = await renderApp("/councils/COUNCIL-2")
    const stage = within(await screen.findByRole("region", { name: "critique stage" }))
    await user.click(stage.getByText("Raw output"))
    expect(stage.getByText(/"priority": "security"/)).toBeInTheDocument()
  })

  test("a content of an unknown shape still shows", async () => {
    const odd = structuredClone(fx.councilRunning)
    const loose = odd.contributions as { content: unknown; evidence_check: unknown }[]
    loose[0]!.content = "just a string"
    loose[1]!.content = { critiques: "nope", options: 3 }
    loose[2]!.evidence_check = null
    server.use(http.get("/api/v1/councils/:ref", () => HttpResponse.json(odd)))
    await renderApp("/councils/COUNCIL-2")
    const stage = within(await screen.findByRole("region", { name: "proposal stage" }))
    expect(stage.getAllByText("Raw output")).toHaveLength(3)
  })

  test("a council that does not exist is an error with a retry", async () => {
    await renderApp("/councils/COUNCIL-99")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })

  test("shows several revision rounds", async () => {
    const more = structuredClone(fx.councilDecided)
    more.contributions.push({ ...more.contributions.find((k) => k.stage === "revision")!, id: "x", round: 2 })
    server.use(http.get("/api/v1/councils/:ref", () => HttpResponse.json(more)))
    await renderApp("/councils/COUNCIL-3")
    const stage = within(await screen.findByRole("region", { name: "revision stage" }))
    expect(stage.getByText("Round 1")).toBeInTheDocument()
    expect(stage.getByText("Round 2")).toBeInTheDocument()
  })

  test("a canceled council, a council without a brief and one that could not finish", async () => {
    const canceled = { ...structuredClone(fx.councilDecided), status: "canceled", brief: undefined, constraints: null, gate: undefined, decision: undefined }
    server.use(http.get("/api/v1/councils/:ref", () => HttpResponse.json(canceled)))
    const { unmount } = await renderApp("/councils/COUNCIL-3")
    expect(await screen.findByText("This council was canceled")).toBeInTheDocument()
    expect(screen.getByText("No brief was written.")).toBeInTheDocument()
    expect(screen.getByText("None.")).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/councils/:ref", () => HttpResponse.json({ ...canceled, status: "failed", contributions: [] })))
    await renderApp("/councils/COUNCIL-3")
    expect(await screen.findByText("It could not finish.")).toBeInTheDocument()
    expect(screen.getByText("Members show up here as they contribute.")).toBeInTheDocument()
  })
})

describe("constraint form", () => {
  test("adds a constraint to a running council", async () => {
    const bodies: unknown[] = []
    server.use(
      http.post("/api/v1/councils/:ref/constraints", async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json(fx.councilConstraint)
      }),
      http.get("/api/v1/councils/:ref", () =>
        HttpResponse.json(bodies.length ? { ...fx.councilRunning, constraints: [...(fx.councilRunning.constraints ?? []), "No new services"] } : fx.councilRunning),
      ),
    )
    const { user } = await renderApp("/councils/COUNCIL-2")
    const box = await screen.findByLabelText("Add a constraint")
    await user.type(box, "  No new services ")
    await user.click(screen.getByRole("button", { name: "Add constraint" }))
    await waitFor(() => expect(bodies).toEqual([{ constraint: "No new services" }]))
    await waitFor(() => expect(box).toHaveValue(""))
    // the council is read again and shows the new constraint
    expect(await within(section("Question")).findByText("No new services", {}, { timeout: 2000 })).toBeInTheDocument()
  })

  test("asks for text first", async () => {
    const bodies: unknown[] = []
    server.use(http.post("/api/v1/councils/:ref/constraints", async ({ request }) => HttpResponse.json(bodies.push(await request.json()))))
    const { user } = await renderApp("/councils/COUNCIL-2")
    await user.click(await screen.findByRole("button", { name: "Add constraint" }))
    expect(screen.getByText("Write the constraint first.")).toBeInTheDocument()
    expect(bodies).toEqual([])
  })

  test("shows the refusal when the council ended meanwhile", async () => {
    server.use(http.post("/api/v1/councils/:ref/constraints", errors.councilEnded))
    const { user } = await renderApp("/councils/COUNCIL-2")
    await user.type(await screen.findByLabelText("Add a constraint"), "No new services")
    await user.click(screen.getByRole("button", { name: "Add constraint" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("COUNCIL-3 is decided")
  })
})

describe("start a council", () => {
  test("asks for a project and a question", async () => {
    const { user } = await renderApp("/councils/new")
    await user.click(await screen.findByRole("button", { name: "Start council" }))
    expect(screen.getByText("Choose a project.")).toBeInTheDocument()
    expect(screen.getByText("Ask the council a question.")).toBeInTheDocument()
  })

  test("starts the council and opens it", async () => {
    const bodies: unknown[] = []
    server.use(
      http.post("/api/v1/councils", async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json(fx.councilStart, { status: 202 })
      }),
    )
    const { user, router } = await renderApp("/councils/new")
    await user.selectOptions(await screen.findByLabelText("Project"), "app")
    await user.type(screen.getByLabelText("Question"), "Which queue should jobs use?")
    await user.selectOptions(screen.getByLabelText("Kind"), "technical")
    await user.type(screen.getByLabelText("Constraints"), "One database{Enter}  {Enter}No new services")
    await user.click(screen.getByRole("button", { name: "Start council" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/councils/COUNCIL-6"))
    expect(bodies).toEqual([{ project: "app", question: "Which queue should jobs use?", kind: "technical", constraints: ["One database", "No new services"] }])
  })

  test("leaves the kind to the server when none is chosen", async () => {
    const bodies: unknown[] = []
    server.use(
      http.post("/api/v1/councils", async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json(fx.councilStart, { status: 202 })
      }),
    )
    const { user } = await renderApp("/councils/new?project=app&question=Why%3F")
    expect(await screen.findByLabelText("Question")).toHaveValue("Why?")
    await user.click(screen.getByRole("button", { name: "Start council" }))
    await waitFor(() => expect(bodies).toEqual([{ project: "app", question: "Why?", constraints: [] }]))
  })

  test("shows a refusal", async () => {
    server.use(http.post("/api/v1/councils", errors.invalidInput))
    const { user } = await renderApp("/councils/new?project=app&question=Why%3F")
    await user.click(await screen.findByRole("button", { name: "Start council" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("feedback is required")
  })

  test("a goal page starts one for its project, from the goal", async () => {
    const { user, router } = await renderApp("/goals/GOAL-3")
    await user.click(await screen.findByRole("link", { name: "Start a council" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/councils/new"))
    expect(router.state.location.search).toEqual({ project: "app", question: "GOAL-3: Sign in with an email link" })
  })

  test("the command menu starts one", async () => {
    const { user, router } = await renderApp("/")
    await screen.findByRole("heading", { name: "Home" })
    await user.keyboard("{Control>}k{/Control}")
    await user.click(await screen.findByRole("option", { name: "Start a council" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/councils/new"))
  })
})

test("the sidebar links to councils and decisions and keeps the project", async () => {
  await renderApp("/councils?project=app")
  const nav = await screen.findByRole("navigation", { name: "Main" })
  expect(within(nav).getByRole("link", { name: "Councils" })).toHaveAttribute("href", "/councils?project=app")
  expect(within(nav).getByRole("link", { name: "Decisions" })).toHaveAttribute("href", "/decisions?project=app")
})
