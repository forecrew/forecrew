import { useState } from "react"
import { api } from "@/api/client"
import { keys, useBindings, useChange, useMembers, useTeamMembers, useTeams } from "@/api/queries"
import type { RoleBinding, Team } from "@/api/types"
import { Async } from "@/components/Async"
import { ConfirmDialog } from "@/components/ConfirmDialog"
import { Field } from "@/components/Field"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { PermButton, useCan } from "@/lib/permissions"
import { AddRole } from "./bindings"

function CreateTeam() {
  const can = useCan()
  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [error, setError] = useState("")
  const create = useChange((n: string) => api.createTeam({ name: n }), keys.teams)
  if (!can("team.manage")) return <PermButton permission="team.manage">New team</PermButton>
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) create.reset()
      }}
    >
      <DialogTrigger render={<Button />}>New team</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New team</DialogTitle>
          <DialogDescription>A team gives several people the same access.</DialogDescription>
        </DialogHeader>
        <form
          noValidate
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault()
            if (!name.trim()) {
              setError("Give the team a name.")
              return
            }
            setError("")
            create.mutate(name.trim(), {
              onSuccess: () => {
                setOpen(false)
                setName("")
              },
            })
          }}
        >
          <Field label="Team name" error={error}>
            {(p) => <Input {...p} value={name} onChange={(e) => setName(e.target.value)} />}
          </Field>
          {create.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {create.error.message}
            </p>
          ) : null}
          <DialogFooter>
            <Button type="submit" disabled={create.isPending}>
              Create team
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function roleText(b: RoleBinding) {
  return b.project ? `${b.role} on ${b.project}` : `${b.role} on the organization`
}

function TeamCard({ team }: { team: Team }) {
  const can = useCan()
  const manage = can("team.manage") && team.source === "manual"
  const members = useMembers()
  const teamMembers = useTeamMembers(team.slug)
  const bindings = useBindings()
  const [user, setUser] = useState("")
  const add = useChange((u: string) => api.addTeamMember(team.slug, u), keys.teamMembers(team.slug), keys.members)
  const drop = useChange((u: string) => api.removeTeamMember(team.slug, u), keys.teamMembers(team.slug), keys.members)
  const unbind = useChange((id: string) => api.deleteBinding(id), ["bindings"], keys.members)
  const remove = useChange((slug: string) => api.deleteTeam(slug), keys.teams)
  const nameOf = (id: string) => members.data?.find((m) => m.user_id === id)?.name ?? id
  const inTeam = new Set((teamMembers.data ?? []).map((m) => m.user_id))
  const candidates = (members.data ?? []).filter((m) => !inTeam.has(m.user_id) && m.status === "active")
  const roles = (bindings.data ?? []).filter((b) => b.subject_kind === "team" && b.subject_id === team.id)
  const chosen = user || candidates[0]?.user_id || ""

  return (
    <Card role="region" aria-label={`Team ${team.name}`}>
      <CardHeader className="flex flex-row flex-wrap items-center gap-2">
        <h3 className="flex-1 font-medium">{team.name}</h3>
        <span className="font-mono text-sm text-muted-foreground">{team.slug}</span>
        {team.source !== "manual" ? <StatusBadge>Managed by your identity provider</StatusBadge> : null}
        {can("team.manage") && team.source === "manual" ? (
          <ConfirmDialog
            trigger="Delete"
            triggerAriaLabel={`Delete team ${team.name}`}
            title={`Delete ${team.name}?`}
            description="Its members lose the roles they had through it."
            confirmLabel={`Delete ${team.name}`}
            keepLabel="Keep team"
            mutation={remove}
            variables={team.slug}
          />
        ) : null}
      </CardHeader>
      <CardContent className="space-y-4">
        <div>
          <h4 className="mb-1 text-sm font-medium">Members</h4>
          <Async query={teamMembers} isEmpty={(d) => d.length === 0} empty="No one is in this team yet.">
            {(items) => (
              <ul className="space-y-1 text-sm">
                {items.map((m) => (
                  <li key={m.user_id} className="flex items-center gap-2">
                    <span className="flex-1">{nameOf(m.user_id)}</span>
                    {manage ? (
                      <Button variant="ghost" aria-label={`Remove ${nameOf(m.user_id)} from ${team.name}`} onClick={() => drop.mutate(m.user_id)} disabled={drop.isPending}>
                        Remove
                      </Button>
                    ) : null}
                  </li>
                ))}
              </ul>
            )}
          </Async>
          {manage && candidates.length > 0 ? (
            <form
              aria-label={`Add a member to ${team.name}`}
              className="mt-2 flex items-end gap-2"
              onSubmit={(e) => {
                e.preventDefault()
                add.mutate(chosen)
              }}
            >
              <Field label="Person">
                {(p) => (
                  <NativeSelect {...p} value={chosen} onChange={(e) => setUser(e.target.value)}>
                    {candidates.map((m) => (
                      <NativeSelectOption key={m.user_id} value={m.user_id}>
                        {m.name}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                )}
              </Field>
              <Button type="submit" variant="outline" disabled={add.isPending}>
                Add to team
              </Button>
            </form>
          ) : null}
          {add.isError || drop.isError ? (
            <p role="alert" className="mt-2 text-sm text-destructive">
              {(add.error ?? drop.error)?.message}
            </p>
          ) : null}
        </div>
        <div>
          <h4 className="mb-1 text-sm font-medium">Access</h4>
          {roles.length === 0 ? (
            <p className="text-sm text-muted-foreground">This team has no roles yet.</p>
          ) : (
            <ul className="space-y-1 text-sm">
              {roles.map((b) => (
                <li key={b.id} className="flex items-center gap-2">
                  <span className="flex-1">{roleText(b)}</span>
                  {manage ? (
                    <Button variant="ghost" aria-label={`Remove ${roleText(b)} from ${team.name}`} onClick={() => unbind.mutate(b.id)} disabled={unbind.isPending}>
                      Remove
                    </Button>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
          {manage ? <div className="mt-2"><AddRole subjectKind="team" subject={team.slug} label={`Add a role to ${team.name}`} allowOrganization /></div> : null}
          {unbind.isError ? (
            <p role="alert" className="mt-2 text-sm text-destructive">
              {unbind.error.message}
            </p>
          ) : null}
        </div>
      </CardContent>
    </Card>
  )
}

export function TeamsTab() {
  const teams = useTeams()
  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <CreateTeam />
      </div>
      <Async query={teams} isEmpty={(d) => d.length === 0} empty="Teams give several people the same access. You don't need one until you do.">
        {(items) => (
          <div className="space-y-4">
            {items.map((t) => (
              <TeamCard key={t.id} team={t} />
            ))}
          </div>
        )}
      </Async>
    </div>
  )
}
