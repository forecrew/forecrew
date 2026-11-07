import { useState } from "react"
import { api } from "@/api/client"
import { keys, useBindings, useChange, useMembers, useRoles, useServiceAccounts, useTeams } from "@/api/queries"
import type { Project, RoleBinding } from "@/api/types"
import { Async } from "@/components/Async"
import { ConfirmDialog } from "@/components/ConfirmDialog"
import { Field } from "@/components/Field"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { useCan } from "@/lib/permissions"
import { PROJECT_ROLES } from "@/routes/settings/bindings"

type Kind = "user" | "team" | "service_account"
const KIND_LABEL: Record<Kind, string> = { user: "Person", team: "Team", service_account: "Service account" }

/** "direct", or "through team Mobile". */
const sourceOf = (b: RoleBinding) => (b.subject_kind === "team" ? `through team ${b.subject_name ?? ""}`.trim() : "direct")

function RemoveBinding({ binding }: { binding: RoleBinding }) {
  const remove = useChange((id: string) => api.deleteBinding(id), ["bindings"], keys.members)
  return (
    <ConfirmDialog
      trigger="Remove"
      triggerAriaLabel={`Remove ${binding.role} of ${binding.subject_name ?? binding.subject_id}`}
      title={`Remove ${binding.role} of ${binding.subject_name ?? "this subject"}?`}
      description="They lose this access to the project. Access they have another way stays."
      confirmLabel="Remove access"
      keepLabel="Keep access"
      mutation={remove}
      variables={binding.id}
    />
  )
}

/** Gives a person, team or service account a role on the project. Choosing needs permission to see the organization's people. */
function AddAccess({ project, canList }: { project: Project; canList: boolean }) {
  const [kind, setKind] = useState<Kind>("user")
  const [subject, setSubject] = useState("")
  const [role, setRole] = useState("")
  const members = useMembers(canList)
  const teams = useTeams(canList)
  const accounts = useServiceAccounts(canList)
  const roles = useRoles(canList)
  const add = useChange((v: { subject_kind: Kind; subject: string; role: string }) => api.createBinding({ ...v, project: project.name }), ["bindings"], keys.members)
  if (!canList) {
    return <p className="text-sm text-muted-foreground">To add people here you need to see the organization&apos;s people (the org.read permission).</p>
  }
  const options: { value: string; label: string }[] =
    kind === "user"
      ? (members.data ?? []).filter((m) => m.status === "active").map((m) => ({ value: m.user_id, label: m.name }))
      : kind === "team"
        ? (teams.data ?? []).map((t) => ({ value: t.slug, label: t.name }))
        : (accounts.data ?? []).filter((a) => !a.disabled_at).map((a) => ({ value: a.name, label: a.name }))
  const roleNames = roles.data ? roles.data.filter((r) => r.scope === "project").map((r) => r.name) : PROJECT_ROLES
  const chosenSubject = subject || options[0]?.value || ""
  const chosenRole = role || roleNames[0] || ""

  return (
    <form
      aria-label="Add access"
      className="flex flex-wrap items-end gap-2"
      onSubmit={(e) => {
        e.preventDefault()
        add.mutate({ subject_kind: kind, subject: chosenSubject, role: chosenRole })
      }}
    >
      <Field label="Add">
        {(p) => (
          <NativeSelect {...p} value={kind} onChange={(e) => { setKind(e.target.value as Kind); setSubject("") }}>
            {(Object.keys(KIND_LABEL) as Kind[]).map((k) => (
              <NativeSelectOption key={k} value={k}>
                {KIND_LABEL[k]}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </Field>
      <Field label="Who">
        {(p) => (
          <NativeSelect {...p} value={chosenSubject} onChange={(e) => setSubject(e.target.value)}>
            {options.map((o) => (
              <NativeSelectOption key={o.value} value={o.value}>
                {o.label}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </Field>
      <Field label="Role on this project">
        {(p) => (
          <NativeSelect {...p} value={chosenRole} onChange={(e) => setRole(e.target.value)}>
            {roleNames.map((r) => (
              <NativeSelectOption key={r} value={r}>
                {r}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </Field>
      <Button type="submit" variant="outline" disabled={add.isPending || !chosenSubject}>
        Add access
      </Button>
      {add.isError ? (
        <p role="alert" className="w-full text-sm text-destructive">
          {add.error.message}
        </p>
      ) : null}
    </form>
  )
}

/**
 * Who has which role on a project, directly or through a team, and, for
 * someone who manages the project, adding and removing. Owners and Admins of
 * the organization reach every project through their organization role.
 */
export function ProjectAccess({ project }: { project: Project }) {
  const can = useCan()
  const manage = can("project.manage", project.id)
  const orgRead = can("org.read")
  const bindings = useBindings(project.name)
  const orgBindings = useBindings(undefined, orgRead)
  const viaOrganization = (orgBindings.data ?? []).filter((b) => !b.project && b.role_scope === "organization" && ["owner", "admin"].includes(b.role))

  return (
    <section aria-labelledby="access" className="mt-8 space-y-3">
      <h2 id="access" className="text-lg font-semibold">
        Access
      </h2>
      <Async query={bindings} isEmpty={(d) => d.length === 0 && viaOrganization.length === 0} empty="Only organization admins can see this project.">
        {(items) => (
          <TableWrap label="Access">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Who</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead>How</TableHead>
                  {manage ? <TableHead>Actions</TableHead> : null}
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((b) => (
                  <TableRow key={b.id}>
                    <TableCell>
                      {b.subject_name ?? b.subject_id} <StatusBadge>{KIND_LABEL[b.subject_kind]}</StatusBadge>
                    </TableCell>
                    <TableCell>{b.role}</TableCell>
                    <TableCell>{sourceOf(b)}</TableCell>
                    {manage ? (
                      <TableCell>
                        <RemoveBinding binding={b} />
                      </TableCell>
                    ) : null}
                  </TableRow>
                ))}
                {viaOrganization.map((b) => (
                  <TableRow key={b.id}>
                    <TableCell>
                      {b.subject_name ?? b.subject_id} <StatusBadge>{KIND_LABEL[b.subject_kind]}</StatusBadge>
                    </TableCell>
                    <TableCell>{b.role}</TableCell>
                    <TableCell>through organization role</TableCell>
                    {manage ? <TableCell /> : null}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        )}
      </Async>
      {manage ? <AddAccess project={project} canList={orgRead} /> : null}
    </section>
  )
}
