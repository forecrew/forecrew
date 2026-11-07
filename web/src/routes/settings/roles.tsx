import { useState } from "react"
import { api } from "@/api/client"
import { keys, useChange, useMe, useRoles } from "@/api/queries"
import type { Role } from "@/api/types"
import { Async } from "@/components/Async"
import { ConfirmDialog } from "@/components/ConfirmDialog"
import { Field } from "@/components/Field"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Textarea } from "@/components/ui/textarea"
import { PermButton, useCan } from "@/lib/permissions"

/** What a permission's first part is about, in the words of the screens. */
const AREAS: Record<string, string> = {
  org: "Organization", billing: "Billing", member: "People", team: "People", role: "People", service_account: "People",
  connection: "Sign-in", token: "Tokens", audit: "Change history", worker: "Machines", pool: "Machines",
  project: "Projects", goal: "Goals", job: "Jobs", gate: "Decisions", council: "Councils", assumption: "Decisions",
  knowledge: "Knowledge", proposal: "Improvements",
}

/** The permissions of a role as lists of what it allows, by area. */
export function capabilities(r: Role): [string, string[]][] {
  const groups = new Map<string, string[]>()
  for (const p of r.permissions) {
    const area = AREAS[p.split(".")[0]!] ?? "Other"
    groups.set(area, [...(groups.get(area) ?? []), p])
  }
  return [...groups].sort(([a], [b]) => a.localeCompare(b))
}

type Scope = Role["scope"]

/**
 * The permissions a role of the scope can hold: those of the built-in roles,
 * which together hold the whole catalog, so the screen needs no copy of it.
 */
export function catalog(roles: Role[], scope: Scope): string[] {
  const all = new Set<string>()
  for (const r of roles) if (r.built_in && (scope === "organization" || r.scope === "project")) r.permissions.forEach((p) => all.add(p))
  return [...all].sort()
}

/** Creates a custom role, or changes one; the server checks No escalation. */
function RoleEditor({ roles, role }: { roles: Role[]; role?: Role }) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState(role?.name ?? "")
  const [scope, setScope] = useState<Scope>(role?.scope ?? "project")
  const [description, setDescription] = useState(role?.description ?? "")
  const [chosen, setChosen] = useState<Set<string>>(new Set(role?.permissions ?? []))
  const [error, setError] = useState("")
  const save = useChange(
    () =>
      role
        ? api.updateRole(role.id ?? role.name, { name: name.trim(), description, permissions: [...chosen].sort() })
        : api.createRole({ name: name.trim(), scope, description, permissions: [...chosen].sort() }),
    keys.roles,
  )
  const held = new Set(useMe().data?.permissions ?? [])
  const title = role ? `Edit ${role.name}` : "New custom role"
  const available = catalog(roles, scope)
  const toggle = (p: string, on: boolean) => {
    const next = new Set(chosen)
    if (on) next.add(p)
    else next.delete(p)
    setChosen(next)
  }
  const groups = capabilities({ name: "", scope, permissions: available, built_in: false, custom: true })

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) save.reset()
      }}
    >
      <DialogTrigger render={<Button variant={role ? "outline" : "default"} aria-label={role ? `Edit role ${role.name}` : undefined} />}>
        {role ? "Edit" : "New custom role"}
      </DialogTrigger>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>
            {role
              ? "The change reaches everyone who holds the role. You can only give permissions you hold yourself."
              : "A custom role holds permissions from the catalog. You can only give permissions you hold yourself."}
          </DialogDescription>
        </DialogHeader>
        <form
          noValidate
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (!/^[a-z0-9][a-z0-9-]*$/.test(name.trim())) {
              setError("Use lowercase letters, digits and -, for example release-manager.")
              return
            }
            if (chosen.size === 0) {
              setError("Choose at least one permission.")
              return
            }
            setError("")
            save.mutate(undefined, { onSuccess: () => setOpen(false) })
          }}
        >
          <Field label="Role name" error={error}>
            {(p) => <Input {...p} value={name} onChange={(e) => setName(e.target.value)} />}
          </Field>
          {role ? null : (
            <Field label="Where it applies" hint="A project role holds project permissions only and can be given on one project.">
              {(p) => (
                <NativeSelect
                  {...p}
                  value={scope}
                  onChange={(e) => {
                    const next = e.target.value as Scope
                    setScope(next)
                    setChosen(new Set([...chosen].filter((x) => catalog(roles, next).includes(x))))
                  }}
                >
                  <NativeSelectOption value="project">On a project</NativeSelectOption>
                  <NativeSelectOption value="organization">On the organization</NativeSelectOption>
                </NativeSelect>
              )}
            </Field>
          )}
          <Field label="Description">{(p) => <Textarea {...p} value={description} onChange={(e) => setDescription(e.target.value)} />}</Field>
          <fieldset className="space-y-3">
            <legend className="text-sm font-medium">Permissions</legend>
            <p className="text-xs text-muted-foreground">You can only give permissions you hold yourself; the others are shown but cannot be chosen.</p>
            {groups.map(([area, perms]) => (
              <div key={area} className="space-y-1">
                <p className="text-sm text-muted-foreground">{area}</p>
                <ul className="grid gap-1 sm:grid-cols-2">
                  {perms.map((perm) => (
                    <li key={perm}>
                      <label className={`flex min-h-8 items-center gap-2 font-mono text-xs ${held.has(perm) ? "" : "text-muted-foreground"}`}>
                        <input
                          type="checkbox"
                          className="size-5"
                          checked={chosen.has(perm)}
                          disabled={!held.has(perm)}
                          onChange={(e) => toggle(perm, e.target.checked)}
                        />
                        {perm}
                        {held.has(perm) ? null : <span className="font-sans"> (you do not hold it)</span>}
                      </label>
                    </li>
                  ))}
                </ul>
              </div>
            ))}
          </fieldset>
          {save.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {save.error.message}
            </p>
          ) : null}
          <DialogFooter>
            <Button type="submit" disabled={save.isPending}>
              {role ? "Save role" : "Create role"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function RoleCard({ role, roles, editable }: { role: Role; roles: Role[]; editable: boolean }) {
  const remove = useChange((r: string) => api.deleteRole(r), keys.roles)
  return (
    <Card role="region" aria-label={`Role ${role.name}`}>
      <CardHeader className="space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="flex-1 font-medium">{role.name}</h3>
          <StatusBadge>{role.custom ? "Custom" : "Built in"}</StatusBadge>
          {editable && role.custom ? (
            <>
              <RoleEditor roles={roles} role={role} />
              <ConfirmDialog
                trigger="Delete"
                triggerAriaLabel={`Delete role ${role.name}`}
                title={`Delete ${role.name}?`}
                description="A role that is given to someone, offered by a pending invitation or given at sign-in cannot be deleted."
                confirmLabel={`Delete ${role.name}`}
                keepLabel="Keep role"
                mutation={remove}
                variables={role.id ?? role.name}
              />
            </>
          ) : null}
        </div>
        {role.description ? <p className="text-sm text-muted-foreground">{role.description}</p> : null}
      </CardHeader>
      <CardContent>
        <details>
          <summary className="cursor-pointer text-sm text-muted-foreground">{role.permissions.length} permissions</summary>
          <dl className="mt-2 space-y-2 text-sm">
            {capabilities(role).map(([area, perms]) => (
              <div key={area}>
                <dt className="font-medium">{area}</dt>
                <dd className="font-mono text-xs text-muted-foreground">{perms.join(", ")}</dd>
              </div>
            ))}
          </dl>
        </details>
      </CardContent>
    </Card>
  )
}

/** Roles of people. Built-in ones are read-only; custom roles come with the enterprise build. */
export function RolesTab() {
  const roles = useRoles()
  const enterprise = Boolean(useMe().data?.enterprise)
  const can = useCan()
  const editable = enterprise && can("role.manage")
  return (
    <Async query={roles} isEmpty={(d) => d.length === 0} empty="No roles.">
      {(items) => (
        <div className="space-y-6">
          {enterprise ? (
            <div className="flex justify-end">{editable ? <RoleEditor roles={items} /> : <PermButton permission="role.manage">New custom role</PermButton>}</div>
          ) : null}
          {(["organization", "project"] as const).map((scope) => (
            <section key={scope} aria-labelledby={`roles-${scope}`} className="space-y-3">
              <h2 id={`roles-${scope}`} className="text-lg font-semibold">
                {scope === "organization" ? "On the organization" : "On a project"}
              </h2>
              <div className="grid gap-3 md:grid-cols-2">
                {items
                  .filter((r) => r.scope === scope)
                  .map((r) => (
                    <RoleCard key={`${scope}-${r.name}`} role={r} roles={items} editable={editable} />
                  ))}
              </div>
            </section>
          ))}
          <p className="text-sm text-muted-foreground">
            {enterprise
              ? "Built-in roles cannot be changed. Custom roles hold permissions you choose from the catalog."
              : "Built-in roles cannot be changed. Custom roles appear here when the enterprise build provides them."}
          </p>
        </div>
      )}
    </Async>
  )
}
