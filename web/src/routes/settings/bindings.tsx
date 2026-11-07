import { useState } from "react"
import { api } from "@/api/client"
import { keys, useChange, useProjects, useRoles } from "@/api/queries"
import type { Role } from "@/api/types"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"

/** The built-in project roles, for a caller who may not list roles. */
export const PROJECT_ROLES = ["project_admin", "maintainer", "contributor", "viewer"]

/** A role binding form: a role, and for a project role the project it applies to (or all). */
export function AddRole({
  subjectKind, subject, label, project, allowOrganization, canList = true,
}: {
  subjectKind: "user" | "team" | "service_account"
  subject: string
  /** Names the form for screen readers: "Add a role to Mobile". */
  label: string
  /** Fixed project: a project page adds access to that project only. */
  project?: string
  /** Offer organization roles too. */
  allowOrganization?: boolean
  canList?: boolean
}) {
  const roles = useRoles(canList)
  const projects = useProjects()
  const [role, setRole] = useState("")
  const [target, setTarget] = useState(project ?? "")
  const add = useChange((v: { role: string; project?: string }) => api.createBinding({ subject_kind: subjectKind, subject, ...v }), ["bindings"], keys.members)
  const options: Pick<Role, "name" | "scope">[] = canList
    ? (roles.data ?? []).filter((r) => allowOrganization || r.scope === "project")
    : PROJECT_ROLES.map((name) => ({ name, scope: "project" as const }))
  // A role name can exist at both scopes (viewer), so the choice is scope and name.
  const keyOf = (r: Pick<Role, "name" | "scope">) => `${r.scope}:${r.name}`
  const chosen = role || (options[0] ? keyOf(options[0]) : "")
  const picked = options.find((r) => keyOf(r) === chosen)
  const scope = picked?.scope

  return (
    <form
      aria-label={label}
      className="flex flex-wrap items-end gap-2"
      onSubmit={(e) => {
        e.preventDefault()
        add.mutate({ role: picked?.name ?? "", ...(scope === "project" && (project ?? target) ? { project: project ?? target } : {}) })
      }}
    >
      <Field label="Role">
        {(p) => (
          <NativeSelect {...p} value={chosen} onChange={(e) => setRole(e.target.value)}>
            {options.map((r) => (
              <NativeSelectOption key={keyOf(r)} value={keyOf(r)}>
                {r.name} ({r.scope === "project" ? "project role" : "organization"})
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </Field>
      {project === undefined ? (
        <Field label="On project">
          {(p) => (
            <NativeSelect {...p} value={target} disabled={scope !== "project"} onChange={(e) => setTarget(e.target.value)}>
              <NativeSelectOption value="">All projects</NativeSelectOption>
              {(projects.data ?? []).map((x) => (
                <NativeSelectOption key={x.id} value={x.name}>
                  {x.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </Field>
      ) : null}
      <Button type="submit" variant="outline" disabled={add.isPending || !chosen}>
        Add role
      </Button>
      {add.isError ? (
        <p role="alert" className="w-full text-sm text-destructive">
          {add.error.message}
        </p>
      ) : null}
    </form>
  )
}
