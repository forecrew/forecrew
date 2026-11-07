import { useState } from "react"
import { api } from "@/api/client"
import { keys, useBindings, useChange, useInvitations, useMe, useMembers, useProjects, useRevokeInvitation, useRoles } from "@/api/queries"
import type { Invitation, Member, Role } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { ConfirmDialog } from "@/components/ConfirmDialog"
import { Field } from "@/components/Field"
import { ShowOnce } from "@/components/ShowOnce"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { PermButton, useCan } from "@/lib/permissions"

const statusTone = { active: "good", invited: "neutral", suspended: "attention" } as const
const EXPIRIES = [
  { value: "24h", label: "1 day" },
  { value: "168h", label: "7 days" },
  { value: "720h", label: "30 days" },
]

/** Who holds what, and through what: direct roles, and the teams. */
function Access({ m }: { m: Member }) {
  const projects = m.roles.filter((r) => r.project)
  if (projects.length === 0 && m.teams.length === 0) return <span className="text-muted-foreground">—</span>
  return (
    <ul className="space-y-0.5 text-sm">
      {projects.map((r) => (
        <li key={`${r.role}@${r.project}`}>
          {r.role} on <span className="font-mono">{r.project}</span> <span className="text-muted-foreground">direct</span>
        </li>
      ))}
      {m.teams.map((t) => (
        <li key={t}>
          <span className="text-muted-foreground">through team</span> <span className="font-mono">{t}</span>
        </li>
      ))}
    </ul>
  )
}

/**
 * Why the caller may not act on a member: someone who holds roles the caller
 * does not fully hold (an Admin and an Owner) is out of reach (No escalation).
 */
export function outOfReach(m: Member, roles: Role[], mine: string[]): string | undefined {
  for (const held of m.roles) {
    if (held.scope !== "organization") continue
    const lacking = roles.find((r) => r.scope === "organization" && r.name === held.role)?.permissions.find((p) => !mine.includes(p))
    if (lacking) return `Only someone who holds everything ${held.role} does can change ${m.name} (you lack ${lacking}).`
  }
  return undefined
}

/** Changes the organization role of a member: the new role is given first, then the old ones are taken. */
function ChangeRole({ m, lastOwner }: { m: Member; lastOwner: boolean }) {
  const roles = useRoles()
  const bindings = useBindings()
  const [open, setOpen] = useState(false)
  const [role, setRole] = useState("")
  const mine = (bindings.data ?? []).filter((b) => b.subject_kind === "user" && b.subject_id === m.user_id && !b.project && b.role_scope === "organization")
  const options = (roles.data ?? []).filter((r) => r.scope === "organization")
  const chosen = role || m.roles.find((r) => r.scope === "organization")?.role || options[0]?.name || ""
  const change = useChange(
    async (name: string) => {
      const given = mine.some((b) => b.role === name) ? undefined : await api.createBinding({ subject_kind: "user", subject: m.user_id, role: name })
      try {
        for (const b of mine.filter((x) => x.role !== name)) await api.deleteBinding(b.id)
      } catch (err) {
        if (given) await api.deleteBinding(given.id).catch(() => {}) // as it was
        throw err
      }
    },
    ["bindings"],
    keys.members,
  )
  if (lastOwner) return <DisabledAction label={`Change role of ${m.name}`} text="Change role" reason="The last Owner cannot be demoted." />
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) change.reset()
      }}
    >
      <DialogTrigger render={<Button variant="outline" aria-label={`Change role of ${m.name}`} />}>Change role</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Change the role of {m.name}</DialogTitle>
          <DialogDescription>The new role replaces their role on the organization. Access on projects, and through teams, stays.</DialogDescription>
        </DialogHeader>
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault()
            change.mutate(chosen, { onSuccess: () => setOpen(false) })
          }}
        >
          <Field label="Role">
            {(p) => (
              <NativeSelect {...p} value={chosen} onChange={(e) => setRole(e.target.value)}>
                {options.map((r) => (
                  <NativeSelectOption key={r.name} value={r.name}>
                    {r.name}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            )}
          </Field>
          {change.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {change.error.message}
            </p>
          ) : null}
          <DialogFooter>
            <Button type="submit" disabled={change.isPending || !chosen}>
              Change role
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function MemberActions({ m, lastOwner, unreachable }: { m: Member; lastOwner: boolean; unreachable?: string }) {
  const suspend = useChange((suspended: boolean) => api.suspendMember(m.user_id, suspended), keys.members)
  const remove = useChange((id: string) => api.removeMember(id), keys.members)
  const whose = `${m.name}`
  if (m.source === "scim") return null // the status column says who manages it
  if (unreachable) {
    return (
      <div className="space-y-1">
        <div className="flex flex-wrap gap-2">
          {[["Change role", `Change role of ${whose}`], [m.status === "suspended" ? "Resume" : "Suspend", `${m.status === "suspended" ? "Resume" : "Suspend"} ${whose}`], ["Remove", `Remove ${whose}`]].map(([text, label]) => (
            <Button key={text} variant="outline" disabled aria-label={label}>
              {text}
            </Button>
          ))}
        </div>
        <p className="text-xs text-muted-foreground">{unreachable}</p>
      </div>
    )
  }
  return (
    <div className="flex flex-wrap items-start gap-2">
      <ChangeRole m={m} lastOwner={lastOwner} />
      {m.status === "suspended" ? (
        <Button variant="outline" aria-label={`Resume ${whose}`} onClick={() => suspend.mutate(false)} disabled={suspend.isPending}>
          Resume
        </Button>
      ) : lastOwner ? (
        <DisabledAction label={`Suspend ${whose}`} text="Suspend" reason="The last Owner cannot be suspended." />
      ) : (
        <ConfirmDialog
          trigger="Suspend"
          triggerAriaLabel={`Suspend ${whose}`}
          title={`Suspend ${whose}?`}
          description="Their sessions end and their personal tokens are revoked now. They can be let back in later, with new tokens."
          confirmLabel={`Suspend ${whose}`}
          keepLabel="Keep active"
          mutation={suspend}
          variables={true}
        />
      )}
      {lastOwner ? (
        <DisabledAction label={`Remove ${whose}`} text="Remove" reason="The last Owner cannot be removed." />
      ) : (
        <ConfirmDialog
          trigger="Remove"
          triggerAriaLabel={`Remove ${whose}`}
          title={`Remove ${whose}?`}
          description="They leave the organization with their teams and roles. Their sessions end and their personal tokens are revoked. This cannot be undone."
          confirmLabel={`Remove ${whose}`}
          keepLabel="Keep member"
          mutation={remove}
          variables={m.user_id}
        />
      )}
      {suspend.isError ? (
        <p role="alert" className="w-full text-xs text-destructive">
          {suspend.error.message}
        </p>
      ) : null}
    </div>
  )
}

/** A disabled action that says why. */
function DisabledAction({ label, text, reason }: { label: string; text: string; reason: string }) {
  return (
    <span className="inline-flex flex-col items-start gap-0.5" title={reason}>
      <Button variant="outline" disabled aria-label={label}>
        {text}
      </Button>
      <span className="text-xs text-muted-foreground">{reason}</span>
    </span>
  )
}

interface RoleRow {
  role: string
  project: string
}

/** Invites by email or GitHub login, with roles and an expiry. The link is shown once. */
function InviteDialog({ onCreated }: { onCreated: (url: string, who: string) => void }) {
  const can = useCan()
  const [open, setOpen] = useState(false)
  const [by, setBy] = useState<"email" | "github_login">("email")
  const [who, setWho] = useState("")
  const [rows, setRows] = useState<RoleRow[]>([{ role: "organization:member", project: "" }])
  const [ttl, setTtl] = useState("168h")
  const [error, setError] = useState("")
  const roles = useRoles(open)
  const projects = useProjects()
  const invite = useChange((v: Parameters<typeof api.createInvitation>[0]) => api.createInvitation(v), keys.invitations)
  // A role name can exist at both scopes (viewer), so a row holds scope and name.
  const scopeOf = (key: string) => (key.startsWith("project:") ? "project" : "organization")

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!who.trim()) {
      setError(by === "email" ? "Enter an email address." : "Enter a GitHub login.")
      return
    }
    setError("")
    invite.mutate(
      { [by]: who.trim(), roles: rows.map((r) => ({ role: r.role.slice(r.role.indexOf(":") + 1), ...(r.project ? { project: r.project } : {}) })), ttl },
      {
        onSuccess: (out) => {
          setOpen(false)
          setWho("")
          setRows([{ role: "organization:member", project: "" }])
          onCreated(out.url, who.trim())
        },
      },
    )
  }
  const setRow = (i: number, patch: Partial<RoleRow>) => setRows((all) => all.map((r, j) => (j === i ? { ...r, ...patch } : r)))

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) invite.reset()
      }}
    >
      {can("member.manage") ? (
        <DialogTrigger render={<Button />}>Invite</DialogTrigger>
      ) : (
        <PermButton permission="member.manage">Invite</PermButton>
      )}
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Invite someone</DialogTitle>
          <DialogDescription>They get the roles below when they sign in through the link. You see the link once.</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-3" noValidate>
          <div className="flex gap-2">
            <Field label="Invite by">
              {(p) => (
                <NativeSelect {...p} value={by} onChange={(e) => setBy(e.target.value as "email" | "github_login")}>
                  <NativeSelectOption value="email">Email</NativeSelectOption>
                  <NativeSelectOption value="github_login">GitHub login</NativeSelectOption>
                </NativeSelect>
              )}
            </Field>
            <div className="flex-1">
              <Field label={by === "email" ? "Email address" : "GitHub login"} error={error}>
                {(p) => <Input {...p} value={who} onChange={(e) => setWho(e.target.value)} />}
              </Field>
            </div>
          </div>
          {rows.map((r, i) => (
            <div key={i} className="flex flex-wrap items-end gap-2">
              <Field label={`Role ${i + 1}`}>
                {(p) => (
                  <NativeSelect {...p} value={r.role} onChange={(e) => setRow(i, { role: e.target.value, project: scopeOf(e.target.value) === "project" ? r.project : "" })}>
                    {(roles.data ?? [{ name: "member", scope: "organization" as const }]).map((x) => (
                      <NativeSelectOption key={`${x.scope}:${x.name}`} value={`${x.scope}:${x.name}`}>
                        {x.name} ({x.scope === "project" ? "project role" : "organization"})
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                )}
              </Field>
              <Field label={`Project for role ${i + 1}`}>
                {(p) => (
                  <NativeSelect {...p} value={r.project} disabled={scopeOf(r.role) !== "project"} onChange={(e) => setRow(i, { project: e.target.value })}>
                    <NativeSelectOption value="">All projects</NativeSelectOption>
                    {(projects.data ?? []).map((x) => (
                      <NativeSelectOption key={x.id} value={x.name}>
                        {x.name}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                )}
              </Field>
              {rows.length > 1 ? (
                <Button type="button" variant="ghost" aria-label={`Remove role ${i + 1}`} onClick={() => setRows((all) => all.filter((_, j) => j !== i))}>
                  Remove
                </Button>
              ) : null}
            </div>
          ))}
          <Button type="button" variant="outline" onClick={() => setRows((all) => [...all, { role: "project:viewer", project: "" }])}>
            Add another role
          </Button>
          <Field label="The link works for">
            {(p) => (
              <NativeSelect {...p} value={ttl} onChange={(e) => setTtl(e.target.value)}>
                {EXPIRIES.map((x) => (
                  <NativeSelectOption key={x.value} value={x.value}>
                    {x.label}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            )}
          </Field>
          {invite.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {invite.error.message}
            </p>
          ) : null}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={invite.isPending}>
              Create invitation
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function invitationState(i: Invitation): { label: string; tone: "good" | "bad" | "neutral" | "attention" } {
  if (i.accepted_at) return { label: "Accepted", tone: "good" }
  if (i.revoked_at) return { label: "Revoked", tone: "bad" }
  if (Date.parse(i.expires_at) <= Date.now()) return { label: "Expired", tone: "attention" }
  return { label: "Pending", tone: "neutral" }
}

function Invitations() {
  const invitations = useInvitations()
  const revoke = useRevokeInvitation()
  return (
    <section aria-labelledby="invitations" className="mt-8 space-y-3">
      <h2 id="invitations" className="text-lg font-semibold">
        Invitations
      </h2>
      <Async query={invitations} isEmpty={(d) => d.length === 0} empty="No invitations yet.">
        {(items) => (
          <TableWrap label="Invitations">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Invited</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Expires</TableHead>
                  <TableHead>Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((i) => {
                  const state = invitationState(i)
                  const who = i.email ?? `@${i.github_login}`
                  return (
                    <TableRow key={i.id}>
                      <TableCell>{who}</TableCell>
                      <TableCell>
                        <StatusBadge tone={state.tone}>{state.label}</StatusBadge>
                      </TableCell>
                      <TableCell>
                        <Age iso={i.expires_at} future />
                      </TableCell>
                      <TableCell>
                        {state.label === "Pending" ? (
                          <Button variant="outline" aria-label={`Revoke the invitation of ${who}`} onClick={() => revoke.mutate(i.id)} disabled={revoke.isPending}>
                            Revoke
                          </Button>
                        ) : null}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </TableWrap>
        )}
      </Async>
    </section>
  )
}

export function MembersTab() {
  const me = useMe().data
  const can = useCan()
  const members = useMembers()
  const roles = useRoles()
  const manage = can("member.manage")
  const [link, setLink] = useState<{ url: string; who: string }>()
  const owners = (members.data ?? []).filter((m) => m.status === "active" && m.roles.some((r) => r.role === "owner" && r.scope === "organization"))

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <InviteDialog onCreated={(url, who) => setLink({ url, who })} />
      </div>
      {link ? <ShowOnce title={`Invitation for ${link.who}`} label="Invitation link" secret={link.url} onDismiss={() => setLink(undefined)} /> : null}
      <Async query={members} isEmpty={(d) => d.length <= 1 && d[0]?.user_id === me?.user?.id} empty="It's just you so far. Invite the people you work with.">
        {(items) => (
          <TableWrap label="Members">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead>Projects</TableHead>
                  <TableHead>Status</TableHead>
                  {manage ? <TableHead>Actions</TableHead> : null}
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((m) => (
                  <TableRow key={m.user_id}>
                    <TableCell>
                      <div className="font-medium">
                        {m.name} {m.user_id === me?.user?.id ? <StatusBadge tone="good">you</StatusBadge> : null}
                      </div>
                      {m.email ? <div className="text-sm text-muted-foreground">{m.email}</div> : null}
                    </TableCell>
                    <TableCell>{m.roles.filter((r) => !r.project).map((r) => r.role).join(", ") || <span className="text-muted-foreground">—</span>}</TableCell>
                    <TableCell>
                      <Access m={m} />
                    </TableCell>
                    <TableCell>
                      <StatusBadge tone={statusTone[m.status]}>{m.status[0]!.toUpperCase() + m.status.slice(1)}</StatusBadge>
                      {m.source === "scim" ? <StatusBadge className="ml-1">Managed by your identity provider</StatusBadge> : null}
                    </TableCell>
                    {manage ? (
                      <TableCell>
                        <MemberActions m={m} lastOwner={owners.length === 1 && owners[0]!.user_id === m.user_id} unreachable={outOfReach(m, roles.data ?? [], me?.permissions ?? [])} />
                      </TableCell>
                    ) : null}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        )}
      </Async>
      {manage ? <Invitations /> : null}
    </div>
  )
}
