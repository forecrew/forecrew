import { useRoles } from "@/api/queries"
import type { Role } from "@/api/types"
import { Async } from "@/components/Async"
import { StatusBadge } from "@/components/StatusBadge"
import { Card, CardContent, CardHeader } from "@/components/ui/card"

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

function RoleCard({ role }: { role: Role }) {
  return (
    <Card role="region" aria-label={`Role ${role.name}`}>
      <CardHeader className="space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="font-medium">{role.name}</h3>
          <StatusBadge>{role.custom ? "Custom" : "Built in"}</StatusBadge>
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
  return (
    <Async query={roles} isEmpty={(d) => d.length === 0} empty="No roles.">
      {(items) => (
        <div className="space-y-6">
          {(["organization", "project"] as const).map((scope) => (
            <section key={scope} aria-labelledby={`roles-${scope}`} className="space-y-3">
              <h2 id={`roles-${scope}`} className="text-lg font-semibold">
                {scope === "organization" ? "On the organization" : "On a project"}
              </h2>
              <div className="grid gap-3 md:grid-cols-2">
                {items
                  .filter((r) => r.scope === scope)
                  .map((r) => (
                    <RoleCard key={`${scope}-${r.name}`} role={r} />
                  ))}
              </div>
            </section>
          ))}
          <p className="text-sm text-muted-foreground">Built-in roles cannot be changed. Custom roles appear here when the enterprise build provides them.</p>
        </div>
      )}
    </Async>
  )
}
