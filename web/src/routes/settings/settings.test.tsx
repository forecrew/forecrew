import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

/** Answers /me as a person with a role: the Owner view is the default, Viewer and Contributor are recorded too. */
const asMe = (me: Record<string, unknown>) => server.use(http.get("/api/v1/me", () => HttpResponse.json(me)))
const asAdmin = () => asMe(fx.meAdmin)
const asViewer = () => asMe(fx.meViewer)

describe("settings navigation", () => {
  test("lists the sections the caller may read, and the sidebar leads to the first", async () => {
    asAdmin()
    const { user } = await renderApp("/")
    await user.click(await screen.findByRole("link", { name: "Settings" }))
    const nav = await screen.findByRole("navigation", { name: "Settings sections" })
    for (const name of ["General", "People", "Sign-in", "Change history"]) expect(within(nav).getByRole("link", { name })).toBeInTheDocument()
    expect(await screen.findByRole("heading", { name: "General" })).toBeInTheDocument()
  })

  test("a Viewer sees general and people, not sign-in or change history", async () => {
    asViewer()
    await renderApp("/settings")
    const nav = await screen.findByRole("navigation", { name: "Settings sections" })
    expect(within(nav).getByRole("link", { name: "General" })).toBeInTheDocument()
    expect(within(nav).getByRole("link", { name: "People" })).toBeInTheDocument()
    expect(within(nav).queryByRole("link", { name: "Sign-in" })).not.toBeInTheDocument()
    expect(within(nav).queryByRole("link", { name: "Change history" })).not.toBeInTheDocument()
  })

  test("a section the caller cannot read says which permission it needs", async () => {
    asViewer()
    await renderApp("/settings/sign-in")
    expect(await screen.findByText(/it needs/)).toHaveTextContent("connection.manage")
    expect(screen.queryByText("okta")).not.toBeInTheDocument()
  })

  test("a Contributor has no settings at all", async () => {
    asMe(fx.meContributor)
    await renderApp("/settings")
    expect(await screen.findByText("Only organization admins and owners can see settings.")).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument()
  })
})

describe("general", () => {
  test("an Owner renames the organization", async () => {
    asMe(fx.me)
    let body: unknown
    server.use(
      http.patch("/api/v1/organization", async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(fx.organization)
      }),
    )
    const { user } = await renderApp("/settings")
    const name = await screen.findByLabelText("Name")
    await user.clear(name)
    await user.type(name, "Acme Inc")
    await user.click(screen.getByRole("button", { name: "Save" }))
    expect(await screen.findByText("Saved.")).toBeInTheDocument()
    expect(body).toEqual({ name: "Acme Inc", slug: "acme" })
  })

  test("an Admin and a Viewer read it, and Save names the missing permission", async () => {
    asViewer()
    await renderApp("/settings")
    const name = await screen.findByLabelText("Name")
    expect(name).toHaveAttribute("readonly")
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled()
    expect(screen.getByText("org.manage")).toBeInTheDocument()
  })

  test("a refused change shows the server's message", async () => {
    asMe(fx.me)
    server.use(http.patch("/api/v1/organization", errors.conflict))
    const { user } = await renderApp("/settings")
    await screen.findByLabelText("Name")
    await user.click(screen.getByRole("button", { name: "Save" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("already approved")
  })
})

const membersTable = async () => within(await screen.findByRole("region", { name: "Members table" })).getByRole("table")

describe("members", () => {
  test("lists members with role, access, source and status, and the emails an Admin may see", async () => {
    asAdmin()
    await renderApp("/settings/people")
    const table = await membersTable()
    const marta = within(table).getByText("Marta Rossi").closest("tr")!
    expect(within(marta).getByText("marta@acme.test")).toBeInTheDocument()
    expect(within(marta).getByText("member")).toBeInTheDocument()
    expect(within(marta).getByText("app")).toBeInTheDocument() // maintainer on app, direct
    expect(within(marta).getByText("direct")).toBeInTheDocument()
    expect(within(marta).getByText("mobile")).toBeInTheDocument() // through team
    expect(within(marta).getByText("Active")).toBeInTheDocument()
    const luca = within(table).getByText("Luca Bianchi").closest("tr")!
    expect(within(luca).getByText("Suspended")).toBeInTheDocument()
    expect(within(luca).getByRole("button", { name: "Resume Luca Bianchi" })).toBeInTheDocument()
    // Agent-style text in a name is plain text.
    const sam = within(table).getByText("Sam <b>Lee</b>").closest("tr")!
    expect(within(sam).getByText("Managed by your identity provider", { selector: "span" })).toBeInTheDocument()
    expect(within(sam).queryByRole("button")).not.toBeInTheDocument()
    expect(within(table).getByText("you")).toBeInTheDocument()
  })

  test("the last Owner cannot be demoted, suspended or removed, and the page says why", async () => {
    asMe(fx.me)
    await renderApp("/settings/people")
    const table = await membersTable()
    const owner = within(table).getByText("Gianmaria").closest("tr")!
    expect(within(owner).getByRole("button", { name: "Remove Gianmaria" })).toBeDisabled()
    expect(within(owner).getByRole("button", { name: "Suspend Gianmaria" })).toBeDisabled()
    expect(within(owner).getByRole("button", { name: "Change role of Gianmaria" })).toBeDisabled()
    expect(within(owner).getByText("The last Owner cannot be removed.")).toBeInTheDocument()
    expect(within(owner).getByText("The last Owner cannot be demoted.")).toBeInTheDocument()
  })

  test("an Admin cannot act on an Owner and the buttons say why", async () => {
    asAdmin()
    const olga = { ...fx.members[0]!, user_id: "018f0000-0000-7000-8000-000000000399", name: "Olga" }
    server.use(http.get("/api/v1/members", () => HttpResponse.json([...fx.members, olga])))
    await renderApp("/settings/people")
    const table = await membersTable()
    const row = (await within(table).findByText("Olga")).closest("tr")!
    for (const name of ["Change role of Olga", "Suspend Olga", "Remove Olga"]) expect(within(row).getByRole("button", { name })).toBeDisabled()
    expect(within(row).getByText(/Only someone who holds everything owner does can change Olga \(you lack /)).toBeInTheDocument()
    // Members the Admin may act on keep their buttons.
    expect(within(within(table).getByText("Marta Rossi").closest("tr")!).getByRole("button", { name: "Remove Marta Rossi" })).toBeEnabled()
  })

  test("changes a role: the new one is given first, then the old one is taken", async () => {
    asMe(fx.me)
    const calls: string[] = []
    server.use(
      http.post("/api/v1/role-bindings", async ({ request }) => {
        calls.push(`give ${JSON.stringify(await request.json())}`)
        return HttpResponse.json(fx.roleBindingCreate, { status: 201 })
      }),
      http.delete("/api/v1/role-bindings/:id", ({ params }) => {
        calls.push(`take ${String(params.id)}`)
        return HttpResponse.json(fx.roleBindingDelete)
      }),
    )
    const { user } = await renderApp("/settings/people")
    await user.click(await screen.findByRole("button", { name: "Change role of Marta Rossi" }))
    const dialog = await screen.findByRole("dialog")
    await within(dialog).findByRole("option", { name: "admin" })
    expect(within(dialog).getByLabelText("Role")).toHaveValue("member")
    await user.selectOptions(within(dialog).getByLabelText("Role"), "admin")
    await user.click(within(dialog).getByRole("button", { name: "Change role" }))
    await waitFor(() => expect(calls).toHaveLength(2))
    expect(calls).toEqual([`give {"subject_kind":"user","subject":"${fx.members[1]!.user_id}","role":"admin"}`, `take ${fx.roleBindings[3]!.id}`])
  })

  test("when the old role cannot be taken the new one is taken back and the reason shown", async () => {
    asMe(fx.me)
    const calls: string[] = []
    server.use(
      http.post("/api/v1/role-bindings", () => {
        calls.push("give")
        return HttpResponse.json(fx.roleBindingCreate, { status: 201 })
      }),
      http.delete("/api/v1/role-bindings/:id", ({ params }) => {
        calls.push(`take ${String(params.id)}`)
        return String(params.id) === fx.roleBindingCreate.id ? HttpResponse.json(fx.roleBindingDelete) : errors.lastOwner()
      }),
    )
    const { user } = await renderApp("/settings/people")
    await user.click(await screen.findByRole("button", { name: "Change role of Marta Rossi" }))
    const dialog = await screen.findByRole("dialog")
    await within(dialog).findByRole("option", { name: "admin" })
    await user.selectOptions(within(dialog).getByLabelText("Role"), "viewer")
    await user.click(within(dialog).getByRole("button", { name: "Change role" }))
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("must keep an active owner")
    expect(calls).toEqual(["give", `take ${fx.roleBindings[3]!.id}`, `take ${fx.roleBindingCreate.id}`])
  })

  test("suspends after confirming, resumes at once and removes after confirming", async () => {
    asAdmin()
    const calls: string[] = []
    server.use(
      http.post("/api/v1/members/:id/suspend", () => {
        calls.push("suspend")
        return HttpResponse.json(fx.memberSuspend)
      }),
      http.post("/api/v1/members/:id/resume", () => {
        calls.push("resume")
        return HttpResponse.json(fx.members[2])
      }),
      http.delete("/api/v1/members/:id", () => {
        calls.push("remove")
        return HttpResponse.json(fx.members[1])
      }),
    )
    const { user } = await renderApp("/settings/people")
    await user.click(await screen.findByRole("button", { name: "Suspend Marta Rossi" }))
    await user.click(await screen.findByRole("button", { name: "Suspend Marta Rossi", description: undefined }).then(() => within(screen.getByRole("dialog")).getByRole("button", { name: "Suspend Marta Rossi" })))
    await waitFor(() => expect(calls).toEqual(["suspend"]))
    await user.click(screen.getByRole("button", { name: "Resume Luca Bianchi" }))
    await waitFor(() => expect(calls).toEqual(["suspend", "resume"]))
    await user.click(screen.getByRole("button", { name: "Remove Marta Rossi" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Remove Marta Rossi" }))
    await waitFor(() => expect(calls).toEqual(["suspend", "resume", "remove"]))
  })

  test("a refused suspend shows the server's message", async () => {
    asAdmin()
    server.use(http.post("/api/v1/members/:id/resume", errors.lastOwner))
    const { user } = await renderApp("/settings/people")
    await user.click(await screen.findByRole("button", { name: "Resume Luca Bianchi" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("must keep an active owner")
  })

  test("a Viewer sees names without emails or actions, and Invite names the permission", async () => {
    asViewer()
    server.use(http.get("/api/v1/members", () => HttpResponse.json(fx.members.map(({ email: _email, ...m }) => m))))
    await renderApp("/settings/people")
    const table = await membersTable()
    expect(within(table).getByText("Marta Rossi")).toBeInTheDocument()
    expect(screen.queryByText("marta@acme.test")).not.toBeInTheDocument()
    expect(within(table).queryByRole("button")).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Invite" })).toBeDisabled()
    expect(screen.getByText("member.manage")).toBeInTheDocument()
    expect(screen.queryByRole("heading", { name: "Invitations" })).not.toBeInTheDocument()
  })

  test("only you: an empty state with the way to invite", async () => {
    asAdmin()
    server.use(http.get("/api/v1/members", () => HttpResponse.json([fx.members[0]])))
    await renderApp("/settings/people")
    expect(await screen.findByText(/It's just you so far/)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Invite" })).toBeEnabled()
  })

  test("invites by email with roles and expiry, and shows the link once with Copy", async () => {
    asAdmin()
    let body: unknown
    server.use(
      http.post("/api/v1/invitations", async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(fx.invitationCreate, { status: 201 })
      }),
    )
    const { user } = await renderApp("/settings/people")
    await user.click(await screen.findByRole("button", { name: "Invite" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Create invitation" }))
    expect(within(dialog).getByText("Enter an email address.")).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Email address"), "new@acme.test")
    await user.selectOptions(within(dialog).getByLabelText("Role 1"), "organization:member")
    await user.click(within(dialog).getByRole("button", { name: "Add another role" }))
    await user.selectOptions(within(dialog).getByLabelText("Role 2"), "project:maintainer")
    await user.selectOptions(within(dialog).getByLabelText("Project for role 2"), "app")
    await user.selectOptions(within(dialog).getByLabelText("The link works for"), "720h")
    await user.click(within(dialog).getByRole("button", { name: "Remove role 1" }))
    await user.click(within(dialog).getByRole("button", { name: "Create invitation" }))
    const card = await screen.findByRole("region", { name: "Invitation for new@acme.test" })
    expect(within(card).getByLabelText("Invitation link")).toHaveValue(fx.invitationCreate.url)
    expect(body).toEqual({ email: "new@acme.test", roles: [{ role: "maintainer", project: "app" }], ttl: "720h" })
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true })
    await user.click(within(card).getByRole("button", { name: "Copy" }))
    expect(writeText).toHaveBeenCalledWith(fx.invitationCreate.url)
    expect(await within(card).findByText("Copied")).toBeInTheDocument()
    await user.click(within(card).getByRole("button", { name: "Done" }))
    expect(screen.queryByLabelText("Invitation link")).not.toBeInTheDocument()
  })

  test("invites by GitHub login, and a refused invitation says why", async () => {
    asAdmin()
    let body: unknown
    server.use(
      http.post("/api/v1/invitations", async ({ request }) => {
        body = await request.json()
        return errors.invalidInput()
      }),
    )
    const { user } = await renderApp("/settings/people")
    await user.click(await screen.findByRole("button", { name: "Invite" }))
    const dialog = await screen.findByRole("dialog")
    await user.selectOptions(within(dialog).getByLabelText("Invite by"), "github_login")
    await user.type(within(dialog).getByLabelText("GitHub login"), "octo")
    await user.click(within(dialog).getByRole("button", { name: "Create invitation" }))
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("feedback is required")
    expect(body).toMatchObject({ github_login: "octo" })
  })

  test("lists invitations with their state and revokes a pending one", async () => {
    asAdmin()
    const revoked: string[] = []
    server.use(
      http.post("/api/v1/invitations/:id/revoke", ({ params }) => {
        revoked.push(String(params.id))
        return HttpResponse.json(fx.invitationRevoke)
      }),
    )
    const { user } = await renderApp("/settings/people")
    const heading = await screen.findByRole("heading", { name: "Invitations" })
    const table = within(heading.closest("section")!).getByRole("table")
    expect(within(table.querySelectorAll("tr")[1] as HTMLElement).getByText("Pending")).toBeInTheDocument()
    expect(within(table).getByText("Expired")).toBeInTheDocument()
    expect(within(table).getByText("Revoked")).toBeInTheDocument()
    expect(within(table).getByText("@octo")).toBeInTheDocument()
    await user.click(within(table).getByRole("button", { name: "Revoke the invitation of new@acme.test" }))
    await waitFor(() => expect(revoked).toEqual([fx.invitations[0]!.id]))
    expect(within(table).getAllByRole("button")).toHaveLength(1)
  })
})

describe("teams", () => {
  test("shows members and access of a team, and marks teams from the identity provider", async () => {
    asAdmin()
    await renderApp("/settings/people?tab=teams")
    const mobile = await screen.findByRole("region", { name: "Team Mobile" })
    expect(await within(mobile).findByText("Marta Rossi")).toBeInTheDocument()
    expect(within(mobile).getByText("member on the organization")).toBeInTheDocument()
    expect(within(mobile).getByText("contributor on app")).toBeInTheDocument()
    const platform = screen.getByRole("region", { name: "Team Platform" })
    expect(within(platform).getByText("Managed by your identity provider")).toBeInTheDocument()
    expect(within(platform).queryByRole("button")).not.toBeInTheDocument()
  })

  test("creates a team, adds and removes members and roles, and deletes the team", async () => {
    asAdmin()
    const calls: string[] = []
    server.use(
      http.post("/api/v1/teams", async ({ request }) => {
        calls.push(`create ${JSON.stringify(await request.json())}`)
        return HttpResponse.json(fx.teamCreate, { status: 201 })
      }),
      http.post("/api/v1/teams/:slug/members", async ({ request }) => {
        calls.push(`add ${JSON.stringify(await request.json())}`)
        return HttpResponse.json(fx.teamMembers[0], { status: 201 })
      }),
      http.delete("/api/v1/teams/:slug/members/:user", ({ params }) => {
        calls.push(`drop ${String(params.user)}`)
        return HttpResponse.json(fx.teamMembers[0])
      }),
      http.post("/api/v1/role-bindings", async ({ request }) => {
        calls.push(`bind ${JSON.stringify(await request.json())}`)
        return HttpResponse.json(fx.roleBindingCreate, { status: 201 })
      }),
      http.delete("/api/v1/role-bindings/:id", ({ params }) => {
        calls.push(`unbind ${String(params.id)}`)
        return HttpResponse.json(fx.roleBindingDelete)
      }),
      http.delete("/api/v1/teams/:slug", ({ params }) => {
        calls.push(`delete ${String(params.slug)}`)
        return HttpResponse.json(fx.teams[0])
      }),
    )
    const { user } = await renderApp("/settings/people?tab=teams")
    await user.click(await screen.findByRole("button", { name: "New team" }))
    let dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Create team" }))
    expect(within(dialog).getByText("Give the team a name.")).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Team name"), "Mobile")
    await user.click(within(dialog).getByRole("button", { name: "Create team" }))
    await waitFor(() => expect(calls).toContain('create {"name":"Mobile"}'))

    const mobile = await screen.findByRole("region", { name: "Team Mobile" })
    await within(mobile).findByText("Marta Rossi")
    const add = within(mobile).getByRole("form", { name: "Add a member to Mobile" })
    await user.selectOptions(within(add).getByLabelText("Person"), fx.members[0]!.user_id)
    await user.click(within(add).getByRole("button", { name: "Add to team" }))
    await user.click(within(mobile).getByRole("button", { name: "Remove Marta Rossi from Mobile" }))
    const bind = within(mobile).getByRole("form", { name: "Add a role to Mobile" })
    await user.selectOptions(within(bind).getByLabelText("Role"), "project:viewer")
    await user.click(within(bind).getByRole("button", { name: "Add role" }))
    await user.click(within(mobile).getByRole("button", { name: "Remove contributor on app from Mobile" }))
    await user.click(within(mobile).getByRole("button", { name: "Delete team Mobile" }))
    dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Delete Mobile" }))
    await waitFor(() => expect(calls).toHaveLength(6))
    expect(calls.slice(1)).toEqual([
      `add {"user":"${fx.members[0]!.user_id}"}`,
      `drop ${fx.members[1]!.user_id}`,
      'bind {"subject_kind":"team","subject":"mobile","role":"viewer"}',
      `unbind ${fx.roleBindings[2]!.id}`,
      "delete mobile",
    ])
  })

  test("a Viewer reads teams and gets no controls, only a disabled New team", async () => {
    asViewer()
    await renderApp("/settings/people?tab=teams")
    const mobile = await screen.findByRole("region", { name: "Team Mobile" })
    expect(within(mobile).queryByRole("button")).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: "New team" })).toBeDisabled()
    expect(screen.getByText("team.manage")).toBeInTheDocument()
  })

  test("says what teams are when there are none", async () => {
    asAdmin()
    server.use(http.get("/api/v1/teams", () => HttpResponse.json(fx.teamsEmpty)))
    await renderApp("/settings/people?tab=teams")
    expect(await screen.findByText(/Teams give several people the same access/)).toBeInTheDocument()
  })
})

describe("roles", () => {
  test("lists built-in roles read-only with capabilities by area", async () => {
    asViewer()
    await renderApp("/settings/people?tab=roles")
    const owner = await screen.findByRole("region", { name: "Role owner" })
    expect(within(owner).getByText("Built in")).toBeInTheDocument()
    expect(within(owner).getByText("People")).toBeInTheDocument()
    expect(within(owner).getByText(/member\.manage/)).toBeInTheDocument()
    expect(screen.getByRole("region", { name: "Role maintainer" })).toBeInTheDocument()
    expect(screen.getByRole("heading", { name: "On a project" })).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /edit|delete|save|add/i })).not.toBeInTheDocument()
    expect(screen.getByText(/Custom roles appear here when the enterprise build provides them/)).toBeInTheDocument()
  })

  test("a custom role is marked", async () => {
    asAdmin()
    server.use(http.get("/api/v1/roles", () => HttpResponse.json([{ name: "qa", scope: "project", permissions: ["bogus.thing"], built_in: false, custom: true }])))
    await renderApp("/settings/people?tab=roles")
    const qa = await screen.findByRole("region", { name: "Role qa" })
    expect(within(qa).getByText("Custom")).toBeInTheDocument()
    expect(within(qa).getByText("Other")).toBeInTheDocument()
  })
})

describe("custom roles (enterprise)", () => {
  const asOwner = () => asMe(fx.meOwnerEnterprise)
  const withCustom = () => server.use(http.get("/api/v1/roles", () => HttpResponse.json(fx.rolesCustom)))

  test("creates a role from the catalog of its scope", async () => {
    asOwner()
    withCustom()
    let body: unknown
    server.use(
      http.post("/api/v1/roles", async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(fx.roleCreate, { status: 201 })
      }),
    )
    const { user } = await renderApp("/settings/people?tab=roles")
    await user.click(await screen.findByRole("button", { name: "New custom role" }))
    const dialog = await screen.findByRole("dialog", { name: "New custom role" })
    // A project role offers project permissions only.
    expect(within(dialog).queryByRole("checkbox", { name: "billing.manage" })).not.toBeInTheDocument()
    await user.click(within(dialog).getByRole("button", { name: "Create role" }))
    expect(within(dialog).getByText(/Use lowercase letters/)).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Role name"), "release-manager")
    await user.click(within(dialog).getByRole("button", { name: "Create role" }))
    expect(within(dialog).getByText("Choose at least one permission.")).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Description"), "Merges and cancels")
    await user.click(within(dialog).getByRole("checkbox", { name: "job.cancel" }))
    await user.click(within(dialog).getByRole("checkbox", { name: "gate.approve.merge" }))
    await user.click(within(dialog).getByRole("button", { name: "Create role" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    expect(body).toEqual({ name: "release-manager", scope: "project", description: "Merges and cancels", permissions: ["gate.approve.merge", "job.cancel"] })
  })

  test("an organization role offers organization permissions, and switching back drops them", async () => {
    asOwner()
    const { user } = await renderApp("/settings/people?tab=roles")
    await user.click(await screen.findByRole("button", { name: "New custom role" }))
    const dialog = await screen.findByRole("dialog", { name: "New custom role" })
    await user.selectOptions(within(dialog).getByLabelText("Where it applies"), "organization")
    await user.click(within(dialog).getByRole("checkbox", { name: "billing.manage" }))
    await user.selectOptions(within(dialog).getByLabelText("Where it applies"), "project")
    expect(within(dialog).queryByRole("checkbox", { name: "billing.manage" })).not.toBeInTheDocument()
  })

  test("edits a custom role and shows what the server refuses", async () => {
    asOwner()
    withCustom()
    let body: unknown
    server.use(
      http.patch("/api/v1/roles/:role", async ({ request, params }) => {
        body = { role: params.role, ...((await request.json()) as object) }
        return HttpResponse.json(fx.roleUpdate)
      }),
      http.delete("/api/v1/roles/:role", () => HttpResponse.json(fx.roleDeleteInUse, { status: 409 })),
    )
    const { user } = await renderApp("/settings/people?tab=roles")
    const card = await screen.findByRole("region", { name: "Role release-manager" })
    expect(within(card).getByText("Custom")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Edit role owner" })).not.toBeInTheDocument()
    await user.click(within(card).getByRole("button", { name: "Edit role release-manager" }))
    const dialog = await screen.findByRole("dialog", { name: "Edit release-manager" })
    expect(within(dialog).queryByLabelText("Where it applies")).not.toBeInTheDocument()
    await user.click(within(dialog).getByRole("checkbox", { name: "job.cancel" }))
    await user.click(within(dialog).getByRole("button", { name: "Save role" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    expect(body).toEqual({ role: fx.rolesCustom.at(-1)!.id, name: "release-manager", description: "Merges and cancels", permissions: ["gate.approve.merge"] })

    await user.click(within(card).getByRole("button", { name: "Delete role release-manager" }))
    await user.click(await screen.findByRole("button", { name: "Delete release-manager" }))
    expect(await screen.findByText(/used by connection okta/)).toBeInTheDocument()
  })

  test("without role.manage the editor is named, not offered", async () => {
    asMe({ ...fx.meViewer, enterprise: true })
    withCustom()
    await renderApp("/settings/people?tab=roles")
    const card = await screen.findByRole("region", { name: "Role release-manager" })
    expect(within(card).queryByRole("button", { name: /Edit|Delete/ })).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: /New custom role/ })).toBeDisabled()
    expect(screen.getByText(/Custom roles hold permissions you choose/)).toBeInTheDocument()
  })
})

describe("service accounts", () => {
  test("lists accounts with their tokens, scopes and expiry and creates a token shown once", async () => {
    asAdmin()
    server.use(http.get("/api/v1/tokens", () => HttpResponse.json(fx.tokensScopes)))
    let body: unknown
    server.use(
      http.post("/api/v1/tokens", async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(fx.tokenCreate, { status: 201 })
      }),
    )
    const { user } = await renderApp("/settings/people?tab=service-accounts")
    const ci = await screen.findByRole("region", { name: "Service account ci" })
    expect(await within(ci).findByText("ci-box")).toBeInTheDocument()
    expect(within(ci).getByText("admin")).toBeInTheDocument()
    expect(within(ci).getByText("revoked")).toBeInTheDocument()
    expect(within(ci).queryByRole("button", { name: "Revoke ci-box" })).not.toBeInTheDocument()
    const old = screen.getByRole("region", { name: "Service account old-bot" })
    expect(within(old).getByText("Disabled")).toBeInTheDocument()
    expect(within(old).queryByRole("form")).not.toBeInTheDocument() // no token for a disabled account
    await user.type(within(ci).getByLabelText("Token name"), "deploy")
    await user.selectOptions(within(ci).getByLabelText("Scopes"), "write")
    await user.selectOptions(within(ci).getByLabelText("Expires after"), "168h")
    await user.click(within(ci).getByRole("button", { name: "Create token" }))
    const secret = await screen.findByLabelText("Token")
    expect(secret).toHaveValue(fx.tokenCreate.secret)
    expect(body).toEqual({ name: "deploy", scopes: ["write"], ttl: "168h", service_account: "ci" })
  })

  test("creates, disables and enables service accounts", async () => {
    asAdmin()
    const calls: string[] = []
    server.use(
      http.post("/api/v1/service-accounts", async ({ request }) => {
        calls.push(`create ${JSON.stringify(await request.json())}`)
        return HttpResponse.json(fx.serviceAccountCreate, { status: 201 })
      }),
      http.post("/api/v1/service-accounts/:name/:action", ({ params }) => {
        calls.push(`${String(params.action)} ${String(params.name)}`)
        return HttpResponse.json(fx.serviceAccounts[0])
      }),
    )
    const { user } = await renderApp("/settings/people?tab=service-accounts")
    await user.click(await screen.findByRole("button", { name: "Add service account" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Add service account" }))
    expect(within(dialog).getByText(/Use lowercase letters/)).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Name"), "ci")
    await user.type(within(dialog).getByLabelText("What it is for"), "Builds")
    await user.click(within(dialog).getByRole("button", { name: "Add service account" }))
    await user.click(await screen.findByRole("button", { name: "Disable ci" }))
    await user.click(screen.getByRole("button", { name: "Enable old-bot" }))
    await waitFor(() => expect(calls).toEqual(['create {"name":"ci","description":"Builds"}', "disable ci", "enable old-bot"]))
  })

  test("a Viewer reads the accounts and cannot add one", async () => {
    asViewer()
    await renderApp("/settings/people?tab=service-accounts")
    const ci = await screen.findByRole("region", { name: "Service account ci" })
    expect(within(ci).queryByRole("button")).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Add service account" })).toBeDisabled()
    expect(screen.getByText("service_account.manage")).toBeInTheDocument()
  })

  test("an empty list explains service accounts", async () => {
    asAdmin()
    server.use(http.get("/api/v1/service-accounts", () => HttpResponse.json([])))
    await renderApp("/settings/people?tab=service-accounts")
    expect(await screen.findByText(/Service accounts run CI and scripts without a person/)).toBeInTheDocument()
  })
})

describe("sign-in", () => {
  test("lists the server's connections as managed by the server file, and the organization's own", async () => {
    asAdmin()
    await renderApp("/settings/sign-in")
    const table = await screen.findByRole("table")
    const github = within(table).getByText("github", { selector: "div" }).closest("tr")!
    expect(within(github).getByText("Managed by server file")).toBeInTheDocument()
    expect(within(github).getByText("All organizations on this server")).toBeInTheDocument()
    const okta = within(table).getByText("okta").closest("tr")!
    expect(within(okta).getByText("On")).toBeInTheDocument()
    expect(within(okta).getByText("Tested")).toBeInTheDocument()
    expect(within(okta).getByText("https://acme.okta.com")).toBeInTheDocument()
    expect(within(okta).getByRole("link", { name: "Test sign-in with okta" })).toHaveAttribute("href", expect.stringContaining("?test=1"))
    const google = within(table).getByText("google", { selector: "div" }).closest("tr")!
    expect(within(google).getByText("Not tested")).toBeInTheDocument()
  })

  test("Turn on waits for a test sign-in, and Turn off needs none", async () => {
    asAdmin()
    const calls: string[] = []
    server.use(
      http.post("/api/v1/connections/:name/:action", ({ params }) => {
        calls.push(`${String(params.action)} ${String(params.name)}`)
        return HttpResponse.json(fx.connections[1])
      }),
    )
    const { user } = await renderApp("/settings/sign-in")
    const google = (await screen.findByText("google", { selector: "div" })).closest("tr")!
    expect(within(google).getByRole("button", { name: "Turn on google" })).toBeDisabled()
    expect(within(google).getByText("Test the sign-in first.")).toBeInTheDocument()
    await user.click(within(screen.getByText("okta").closest("tr")!).getByRole("button", { name: "Turn off okta" }))
    await waitFor(() => expect(calls).toEqual(["disable okta"]))
  })

  test("a tested connection is turned on, and the server's refusal is shown", async () => {
    asAdmin()
    server.use(
      http.get("/api/v1/connections", () => HttpResponse.json(fx.connections.map((c) => (c.name === "google" ? { ...c, tested: true } : c)))),
      http.post("/api/v1/connections/:name/enable", errors.untestedConnection),
    )
    const { user } = await renderApp("/settings/sign-in")
    await user.click(await screen.findByRole("button", { name: "Turn on google" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("no test sign-in that worked")
  })

  test("adds a connection through a plan with a secure field, and says where to allow the redirect", async () => {
    asAdmin()
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.post("/api/v1/config/plans", async ({ request }) => {
        calls.push({ path: "plan", body: await request.json() })
        return HttpResponse.json(fx.configPlanConnection, { status: 201 })
      }),
      http.put("/api/v1/config/plans/:plan/secrets/:secret", async ({ request, params }) => {
        calls.push({ path: `secret ${String(params.secret)}`, body: await request.json() })
        return HttpResponse.json(fx.configPlanSecret)
      }),
      http.post("/api/v1/config/plans/:plan/apply", () => {
        calls.push({ path: "apply", body: undefined })
        return HttpResponse.json(fx.configPlanApply)
      }),
    )
    const { user } = await renderApp("/settings/sign-in")
    await user.click(await screen.findByRole("button", { name: "Add company sign-in" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Add sign-in" }))
    for (const text of [/Use lowercase letters/, /https address of the issuer/, /client ID from your provider/, /Paste the client secret/]) {
      expect(within(dialog).getByText(text)).toBeInTheDocument()
    }
    await user.type(within(dialog).getByLabelText("Name"), "okta")
    await user.type(within(dialog).getByLabelText("Issuer"), "https://acme.okta.com")
    await user.type(within(dialog).getByLabelText("Client ID"), "0oa1")
    const secret = within(dialog).getByLabelText("Client secret")
    expect(secret).toHaveAttribute("type", "password") // a secure field
    await user.type(secret, "the-client-secret")
    await user.click(within(dialog).getByRole("button", { name: "Add sign-in" }))
    const status = await screen.findByRole("status")
    expect(status).toHaveTextContent("Allow this redirect address")
    // The plan asks for a connection that starts off, with a stored secret; the secret goes only to its own call.
    expect(calls.map((c) => c.path)).toEqual(["plan", "secret conn-okta", "apply"])
    expect(calls[0]!.body).toEqual({
      title: "Add sign-in okta", source: "form",
      changes: [{ kind: "connection", name: "okta", spec: { kind: "oidc", client_id: "0oa1", client_secret_ref: "store:conn-okta", jit: { enabled: false }, enabled: false, issuer: "https://acme.okta.com" } }],
    })
    expect(calls[1]!.body).toEqual({ value: "the-client-secret" })
    expect(JSON.stringify(calls[0]!.body)).not.toContain("the-client-secret")
    expect(document.body.textContent).not.toContain("the-client-secret")
  })

  test("GitHub and Google need no issuer, and a refused plan is canceled and shown", async () => {
    asAdmin()
    let canceled = false
    server.use(
      http.put("/api/v1/config/plans/:plan/secrets/:secret", errors.conflict),
      http.post("/api/v1/config/plans/:plan/cancel", () => {
        canceled = true
        return HttpResponse.json(fx.configPlanCancel)
      }),
    )
    const { user } = await renderApp("/settings/sign-in")
    await user.click(await screen.findByRole("button", { name: "Add company sign-in" }))
    const dialog = await screen.findByRole("dialog")
    await user.selectOptions(within(dialog).getByLabelText("Provider"), "google")
    expect(within(dialog).queryByLabelText("Issuer")).not.toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Name"), "google")
    await user.type(within(dialog).getByLabelText("Client ID"), "123.apps")
    await user.type(within(dialog).getByLabelText("Client secret"), "s3cret")
    await user.click(within(dialog).getByRole("button", { name: "Add sign-in" }))
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("already approved")
    expect(canceled).toBe(true)
  })

  test("closing the dialog forgets what was typed, the client secret included", async () => {
    asAdmin()
    const { user } = await renderApp("/settings/sign-in")
    await user.click(await screen.findByRole("button", { name: "Add company sign-in" }))
    let dialog = await screen.findByRole("dialog")
    await user.type(within(dialog).getByLabelText("Name"), "okta")
    await user.type(within(dialog).getByLabelText("Issuer"), "https://acme.okta.com")
    await user.type(within(dialog).getByLabelText("Client ID"), "0oa1")
    await user.type(within(dialog).getByLabelText("Client secret"), "the-client-secret")
    await user.keyboard("{Escape}")
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    await user.click(screen.getByRole("button", { name: "Add company sign-in" }))
    dialog = await screen.findByRole("dialog")
    for (const label of ["Name", "Issuer", "Client ID", "Client secret"]) expect(within(dialog).getByLabelText(label)).toHaveValue("")
    expect(within(dialog).queryByText(/Use lowercase letters/)).not.toBeInTheDocument()
  })

  test("a plan the server refuses outright shows its message", async () => {
    asAdmin()
    server.use(http.post("/api/v1/config/plans", errors.invalidInput))
    const { user } = await renderApp("/settings/sign-in")
    await user.click(await screen.findByRole("button", { name: "Add company sign-in" }))
    const dialog = await screen.findByRole("dialog")
    await user.selectOptions(within(dialog).getByLabelText("Provider"), "github")
    await user.type(within(dialog).getByLabelText("Name"), "gh")
    await user.type(within(dialog).getByLabelText("Client ID"), "Iv1")
    await user.type(within(dialog).getByLabelText("Client secret"), "s")
    await user.click(within(dialog).getByRole("button", { name: "Add sign-in" }))
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("feedback is required")
  })
})

describe("change history", () => {
  test("lists changes with who and when, and opens the details as plain text", async () => {
    asAdmin()
    const { user } = await renderApp("/settings/history?tab=activity")
    const table = await screen.findByRole("table")
    expect(within(table).getByText("Member suspended")).toBeInTheDocument()
    expect(within(table).getAllByText("Gianmaria").length).toBeGreaterThan(0)
    expect(within(table).getByText("github:octo")).toBeInTheDocument()
    await user.click(within(table).getByRole("button", { name: /Details of Team created/ }))
    const sheet = await screen.findByRole("dialog")
    // Whatever was recorded is text, never markup.
    expect(within(sheet).getByText("<img src=x onerror=alert(1)>")).toBeInTheDocument()
    expect(sheet.querySelector("img")).toBeNull()
    expect(within(sheet).getByText(/service_account:/)).toBeInTheDocument()
  })

  test("filters by person, action, what it was done to and dates", async () => {
    asAdmin()
    const queries: string[] = []
    server.use(
      http.get("/api/v1/audit", ({ request }) => {
        queries.push(new URL(request.url).search)
        return HttpResponse.json(fx.audit)
      }),
    )
    const { user } = await renderApp("/settings/history?tab=activity")
    await screen.findByRole("table")
    await user.selectOptions(screen.getByLabelText("Who"), `user:${fx.members[0]!.user_id}`)
    await user.type(screen.getByLabelText("Action"), "member.removed")
    await user.type(screen.getByLabelText("Done to"), "user")
    await user.type(screen.getByLabelText("From"), "2026-02-01")
    await user.type(screen.getByLabelText("To"), "2026-03-01")
    await waitFor(() => {
      const q = new URLSearchParams(queries.at(-1))
      expect(q.get("actor")).toBe(`user:${fx.members[0]!.user_id}`)
      expect(q.get("action")).toBe("member.removed")
      expect(q.get("subject_type")).toBe("user")
      expect(q.get("since")).toBe("2026-02-01T00:00:00Z")
      expect(q.get("until")).toBe("2026-03-01T23:59:59Z")
    })
    await user.click(screen.getByRole("button", { name: "Clear filters" }))
    await waitFor(() => expect(new URLSearchParams(queries.at(-1)).get("actor")).toBeNull())
  })

  test("says when nothing changed yet", async () => {
    asAdmin()
    server.use(http.get("/api/v1/audit", () => HttpResponse.json(fx.auditEmpty)))
    await renderApp("/settings/history?tab=activity")
    expect(await screen.findByText(/No changes yet/)).toBeInTheDocument()
  })
})

describe("tokens of a service account", () => {
  test("revokes one after confirming", async () => {
    asAdmin()
    const revoked: string[] = []
    server.use(
      http.get("/api/v1/tokens", () => HttpResponse.json(fx.tokensScopes)),
      http.post("/api/v1/tokens/:id/revoke", ({ params }) => {
        revoked.push(String(params.id))
        return HttpResponse.json(fx.tokenRevoke)
      }),
    )
    const { user } = await renderApp("/settings/people?tab=service-accounts")
    const ci = await screen.findByRole("region", { name: "Service account ci" })
    await user.click(await within(ci).findByRole("button", { name: "Revoke deploy" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Revoke deploy" }))
    await waitFor(() => expect(revoked).toEqual([fx.tokensScopes.find((t) => t.name === "deploy")!.id]))
  })
})
