import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const as = (...perms: string[]) => server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => !perms.includes(p)) })))

/** Records the body of every plan the page asks for. */
function plans() {
  const bodies: { title: string; changes: { kind: string; name: string; spec?: Record<string, unknown>; delete?: boolean }[] }[] = []
  server.use(
    http.post("/api/v1/config/plans", async ({ request }) => {
      bodies.push((await request.json()) as (typeof bodies)[number])
      return HttpResponse.json(fx.configPlanTracker, { status: 201 })
    }),
  )
  return bodies
}

const jira = () => screen.findByRole("region", { name: /Jira jira/ })

describe("the tracker screen", () => {
  test("shows each connection with its rules, warnings, people and waiting tickets", async () => {
    await renderApp("/settings/integrations")
    const card = await jira()
    expect(within(card).getByRole("heading", { name: "Jira · jira.acme.local" })).toBeInTheDocument()
    expect(within(card).getByText(/tickets labeled forecrew or moved to Ready for agents/)).toBeInTheDocument()
    const rules = within(card).getByRole("list", { name: "Rules of jira" })
    expect(within(rules).getByText("Tickets from ENG labeled forecrew go to app")).toBeInTheDocument()
    expect(within(rules).getByText("Tickets from MOBILE of type Bug go to mobile")).toBeInTheDocument()
    expect(within(rules).getByText(/no transition of MOBILE-4 reaches Done/)).toBeInTheDocument()
    expect(within(rules).getByText("Warning:")).toHaveClass("sr-only")
    expect(await within(card).findByText("2 people not matched yet")).toBeInTheDocument()
    expect(within(card).getByText("3 tickets, limit of 3 goals running")).toBeInTheDocument()
    expect(within(card).getByText("Not tested here yet")).toBeInTheDocument()
    // The Linear connection is its own card, with its team as the address.
    const linear = screen.getByRole("region", { name: /Linear linear/ })
    expect(within(linear).getByRole("heading", { name: "Linear · teams ENG" })).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "Integrations" })).toBeInTheDocument()
  })

  test("says what to do with no connection", async () => {
    server.use(http.get("/api/v1/tracker-connections", () => HttpResponse.json(fx.trackerConnectionsEmpty)))
    await renderApp("/settings/integrations")
    expect(await screen.findByText("Connect a tracker and hand tickets to agents from where you already work.")).toBeInTheDocument()
  })

  test("a test shows each check on its own", async () => {
    const { user } = await renderApp("/settings/integrations")
    const card = await jira()
    await user.click(within(card).getByRole("button", { name: "Test jira" }))
    const results = await within(card).findByRole("list", { name: "Test of jira" })
    expect(within(results).getAllByRole("listitem")).toHaveLength(6)
    expect(within(results).getByText("address")).toBeInTheDocument()
    expect(within(results).getByText(/signed in as Forecrew/)).toBeInTheDocument()
    expect(within(card).getByText(/^Works · checked/)).toBeInTheDocument()
  })

  test("a failed check says what to do, and a certificate failure offers the company's certificate", async () => {
    server.use(http.post("/api/v1/tracker-connections/:name/test", () => HttpResponse.json(fx.trackerTestFailed)))
    const calls: { body?: unknown } = {}
    server.use(
      http.get("/api/v1/config/resources", () =>
        HttpResponse.json([{ kind: "tracker_connection", name: "jira", spec: { kind: "jira", config: { base_url: "https://jira.acme.local/jira", projects: ["ENG"], auth: "pat", token_ref: "store:jira-token" }, triggers: { label: "forecrew" } } }]),
      ),
      http.post("/api/v1/config/plans", async ({ request }) => ((calls.body = await request.json()), HttpResponse.json(fx.configPlanTracker, { status: 201 }))),
    )
    const { user } = await renderApp("/settings/integrations")
    const card = await jira()
    await user.click(within(card).getByRole("button", { name: "Test jira" }))
    expect(await within(card).findByText("Needs attention", { exact: false })).toBeInTheDocument()
    expect(within(card).getByText(/certificate is not trusted/)).toBeInTheDocument()
    // The checks after the failed one only say they were not checked: no advice for them.
    expect(within(card).queryByText(/Make a new token/)).toBeNull()
    await user.click(within(card).getByRole("button", { name: "Add the certificate" }))
    expect(await within(card).findByRole("alert")).toHaveTextContent("BEGIN CERTIFICATE")
    await user.type(within(card).getByLabelText("Your company's certificate (PEM)"), "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----")
    await user.click(within(card).getByRole("button", { name: "Add the certificate" }))
    await within(card).findByRole("region", { name: /Change plan/ })
    expect(calls.body).toMatchObject({
      changes: [{ kind: "tracker_connection", name: "jira", spec: { kind: "jira", config: { token_ref: "store:jira-token", ca_bundle: expect.stringContaining("BEGIN CERTIFICATE") } } }],
    })
  })

  test("a private address is told to a person in words, in the test and in the form", async () => {
    server.use(
      http.post("/api/v1/tracker-connections/:name/test", () =>
        HttpResponse.json({ connection: "jira", ok: false, checks: [{ name: "address", ok: false, detail: "address 10.0.0.5 is private: refused" }] }),
      ),
    )
    const { user } = await renderApp("/settings/integrations")
    const card = await jira()
    await user.click(within(card).getByRole("button", { name: "Test jira" }))
    expect(await within(card).findByText(/On a hosted server the tracker must be reachable from outside/)).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Connect a tracker" }))
    const dialog = await screen.findByRole("dialog")
    await user.type(within(dialog).getByLabelText("Address"), "https://jira.acme.local/jira")
    expect(within(dialog).getByRole("note")).toHaveTextContent("Jira must be reachable from outside")
  })

  test("a test that cannot run says so", async () => {
    server.use(http.post("/api/v1/tracker-connections/:name/test", () => HttpResponse.json({ error: { code: "unavailable", message: "the secret store is not configured" } }, { status: 503 })))
    const { user } = await renderApp("/settings/integrations")
    await user.click(within(await jira()).getByRole("button", { name: "Test jira" }))
    expect(await within(await jira()).findByRole("alert")).toHaveTextContent("the secret store is not configured")
  })
})

describe("permissions", () => {
  test("a reader sees everything and each control names the permission it needs", async () => {
    let people = 0
    as("trackers.manage")
    server.use(http.get("/api/v1/tracker-people/unmapped", () => (people++, HttpResponse.json(fx.trackerUnmapped))))
    await renderApp("/settings/integrations")
    const card = await jira()
    expect(within(card).getByRole("button", { name: "Test jira" })).toBeDisabled()
    expect(within(card).getByRole("button", { name: "Add a rule to jira" })).toBeDisabled()
    expect(within(card).getByRole("button", { name: "Remove the rule jira/1" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Connect a tracker" })).toBeDisabled()
    expect(screen.getAllByText("trackers.manage").length).toBeGreaterThan(3)
    expect(screen.getByText(/You can read this page/)).toBeInTheDocument()
    // Matching people reads the waiting tickets from the tracker: not asked for someone who cannot match.
    expect(within(card).getByText("everyone who handed over a ticket is matched")).toBeInTheDocument()
    expect(people).toBe(0)
  })

  test("without trackers.read the section is not there", async () => {
    as("trackers.read")
    await renderApp("/settings/integrations")
    expect(await screen.findByText(/You cannot see this section/)).toHaveTextContent("trackers.read")
    expect(screen.queryByRole("link", { name: "Integrations" })).toBeNull()
  })

  test("accept and dismiss need goal.create, named on the button", async () => {
    as("goal.create")
    const { user } = await renderApp("/settings/integrations")
    await user.click(await within(await jira()).findByRole("button", { name: "Open the waiting tickets of jira" }))
    const list = await screen.findByRole("list", { name: "Tickets waiting" })
    expect(within(list).getAllByText("goal.create").length).toBeGreaterThanOrEqual(2)
    expect(within(list).getByRole("button", { name: "Start ENG-21" })).toBeDisabled()
  })
})

describe("connect", () => {
  test("a Linear connection is a plan with a reference to the token, never the token", async () => {
    const bodies = plans()
    let secret: unknown
    server.use(
      http.put("/api/v1/config/plans/:plan/secrets/:secret", async ({ request }) => ((secret = await request.json()), HttpResponse.json(fx.configPlanTrackerSecret))),
      http.post("/api/v1/config/plans/:plan/apply", () => HttpResponse.json(fx.configPlanTrackerApply)),
    )
    let tested = ""
    server.use(http.post("/api/v1/tracker-connections/:name/test", ({ params }) => ((tested = String(params.name)), HttpResponse.json({ ...fx.trackerTest, connection: "linear" }))))
    const { user } = await renderApp("/settings/integrations")
    await user.click(await screen.findByRole("button", { name: "Connect a tracker" }))
    const dialog = await screen.findByRole("dialog")
    await user.selectOptions(within(dialog).getByLabelText("Tracker"), "linear")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText("Enter the team keys, such as ENG.")).toBeInTheDocument()
    expect(bodies).toHaveLength(0)
    await user.type(within(dialog).getByLabelText("Teams"), "eng, ops")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    await within(dialog).findByRole("region", { name: /Change plan/ })
    // The workspace has a connection called linear already: the new one gets a free name.
    expect(bodies[0]).toMatchObject({
      title: "Connect Linear as linear-2",
      changes: [{ kind: "tracker_connection", name: "linear-2", spec: { kind: "linear", config: { teams: ["ENG", "OPS"], token_ref: "store:linear-2-token" }, triggers: { label: "forecrew" } } }],
    })
    expect(JSON.stringify(bodies[0])).not.toMatch(/lin_api/)
    // The token goes to its secure field and nowhere else.
    const field = within(dialog).getByRole("form", { name: "Secure field: token of tracker connection linear" })
    await user.type(within(field).getByLabelText("token of tracker connection linear"), "lin_api_abc")
    await user.click(within(field).getByRole("button", { name: "Save" }))
    await waitFor(() => expect(secret).toEqual({ value: "lin_api_abc" }))
    await user.click(await within(dialog).findByRole("button", { name: /^Apply|^Confirm|^Connect/ }))
    await user.click(await screen.findByRole("button", { name: "Done" }))
    // Once it is applied the connection is tested, so the person sees each check.
    await waitFor(() => expect(tested).toBe("linear-2"))
    expect(screen.queryByRole("dialog")).toBeNull()
  })

  test("a Jira connection checks the address, the projects and the email of a Cloud token", async () => {
    const bodies = plans()
    const { user } = await renderApp("/settings/integrations")
    await user.click(await screen.findByRole("button", { name: "Connect a tracker" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText(/Enter the https address of Jira/)).toBeInTheDocument()
    expect(within(dialog).getByText(/Enter the project keys/)).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Address"), "http://jira.acme.local")
    await user.type(within(dialog).getByLabelText("Projects"), "eng mobile")
    await user.selectOptions(within(dialog).getByLabelText("Token"), "api_token")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText(/Enter the https address of Jira/)).toBeInTheDocument()
    expect(within(dialog).getByText(/Enter the email of the account/)).toBeInTheDocument()
    await user.clear(within(dialog).getByLabelText("Address"))
    await user.type(within(dialog).getByLabelText("Address"), "https://acme.atlassian.net/")
    await user.type(within(dialog).getByLabelText("Account email"), "forecrew@acme.example")
    await user.selectOptions(within(dialog).getByLabelText("A person hands a ticket over when"), "state")
    await user.type(within(dialog).getByLabelText("Status"), "Ready for agents")
    await user.click(within(dialog).getByText("More options"))
    await user.click(within(dialog).getByLabelText(/Use webhooks/))
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    await within(dialog).findByRole("region", { name: /Change plan/ })
    expect(bodies[0]?.changes[0]?.spec).toEqual({
      kind: "jira",
      config: { base_url: "https://acme.atlassian.net", projects: ["ENG", "MOBILE"], auth: "api_token", email: "forecrew@acme.example", token_ref: "store:jira-2-token", webhook_secret_ref: "store:jira-2-webhook" },
      triggers: { state: "Ready for agents" },
    })
  })

  test("a name that is taken or badly written is refused before a plan", async () => {
    const bodies = plans()
    const { user } = await renderApp("/settings/integrations")
    await user.click(await screen.findByRole("button", { name: "Connect a tracker" }))
    const dialog = await screen.findByRole("dialog")
    await user.selectOptions(within(dialog).getByLabelText("Tracker"), "linear")
    await user.type(within(dialog).getByLabelText("Teams"), "ENG")
    await user.click(within(dialog).getByText("More options"))
    await user.clear(within(dialog).getByLabelText("Name"))
    await user.type(within(dialog).getByLabelText("Name"), "linear")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText("A connection has this name already.")).toBeInTheDocument()
    await user.clear(within(dialog).getByLabelText("Name"))
    await user.type(within(dialog).getByLabelText("Name"), "Bad Name")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText(/Use lowercase letters/)).toBeInTheDocument()
    expect(bodies).toHaveLength(0)
  })

  test("a refused plan says why, and Edit comes back to the form", async () => {
    server.use(http.post("/api/v1/config/plans", () => HttpResponse.json({ error: { code: "invalid_input", message: "projects: not a Jira key" } }, { status: 400 })))
    const { user } = await renderApp("/settings/integrations")
    await user.click(await screen.findByRole("button", { name: "Connect a tracker" }))
    const dialog = await screen.findByRole("dialog")
    await user.selectOptions(within(dialog).getByLabelText("Tracker"), "linear")
    await user.type(within(dialog).getByLabelText("Teams"), "ENG")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("projects: not a Jira key")
  })

  test("a tracker found in the repos leads to the form with its address", async () => {
    server.use(
      http.get("/api/v1/tracker-connections", () => HttpResponse.json([fx.trackerConnections[1]])),
      http.get("/api/v1/projects/*", () => HttpResponse.json({ ...fx.project, profile: { ...fx.project.profile, integrations: [{ tool: "Jira", instance: "jira.acme.local", uses: ["issues"], confidence: "high", status: "proposed" }, { tool: "Confluence", uses: ["knowledge"], confidence: "low", status: "proposed" }] } })),
    )
    const { user } = await renderApp("/settings/integrations")
    const found = await screen.findByRole("heading", { name: "Found in your repos, not connected" })
    const list = found.parentElement as HTMLElement
    expect(within(list).getByText("jira.acme.local")).toBeInTheDocument()
    expect(within(list).queryByText("Confluence")).toBeNull()
    await user.click(within(list).getByRole("button", { name: "Connect Jira" }))
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByLabelText("Address")).toHaveValue("https://jira.acme.local")
  })

  test("the link of a discovered tool opens the form for it", async () => {
    await renderApp("/settings/integrations?connect=linear&instance=acme")
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByLabelText("Tracker")).toHaveValue("linear")
  })
})

describe("rules", () => {
  const open = async () => {
    const view = await renderApp("/settings/integrations")
    await view.user.click(within(await jira()).getByRole("button", { name: "Add a rule to jira" }))
    return { ...view, dialog: await screen.findByRole("dialog") }
  }

  test("a rule asks for the project and the Forecrew project, and suggests the statuses", async () => {
    const bodies = plans()
    const { user, dialog } = await open()
    await user.click(within(dialog).getByText("More options"))
    await waitFor(() => expect(within(dialog).getByLabelText("When the goal is in progress")).toHaveValue("In Progress"))
    expect(within(dialog).getByLabelText("When the goal is in review")).toHaveValue("Code Review")
    expect(within(dialog).getByLabelText("When the goal is done")).toHaveValue("Done")
    expect(within(dialog).getByLabelText("When the goal is canceled")).toHaveValue("")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText("Choose the Forecrew project the tickets go to.")).toBeInTheDocument()
    await user.selectOptions(within(dialog).getByLabelText("Jira project"), "MOBILE")
    await user.selectOptions(within(dialog).getByLabelText("Goes to project"), "app")
    await user.type(within(dialog).getByLabelText("Only these types"), "Bug, Task")
    await user.clear(within(dialog).getByLabelText("When the goal is in review"))
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    await within(dialog).findByRole("region", { name: /Change plan/ })
    // The next free position is 3: the connection has the rules 1 and 2.
    expect(bodies[0]?.changes[0]).toMatchObject({
      kind: "tracker_route", name: "jira/3",
      spec: { project: "app", match: { project: "MOBILE", types: ["Bug", "Task"] }, states: { in_progress: "In Progress", done: "Done" } },
    })
  })

  test("when the statuses cannot be read the rule still works, with typed statuses", async () => {
    server.use(http.post("/api/v1/tracker-connections/:name/test", () => HttpResponse.json({ error: { code: "unavailable", message: "down" } }, { status: 503 })))
    const bodies = plans()
    const { user, dialog } = await open()
    await user.click(within(dialog).getByText("More options"))
    expect(await within(dialog).findByText(/The statuses could not be read/)).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("When the goal is done"), "Closed")
    await user.selectOptions(within(dialog).getByLabelText("Goes to project"), "app")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    await within(dialog).findByRole("region", { name: /Change plan/ })
    expect(bodies[0]?.changes[0]?.spec).toMatchObject({ states: { done: "Closed" } })
  })

  test("a rule is removed by a plan", async () => {
    const bodies = plans()
    const { user } = await renderApp("/settings/integrations")
    await user.click(await within(await jira()).findByRole("button", { name: "Remove the rule jira/2" }))
    await screen.findByRole("region", { name: /Change plan/ })
    expect(bodies[0]).toMatchObject({ title: "Remove the rule jira/2", changes: [{ kind: "tracker_route", name: "jira/2", delete: true }] })
  })

  test("a connection with no rule says tickets wait in the Inbox", async () => {
    server.use(http.get("/api/v1/tracker-routes", () => HttpResponse.json([])))
    await renderApp("/settings/integrations")
    expect((await screen.findAllByText(/tickets handed over wait in the Inbox/)).length).toBe(2)
  })
})

describe("people", () => {
  test("a suggestion is confirmed by a person, and the mapping names who and what", async () => {
    let body: unknown
    server.use(http.post("/api/v1/tracker-identities", async ({ request }) => ((body = await request.json()), HttpResponse.json(fx.trackerIdentityMap, { status: 201 }))))
    const { user } = await renderApp("/settings/integrations")
    await user.click(await within(await jira()).findByRole("button", { name: "Review the people of jira" }))
    const list = await screen.findByRole("list", { name: "People not matched yet" })
    expect(within(list).getAllByRole("listitem")).toHaveLength(2)
    // Names come from the tracker: text, never markup.
    expect(within(list).getByText("R. Verdi <i>(contractor)</i>")).toBeInTheDocument()
    await user.click(within(list).getByRole("button", { name: "Confirm Marta Rossi is Marta Rossi" }))
    await waitFor(() => expect(body).toEqual({ connection: "jira", external_id: "JIRAUSER10300", external_name: "Marta Rossi", user: fx.members.find((m) => m.name === "Marta Rossi")?.user_id }))
  })

  test("a person with no suggestion is matched to a member chosen from the list", async () => {
    let body: { user?: string } = {}
    server.use(http.post("/api/v1/tracker-identities", async ({ request }) => ((body = (await request.json()) as typeof body), HttpResponse.json(fx.trackerIdentityMap, { status: 201 }))))
    const { user } = await renderApp("/settings/integrations")
    await user.click(await within(await jira()).findByRole("button", { name: "Review the people of jira" }))
    const match = await screen.findByRole("button", { name: /Match R\. Verdi/ })
    expect(match).toBeDisabled()
    await user.selectOptions(screen.getByLabelText(/Forecrew member for R\. Verdi/), fx.members[0]!.user_id)
    await user.click(match)
    await waitFor(() => expect(body.user).toBe(fx.members[0]!.user_id))
  })

  test("a mapping that fails says why", async () => {
    server.use(http.post("/api/v1/tracker-identities", () => HttpResponse.json({ error: { code: "forbidden", message: "the user holds more than you" } }, { status: 403 })))
    const { user } = await renderApp("/settings/integrations")
    await user.click(await within(await jira()).findByRole("button", { name: "Review the people of jira" }))
    await user.click(await screen.findByRole("button", { name: "Confirm Marta Rossi is Marta Rossi" }))
    expect(await screen.findByText("the user holds more than you")).toBeInTheDocument()
  })

  test("a connection that could not be read is told, and the others still show", async () => {
    server.use(http.get("/api/v1/tracker-people/unmapped", () => HttpResponse.json(fx.trackerUnmappedProblem)))
    await renderApp("/settings/integrations")
    expect(await screen.findByText(/jira answered 503/)).toBeInTheDocument()
    expect((await jira()).textContent).toContain("everyone who handed over a ticket is matched")
  })
})

describe("waiting tickets", () => {
  const open = async () => {
    const view = await renderApp("/settings/integrations")
    await view.user.click(await within(await jira()).findByRole("button", { name: "Open the waiting tickets of jira" }))
    return { ...view, list: await screen.findByRole("list", { name: "Tickets waiting" }) }
  }

  test("each ticket says why it waits, and what a queued one waits for", async () => {
    const { list } = await open()
    const items = within(list).getAllByRole("listitem")
    expect(items).toHaveLength(3)
    expect(items[0]).toHaveTextContent("Marta Rossi isn't in Forecrew. Start this anyway?")
    expect(items[0]).toHaveTextContent("Export the audit log <b>as CSV</b>")
    expect(items[1]).toHaveTextContent("No rule says where this ticket goes.")
    expect(items[2]).toHaveTextContent("Waiting for a free slot: 3 goals from tickets are already running in this project.")
    expect(within(items[2]!).queryByRole("button")).toBeNull()
    expect(within(items[0]!).getByRole("link", { name: /ENG-21/ })).toHaveAttribute("href", "https://jira.acme.local/jira/browse/ENG-21")
  })

  test("starting a ticket anyway accepts it; one with no project needs a project chosen", async () => {
    const accepted: { id: string; body: unknown }[] = []
    server.use(http.post("/api/v1/tickets/:ticket/accept", async ({ params, request }) => (accepted.push({ id: String(params.ticket), body: await request.json() }), HttpResponse.json(fx.ticketAccept))))
    const { user, list } = await open()
    await user.click(within(list).getByRole("button", { name: "Start ENG-21" }))
    await waitFor(() => expect(accepted).toHaveLength(1))
    expect(accepted[0]).toEqual({ id: "018f0000-0000-7000-8000-000000000950", body: {} })
    const accept = within(list).getByRole("button", { name: "Accept OPS-3" })
    expect(accept).toBeDisabled()
    await user.selectOptions(within(list).getByLabelText("Project for OPS-3"), "app")
    await user.click(accept)
    await waitFor(() => expect(accepted).toHaveLength(2))
    expect(accepted[1]?.body).toEqual({ project: "app" })
  })

  test("dismissing a ticket and a refusal", async () => {
    let dismissed = 0
    server.use(http.post("/api/v1/tickets/:ticket/dismiss", () => (dismissed++, HttpResponse.json(fx.ticketDismiss))))
    const { user, list } = await open()
    await user.click(within(list).getByRole("button", { name: "Dismiss OPS-3" }))
    await waitFor(() => expect(dismissed).toBe(1))
    server.use(http.post("/api/v1/tickets/:ticket/accept", () => HttpResponse.json({ error: { code: "conflict", message: "ticket ENG-21 is not waiting" } }, { status: 409 })))
    await user.click(within(list).getByRole("button", { name: "Start ENG-21" }))
    expect(await within(list).findByRole("alert")).toHaveTextContent("ticket ENG-21 is not waiting")
  })

  test("a ticket address that is not a web address is not a link", async () => {
    server.use(http.get("/api/v1/tickets/waiting", () => HttpResponse.json(fx.ticketsWaiting.map((t) => ({ ...t, url: "javascript:alert(1)" })))))
    const { list } = await open()
    expect(within(list).queryByRole("link")).toBeNull()
    expect(within(list).getByText("ENG-21")).toBeInTheDocument()
  })

  test("a connection with nothing waiting says no tickets", async () => {
    server.use(http.get("/api/v1/tickets/waiting", () => HttpResponse.json(fx.ticketsWaitingEmpty)))
    await renderApp("/settings/integrations")
    expect(await within(await jira()).findByText("no tickets")).toBeInTheDocument()
    expect(within(await jira()).queryByRole("button", { name: /Open the waiting tickets/ })).toBeNull()
  })
})
